package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/audit"
	"github.com/intrabin/intrabin/internal/config"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/monitoring"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

const setupTok = "setup-token-for-tests-123"

type env struct {
	t     *testing.T
	srv   *httptest.Server
	store *storage.Store
	node  *node.Node
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := storage.Open(ctx, filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n, _ := node.LoadIdentity(ctx, st, "test-node")
	box, _ := security.NewSecretBox(bytes.Repeat([]byte{1}, 32))
	inc := incidents.NewManager(st, log)
	prober := monitoring.NewProber("test")
	eng := &monitoring.Engine{Store: st, Prober: prober, Incidents: inc, Log: log, NodeID: n.Identity.ID}
	go eng.Run(t.Context())
	cfg := &config.Config{MinInterval: 10 * time.Second, SessionTTL: time.Hour, SessionIdle: time.Hour}
	fast := security.Argon2Params{Memory: 1024, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16}
	s := New(Deps{Config: cfg, Store: st, Node: n, Engine: eng, Prober: prober, Incidents: inc, Audit: audit.New(st, log), SecretBox: box, Log: log, SetupToken: setupTok, Argon2: &fast})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, store: st, node: n}
}

type client struct {
	e    *env
	http *http.Client
	csrf string
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path string, body any, out any) int {
	c.e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			c.e.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

func (c *client) loadCSRF() {
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	if code := c.do("GET", "/api/v1/auth/me", nil, &me); code != 200 {
		c.e.t.Fatalf("me: %d", code)
	}
	c.csrf = me.CSRF
}

func (e *env) owner() *client {
	c := e.client()
	code := c.do("POST", "/api/v1/setup", map[string]any{"setup_token": setupTok, "username": "owner", "password": "owner-password-1", "language": "en"}, nil)
	if code != 201 {
		e.t.Fatalf("setup: %d", code)
	}
	c.loadCSRF()
	return c
}

func TestSetupAndAuth(t *testing.T) {
	e := newEnv(t)
	c := e.client()
	var st map[string]any
	c.do("GET", "/api/v1/setup/status", nil, &st)
	if st["needs_setup"] != true {
		t.Fatal("expected needs_setup")
	}
	if code := c.do("POST", "/api/v1/setup", map[string]any{"setup_token": "wrong-token-xxxxxxxx", "username": "owner", "password": "owner-password-1"}, nil); code != 422 {
		t.Fatalf("bad token accepted: %d", code)
	}
	if code := c.do("POST", "/api/v1/setup", map[string]any{"setup_token": setupTok, "username": "owner", "password": "short"}, nil); code != 422 {
		t.Fatalf("weak password accepted: %d", code)
	}
	owner := e.owner()
	// Setup is one-time.
	if code := e.client().do("POST", "/api/v1/setup", map[string]any{"setup_token": setupTok, "username": "x2", "password": "owner-password-1"}, nil); code != 409 {
		t.Fatalf("second setup: %d", code)
	}
	// CSRF required for unsafe methods.
	saved := owner.csrf
	owner.csrf = ""
	if code := owner.do("POST", "/api/v1/monitor-groups", map[string]any{"name": "g"}, nil); code != 403 {
		t.Fatalf("missing CSRF accepted: %d", code)
	}
	owner.csrf = saved
	// Unauthenticated access denied.
	if code := e.client().do("GET", "/api/v1/monitors", nil, nil); code != 401 {
		t.Fatalf("anon access: %d", code)
	}
	// Login failures and rate limiting.
	anon := e.client()
	for i := 0; i < 5; i++ {
		if code := anon.do("POST", "/api/v1/auth/login", map[string]any{"username": "owner", "password": "wrong-password"}, nil); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code := anon.do("POST", "/api/v1/auth/login", map[string]any{"username": "owner", "password": "owner-password-1"}, nil); code != 429 {
		t.Fatalf("expected rate limit, got %d", code)
	}
	// Logout invalidates session.
	if code := owner.do("POST", "/api/v1/auth/logout", nil, nil); code != 200 {
		t.Fatalf("logout %d", code)
	}
	if code := owner.do("GET", "/api/v1/auth/me", nil, nil); code != 401 {
		t.Fatalf("session survived logout: %d", code)
	}
	// Audit trail contains login failures.
	entries, _ := e.store.ListAudit(context.Background(), storage.AuditFilter{Action: "auth.login"})
	if len(entries) < 5 {
		t.Fatalf("expected login audit entries, got %d", len(entries))
	}
}

func TestRBACAndUsers(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	if code := owner.do("POST", "/api/v1/users", map[string]any{"username": "viewer1", "password": "viewer-password", "role": "viewer"}, nil); code != 201 {
		t.Fatalf("create viewer %d", code)
	}
	var admin struct{ User model.User }
	if code := owner.do("POST", "/api/v1/users", map[string]any{"username": "admin1", "password": "admin-password", "role": "admin"}, &admin); code != 201 {
		t.Fatalf("create admin %d", code)
	}
	v := e.client()
	if code := v.do("POST", "/api/v1/auth/login", map[string]any{"username": "viewer1", "password": "viewer-password"}, nil); code != 200 {
		t.Fatalf("viewer login %d", code)
	}
	v.loadCSRF()
	if code := v.do("GET", "/api/v1/monitors", nil, nil); code != 200 {
		t.Fatalf("viewer read %d", code)
	}
	if code := v.do("POST", "/api/v1/monitors", map[string]any{"name": "x", "target": "https://example.com"}, nil); code != 403 {
		t.Fatalf("viewer created monitor: %d", code)
	}
	if code := v.do("GET", "/api/v1/users", nil, nil); code != 403 {
		t.Fatalf("viewer listed users: %d", code)
	}
	a := e.client()
	a.do("POST", "/api/v1/auth/login", map[string]any{"username": "admin1", "password": "admin-password"}, nil)
	a.loadCSRF()
	var users struct{ Users []model.User }
	a.do("GET", "/api/v1/users", nil, &users)
	var ownerID string
	for _, u := range users.Users {
		if u.Role == model.RoleOwner {
			ownerID = u.ID
		}
	}
	if code := a.do("PATCH", "/api/v1/users/"+ownerID, map[string]any{"disabled": true}, nil); code != 403 {
		t.Fatalf("admin disabled owner: %d", code)
	}
	// Owner cannot disable themselves (last owner protection).
	if code := owner.do("PATCH", "/api/v1/users/"+ownerID, map[string]any{"role": "admin"}, nil); code != 409 {
		t.Fatalf("self-demotion: %d", code)
	}
	// Password hash never exposed.
	var raw []map[string]any
	var wrap struct {
		Users []map[string]any `json:"users"`
	}
	owner.do("GET", "/api/v1/users", nil, &wrap)
	raw = wrap.Users
	for _, u := range raw {
		if _, ok := u["password_hash"]; ok {
			t.Fatal("password hash exposed")
		}
	}
	// Disabling a user revokes their sessions.
	var vu struct{ Users []model.User }
	owner.do("GET", "/api/v1/users", nil, &vu)
	for _, u := range vu.Users {
		if u.Username == "viewer1" {
			if code := owner.do("PATCH", "/api/v1/users/"+u.ID, map[string]any{"disabled": true}, nil); code != 200 {
				t.Fatalf("disable %d", code)
			}
		}
	}
	if code := v.do("GET", "/api/v1/monitors", nil, nil); code != 401 {
		t.Fatalf("disabled user still authenticated: %d", code)
	}
}

func TestMonitorCRUDAndSecrets(t *testing.T) {
	e := newEnv(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer topsecret" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer target.Close()
	owner := e.owner()
	in := map[string]any{"name": "API", "type": "http", "target": target.URL, "interval_seconds": 30, "timeout_seconds": 5,
		"settings": map[string]any{"headers": map[string]string{"Authorization": "Bearer topsecret", "Accept": "text/plain"}}}
	var created struct {
		Monitor map[string]any `json:"monitor"`
	}
	if code := owner.do("POST", "/api/v1/monitors", in, &created); code != 201 {
		t.Fatalf("create %d %v", code, created)
	}
	b, _ := json.Marshal(created)
	if strings.Contains(string(b), "topsecret") {
		t.Fatal("secret leaked in API response")
	}
	if !strings.Contains(string(b), "header:Authorization") {
		t.Fatalf("secret key name missing: %s", b)
	}
	id := created.Monitor["id"].(string)
	m, _ := e.store.GetMonitor(context.Background(), id)
	if bytes.Contains(m.SecretBlob, []byte("topsecret")) {
		t.Fatal("secret stored in plaintext")
	}
	// The probe uses the decrypted secret header.
	var test map[string]any
	if code := owner.do("POST", "/api/v1/monitors/test", in, &test); code != 200 || test["success"] != true {
		t.Fatalf("test probe: %d %v", code, test)
	}
	// Validation
	var verr struct {
		Error APIError `json:"error"`
	}
	if code := owner.do("POST", "/api/v1/monitors", map[string]any{"name": "", "target": "ftp://x", "interval_seconds": 5}, &verr); code != 422 {
		t.Fatalf("invalid monitor accepted: %d", code)
	}
	if verr.Error.Fields["target"] == "" || verr.Error.Fields["interval_seconds"] == "" || verr.Error.Fields["name"] == "" {
		t.Fatalf("missing field errors: %+v", verr)
	}
	// 30 s interval works; update without secrets keeps them.
	in["name"] = "API renamed"
	delete(in["settings"].(map[string]any)["headers"].(map[string]string), "Authorization")
	if code := owner.do("PUT", "/api/v1/monitors/"+id, in, nil); code != 200 {
		t.Fatalf("update %d", code)
	}
	m2, _ := e.store.GetMonitor(context.Background(), id)
	if len(m2.SecretBlob) == 0 || m2.Name != "API renamed" {
		t.Fatal("secrets dropped on update")
	}
	if code := owner.do("POST", "/api/v1/monitors/"+id+"/pause", nil, nil); code != 200 {
		t.Fatal("pause")
	}
	st, _ := e.store.GetMonitorState(context.Background(), id)
	if st.Status != model.StatusPaused {
		t.Fatalf("expected paused, got %s", st.Status)
	}
	for _, p := range []string{"/uptime", "/stats?range=24h", "/results"} {
		if code := owner.do("GET", "/api/v1/monitors/"+id+p, nil, nil); code != 200 {
			t.Fatalf("%s: %d", p, code)
		}
	}
	if code := owner.do("GET", "/api/v1/overview", nil, nil); code != 200 {
		t.Fatal("overview")
	}
	if code := owner.do("DELETE", "/api/v1/monitors/"+id, nil, nil); code != 200 {
		t.Fatal("delete")
	}
	if code := owner.do("GET", "/api/v1/monitors/"+id, nil, nil); code != 404 {
		t.Fatal("deleted monitor still visible")
	}
}

func TestReadOnlyFollowerNode(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	e.node.SetRole(node.RoleFollower)
	var errBody struct {
		Error APIError `json:"error"`
	}
	if code := owner.do("POST", "/api/v1/monitors", map[string]any{"name": "x", "target": "https://example.com"}, &errBody); code != 409 || errBody.Error.Code != "node_read_only" {
		t.Fatalf("follower allowed mutation: %d %+v", code, errBody)
	}
	if code := owner.do("GET", "/api/v1/node", nil, nil); code != 200 {
		t.Fatal("node status must remain available on followers")
	}
}

func TestIncidentAPI(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	var created struct{ Monitor model.Monitor }
	owner.do("POST", "/api/v1/monitors", map[string]any{"name": "Site", "target": "https://example.invalid", "interval_seconds": 30}, &created)
	ctx := context.Background()
	inc := incidents.NewManager(e.store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m, _ := e.store.GetMonitor(ctx, created.Monitor.ID)
	if err := inc.HandleMonitorChange(ctx, incidents.MonitorChange{Monitor: m, From: model.StatusUp, To: model.StatusDown, Round: &model.RoundResult{FailingNodes: []string{"n1"}}}); err != nil {
		t.Fatal(err)
	}
	var list struct{ Incidents []model.Incident }
	owner.do("GET", "/api/v1/incidents?open=true", nil, &list)
	if len(list.Incidents) != 1 || list.Incidents[0].Status != model.IncidentConfirmed {
		t.Fatalf("%+v", list)
	}
	id := list.Incidents[0].ID
	var detail struct {
		Incident model.Incident
		Events   []model.IncidentEvent
	}
	if code := owner.do("POST", "/api/v1/incidents/"+id+"/acknowledge", map[string]any{"note": "looking"}, &detail); code != 200 || detail.Incident.Status != model.IncidentAcknowledged {
		t.Fatalf("ack: %d %+v", code, detail.Incident)
	}
	if code := owner.do("POST", "/api/v1/incidents/"+id+"/resolve", map[string]any{"note": "fixed"}, &detail); code != 200 || detail.Incident.Status != model.IncidentResolved {
		t.Fatalf("resolve: %d", code)
	}
	if code := owner.do("POST", "/api/v1/incidents/"+id+"/acknowledge", nil, nil); code != 409 {
		t.Fatalf("ack of resolved incident: %d", code)
	}
	if len(detail.Events) < 3 {
		t.Fatalf("timeline events: %d", len(detail.Events))
	}
}

func TestSecurityHeadersAndHealth(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", resp.Header)
	}
	resp, _ = http.Get(e.srv.URL + "/metrics")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("metrics exposed without auth: %d", resp.StatusCode)
	}
}
