package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeCluster struct {
	self   string
	active bool
	peers  []UpdatePeer
	states map[string]*State
	calls  []string
	err    error
}

func (f *fakeCluster) UpdateMembers(context.Context) (string, bool, []UpdatePeer) {
	return f.self, f.active, append([]UpdatePeer{}, f.peers...)
}
func (f *fakeCluster) PeerUpdateState(_ context.Context, id string) (*State, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.states[id], nil
}
func (f *fakeCluster) RequestPeerUpdate(_ context.Context, id, version string) error {
	if !f.states[id].Pending {
		f.calls = append(f.calls, id)
		f.states[id].Pending = true
	}
	return nil
}

func TestRollingUpdateSurvivesLeaderChangeAndStopsOnFailure(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, fakeGitHub(t, "v1.3.0", "").URL, "v1.2.0", t.TempDir())
	os.WriteFile(filepath.Join(s.Dir, "agent.json"), []byte(`{"agent":1}`), 0600)
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	f := &fakeCluster{self: "a", active: true, peers: []UpdatePeer{{ID: "a", Name: "leader", Voter: true, Reachable: true}, {ID: "b", Name: "voter", Voter: true, Reachable: true}, {ID: "c", Name: "worker", Reachable: true}}, states: map[string]*State{}}
	for _, id := range []string{"a", "b", "c"} {
		f.states[id] = &State{Current: "v1.2.0", Agent: Agent{Available: true}}
	}
	c := &Rollout{Service: s, Cluster: f, Store: s.Store, Write: s.Store.SetSetting}
	if err := c.Start(ctx, "v1.3.0", "owner"); err != nil {
		t.Fatal(err)
	}
	p := c.State(ctx)
	if p.Nodes[0].ID != "a" || p.Nodes[1].ID != "c" {
		t.Fatalf("leader, then non-voters: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "request")); err != nil {
		t.Fatal(err)
	}
	c.Step(ctx)
	if len(f.calls) != 0 {
		t.Fatal("must wait for leader to finish")
	}
	// Leader a returns on the target version, while b now leads. A new coordinator uses the
	// replicated plan; no in-memory task is needed to finish it.
	f.self = "b"
	s2 := newSvc(t, s.APIBase, "v1.2.0", t.TempDir())
	s2.Store = s.Store
	c = &Rollout{Service: s2, Cluster: f, Store: s.Store, Write: s.Store.SetSetting}
	f.states["a"].Current = "v1.3.0"
	c.Step(ctx)
	f.peers[2].Reachable = false
	c.Step(ctx)
	if len(f.calls) != 0 {
		t.Fatal("unreachable member must pause rollout")
	}
	f.peers[2].Reachable = true
	c.Step(ctx)
	c.Step(ctx)
	if len(f.calls) != 1 || f.calls[0] != "c" {
		t.Fatalf("one worker at a time: %v", f.calls)
	}
	progress := 57
	f.states["c"].Status = &Status{State: "running", Target: "v1.3.0", StartedAt: time.Now().UTC().Format(time.RFC3339), Progress: &progress, Stage: "building", Service: "runner"}
	c.Step(ctx)
	if p := c.State(ctx); p.Nodes[1].Progress == nil || *p.Nodes[1].Progress != 57 || p.Nodes[1].Service != "runner" {
		t.Fatalf("worker deployment progress must be replicated: %+v", p)
	}
	f.states["c"].Pending = false
	f.states["c"].Status = &Status{State: "failed", Target: "v1.3.0", StartedAt: time.Now().UTC().Format(time.RFC3339), Message: "build failed"}
	c.Step(ctx)
	if c.State(ctx).State != "failed" {
		t.Fatal("failure must stop rollout")
	}
	c.Step(ctx)
	if len(f.calls) != 1 {
		t.Fatal("failed update was retried automatically")
	}
	if err := c.Retry(ctx); err != nil {
		t.Fatal(err)
	}
	f.states["c"].Current = "v1.3.0"
	c.Step(ctx)
	// Current leader has no agent in this test; failure must be visible rather than skipped.
	c.Step(ctx)
	if c.State(ctx).Nodes[2].Error != ErrNoAgent.Error() {
		t.Fatalf("missing agent: %+v", c.State(ctx))
	}
}

