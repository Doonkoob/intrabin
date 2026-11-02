package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as the rlimit wrapper (`intrabin runner-exec`).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "runner-exec" {
		os.Exit(ExecWrapper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

const tok = "test-token-0123456789abcdef"

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{Token: tok, WorkDir: t.TempDir(), MaxOutput: 1024, MaxTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRejectsWeakToken(t *testing.T) {
	if _, err := New(Config{Token: "short"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestBashAndPython(t *testing.T) {
	s := newServer(t)
	r := s.Execute(context.Background(), &Request{ID: "action-0001", Language: "bash", Script: "echo hello $GREETING; exit 3", Env: map[string]string{"GREETING": "world"}, TimeoutSeconds: 10})
	if r.Status != "failed" || r.ExitCode == nil || *r.ExitCode != 3 || !strings.Contains(r.Output, "hello world") {
		t.Fatalf("unexpected result %+v", r)
	}
	if _, ok := interpreters()["python"]; ok {
		r = s.Execute(context.Background(), &Request{ID: "action-0002", Language: "python", Script: "import os\nprint('py', os.environ['X'])", Env: map[string]string{"X": "1"}, TimeoutSeconds: 10})
		if r.Status != "succeeded" || !strings.Contains(r.Output, "py 1") {
			t.Fatalf("python: %+v", r)
		}
	}
}

func TestExecutesOncePerActionID(t *testing.T) {
	s := newServer(t)
	marker := filepath.Join(t.TempDir(), "count")
	req := &Request{ID: "action-dedup-1", Language: "bash", Script: "sleep 0.3; echo x >> " + marker, TimeoutSeconds: 10}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); s.Execute(context.Background(), req) }()
	}
	wg.Wait()
	s.Execute(context.Background(), req) // a later retry returns the recorded result
	b, _ := os.ReadFile(marker)
	if got := strings.Count(string(b), "x"); got != 1 {
		t.Fatalf("script ran %d times, want 1", got)
	}
}

func TestTimeoutKillsProcessGroup(t *testing.T) {
	s := newServer(t)
	marker := filepath.Join(t.TempDir(), "late")
	start := time.Now()
	r := s.Execute(context.Background(), &Request{ID: "action-timeout", Language: "bash", Script: "(sleep 3; touch " + marker + ") & sleep 30", TimeoutSeconds: 1})
	if r.Status != "timeout" || time.Since(start) > 6*time.Second {
		t.Fatalf("unexpected %+v after %s", r, time.Since(start))
	}
	time.Sleep(3500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background child survived the timeout")
	}
}

func TestOutputCapAndEnvIsolation(t *testing.T) {
	t.Setenv("INTRABIN_MASTER_KEY", "should-not-leak")
	s := newServer(t)
	r := s.Execute(context.Background(), &Request{ID: "action-output", Language: "bash", Script: "env; head -c 5000 /dev/zero | tr '\\0' a", TimeoutSeconds: 10})
	if !r.Truncated || len(r.Output) > 1024 {
		t.Fatalf("output not capped: %d %v", len(r.Output), r.Truncated)
	}
	if strings.Contains(r.Output, "should-not-leak") {
		t.Fatal("runner environment leaked into the script")
	}
}

func TestResourceLimitApplied(t *testing.T) {
	s := newServer(t)
	r := s.Execute(context.Background(), &Request{ID: "action-limits", Language: "bash", Script: "ulimit -n; ulimit -c", TimeoutSeconds: 10})
	if r.Status != "succeeded" || !strings.Contains(r.Output, "256") {
		t.Fatalf("limits not applied: %+v", r)
	}
}

func TestInterruptedActionIsNeverRerun(t *testing.T) {
	s := newServer(t)
	// Simulate a runner that crashed after starting the action.
	if err := os.WriteFile(s.startedPath("action-crashed"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	r := s.Execute(context.Background(), &Request{ID: "action-crashed", Language: "bash", Script: "touch " + marker, TimeoutSeconds: 5})
	if r.Status != "interrupted" {
		t.Fatalf("status %s", r.Status)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("interrupted action was executed again")
	}
}

func TestHTTPAPI(t *testing.T) {
	s := newServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	post := func(auth string, body any) *http.Response {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", ts.URL+"/v1/actions", bytes.NewReader(b))
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("Bearer wrong", Request{ID: "action-http1", Language: "bash", Script: "true"}); resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	if resp := post("Bearer "+tok, Request{ID: "action-http1", Language: "bash", Script: "true", Env: map[string]string{"LD_PRELOAD": "x"}}); resp.StatusCode != 400 {
		t.Fatalf("want 400 for LD_PRELOAD, got %d", resp.StatusCode)
	}
	resp := post("Bearer "+tok, Request{ID: "action-http1", Language: "bash", Script: "echo ok"})
	var res Result
	_ = json.NewDecoder(resp.Body).Decode(&res)
	if resp.StatusCode != 200 || res.Status != "succeeded" {
		t.Fatalf("got %d %+v", resp.StatusCode, res)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/actions/action-unknown", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if r2, _ := http.DefaultClient.Do(req); r2.StatusCode != 404 {
		t.Fatalf("want 404, got %d", r2.StatusCode)
	}
}
