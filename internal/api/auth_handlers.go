package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/audit"
	"github.com/intrabin/intrabin/internal/auth"
	"github.com/intrabin/intrabin/internal/metrics"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

const sessionCookie = "intrabin_session"

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{2,63}$`)

func validLanguage(l string) bool { return l == "en" || l == "fa" }

// authenticate resolves the session cookie into a principal.
func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" || len(c.Value) > 128 {
		return nil, errUnauthorized
	}
	hash := security.HashToken(c.Value)
	se, err := s.store.GetSession(r.Context(), hash)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, errUnauthorized
	}
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if now.After(se.ExpiresAt) || now.Sub(se.LastSeenAt) > s.cfg.SessionIdle {
		_ = s.cp.DeleteSession(r.Context(), hash)
		return nil, errUnauthorized
	}
	u, err := s.store.GetUser(r.Context(), se.UserID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, errUnauthorized
	}
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		_ = s.cp.DeleteSession(r.Context(), hash)
		return nil, errUnauthorized
	}
	if now.Sub(se.LastSeenAt) > time.Minute {
		_ = s.cp.TouchSession(r.Context(), hash, now)
	}
	return &Principal{User: u, Session: se, IP: s.clientIP(r)}, nil
}

// requireAuth enforces authentication, CSRF for unsafe methods, permissions,
// and — for management endpoints — that this node is the active controller.
func (s *Server) requireAuth(perm auth.Permission, mgmt bool, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.authenticate(r)
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) || !security.ConstantTimeEqual(r.Header.Get("X-CSRF-Token"), p.Session.CSRFToken) {
				s.writeErr(w, r, errCSRF)
				return
			}
		}
		if perm != "" && !auth.Can(p.User.Role, perm) {
			s.writeErr(w, r, errForbidden)
			return
		}
		if mgmt && !s.node.CanManage() {
			s.writeErr(w, r, s.readOnlyErr(r))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, p)))
	})
}

// ---- setup ----

func (s *Server) needsSetup(ctx context.Context) (bool, error) {
	if s.cluster != nil && s.cluster.Enabled() && !s.node.CanManage() {
		return false, nil // users are replicated from the cluster; setup happens on the leader
	}
	n, err := s.store.CountUsers(ctx)
	return n == 0, err
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	need, err := s.needsSetup(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	name, _ := s.store.GetSetting(r.Context(), "app.instance_name")
	lang, _ := s.store.GetSetting(r.Context(), "app.default_language")
	writeJSON(w, 200, map[string]any{"needs_setup": need, "version": node.Version, "instance_name": name, "default_language": lang, "role": s.node.Role()})
}

type setupReq struct {
	SetupToken   string `json:"setup_token"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	DisplayName  string `json:"display_name"`
	Language     string `json:"language"`
	InstanceName string `json:"instance_name"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if ok, retry := s.limiter.Allowed("setup:" + ip); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())))
		s.writeErr(w, r, &APIError{Status: 429, Code: "rate_limited", Message: "Too many attempts. Try again later."})
		return
	}
	var req setupReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	need, err := s.needsSetup(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if !need || s.setupToken == "" {
		s.writeErr(w, r, &APIError{Status: 409, Code: "already_initialized", Message: "Initial setup has already been completed."})
		return
	}
	if !security.ConstantTimeEqual(strings.TrimSpace(req.SetupToken), s.setupToken) {
		s.limiter.Fail("setup:" + ip)
		s.audit.Record(r.Context(), audit.Actor{Name: "anonymous", IP: ip}, "setup.failed", "", "", "denied", map[string]any{"reason": "invalid setup token"})
		s.writeErr(w, r, errValidation(map[string]string{"setup_token": "Invalid setup token. Find it in the server logs."}))
		return
	}
	fields := map[string]string{}
	if !usernameRe.MatchString(req.Username) {
		fields["username"] = "3–64 characters: letters, digits, dot, dash, underscore."
	}
	if err := security.ValidatePassword(req.Password); err != nil {
		fields["password"] = err.Error()
	}
	if req.Language == "" {
		req.Language = "en"
	}
	if !validLanguage(req.Language) {
		fields["language"] = "Must be en or fa."
	}
	if len(req.InstanceName) > 80 {
		fields["instance_name"] = "At most 80 characters."
	}
	if len(fields) > 0 {
		s.writeErr(w, r, errValidation(fields))
		return
	}
	hash, err := security.HashPassword(req.Password, s.argon)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	now := time.Now().UTC()
	u := &model.User{ID: uuid.NewString(), Username: req.Username, DisplayName: strings.TrimSpace(req.DisplayName), PasswordHash: hash,
		Role: model.RoleOwner, Language: req.Language, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now}
	if err := s.cp.CreateFirstUser(r.Context(), u); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			s.writeErr(w, r, &APIError{Status: 409, Code: "already_initialized", Message: "Initial setup has already been completed."})
			return
		}
		s.writeErr(w, r, err)
		return
	}
	s.setupToken = "" // one-time
	_ = s.cp.SetSetting(r.Context(), "app.default_language", req.Language)
	if req.InstanceName != "" {
		_ = s.cp.SetSetting(r.Context(), "app.instance_name", strings.TrimSpace(req.InstanceName))
	}
	s.audit.Record(r.Context(), audit.Actor{ID: u.ID, Name: u.Username, IP: ip}, "setup.completed", "user", u.ID, "", nil)
	if err := s.startSession(w, r, u); err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"user": u})
}

// ---- login / logout ----

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		s.writeErr(w, r, errCSRF)
		return
	}
	var req loginReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	ip := s.clientIP(r)
	ipKey, userKey := "ip:"+ip, "user:"+strings.ToLower(req.Username)
	okIP, retryIP := s.limiter.Allowed(ipKey)
	okUser, retryUser := s.limiter.Allowed(userKey)
	if !okIP || !okUser {
		retry := max(retryIP, retryUser)
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())))
		s.audit.Record(r.Context(), audit.Actor{Name: req.Username, IP: ip}, "auth.login", "user", "", "rate_limited", nil)
		s.writeErr(w, r, &APIError{Status: 429, Code: "rate_limited", Message: "Too many failed attempts. Try again later."})
		return
	}
	u, err := s.store.GetUserByUsername(r.Context(), req.Username)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.writeErr(w, r, err)
		return
	}
	valid := false
	if u != nil {
		valid, _ = security.VerifyPassword(req.Password, u.PasswordHash)
	} else {
		security.DummyVerify(req.Password)
	}
	if !valid || u.Disabled {
		s.limiter.Fail(ipKey)
		s.limiter.Fail(userKey)
		metrics.LoginFailures.Inc()
		reason := "invalid_credentials"
		if valid && u.Disabled {
			reason = "account_disabled"
		}
		s.audit.Record(r.Context(), audit.Actor{Name: req.Username, IP: ip}, "auth.login", "user", "", "denied", map[string]any{"reason": reason})
		s.writeErr(w, r, &APIError{Status: 401, Code: "invalid_credentials", Message: "Invalid username or password."})
		return
	}
	s.limiter.Succeed(ipKey)
	s.limiter.Succeed(userKey)
	_ = s.cp.TouchLogin(r.Context(), u.ID)
	if err := s.startSession(w, r, u); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), audit.Actor{ID: u.ID, Name: u.Username, IP: ip}, "auth.login", "user", u.ID, "", nil)
	writeJSON(w, 200, map[string]any{"user": u})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *model.User) error {
	tok := security.RandomToken(32)
	now := time.Now().UTC()
	ua := r.UserAgent()
	if len(ua) > 256 {
		ua = ua[:256]
	}
	se := &model.Session{TokenHash: security.HashToken(tok), UserID: u.ID, CSRFToken: security.RandomToken(24), CreatedAt: now, LastSeenAt: now,
		ExpiresAt: now.Add(s.cfg.SessionTTL), IP: s.clientIP(r), UserAgent: ua}
	if err := s.cp.CreateSession(r.Context(), se); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: s.isSecureRequest(r), SameSite: http.SameSiteLaxMode, Expires: se.ExpiresAt})
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	_ = s.cp.DeleteSession(r.Context(), p.Session.TokenHash)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.isSecureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	s.audit.Record(r.Context(), p.actor(), "auth.logout", "user", p.User.ID, "", nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, 200, map[string]any{
		"user":        p.User,
		"csrf_token":  p.Session.CSRFToken,
		"permissions": auth.Permissions(p.User.Role),
		"node":        s.nodeInfo(),
	})
}

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req changePasswordReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if ok, _ := security.VerifyPassword(req.CurrentPassword, p.User.PasswordHash); !ok {
		s.audit.Record(r.Context(), p.actor(), "auth.password_change", "user", p.User.ID, "denied", nil)
		s.writeErr(w, r, errValidation(map[string]string{"current_password": "Current password is incorrect."}))
		return
	}
	if err := security.ValidatePassword(req.NewPassword); err != nil {
		s.writeErr(w, r, errValidation(map[string]string{"new_password": err.Error()}))
		return
	}
	hash, err := security.HashPassword(req.NewPassword, s.argon)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.cp.SetPassword(r.Context(), p.User.ID, hash); err != nil { // revokes all sessions
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "auth.password_change", "user", p.User.ID, "", nil)
	if err := s.startSession(w, r, p.User); err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type profileReq struct {
	DisplayName *string `json:"display_name"`
	Language    *string `json:"language"`
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var req profileReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	u := p.User
	if req.DisplayName != nil {
		if len(*req.DisplayName) > 80 {
			s.writeErr(w, r, errValidation(map[string]string{"display_name": "At most 80 characters."}))
			return
		}
		u.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Language != nil {
		if !validLanguage(*req.Language) {
			s.writeErr(w, r, errValidation(map[string]string{"language": "Must be en or fa."}))
			return
		}
		u.Language = *req.Language
	}
	if err := s.cp.UpdateUser(r.Context(), u); err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"user": u})
}

func itoa(n int) string {
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}