func TestRolloutDoesNotDispatchWithoutLeadershipOrCommit(t *testing.T) {
	s := newSvc(t, "", "v1.0.0", "")
	f := &fakeCluster{self: "a", active: false}
	c := &Rollout{Service: s, Cluster: f, Store: s.Store, Write: s.Store.SetSetting}
	if c.Start(context.Background(), "v1.1.0", "owner") == nil {
		t.Fatal("follower started update")
	}
	p := &RolloutPlan{Version: "v1.1.0", State: "running", Nodes: []RolloutNode{{ID: "b", State: "queued"}}}
	c.save(context.Background(), p)
	c.Step(context.Background())
	if len(f.calls) != 0 {
		t.Fatal("follower dispatched update")
	}
	f.active = true
	f.peers = []UpdatePeer{{ID: "b", Reachable: true}}
	f.states = map[string]*State{"b": {Current: "v1.0.0", Agent: Agent{Available: true}}}
	c.Write = func(context.Context, string, string) error { return errors.New("quorum lost") }
	c.Step(context.Background())
	if len(f.calls) != 0 {
		t.Fatal("dispatch without replicated active node")
	}
}

func TestRequestPinnedReleaseAndHistory(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/intrabin/releases/latest":
			json.NewEncoder(w).Encode(ghRelease{TagName: "v1.4.0"})
		case "/repos/acme/intrabin/releases":
			json.NewEncoder(w).Encode([]ghRelease{{TagName: "v1.4.0", Body: "New charts"}, {TagName: "v1.3.0", Body: "Cluster updates"}, {TagName: "v1.5.0-rc1", Prerelease: true}, {TagName: "v9.0.0", Draft: true}})
		case "/repos/acme/intrabin/releases/tags/v1.3.0":
			json.NewEncoder(w).Encode(ghRelease{TagName: "v1.3.0"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	s := newSvc(t, gh.URL, "v1.2.0", t.TempDir())
	os.WriteFile(filepath.Join(s.Dir, "agent.json"), []byte(`{"agent":1}`), 0600)
	ctx := context.Background()
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.State(ctx).Releases; len(got) != 2 || got[1].Notes != "Cluster updates" {
		t.Fatalf("history: %+v", got)
	}
	if err := s.RequestVersion(ctx, "v1.3.0", "cluster"); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestVersion(ctx, "v1.3.0", "cluster"); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate: %v", err)
	}
	os.Remove(filepath.Join(s.Dir, "request"))
	if err := s.RequestVersion(ctx, "v1.1.0", "cluster"); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("downgrade: %v", err)
	}
	if err := s.RequestVersion(ctx, "v1.9.0", "cluster"); err == nil {
		t.Fatal("unpublished version accepted")
	}
}

func TestRollingUpdateCompletesOneNodeAtATime(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, fakeGitHub(t, "v1.3.0", "").URL, "v1.2.0", t.TempDir())
	os.WriteFile(filepath.Join(s.Dir, "agent.json"), []byte(`{"agent":1}`), 0600)
	s.Check(ctx)
	f := &fakeCluster{self: "a", active: true, peers: []UpdatePeer{{ID: "a", Voter: true, Reachable: true}, {ID: "b", Voter: true, Reachable: true}, {ID: "c", Reachable: true}}, states: map[string]*State{}}
	for _, id := range []string{"a", "b", "c"} {
		f.states[id] = &State{Current: "v1.2.0", Agent: Agent{Available: true}}
	}
	c := &Rollout{Service: s, Cluster: f, Store: s.Store, Write: s.Store.SetSetting}
	if err := c.Start(ctx, "v1.3.0", "owner"); err != nil {
		t.Fatal(err)
	}
	// Restarting the leader replaces the service/coordinator. Its current version is now the
	// target, and its old available=true cache must not create another update.
	next := newSvc(t, s.APIBase, "v1.3.0", t.TempDir())
	next.Store = s.Store
	c = &Rollout{Service: next, Cluster: f, Store: s.Store, Write: s.Store.SetSetting}
	c.Step(ctx)
	c.Step(ctx)
	c.Step(ctx)
	if len(f.calls) != 1 || f.calls[0] != "c" {
		t.Fatalf("dispatches: %v", f.calls)
	}
	f.states["c"].Current = "v1.3.0"
	f.states["c"].Pending = false
	c.Step(ctx)
	c.Step(ctx)
	if len(f.calls) != 2 || f.calls[1] != "b" {
		t.Fatalf("dispatches: %v", f.calls)
	}
	f.states["b"].Current = "v1.3.0"
	f.states["b"].Pending = false
	c.Step(ctx)
	c.Step(ctx)
	if p := c.State(ctx); p.State != "succeeded" {
		t.Fatalf("completion: %+v", p)
	}
	c.Step(ctx)
	if len(f.calls) != 2 {
		t.Fatal("completed plan dispatched again")
	}
}

