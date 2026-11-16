// Package update checks GitHub once a day for a newer Intrabin release and hands an update that a
// user confirmed in the UI to the host-side update agent.
//
// The container never touches Docker. It writes a request file into a directory shared with the
// host (INTRABIN_UPDATE_DIR); the agent installed by scripts/install.sh (a systemd path unit that
// runs `intrabin apply-update`) backs up the data, fetches the release from GitHub and recreates
// the containers. Data volumes and the configuration are kept; schema migrations run when the new
// version starts. The agent reports progress through status.json and update.log in the same
// directory.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node-local settings (never replicated).
const (
	keyLatest    = "local.update.latest"
	keyAvailable = "local.update.available"
	keyCheckedAt = "local.update.checked_at"
	keyError     = "local.update.error"
	keyDismissed = "local.update.dismissed_until"
	keyReleases  = "local.update.releases"
)

const (
	checkEvery  = 24 * time.Hour
	dismissFor  = 30 * 24 * time.Hour
	maxNotes    = 4000
	staleStatus = 2 * time.Hour
)

var (
	ErrNoAgent      = errors.New("automatic updates are not set up on this server")
	ErrNotAvailable = errors.New("this version is not the latest available release")
	ErrBusy         = errors.New("an update is already in progress")
	ErrDisabled     = errors.New("update checks are disabled (INTRABIN_UPDATE_CHECK=false)")
)

var (
	semverRe  = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)
	commitRe  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	githubURL = "https://github.com/"
)

// ValidRepo reports whether repo looks like "owner/name".
func ValidRepo(repo string) bool { return repoRe.MatchString(repo) }

// Settings is the node-local key/value store.
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Release is a published Intrabin version.
type Release struct {
	Version     string    `json:"version"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	Notes       string    `json:"notes"`
}

// Agent describes the host-side update agent (written by the installer as agent.json).
type Agent struct {
	Available bool   `json:"available"`
	Mode      string `json:"mode,omitempty"` // release (published images) or source (built on the server)
}

// Status is the agent's report about the last update (status.json).
type Status struct {
	State      string `json:"state"` // running | succeeded | failed
	Target     string `json:"target"`
	From       string `json:"from"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Message    string `json:"message"`
}

// State is what the UI shows.
type State struct {
	Enabled        bool         `json:"enabled"`
	Current        string       `json:"current"`
	Latest         *Release     `json:"latest,omitempty"`
	Available      bool         `json:"available"`
	CheckedAt      *time.Time   `json:"checked_at,omitempty"`
	CheckError     string       `json:"check_error,omitempty"`
	DismissedUntil *time.Time   `json:"dismissed_until,omitempty"`
	ShowBanner     bool         `json:"show_banner"`
	Agent          Agent        `json:"agent"`
	Pending        bool         `json:"pending"` // requested, not yet picked up by the agent
	PendingVersion string       `json:"pending_version,omitempty"`
	Status         *Status      `json:"status,omitempty"`
	Log            string       `json:"log,omitempty"`
	Repo           string       `json:"repo"`
	Releases       []Release    `json:"releases"`
	Cluster        *RolloutPlan `json:"cluster,omitempty"`
}

