package cluster

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/automation"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
)

// TestFailoverDuringActionYieldsUnknownOutcome kills the leader while an HTTP action is in flight.
// The new leader must not replay the action: it records UNKNOWN_OUTCOME, replicated to all nodes.
func TestFailoverDuringActionYieldsUnknownOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns := threeVoters(t)
	waitFor(t, "followers", 10*time.Second, func() bool {
		return ns[1].m.computeRole() == node.RoleFollower && ns[2].m.computeRole() == node.RoleFollower
	})
	waitFor(t, "roles published", 5*time.Second, func() bool { return ns[0].n.Role() == node.RoleLeader })

	// Fencing: only the gated leader gets a fence.
	term, leader, err := ns[0].m.Fence(ctx)
	if err != nil || term == 0 || leader != ns[0].n.Identity.ID {
		t.Fatalf("leader fence: %d %q %v", term, leader, err)
	}
	if _, _, err := ns[1].m.Fence(ctx); !errors.Is(err, controlplane.ErrNotLeader) {
		t.Fatalf("follower fence must fail, got %v", err)
	}

	var hits atomic.Int32
	started := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(3 * time.Second)
	}))
	defer target.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, n := range ns {
		e := &automation.Engine{Store: n.store, CP: n.cp, Fence: n.m.Fence, Active: n.n.CanManage, Box: n.box, Log: log, Interval: 200 * time.Millisecond}
		ectx, ecancel := context.WithCancel(ctx)
		defer ecancel()
		go e.Run(ectx)
	}
	now := time.Now().UTC()
	wf := &model.Workflow{ID: uuid.NewString(), Name: "failover", Enabled: true, Trigger: model.WorkflowTrigger{Type: "manual"}, Version: 1, CreatedAt: now, UpdatedAt: now,
		Steps: []model.WorkflowStep{{Name: "hook", Type: "http", Method: "POST", URL: target.URL, TimeoutSeconds: 10, AllowPrivateNetwork: true}}}
	if err := ns[0].cp.CreateWorkflow(ctx, wf); err != nil {
		t.Fatal(err)
	}
	run := &model.WorkflowRun{ID: uuid.NewString(), WorkflowID: wf.ID, WorkflowVersion: 1, Steps: wf.Steps, TriggerType: "manual", Context: map[string]string{},
		Status: model.RunPending, CreatedAt: now, UpdatedAt: now}
	if err := ns[0].cp.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader never dispatched the action")
	}
	ns[0].close() // the leader dies while the request is in flight

	var newLeader *tnode
	waitFor(t, "new leader", 15*time.Second, func() bool {
		for _, n := range ns[1:] {
			if n.n.Role() == node.RoleLeader {
				newLeader = n
				return true
			}
		}
		return false
	})
	waitFor(t, "unknown outcome replicated", 15*time.Second, func() bool {
		for _, n := range ns[1:] {
			r, err := n.store.GetRun(ctx, run.ID)
			if err != nil || r.Status != model.RunUnknownOutcome {
				return false
			}
		}
		return true
	})
	acts, _ := newLeader.store.RunActions(ctx, run.ID)
	if len(acts) != 1 || acts[0].Status != model.ActionUnknownOutcome || acts[0].FenceLeader != ns[0].n.Identity.ID || acts[0].FenceTerm != term {
		t.Fatalf("unexpected action record %+v", acts)
	}
	time.Sleep(4 * time.Second)
	if hits.Load() != 1 {
		t.Fatalf("the action was dispatched %d times, want exactly 1", hits.Load())
	}
}