func TestExistingRolloutLabelsRefreshByID(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, "", "v0.2.6", t.TempDir())
	f := &fakeCluster{peers: []UpdatePeer{{ID: "a", Name: "https://leader.example · a"}, {ID: "b", Name: "203.0.113.2:7443 · b"}}}
	c := &Rollout{Cluster: f, Store: s.Store, Write: s.Store.SetSetting}
	p := &RolloutPlan{Version: "v0.2.6", State: "running", Nodes: []RolloutNode{{ID: "a", Name: "same-host", State: "succeeded", Version: "v0.2.6"}, {ID: "b", Name: "same-host", State: "queued"}}}
	if err := c.save(ctx, p); err != nil {
		t.Fatal(err)
	}
	got := c.State(ctx)
	if got.Nodes[0].Name != f.peers[0].Name || got.Nodes[1].Name != f.peers[1].Name || got.Nodes[0].State != "succeeded" || got.Nodes[1].State != "queued" {
		t.Fatalf("legacy plan labels/progress: %+v", got)
	}
}

func TestQueuedAgentTimeoutStopsRolloutUntilRepair(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, "", "v1.3.0", t.TempDir())
	cluster := &fakeCluster{self: "leader", active: true, peers: []UpdatePeer{{ID: "leader", Name: "leader", Reachable: true}, {ID: "worker", Name: "worker", Reachable: true}, {ID: "next", Name: "next", Reachable: true}}, states: map[string]*State{
		"worker": {Current: "v1.2.0", Agent: Agent{Available: true}, Pending: true, PendingVersion: "v1.3.0"},
		"next":   {Current: "v1.2.0", Agent: Agent{Available: true}},
	}}
	c := &Rollout{Service: s, Cluster: cluster, Store: s.Store, Write: s.Store.SetSetting}
	plan := &RolloutPlan{Version: "v1.3.0", State: "running", Nodes: []RolloutNode{{ID: "leader", State: "succeeded", Version: "v1.3.0"}, {ID: "worker", State: "updating", StartedAt: time.Now().Add(-agentPickupTimeout - time.Second), Stage: "queued"}, {ID: "next", State: "queued"}}}
	if err := c.save(ctx, plan); err != nil {
		t.Fatal(err)
	}
	c.Step(ctx)
	got := c.State(ctx)
	if got.State != "failed" || got.Nodes[1].State != "failed" || got.Nodes[2].State != "queued" || len(cluster.calls) != 0 {
		t.Fatalf("unclaimed request must pause the rollout: %+v calls=%v", got, cluster.calls)
	}
	if got.Nodes[1].Error == "" {
		t.Fatal("missing actionable pickup error")
	}
	// A late-starting agent can finish its queued request. Retry observes the actual version
	// and advances only then; it never starts another member while the first is still queued.
	cluster.states["worker"] = &State{Current: "v1.3.0", Agent: Agent{Available: true}}
	if err := c.Retry(ctx); err != nil {
		t.Fatal(err)
	}
	c.Step(ctx)
	if got := c.State(ctx); got.Nodes[1].State != "succeeded" {
		t.Fatalf("repaired worker not recognized: %+v", got)
	}
	c.Step(ctx)
	if len(cluster.calls) != 1 || cluster.calls[0] != "next" {
		t.Fatalf("next member should start after repaired worker: %v", cluster.calls)
	}
}

func TestRunningAgentIsNotTreatedAsUnclaimedRequest(t *testing.T) {
	ctx := context.Background()
	s := newSvc(t, "", "v1.3.0", t.TempDir())
	cluster := &fakeCluster{self: "leader", active: true, peers: []UpdatePeer{{ID: "leader", Reachable: true}, {ID: "worker", Reachable: true}}, states: map[string]*State{"worker": {Current: "v1.2.0", Agent: Agent{Available: true}, Status: &Status{Target: "v1.3.0", State: "running", StartedAt: time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339), Stage: "pulling"}}}}
	c := &Rollout{Service: s, Cluster: cluster, Store: s.Store, Write: s.Store.SetSetting}
	plan := &RolloutPlan{Version: "v1.3.0", State: "running", Nodes: []RolloutNode{{ID: "leader", State: "succeeded"}, {ID: "worker", State: "updating", StartedAt: time.Now().Add(-6 * time.Minute)}}}
	if err := c.save(ctx, plan); err != nil {
		t.Fatal(err)
	}
	c.Step(ctx)
	if got := c.State(ctx); got.State != "running" || got.Nodes[1].State != "updating" {
		t.Fatalf("active agent must keep working: %+v", got)
	}
}
