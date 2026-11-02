package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/monitoring"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

// monitorInput is the create/update payload. Secrets are write-only.
type monitorInput struct {
	Name            string                `json:"name"`
	Description     string                `json:"description"`
	Type            model.MonitorType     `json:"type"`
	Target          string                `json:"target"`
	GroupID         string                `json:"group_id"`
	Tags            []string              `json:"tags"`
	IntervalSeconds int                   `json:"interval_seconds"`
	TimeoutSeconds  int                   `json:"timeout_seconds"`
	Retries         int                   `json:"retries"`
	Enabled         *bool                 `json:"enabled"`
	Settings        model.MonitorSettings `json:"settings"`
	Policy          *model.Policy         `json:"policy"`
	NodeIDs         []string              `json:"node_ids"`
	Secrets         *model.MonitorSecrets `json:"secrets"`
	ClearSecrets    bool                  `json:"clear_secrets"`
}

var headerNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
var hostnameRe = regexp.MustCompile(`^(?i)([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.)*[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9])?\.?$`)

func validHostPort(v string, requirePort bool) bool {
	host, port, err := net.SplitHostPort(v)
	if err != nil {
		if requirePort {
			return false
		}
		host = v
	} else {
		p, err := strconv.Atoi(port)
		if err != nil || p < 1 || p > 65535 {
			return false
		}
	}
	return net.ParseIP(host) != nil || hostnameRe.MatchString(host)
}

