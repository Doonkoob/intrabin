// Package api implements the versioned HTTP API (/api/v1) and serves the web UI.
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/audit"
	"github.com/intrabin/intrabin/internal/auth"
	"github.com/intrabin/intrabin/internal/config"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/metrics"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/monitoring"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server holds API dependencies.
type Server struct {
	cfg       *config.Config
	store     *storage.Store
	node      *node.Node
	engine    *monitoring.Engine
	prober    *monitoring.Prober
	incidents *incidents.Manager
	audit     *audit.Logger
	box       *security.SecretBox
	limiter   *auth.LoginLimiter
	log       *slog.Logger
	web       http.Handler
	argon     security.Argon2Params

	setupMu    sync.Mutex
	setupToken string
}

// Deps bundles constructor arguments.
type Deps struct {
	Config     *config.Config
	Store      *storage.Store
	Node       *node.Node
	Engine     *monitoring.Engine
	Prober     *monitoring.Prober
	Incidents  *incidents.Manager
	Audit      *audit.Logger
	SecretBox  *security.SecretBox
	Log        *slog.Logger
	Web        http.Handler
	SetupToken string
	Argon2     *security.Argon2Params
}

func New(d Deps) *Server {
	s := &Server{cfg: d.Config, store: d.Store, node: d.Node, engine: d.Engine, prober: d.Prober, incidents: d.Incidents,
		audit: d.Audit, box: d.SecretBox, limiter: auth.NewLoginLimiter(), log: d.Log, web: d.Web, setupToken: d.SetupToken, argon: security.DefaultArgon2}
	if d.Argon2 != nil {
		s.argon = *d.Argon2
	}
	return s
}

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxPrincipal
)

func requestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// Principal is the authenticated user of a request.
type Principal struct {
	User    *model.User
	Session *model.Session
	IP      string
}

func principal(r *http.Request) *Principal {
	p, _ := r.Context().Value(ctxPrincipal).(*Principal)
	return p
}

func (p *Principal) actor() audit.Actor {
	return audit.Actor{ID: p.User.ID, Name: p.User.Username, IP: p.IP}
}

// Handler builds the full HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	type route struct {
		pattern string
		h       http.HandlerFunc
	}
	public := []route{
		{"GET /api/v1/health", s.handleHealth},
		{"GET /api/v1/readiness", s.handleReadiness},
		{"GET /api/v1/setup/status", s.handleSetupStatus},
		{"POST /api/v1/setup", s.handleSetup},
		{"POST /api/v1/auth/login", s.handleLogin},
		{"GET /api/v1/openapi.yaml", s.handleOpenAPI},
	}
	for _, rt := range public {
		mux.HandleFunc(rt.pattern, rt.h)
	}
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// authenticated routes: perm, requiresController
	type aroute struct {
		pattern string
		perm    auth.Permission
		mgmt    bool
		h       http.HandlerFunc
	}
	authed := []aroute{
		{"POST /api/v1/auth/logout", "", false, s.handleLogout},
		{"GET /api/v1/auth/me", "", false, s.handleMe},
		{"POST /api/v1/auth/password", "", false, s.handleChangePassword},
		{"PATCH /api/v1/auth/profile", "", false, s.handleUpdateProfile},

		{"GET /api/v1/node", auth.PermView, false, s.handleNode},
		{"GET /api/v1/cluster", auth.PermView, false, s.handleCluster},

		{"GET /api/v1/overview", auth.PermView, true, s.handleOverview},

		{"GET /api/v1/users", auth.PermUserManage, true, s.handleListUsers},
		{"POST /api/v1/users", auth.PermUserManage, true, s.handleCreateUser},
		{"PATCH /api/v1/users/{id}", auth.PermUserManage, true, s.handleUpdateUser},
		{"POST /api/v1/users/{id}/password", auth.PermUserManage, true, s.handleResetUserPassword},
		{"DELETE /api/v1/users/{id}", auth.PermUserManage, true, s.handleDeleteUser},

		{"GET /api/v1/monitor-groups", auth.PermView, true, s.handleListGroups},
		{"POST /api/v1/monitor-groups", auth.PermMonitorManage, true, s.handleCreateGroup},
		{"PATCH /api/v1/monitor-groups/{id}", auth.PermMonitorManage, true, s.handleUpdateGroup},
		{"DELETE /api/v1/monitor-groups/{id}", auth.PermMonitorManage, true, s.handleDeleteGroup},

		{"GET /api/v1/monitors", auth.PermView, true, s.handleListMonitors},
		{"POST /api/v1/monitors", auth.PermMonitorManage, true, s.handleCreateMonitor},
		{"POST /api/v1/monitors/test", auth.PermMonitorManage, true, s.handleTestMonitor},
		{"GET /api/v1/monitors/{id}", auth.PermView, true, s.handleGetMonitor},
		{"PUT /api/v1/monitors/{id}", auth.PermMonitorManage, true, s.handleUpdateMonitor},
		{"DELETE /api/v1/monitors/{id}", auth.PermMonitorManage, true, s.handleDeleteMonitor},
		{"POST /api/v1/monitors/{id}/pause", auth.PermMonitorManage, true, s.handlePauseMonitor},
		{"POST /api/v1/monitors/{id}/resume", auth.PermMonitorManage, true, s.handleResumeMonitor},
		{"GET /api/v1/monitors/{id}/results", auth.PermView, true, s.handleMonitorResults},
		{"GET /api/v1/monitors/{id}/uptime", auth.PermView, true, s.handleMonitorUptime},
		{"GET /api/v1/monitors/{id}/stats", auth.PermView, true, s.handleMonitorStats},

		{"GET /api/v1/incidents", auth.PermView, true, s.handleListIncidents},
		{"GET /api/v1/incidents/{id}", auth.PermView, true, s.handleGetIncident},
		{"POST /api/v1/incidents/{id}/acknowledge", auth.PermIncidentRespond, true, s.handleAckIncident},
		{"POST /api/v1/incidents/{id}/resolve", auth.PermIncidentRespond, true, s.handleResolveIncident},
		{"POST /api/v1/incidents/{id}/notes", auth.PermIncidentRespond, true, s.handleIncidentNote},

		{"GET /api/v1/maintenance-windows", auth.PermView, true, s.handleListMaintenance},
		{"POST /api/v1/maintenance-windows", auth.PermMonitorManage, true, s.handleCreateMaintenance},
		{"DELETE /api/v1/maintenance-windows/{id}", auth.PermMonitorManage, true, s.handleDeleteMaintenance},

		{"GET /api/v1/audit-logs", auth.PermAuditView, true, s.handleListAudit},
		{"GET /api/v1/settings", auth.PermView, true, s.handleGetSettings},
		{"PATCH /api/v1/settings", auth.PermSettingsManage, true, s.handleUpdateSettings},
	}
	for _, rt := range authed {
		mux.Handle(rt.pattern, s.requireAuth(rt.perm, rt.mgmt, rt.h))
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { s.writeErr(w, r, errNotFound) })
	if s.web != nil {
		mux.Handle("/", s.web)
	}
	return s.middleware(mux)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-ID")
		if rid == "" || len(rid) > 64 {
			rid = uuid.NewString()
		}
		ctx := context.WithValue(r.Context(), ctxRequestID, rid)
		r = r.WithContext(ctx)
		h := w.Header()
		h.Set("X-Request-ID", rid)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic", "request_id", rid, "panic", v, "stack", string(debug.Stack()))
				if rec.status == 0 {
					s.writeErr(rec, r, &APIError{Status: 500, Code: "internal", Message: "Internal server error. Request ID: " + rid})
				}
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				metrics.HTTPRequests.WithLabelValues(r.Method, strconv.Itoa(rec.status/100)+"xx").Inc()
				s.log.Debug("http", "request_id", rid, "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		if !s.hostAllowed(r) {
			s.writeErr(rec, r, &APIError{Status: http.StatusMisdirectedRequest, Code: "host_not_allowed", Message: "Host not allowed."})
			return
		}
		next.ServeHTTP(rec, r)
	})
}

