package cluster

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/update"
)

func TestClusterUpdatePeerAuthorizationAndPinnedRelease(t *testing.T) {
	ctx := context.Background()
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/intrabin/releases/tags/v1.1.0" {
			io.WriteString(w, `{"tag_name":"v1.1.0"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer gh.Close()
	nodes := []*tnode{newTNode(t, "leader"), newTNode(t, "worker"), newTNode(t, "other")}
	defer func() {
		for _, n := range nodes {
			n.close()
		}
	}()
	for _, n := range nodes {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"agent":1}`), 0600)
		n.m.opt.Update = &update.Service{Enabled: true, Repo: "acme/intrabin", Current: "v1.0.0", Dir: dir, Store: n.store, APIBase: gh.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	}
	leader, worker, other := nodes[0], nodes[1], nodes[2]
	if err := leader.m.Init(ctx, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "leader", 5*time.Second, func() bool { return leader.m.GateOK() })
	for _, n := range nodes[1:] {
		tok, _, err := leader.m.CreateToken(ctx, 5*time.Minute, "worker", "owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = n.m.Join(ctx, tok, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "mesh", 10*time.Second, func() bool {
		return leader.m.mesh.bidirectional(worker.n.Identity.ID, 3*time.Second) && leader.m.mesh.bidirectional(other.n.Identity.ID, 3*time.Second)
	})
	// A member's certificate alone cannot initiate an update. Only the current Raft leader may.
	if err := other.m.RequestPeerUpdate(ctx, worker.n.Identity.ID, "v1.1.0"); err == nil {
		t.Fatal("non-leader dispatched update")
	}
	if err := leader.m.RequestPeerUpdate(ctx, worker.n.Identity.ID, "v1.1.0"); err == nil {
		t.Fatal("uncommitted update accepted")
	}
	plan := update.RolloutPlan{Version: "v1.1.0", State: "running", Nodes: []update.RolloutNode{{ID: worker.n.Identity.ID, State: "updating", StartedAt: time.Now().UTC()}}}
	b, _ := json.Marshal(plan)
	if err := leader.cp.SetSetting(ctx, update.RolloutKey, string(b)); err != nil {
		t.Fatal(err)
	}
	if err := leader.m.RequestPeerUpdate(ctx, worker.n.Identity.ID, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(worker.m.opt.Update.Dir, "request")
	req, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(req), "version=v1.1.0\n") {
		t.Fatalf("request %q: %v", req, err)
	}
	if err := leader.m.RequestPeerUpdate(ctx, worker.n.Identity.ID, "v1.1.0"); err != nil {
		t.Fatal("lost-reply retry", err)
	}
	again, _ := os.ReadFile(file)
	if string(again) != string(req) {
		t.Fatal("request was rewritten instead of acknowledged")
	}
	st, err := leader.m.PeerUpdateState(ctx, worker.n.Identity.ID)
	if err != nil || !st.Pending || st.Current != "v1.0.0" || st.Log != "" {
		t.Fatalf("state %+v: %v", st, err)
	}
}