// normalize validates input, applies defaults and returns field errors.
func (s *Server) normalizeMonitor(ctx context.Context, in *monitorInput) map[string]string {
	f := map[string]string{}
	in.Name = strings.TrimSpace(in.Name)
	in.Target = strings.TrimSpace(in.Target)
	if in.Name == "" || len(in.Name) > 120 {
		f["name"] = "Name is required (max 120 characters)."
	}
	if len(in.Description) > 2000 {
		f["description"] = "At most 2000 characters."
	}
	if in.Type == "" {
		in.Type = model.MonitorHTTP
	}
	if !in.Type.Valid() {
		f["type"] = "Must be http, tcp, dns or tls."
	}
	switch in.Type {
	case model.MonitorHTTP:
		u, err := url.Parse(in.Target)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			f["target"] = "Enter a full URL starting with http:// or https://."
		} else if u.User != nil {
			f["target"] = "Do not embed credentials in the URL; use the basic authentication fields."
		}
	case model.MonitorTCP:
		if !validHostPort(in.Target, true) {
			f["target"] = "Enter host:port, e.g. db.internal:5432."
		}
	case model.MonitorTLS:
		if !validHostPort(in.Target, false) {
			f["target"] = "Enter host or host:port (default port 443)."
		}
	case model.MonitorDNS:
		if !hostnameRe.MatchString(in.Target) {
			f["target"] = "Enter a hostname to resolve."
		}
		rt := strings.ToUpper(in.Settings.RecordType)
		if rt == "" {
			rt = "A"
		}
		switch rt {
		case "A", "AAAA", "CNAME", "MX", "TXT", "NS":
		default:
			f["settings.record_type"] = "Must be A, AAAA, CNAME, MX, TXT or NS."
		}
		in.Settings.RecordType = rt
		if in.Settings.Resolver != "" && !validHostPort(in.Settings.Resolver, false) {
			f["settings.resolver"] = "Enter an IP address or ip:port."
		}
	}
	if len(in.Tags) > 20 {
		f["tags"] = "At most 20 tags."
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, t := range in.Tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		if len(t) > 40 {
			f["tags"] = "Each tag must be at most 40 characters."
		}
		seen[t] = true
		clean = append(clean, t)
	}
	in.Tags = clean
	if in.IntervalSeconds == 0 {
		in.IntervalSeconds = 60
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = max(1, min(10, in.IntervalSeconds/3))
	}
	minInterval := int(s.cfg.MinInterval.Seconds())
	if in.IntervalSeconds < minInterval || in.IntervalSeconds > 86400 {
		f["interval_seconds"] = fmt.Sprintf("Must be between %d and 86400 seconds.", minInterval)
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 60 {
		f["timeout_seconds"] = "Must be between 1 and 60 seconds."
	}
	if in.Retries < 0 || in.Retries > 5 {
		f["retries"] = "Must be between 0 and 5."
	}
	// A probe including retries must finish inside its round (after the per-monitor start offset).
	budget := in.TimeoutSeconds*(in.Retries+1) + in.Retries
	maxOffset := min(in.IntervalSeconds/4, 10)
	if _, bad := f["interval_seconds"]; !bad && budget > in.IntervalSeconds-maxOffset {
		f["timeout_seconds"] = fmt.Sprintf("Timeout × attempts (%ds) must fit inside the check interval (max %ds). Reduce timeout or retries.", budget, in.IntervalSeconds-maxOffset)
	}
	st := &in.Settings
	if in.Type == model.MonitorHTTP {
		st.Method = strings.ToUpper(st.Method)
		if st.Method == "" {
			st.Method = "GET"
		}
		switch st.Method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		default:
			f["settings.method"] = "Unsupported HTTP method."
		}
		for _, p := range st.ExpectedStatus {
			if !monitoring.ValidStatusPattern(p) {
				f["settings.expected_status"] = "Use codes like 200, ranges like 200-299 or classes like 2xx."
			}
		}
		if len(st.Body) > 64*1024 || len(st.ExpectedBody) > 4096 {
			f["settings.body"] = "Body too large."
		}
		// Sensitive headers are moved to encrypted secrets automatically.
		for k, v := range st.Headers {
			if !headerNameRe.MatchString(k) || strings.ContainsAny(v, "\r\n") {
				f["settings.headers"] = "Invalid header name or value."
				continue
			}
			if security.IsSensitiveKey(k) {
				if in.Secrets == nil {
					in.Secrets = &model.MonitorSecrets{}
				}
				if in.Secrets.SecretHeaders == nil {
					in.Secrets.SecretHeaders = map[string]string{}
				}
				in.Secrets.SecretHeaders[textproto.CanonicalMIMEHeaderKey(k)] = v
				delete(st.Headers, k)
			}
		}
		if st.OriginAddress != "" && !validHostPort(st.OriginAddress, false) {
			f["settings.origin_address"] = "Enter an IP or ip:port of the origin server."
		}
		if st.MaxRedirects < 0 || st.MaxRedirects > 20 {
			f["settings.max_redirects"] = "Must be between 0 and 20."
		}
	} else {
		st.Method, st.Headers, st.Body, st.ExpectedStatus, st.ExpectedBody, st.FollowRedirects = "", nil, "", nil, "", nil
	}
	if in.Secrets != nil {
		for k, v := range in.Secrets.SecretHeaders {
			if !headerNameRe.MatchString(k) || strings.ContainsAny(v, "\r\n") {
				f["secrets.secret_headers"] = "Invalid header name or value."
			}
		}
	}
	switch st.CheckPath {
	case "", "public", "origin", "internal":
	default:
		f["settings.check_path"] = "Must be public, origin or internal."
	}
	if st.DegradedLatencyMs < 0 || st.CertExpiryFailDays < 0 || st.CertExpiryWarnDays < 0 {
		f["settings"] = "Thresholds must not be negative."
	}
	if in.Policy == nil {
		p := model.DefaultPolicy()
		in.Policy = &p
	}
	p := in.Policy
	if p.FailMode == "" {
		p.FailMode = model.FailConsecutive
	}
	if p.ConfirmationsRequired < 1 || p.ConfirmationsRequired > 50 {
		f["policy.confirmations_required"] = "Must be between 1 and 50."
	}
	if p.MinLocations < 1 || p.MinLocations > 50 {
		f["policy.min_locations"] = "Must be between 1 and 50."
	}
	if p.MinValidProbes < 1 || p.MinValidProbes > 50 {
		f["policy.min_valid_probes"] = "Must be between 1 and 50."
	}
	if p.FailureRounds < 1 || p.FailureRounds > 100 {
		f["policy.failure_rounds"] = "Must be between 1 and 100."
	}
	if p.RecoveryRounds < 1 || p.RecoveryRounds > 100 {
		f["policy.recovery_rounds"] = "Must be between 1 and 100."
	}
	if p.CooldownSeconds < 0 || p.CooldownSeconds > 86400 {
		f["policy.cooldown_seconds"] = "Must be between 0 and 86400."
	}
	switch p.FailMode {
	case model.FailConsecutive:
		p.WindowSeconds = 0
	case model.FailWindow:
		if p.WindowSeconds < in.IntervalSeconds*p.FailureRounds {
			f["policy.window_seconds"] = fmt.Sprintf("Window must be at least interval × failure rounds (%ds).", in.IntervalSeconds*p.FailureRounds)
		}
	default:
		f["policy.fail_mode"] = "Must be consecutive or window."
	}
	if p.ConfirmationsRequired > p.MinValidProbes {
		// confirmations can never be reached with fewer valid probes; make it explicit
		p.MinValidProbes = p.ConfirmationsRequired
	}
	if in.GroupID != "" {
		if _, err := s.store.GetGroup(ctx, in.GroupID); err != nil {
			f["group_id"] = "Unknown group."
		}
	}
	known := map[string]bool{s.node.Identity.ID: true}
	if cn, err := s.store.ListClusterNodes(ctx); err == nil {
		for _, n := range cn {
			known[n.ID] = true
		}
	}
	nodes := []string{}
	seenNode := map[string]bool{}
	for _, id := range in.NodeIDs {
		if !known[id] {
			f["node_ids"] = "Unknown monitoring node."
		}
		if !seenNode[id] {
			seenNode[id] = true
			nodes = append(nodes, id)
		}
	}
	in.NodeIDs = nodes
	return f
}

