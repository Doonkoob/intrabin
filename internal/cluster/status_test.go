package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/node"
)

func TestLiveMemberIdentityAndVersionAfterUpdate(t *testing.T) {
	ctx := context.Background()
	leader, worker := newTNode(t, "same-host"), newTNode(t, "same-host")
	defer leader.close()
	defer worker.close()
	if err := leader.m.Init(ctx, ""); err != nil {
		t.Fatal(err)
	}
	token, _, err := leader.m.CreateToken(ctx, 5*time.Minute, "worker", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.m.Join(ctx, token, "", ""); err != nil {
		t.Fatal(err)
	}
	id := worker.n.Identity.ID
	// Pause periodic refresh so the injected heartbeat cannot be overwritten mid-assertion.
	leader.m.cancel()
	leader.m.wg.Wait()
	// Simulate a refreshed process heartbeat while its enrollment metadata is stale.
	leader.m.mesh.mu.Lock()
	leader.m.mesh.views[id] = peerView{At: time.Now(), Version: "v0.2.6", Name: "same-host", APIURL: "https://worker-new.example"}
	leader.m.mesh.mu.Unlock()
	status := leader.m.Status(ctx)
	for _, member := range status.Members {
		if member.ID != id {
			continue
		}
		if member.Version != "v0.2.6" || !strings.Contains(member.Label, "worker-new.example") || !strings.Contains(member.Label, id[:8]) {
			t.Fatalf("stale identity/version: %+v", member)
		}
		return
	}
	t.Fatal("worker missing")
}

func TestWaitAppliedRejectsUnreplicatedSession(t *testing.T) {
	n := newTNode(t, "n")
	defer n.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := n.m.waitApplied(ctx, 99); err == nil {
		t.Fatal("unapplied state reported ready")
	}
	if err := n.store.SetSetting(context.Background(), appliedIndexKey, "99"); err != nil {
		t.Fatal(err)
	}
	if err := n.m.waitApplied(context.Background(), 99); err != nil {
		t.Fatal(err)
	}
	// The cluster role must be conservative before the first election has completed.
	if n.n.Role() != node.RoleStandalone {
		t.Fatal("disabled cluster changed node role")
	}
}

func TestCustomNameSurvivesRestartAndHeartbeats(t *testing.T) {
	ctx := context.Background()
	leader, worker := newTNode(t, "same-host"), newTNode(t, "same-host")
	defer leader.close()
	defer worker.close()
	if err := leader.m.Init(ctx, ""); err != nil {
		t.Fatal(err)
	}
	token, _, err := leader.m.CreateToken(ctx, 5*time.Minute, "worker", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.m.Join(ctx, token, "", ""); err != nil {
		t.Fatal(err)
	}
	id := worker.n.Identity.ID
	if err := leader.m.SetName(ctx, id, "London-01"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "custom name replicated", 5*time.Second, func() bool { return worker.m.LocalLabel() == "London-01" })
	worker.close()
	worker.open(t, "different-hostname")
	if err := worker.m.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if worker.n.Identity.ID != id || worker.m.LocalLabel() != "London-01" {
		t.Fatalf("rename lost on restart: %+v %s", worker.n.Identity, worker.m.LocalLabel())
	}
	waitFor(t, "fresh heartbeat after restart", 10*time.Second, func() bool {
		st := leader.m.Status(ctx)
		for _, member := range st.Members {
			if member.ID == id {
				return member.Reachable && member.Name == "London-01" && member.Label == "London-01" && member.NodeKey == "London-01"
			}
		}
		return false
	})
	if resolved, err := worker.m.ResolveNodeID(ctx, "LONDON-01"); err != nil || resolved != id {
		t.Fatalf("alias resolution after restart: %s %v", resolved, err)
	}
}