// hostAllowed mitigates DNS rebinding when INTRABIN_ALLOWED_HOSTS is configured.
func (s *Server) hostAllowed(r *http.Request) bool {
	if len(s.cfg.AllowedHosts) == 0 {
		return true
	}
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	for _, a := range s.cfg.AllowedHosts {
		if a == host {
			return true
		}
	}
	return false
}

// clientIP returns the client address, honouring X-Forwarded-For only from trusted proxies.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !s.trusted(addr) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		a, err := netip.ParseAddr(p)
		if err != nil {
			break
		}
		if !s.trusted(a) {
			return a.String()
		}
	}
	return host
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

func (s *Server) isSecureRequest(r *http.Request) bool {
	if s.cfg.CookieSecure != nil {
		return *s.cfg.CookieSecure
	}
	if r.TLS != nil {
		return true
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if a, err := netip.ParseAddr(host); err == nil && s.trusted(a) {
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}
	return false
}

// sameOrigin rejects cross-origin unsafe requests when an Origin header is present.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	allowed := s.cfg.MetricsPublic
	if !allowed && s.cfg.MetricsToken != "" {
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && security.ConstantTimeEqual(tok, s.cfg.MetricsToken) {
			allowed = true
		}
	}
	if !allowed {
		if p, err := s.authenticate(r); err == nil && auth.Can(p.User.Role, auth.PermAuditView) {
			allowed = true
		}
	}
	if !allowed {
		s.writeErr(w, r, errUnauthorized)
		return
	}
	s.refreshGauges(r.Context())
	promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP(w, r)
}

func (s *Server) refreshGauges(ctx context.Context) {
	states, err := s.store.ListMonitorStates(ctx)
	if err == nil {
		counts := map[string]float64{}
		for _, st := range states {
			counts[string(st.Status)]++
		}
		for _, st := range []model.MonitorStatus{model.StatusUp, model.StatusDown, model.StatusDegraded, model.StatusSuspectedDown, model.StatusUnknown, model.StatusPaused, model.StatusMaintenance} {
			metrics.MonitorsByStatus.WithLabelValues(string(st)).Set(counts[string(st)])
		}
	}
	if open, err := s.store.ListIncidents(ctx, storage.IncidentFilter{OpenOnly: true, Limit: 500}); err == nil {
		metrics.OpenIncidents.Set(float64(len(open)))
	}
	metrics.StorageBytes.Set(float64(s.store.SizeBytes()))
	for _, r := range []node.Role{node.RoleStandalone, node.RoleFollower, node.RoleLeader, node.RoleNoQuorum} {
		v := 0.0
		if s.node.Role() == r {
			v = 1
		}
		metrics.ClusterRole.WithLabelValues(string(r)).Set(v)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "version": node.Version, "node_id": s.node.Identity.ID, "role": s.node.Role()})
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.DB.PingContext(ctx); err != nil {
		writeJSON(w, 503, map[string]any{"status": "unavailable", "checks": map[string]string{"database": "unreachable"}})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "checks": map[string]string{"database": "ok"}, "role": s.node.Role()})
}
