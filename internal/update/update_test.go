package update

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type memSettings struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memSettings) GetSetting(_ context.Context, k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k], nil
}

func (s *memSettings) SetSetting(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

// fakeGitHub serves the three GitHub API calls the checker makes.
func fakeGitHub(t *testing.T, latest string, compare string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/intrabin/releases/latest" && latest != "":
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": latest, "name": "Intrabin " + latest, "html_url": "https://github.com/acme/intrabin/releases/tag/" + latest,
				"body": "notes", "published_at": "2026-11-10T10:00:00Z"})
		case r.URL.Path == "/repos/acme/intrabin/tags":
			_, _ = io.WriteString(w, `[{"name":"v1.2.0"},{"name":"v1.10.0"},{"name":"v2.0.0-rc1"},{"name":"nightly"}]`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/intrabin/compare/"):
			if compare == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": compare})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newSvc(t *testing.T, base, current, dir string) *Service {
	return &Service{Enabled: true, Repo: "acme/intrabin", Current: current, Dir: dir, Store: &memSettings{m: map[string]string{}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), APIBase: base}
}

func TestCheckComparesVersions(t *testing.T) {
	ctx := context.Background()
	gh := fakeGitHub(t, "v1.3.0", "")
	for _, tc := range []struct {
		current string
		want    bool
	}{{"v1.2.9", true}, {"v1.3.0", false}, {"v1.4.0", false}, {"v1.3.0-rc2", true}, {"dev", false}} {
		s := newSvc(t, gh.URL, tc.current, "")
		if err := s.Check(ctx); err != nil {
			t.Fatal(err)
		}
		st := s.State(ctx)
		if st.Available != tc.want || st.Latest == nil || st.Latest.Version != "v1.3.0" || st.CheckedAt == nil {
			t.Errorf("%s: available=%v latest=%+v", tc.current, st.Available, st.Latest)
		}
		if st.ShowBanner != tc.want {
			t.Errorf("%s: banner=%v", tc.current, st.ShowBanner)
		}
	}
}

func TestCheckSourceBuildUsesCompare(t *testing.T) {
	ctx := context.Background()
	for status, want := range map[string]bool{"ahead": true, "behind": false, "identical": false, "diverged": false, "": false} {
		s := newSvc(t, fakeGitHub(t, "v1.3.0", status).URL, "64b4d0a", "")
		if err := s.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if got := s.State(ctx).Available; got != want {
			t.Errorf("compare %q: available=%v, want %v", status, got, want)
		}
	}
}

func TestCheckFallsBackToTags(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, fakeGitHub(t, "", "").URL, "v1.2.0", "")
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	st := s.State(ctx)
	if st.Latest == nil || st.Latest.Version != "v1.10.0" || !st.Available {
		t.Fatalf("expected v1.10.0 from tags (numeric order, no pre-releases), got %+v", st.Latest)
	}
}

func TestCheckErrorIsRecorded(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	s := newSvc(t, srv.URL, "v1.0.0", "")
	if err := s.Check(ctx); err == nil {
		t.Fatal("expected an error")
	}
	if st := s.State(ctx); !strings.Contains(st.CheckError, "403") || st.Available {
		t.Fatalf("unexpected state: %+v", st)
	}
	if s.due(ctx) {
		t.Fatal("a failed check still counts as today's check")
	}
}

func TestDismissHidesBannerForAMonth(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, fakeGitHub(t, "v1.3.0", "").URL, "v1.0.0", "")
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	_ = s.Check(ctx)
	if _, err := s.Dismiss(ctx); err != nil {
		t.Fatal(err)
	}
	if s.State(ctx).ShowBanner {
		t.Fatal("banner must be hidden after dismissing")
	}
	now = now.Add(29 * 24 * time.Hour)
	if s.State(ctx).ShowBanner {
		t.Fatal("banner must stay hidden for 30 days")
	}
	now = now.Add(2 * 24 * time.Hour)
	if !s.State(ctx).ShowBanner {
		t.Fatal("banner must come back after 30 days")
	}
}

func TestRequestWritesFileForAgent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := newSvc(t, fakeGitHub(t, "v1.3.0", "").URL, "v1.2.0", dir)
	_ = s.Check(ctx)
	if err := s.Request(ctx, "v1.3.0", "admin"); !errors.Is(err, ErrNoAgent) {
		t.Fatalf("without agent.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"agent":1,"mode":"release"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := s.State(ctx); !st.Agent.Available || st.Agent.Mode != "release" {
		t.Fatalf("agent not detected: %+v", st.Agent)
	}
	if err := s.Request(ctx, "v1.2.5", "admin"); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("only the latest release may be requested: %v", err)
	}
	if err := s.Request(ctx, "v1.3.0\nversion=v9.9.9", "admin"); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("malformed version must be refused: %v", err)
	}
	if err := s.Request(ctx, "v1.3.0", "ad\nmin"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "request"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "version=v1.3.0\nfrom=v1.2.0\nrequested_by=admin\n") {
		t.Fatalf("unexpected request file:\n%s", b)
	}
	st := s.State(ctx)
	if !st.Pending || st.ShowBanner {
		t.Fatalf("pending request expected, banner hidden: %+v", st)
	}
	if err := s.Request(ctx, "v1.3.0", "admin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second request while pending: %v", err)
	}

	// The agent picked it up and reports progress.
	_ = os.Remove(filepath.Join(dir, "request"))
	status := `{"state":"failed","target":"v1.3.0","from":"v1.2.0","started_at":"2026-11-16T12:00:00Z","finished_at":"2026-11-16T12:03:00Z","message":"image pull failed"}`
	_ = os.WriteFile(filepath.Join(dir, "status.json"), []byte(status), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "update.log"), []byte("pulling…\nerror: denied\n"), 0o644)
	st = s.State(ctx)
	if st.Status == nil || st.Status.State != "failed" || !strings.Contains(st.Log, "denied") || !st.ShowBanner {
		t.Fatalf("status not reported: %+v", st)
	}
	if err := s.Request(ctx, "v1.3.0", "admin"); err != nil {
		t.Fatalf("retry after a failed update: %v", err)
	}
}

func TestAvailableClearsAfterUpdate(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, fakeGitHub(t, "v1.3.0", "").URL, "v1.2.0", "")
	_ = s.Check(ctx)
	s.Current = "v1.3.0" // restarted on the new version, before the next daily check
	if st := s.State(ctx); st.Available || st.ShowBanner {
		t.Fatalf("no update should be offered for the running version: %+v", st)
	}
}