// monitorView is the API representation of a monitor.
type monitorView struct {
	*model.Monitor
	State     *model.MonitorState `json:"state,omitempty"`
	Uptime24h *float64            `json:"uptime_24h"`
	GroupName string              `json:"group_name,omitempty"`
}

func secretKeys(sec *model.MonitorSecrets) []string {
	keys := []string{}
	if sec == nil {
		return keys
	}
	if sec.BasicAuthUser != "" {
		keys = append(keys, "basic_auth")
	}
	for k := range sec.SecretHeaders {
		keys = append(keys, "header:"+k)
	}
	sort.Strings(keys)
	return keys
}

func (s *Server) sealSecrets(id string, sec *model.MonitorSecrets) ([]byte, error) {
	if sec == nil || (sec.BasicAuthUser == "" && sec.BasicAuthPassword == "" && len(sec.SecretHeaders) == 0) {
		return nil, nil
	}
	b, err := json.Marshal(sec)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(b, []byte("monitor:"+id))
}

// DecryptMonitor populates m.Secrets from m.SecretBlob.
func DecryptMonitor(box *security.SecretBox) func(m *model.Monitor) error {
	return func(m *model.Monitor) error {
		if len(m.SecretBlob) == 0 {
			return nil
		}
		pt, err := box.Open(m.SecretBlob, []byte("monitor:"+m.ID))
		if err != nil {
			return err
		}
		var sec model.MonitorSecrets
		if err := json.Unmarshal(pt, &sec); err != nil {
			return err
		}
		m.Secrets = &sec
		return nil
	}
}

