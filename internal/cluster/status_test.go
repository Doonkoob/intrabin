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
