package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intrabin/intrabin/internal/cluster"
)

type updateResp struct {
	Update struct {
		Current    string `json:"current"`
		Available  bool   `json:"available"`
		ShowBanner bool   `json:"show_banner"`
		Pending    bool   `json:"pending"`
		Latest     *struct {
			Version string `json:"version"`
		} `json:"latest"`
		Agent struct {
			Available bool `json:"available"`
		} `json:"agent"`
	} `json:"update"`
	Error APIError `json:"error"`
}

func TestUpdateEndpoints(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	if code := owner.do("POST", "/api/v1/users", map[string]any{"username": "viewer1", "password": "viewer-password", "role": "viewer"}, nil); code != 201 {
		t.Fatalf("create viewer %d", code)
	}
	v := e.client()
	v.do("POST", "/api/v1/auth/login", map[string]any{"username": "viewer1", "password": "viewer-password"}, nil)
	v.loadCSRF()
	if code := v.do("GET", "/api/v1/system/update", nil, nil); code != 403 {
		t.Fatalf("viewer must not see update state: %d", code)
	}

	var st updateResp
	if code := owner.do("POST", "/api/v1/system/update/check", nil, &st); code != 200 {
		t.Fatalf("check: %d", code)
	}
	if !st.Update.Available || !st.Update.ShowBanner || st.Update.Latest == nil || st.Update.Latest.Version != "v1.1.0" || st.Update.Agent.Available {
		t.Fatalf("unexpected state after check: %+v", st.Update)
	}
	if code := owner.do("POST", "/api/v1/system/update/apply", map[string]any{"version": "v1.1.0"}, &st); code != 409 || st.Error.Code != "update_agent_missing" {
		t.Fatalf("apply without the host agent: %d %+v", code, st.Error)
	}

	// The installer creates the shared directory and agent.json.
	if err := os.MkdirAll(e.updateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.updateDir, "agent.json"), []byte(`{"agent":1,"mode":"source"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := owner.do("POST", "/api/v1/system/update/apply", map[string]any{"version": "v1.0.5"}, &st); code != 409 || st.Error.Code != "update_not_available" {
		t.Fatalf("apply of another version: %d %+v", code, st.Error)
	}
	if code := owner.do("POST", "/api/v1/system/update/apply", map[string]any{"version": "v1.1.0"}, &st); code != 202 || !st.Update.Pending || st.Update.ShowBanner {
		t.Fatalf("apply: %d %+v", code, st.Update)
	}
	b, err := os.ReadFile(filepath.Join(e.updateDir, "request"))
	if err != nil || !strings.Contains(string(b), "version=v1.1.0\n") || !strings.Contains(string(b), "requested_by=owner\n") {
		t.Fatalf("request file: %q %v", b, err)
	}
	if code := owner.do("POST", "/api/v1/system/update/apply", map[string]any{"version": "v1.1.0"}, &st); code != 409 || st.Error.Code != "update_in_progress" {
		t.Fatalf("second apply: %d %+v", code, st.Error)
	}
	_ = os.Remove(filepath.Join(e.updateDir, "request"))
	if code := owner.do("POST", "/api/v1/system/update/dismiss", nil, &st); code != 200 || st.Update.ShowBanner {
		t.Fatalf("dismiss: %d %+v", code, st.Update)
	}
	var audit struct {
		Entries []struct {
			Action string `json:"action"`
		} `json:"entries"`
	}
	owner.do("GET", "/api/v1/audit-logs?action=system.update_requested", nil, &audit)
	if len(audit.Entries) < 2 {
		t.Fatalf("update requests must be audited: %+v", audit)
	}
}

func TestClusterTunnelRouteWithoutCluster(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest("GET", e.srv.URL+cluster.TunnelPath, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("tunnel on a standalone node: %d", resp.StatusCode)
	}
}