func (s *Server) withSecretKeys(m *model.Monitor) {
	m.SecretKeys = []string{}
	if len(m.SecretBlob) > 0 {
		mm := *m
		if err := DecryptMonitor(s.box)(&mm); err == nil {
			m.SecretKeys = secretKeys(mm.Secrets)
		} else {
			m.SecretKeys = []string{"undecryptable"}
		}
	}
}

func (s *Server) view(ctx context.Context, m *model.Monitor, st *model.MonitorState, groups map[string]string) (*monitorView, error) {
	s.withSecretKeys(m)
	v := &monitorView{Monitor: m, State: st, GroupName: groups[m.GroupID]}
	u, err := s.store.Uptime(ctx, m.ID, time.Now().Add(-24*time.Hour))
	if err != nil {
		return nil, err
	}
	v.Uptime24h = u.Availability
	return v, nil
}

func (s *Server) groupNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	gs, err := s.store.ListGroups(ctx)
	if err != nil {
		return out
	}
	for _, g := range gs {
		out[g.ID] = g.Name
	}
	return out
}

func (s *Server) handleListMonitors(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.ListMonitors(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	states, err := s.store.ListMonitorStates(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	groups := s.groupNames(r.Context())
	q := strings.ToLower(r.URL.Query().Get("q"))
	status := r.URL.Query().Get("status")
	group := r.URL.Query().Get("group_id")
	tag := r.URL.Query().Get("tag")
	out := []*monitorView{}
	for _, m := range ms {
		st := states[m.ID]
		if q != "" && !strings.Contains(strings.ToLower(m.Name+" "+m.Target+" "+strings.Join(m.Tags, " ")), q) {
			continue
		}
		if status != "" && (st == nil || string(st.Status) != status) {
			continue
		}
		if group != "" && m.GroupID != group {
			continue
		}
		if tag != "" && !contains(m.Tags, tag) {
			continue
		}
		v, err := s.view(r.Context(), m, st, groups)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"monitors": out})
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (s *Server) handleGetMonitor(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.GetMonitor(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	st, err := s.store.GetMonitorState(r.Context(), m.ID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	v, err := s.view(r.Context(), m, st, s.groupNames(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"monitor": v})
}

func (s *Server) handleCreateMonitor(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in monitorInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeMonitor(r.Context(), &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	now := time.Now().UTC()
	m := &model.Monitor{ID: uuid.NewString(), Name: in.Name, Description: in.Description, Type: in.Type, Target: in.Target, GroupID: in.GroupID,
		Tags: in.Tags, IntervalSeconds: in.IntervalSeconds, TimeoutSeconds: in.TimeoutSeconds, Retries: in.Retries, Enabled: in.Enabled == nil || *in.Enabled,
		Settings: in.Settings, Policy: *in.Policy, NodeSelector: in.NodeIDs, Version: 1, CreatedAt: now, UpdatedAt: now}
	blob, err := s.sealSecrets(m.ID, in.Secrets)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	m.SecretBlob = blob
	if err := s.cp.CreateMonitor(r.Context(), m); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.engine.Reload()
	s.audit.Record(r.Context(), p.actor(), "monitor.create", "monitor", m.ID, "", map[string]any{"name": m.Name, "type": m.Type, "target": security.RedactString(m.Target)})
	st, _ := s.store.GetMonitorState(r.Context(), m.ID)
	v, err := s.view(r.Context(), m, st, s.groupNames(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"monitor": v})
}

// probeFingerprint captures fields that affect probe execution or evaluation.
func probeFingerprint(m *model.Monitor) string {
	b, _ := json.Marshal([]any{m.Type, m.Target, m.IntervalSeconds, m.TimeoutSeconds, m.Retries, m.Settings, m.Policy, m.NodeSelector, m.SecretBlob})
	return string(b)
}

func (s *Server) handleUpdateMonitor(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	old, err := s.store.GetMonitor(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	var in monitorInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeMonitor(r.Context(), &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	m := *old
	m.Name, m.Description, m.Type, m.Target, m.GroupID, m.Tags = in.Name, in.Description, in.Type, in.Target, in.GroupID, in.Tags
	m.IntervalSeconds, m.TimeoutSeconds, m.Retries, m.Settings, m.Policy, m.NodeSelector = in.IntervalSeconds, in.TimeoutSeconds, in.Retries, in.Settings, *in.Policy, in.NodeIDs
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	switch {
	case in.ClearSecrets:
		m.SecretBlob = nil
	case in.Secrets != nil:
		// Merge: provided values replace; existing secrets are kept unless cleared.
		merged := &model.MonitorSecrets{}
		if len(old.SecretBlob) > 0 {
			oo := *old
			if err := DecryptMonitor(s.box)(&oo); err == nil && oo.Secrets != nil {
				merged = oo.Secrets
			}
		}
		if in.Secrets.BasicAuthUser != "" || in.Secrets.BasicAuthPassword != "" {
			merged.BasicAuthUser, merged.BasicAuthPassword = in.Secrets.BasicAuthUser, in.Secrets.BasicAuthPassword
		}
		for k, v := range in.Secrets.SecretHeaders {
			if merged.SecretHeaders == nil {
				merged.SecretHeaders = map[string]string{}
			}
			if v == "" {
				delete(merged.SecretHeaders, k)
			} else {
				merged.SecretHeaders[k] = v
			}
		}
		blob, err := s.sealSecrets(m.ID, merged)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		m.SecretBlob = blob
	}
	probeChanged := probeFingerprint(old) != probeFingerprint(&m)
	if probeChanged {
		m.Version = old.Version + 1
	}
	m.UpdatedAt = time.Now().UTC()
	if err := s.cp.UpdateMonitor(r.Context(), &m); err != nil {
		s.writeErr(w, r, err)
		return
	}
	// Alignment or target changes make previous counters meaningless.
	if old.Type != m.Type || old.Target != m.Target || old.IntervalSeconds != m.IntervalSeconds || (old.Enabled != m.Enabled) {
		if err := s.resetState(r.Context(), &m); err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	s.engine.Reload()
	s.audit.Record(r.Context(), p.actor(), "monitor.update", "monitor", m.ID, "", map[string]any{"name": m.Name, "version": m.Version, "probe_config_changed": probeChanged,
		"policy_changed": !reflect.DeepEqual(old.Policy, m.Policy)})
	st, _ := s.store.GetMonitorState(r.Context(), m.ID)
	v, err := s.view(r.Context(), &m, st, s.groupNames(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"monitor": v})
}

func (s *Server) resetState(ctx context.Context, m *model.Monitor) error {
	now := time.Now().UTC()
	status := model.StatusUnknown
	if !m.Enabled {
		status = model.StatusPaused
	}
	return s.cp.SaveMonitorState(ctx, &model.MonitorState{MonitorID: m.ID, Status: status, StatusSince: now, UpdatedAt: now})
}

func (s *Server) handleDeleteMonitor(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	m, err := s.store.GetMonitor(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.cp.DeleteMonitor(r.Context(), m.ID); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.engine.Reload()
	s.audit.Record(r.Context(), p.actor(), "monitor.delete", "monitor", m.ID, "", map[string]any{"name": m.Name})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	p := principal(r)
	m, err := s.store.GetMonitor(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if m.Enabled != enabled {
		m.Enabled = enabled
		m.UpdatedAt = time.Now().UTC()
		if err := s.cp.UpdateMonitor(r.Context(), m); err != nil {
			s.writeErr(w, r, err)
			return
		}
		if err := s.resetState(r.Context(), m); err != nil {
			s.writeErr(w, r, err)
			return
		}
		s.engine.Reload()
		action := "monitor.pause"
		if enabled {
			action = "monitor.resume"
		}
		s.audit.Record(r.Context(), p.actor(), action, "monitor", m.ID, "", map[string]any{"name": m.Name})
	}
	st, _ := s.store.GetMonitorState(r.Context(), m.ID)
	v, err := s.view(r.Context(), m, st, s.groupNames(r.Context()))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"monitor": v})
}

func (s *Server) handlePauseMonitor(w http.ResponseWriter, r *http.Request) {
	s.setEnabled(w, r, false)
}
func (s *Server) handleResumeMonitor(w http.ResponseWriter, r *http.Request) {
	s.setEnabled(w, r, true)
}

// handleTestMonitor runs one probe with the submitted configuration without saving it.
func (s *Server) handleTestMonitor(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in monitorInput
	if err := decode(r, &in); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if f := s.normalizeMonitor(r.Context(), &in); len(f) > 0 {
		s.writeErr(w, r, errValidation(f))
		return
	}
	m := &model.Monitor{ID: "test", Type: in.Type, Target: in.Target, TimeoutSeconds: in.TimeoutSeconds, Retries: 0, Settings: in.Settings, Secrets: in.Secrets, Version: 1}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(in.TimeoutSeconds+2)*time.Second)
	defer cancel()
	out, _ := s.prober.Probe(ctx, m)
	s.audit.Record(r.Context(), p.actor(), "monitor.test", "monitor", "", "", map[string]any{"type": in.Type, "target": security.RedactString(in.Target), "success": out.Success})
	resp := map[string]any{"success": out.Success, "latency_ms": float64(out.Latency.Microseconds()) / 1000, "http_status": out.HTTPStatus,
		"error_category": out.Category, "message": out.Message, "cert_expires_at": out.CertExpiresAt, "slow": out.Slow, "node_id": s.node.Identity.ID}
	writeJSON(w, 200, resp)
}

func (s *Server) handleMonitorResults(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetMonitor(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	since := time.Now().Add(-parseRange(r.URL.Query().Get("range"), 24*time.Hour))
	rs, err := s.store.ListResults(r.Context(), id, since, r.URL.Query().Get("node_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"results": rs})
}

func (s *Server) handleMonitorUptime(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.GetMonitor(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	out := map[string]any{}
	for _, p := range []struct {
		k string
		d time.Duration
	}{{"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"90d", 90 * 24 * time.Hour}} {
		u, err := s.store.Uptime(r.Context(), id, now.Add(-p.d))
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		out[p.k] = u
	}
	daily, err := s.store.DailyUptime(r.Context(), id, 90, now)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"periods": out, "daily": daily})
}

func (s *Server) handleMonitorStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m, err := s.store.GetMonitor(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	rng := parseRange(r.URL.Query().Get("range"), 24*time.Hour)
	since := time.Now().UTC().Add(-rng)
	bucket := rng / 120
	if min := time.Duration(m.IntervalSeconds) * time.Second; bucket < min {
		bucket = min
	}
	series, err := s.store.LatencySeries(r.Context(), id, since, bucket)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	br, err := s.store.Breakdown(r.Context(), id, since)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	rounds, err := s.store.ListRounds(r.Context(), id, time.Now().Add(-min(rng, 6*time.Hour)), 120)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	cert, err := s.store.LatestCertExpiry(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	incs, err := s.store.ListIncidents(r.Context(), storage.IncidentFilter{MonitorID: id, Limit: 20})
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if rounds == nil {
		rounds = []*model.RoundResult{}
	}
	writeJSON(w, 200, map[string]any{"latency": series, "bucket_seconds": int(bucket.Seconds()), "breakdown": br, "rounds": rounds,
		"cert_expires_at": cert, "incidents": incs, "node_names": s.nodeNames()})
}

func (s *Server) nodeNames() map[string]string {
	out := map[string]string{s.node.Identity.ID: s.node.Identity.Name}
	if nodes, err := s.store.ListClusterNodes(context.Background()); err == nil {
		for _, n := range nodes {
			out[n.ID] = n.Name
		}
	}
	return out
}