// Service implements the check and the hand-over to the agent.
type Service struct {
	Enabled bool
	Repo    string // owner/name on GitHub
	Current string // running version (node.Version)
	Dir     string // directory shared with the host agent; "" when not configured
	Store   Settings
	Log     *slog.Logger
	APIBase string       // default https://api.github.com (tests override)
	Client  *http.Client // default: 20 s timeout
	Now     func() time.Time
	Rollout *Rollout

	mu sync.Mutex // serialises checks and requests
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (s *Service) apiBase() string {
	if s.APIBase != "" {
		return strings.TrimSuffix(s.APIBase, "/")
	}
	return "https://api.github.com"
}

// Run checks once a day (the time of the last check survives restarts).
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if s.due(ctx) {
			if err := s.Check(ctx); err != nil && ctx.Err() == nil {
				s.Log.Warn("update check failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) due(ctx context.Context) bool {
	v, _ := s.Store.GetSetting(ctx, keyCheckedAt)
	at, err := time.Parse(time.RFC3339, v)
	return err != nil || s.now().Sub(at) >= checkEvery
}

// Check asks GitHub for the latest release and records whether it is newer than this version.
func (s *Service) Check(ctx context.Context) error {
	if !s.Enabled {
		return ErrDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.Store.SetSetting(ctx, keyCheckedAt, s.now().Format(time.RFC3339))
	rel, err := s.latestRelease(ctx)
	if err == nil {
		var newer bool
		if newer, err = s.isNewer(ctx, rel.Version); err == nil {
			b, _ := json.Marshal(rel)
			_ = s.Store.SetSetting(ctx, keyLatest, string(b))
			_ = s.Store.SetSetting(ctx, keyAvailable, strconv.FormatBool(newer))
			_ = s.Store.SetSetting(ctx, keyError, "")
			// History failure must not hide an otherwise valid update.
			if releases, e := s.releases(ctx); e == nil {
				b, _ := json.Marshal(releases)
				_ = s.Store.SetSetting(ctx, keyReleases, string(b))
			}
			if newer {
				s.Log.Info("a new Intrabin release is available", "current", s.Current, "latest", rel.Version)
			}
			return nil
		}
	}
	_ = s.Store.SetSetting(ctx, keyError, err.Error())
	return err
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

func (s *Service) releases(ctx context.Context) ([]Release, error) {
	var rows []ghRelease
	if _, err := s.get(ctx, "/repos/"+s.Repo+"/releases?per_page=20", &rows); err != nil {
		return nil, err
	}
	out := []Release{}
	for _, r := range rows {
		v, ok := parseSemver(r.TagName)
		if !ok || v.pre != "" || r.Draft || r.Prerelease {
			continue
		}
		notes := r.Body
		if len(notes) > maxNotes {
			notes = notes[:maxNotes] + "…"
		}
		out = append(out, Release{Version: r.TagName, Name: r.Name, Notes: notes, PublishedAt: r.PublishedAt, URL: githubURL + s.Repo + "/releases/tag/" + url.PathEscape(r.TagName)})
	}
	return out, nil
}

func (s *Service) get(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase()+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "intrabin/"+s.Current)
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// latestRelease returns the newest published (non-draft, non-prerelease) release, or — for a
// repository that only has tags — the highest version tag.
func (s *Service) latestRelease(ctx context.Context) (*Release, error) {
	var r ghRelease
	code, err := s.get(ctx, "/repos/"+s.Repo+"/releases/latest", &r)
	if err == nil {
		if !semverRe.MatchString(r.TagName) {
			return nil, fmt.Errorf("latest release has an unexpected tag %q", r.TagName)
		}
		rel := &Release{Version: r.TagName, Name: r.Name, URL: r.HTMLURL, PublishedAt: r.PublishedAt, Notes: r.Body}
		if !strings.HasPrefix(rel.URL, githubURL) {
			rel.URL = githubURL + s.Repo + "/releases/tag/" + url.PathEscape(r.TagName)
		}
		if len(rel.Notes) > maxNotes {
			rel.Notes = rel.Notes[:maxNotes] + "…"
		}
		return rel, nil
	}
	if code != http.StatusNotFound {
		return nil, err
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if _, err := s.get(ctx, "/repos/"+s.Repo+"/tags?per_page=100", &tags); err != nil {
		return nil, err
	}
	best := ""
	for _, t := range tags {
		if v, ok := parseSemver(t.Name); ok && v.pre == "" && (best == "" || compareSemver(t.Name, best) > 0) {
			best = t.Name
		}
	}
	if best == "" {
		return nil, errors.New("no release has been published yet")
	}
	return &Release{Version: best, Name: best, URL: githubURL + s.Repo + "/releases/tag/" + url.PathEscape(best)}, nil
}

// isNewer compares the running version with a release. A release build compares versions; a
// build from source (its version is a commit) asks GitHub whether the release contains commits
// that this build does not have. Development builds never report updates.
func (s *Service) isNewer(ctx context.Context, latest string) (bool, error) {
	if _, ok := parseSemver(s.Current); ok {
		return compareSemver(latest, s.Current) > 0, nil
	}
	if !commitRe.MatchString(s.Current) {
		return false, nil
	}
	var cmp struct {
		Status string `json:"status"` // ahead | behind | identical | diverged (of head relative to base)
	}
	code, err := s.get(ctx, "/repos/"+s.Repo+"/compare/"+s.Current+"..."+url.PathEscape(latest), &cmp)
	if code == http.StatusNotFound {
		return false, nil // a commit GitHub does not know (local changes)
	}
	if err != nil {
		return false, err
	}
	return cmp.Status == "ahead", nil // "diverged" would drop commits this build has
}

type semver struct {
	n   [3]int
	pre string
}

func parseSemver(v string) (semver, bool) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return semver{}, false
	}
	var out semver
	for i := 0; i < 3; i++ {
		out.n[i], _ = strconv.Atoi(m[i+1])
	}
	out.pre = m[4]
	return out, true
}

// compareSemver returns >0 if a is newer than b (both must be valid).
func compareSemver(a, b string) int {
	x, _ := parseSemver(a)
	y, _ := parseSemver(b)
	for i := 0; i < 3; i++ {
		if x.n[i] != y.n[i] {
			return x.n[i] - y.n[i]
		}
	}
	switch {
	case x.pre == y.pre:
		return 0
	case x.pre == "":
		return 1
	case y.pre == "":
		return -1
	}
	return strings.Compare(x.pre, y.pre)
}

// Dismiss hides the update banner for 30 days.
func (s *Service) Dismiss(ctx context.Context) (time.Time, error) {
	until := s.now().Add(dismissFor)
	return until, s.Store.SetSetting(ctx, keyDismissed, until.Format(time.RFC3339))
}

// State returns the update state shown in the UI.
func (s *Service) State(ctx context.Context) *State {
	st := &State{Enabled: s.Enabled, Current: s.Current, Repo: s.Repo, Releases: []Release{}}
	if b, _ := s.Store.GetSetting(ctx, keyReleases); b != "" {
		_ = json.Unmarshal([]byte(b), &st.Releases)
	}
	if s.Rollout != nil {
		st.Cluster = s.Rollout.State(ctx)
	}
	if v, _ := s.Store.GetSetting(ctx, keyLatest); v != "" {
		var r Release
		if json.Unmarshal([]byte(v), &r) == nil && r.Version != "" {
			st.Latest = &r
		}
	}
	if v, _ := s.Store.GetSetting(ctx, keyAvailable); v == "true" && st.Latest != nil {
		// Still newer than what runs now (the setting predates an update that already happened).
		if _, ok := parseSemver(s.Current); !ok || compareSemver(st.Latest.Version, s.Current) > 0 {
			st.Available = true
		}
	}
	if v, _ := s.Store.GetSetting(ctx, keyCheckedAt); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			st.CheckedAt = &t
		}
	}
	st.CheckError, _ = s.Store.GetSetting(ctx, keyError)
	if v, _ := s.Store.GetSetting(ctx, keyDismissed); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(s.now()) {
			st.DismissedUntil = &t
		}
	}
	st.Agent = s.agent()
	if s.Dir != "" {
		_, err := os.Lstat(filepath.Join(s.Dir, "request"))
		st.Pending = err == nil
		if st.Pending {
			if b, err := s.readSmall("request", 4096); err == nil {
				for _, line := range strings.Split(string(b), "\n") {
					if strings.HasPrefix(line, "version=") {
						st.PendingVersion = strings.TrimPrefix(line, "version=")
						break
					}
				}
			}
		}
		st.Status = s.status()
		if st.Status != nil {
			st.Log = s.logTail()
		}
	}
	st.ShowBanner = st.Available && st.DismissedUntil == nil && !st.Pending && (st.Status == nil || st.Status.State != "running")
	if len(st.Releases) == 0 && st.Latest != nil {
		st.Releases = []Release{*st.Latest}
	}
	return st
}

func (s *Service) readSmall(name string, limit int64) ([]byte, error) {
	f, err := os.Open(filepath.Join(s.Dir, name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

func (s *Service) agent() Agent {
	if s.Dir == "" {
		return Agent{}
	}
	b, err := s.readSmall("agent.json", 4<<10)
	if err != nil {
		return Agent{}
	}
	var a struct {
		Agent int    `json:"agent"`
		Mode  string `json:"mode"`
	}
	if json.Unmarshal(b, &a) != nil || a.Agent < 1 {
		return Agent{}
	}
	return Agent{Available: true, Mode: a.Mode}
}

func (s *Service) status() *Status {
	b, err := s.readSmall("status.json", 64<<10)
	if err != nil {
		return nil
	}
	var st Status
	if json.Unmarshal(b, &st) != nil || st.State == "" {
		return nil
	}
	return &st
}

func (s *Service) logTail() string {
	f, err := os.Open(filepath.Join(s.Dir, "update.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	const max = 16 << 10
	if fi, err := f.Stat(); err == nil && fi.Size() > max {
		_, _ = f.Seek(fi.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return string(b)
}

// Request asks the host agent to install version, which must be the latest release and newer
// than the running version.
func (s *Service) Request(ctx context.Context, version, actor string) error {
	return s.request(ctx, version, actor, false)
}

// RequestVersion pins a rolling update to its confirmed release, even if GitHub has since
// published another one. Every node verifies the release itself; peers cannot supply artifacts.
func (s *Service) RequestVersion(ctx context.Context, version, actor string) error {
	v, ok := parseSemver(version)
	if !ok || v.pre != "" {
		return ErrNotAvailable
	}
	if s.Current == version {
		return nil
	}
	st := s.State(ctx)
	if st.Pending || (st.Status != nil && st.Status.Target == version && st.Status.State == "running") {
		return ErrBusy
	}
	newer, err := s.isNewer(ctx, version)
	if err != nil {
		return err
	}
	if !newer {
		return ErrNotAvailable
	}
	var rel ghRelease
	if _, err := s.get(ctx, "/repos/"+s.Repo+"/releases/tags/"+url.PathEscape(version), &rel); err != nil {
		return err
	}
	if rel.TagName != version || rel.Draft || rel.Prerelease {
		return ErrNotAvailable
	}
	return s.request(ctx, version, actor, true)
}

func (s *Service) request(ctx context.Context, version, actor string, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.State(ctx)
	if !st.Agent.Available {
		return ErrNoAgent
	}
	if !semverRe.MatchString(version) || (!pinned && (st.Latest == nil || st.Latest.Version != version || !st.Available)) {
		return ErrNotAvailable
	}
	if st.Pending {
		return ErrBusy
	}
	if st.Status != nil && st.Status.State == "running" {
		if t, err := time.Parse(time.RFC3339, st.Status.StartedAt); err != nil || s.now().Sub(t) < staleStatus {
			return ErrBusy
		}
	}
	actor = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, actor)
	f, err := os.CreateTemp(s.Dir, ".request-*")
	if err != nil {
		return fmt.Errorf("cannot write the update request (is %s writable by the container?): %w", s.Dir, err)
	}
	_, err = fmt.Fprintf(f, "version=%s\nfrom=%s\nrequested_by=%s\nrequested_at=%s\n", version, s.Current, actor, s.now().Format(time.RFC3339))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.Dir, "request"))
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	s.Log.Warn("update requested", "from", s.Current, "to", version, "by", actor)
	return nil
}
