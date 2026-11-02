package automation

import (
	"context"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

func ptrF(v float64) *float64 { return &v }

func TestConditionTriggerFiresOnceWhenCrossing(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	mon := &model.Monitor{ID: "m1", Name: "shop", Target: "https://shop.example"}
	other := &model.Monitor{ID: "m2", Name: "other"}
	wf := v.workflow([]model.WorkflowStep{{Name: "wait", Type: "wait", Seconds: 1}}, nil,
		model.WorkflowTrigger{Type: "condition", MonitorIDs: []string{"m1"}, Metric: "latency_ms", Op: "gt", Threshold: 500, RepeatMinutes: 60})
	runs := func() []*model.WorkflowRun {
		rs, err := v.st.ListRuns(ctx, storage.RunFilter{WorkflowID: wf.ID})
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	t0 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	round := func(e *Engine, m *model.Monitor, minute int, lat float64) {
		at := t0.Add(time.Duration(minute) * time.Minute)
		e.OnRound(ctx, m, &model.RoundResult{MonitorID: m.ID, RoundStart: at, EvaluatedAt: at, Verdict: model.VerdictOK, AvgLatencyMs: ptrF(lat)}, model.MonitorState{Status: model.StatusUp})
	}
	round(v.e, mon, 0, 100)
	round(v.e, other, 0, 2000) // not matched by the filter
	if n := len(runs()); n != 0 {
		t.Fatalf("%d runs before the condition held", n)
	}
	round(v.e, mon, 1, 900)
	round(v.e, mon, 2, 950) // still true: no new run
	if rs := runs(); len(rs) != 1 || rs[0].TriggerType != "condition" || rs[0].Context["value"] != "900" || rs[0].Context["condition"] != "latency_ms > 500" {
		t.Fatalf("runs after crossing: %+v", rs)
	}
	round(v.e, mon, 3, 100)
	round(v.e, mon, 4, 900) // true again but inside the repeat window
	if n := len(runs()); n != 1 {
		t.Fatalf("%d runs inside the repeat window", n)
	}
	// A new leader (empty memory) re-evaluating the first crossing round cannot start a duplicate.
	fresh := &Engine{Store: v.st, CP: v.cp, Active: func() bool { return true }}
	round(fresh, mon, 1, 900)
	if n := len(runs()); n != 1 {
		t.Fatalf("duplicate run after leader change: %d", n)
	}
	round(v.e, mon, 70, 100)
	round(v.e, mon, 71, 900)
	if n := len(runs()); n != 2 {
		t.Fatalf("%d runs after the repeat window", n)
	}
	// Inactive (follower) nodes never start runs.
	v.active.Store(false)
	round(v.e, mon, 200, 100)
	round(v.e, mon, 201, 900)
	if n := len(runs()); n != 2 {
		t.Fatalf("follower started a run: %d", n)
	}
}

func TestConditionMetrics(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	exp := now.Add(5 * 24 * time.Hour)
	rr := &model.RoundResult{RoundStart: now, EvaluatedAt: now, Verdict: model.VerdictPartial, StatusCodes: []int{200, 503}, CertExpiresAt: &exp}
	m := &model.Monitor{ID: "m"}
	st := model.MonitorState{Status: model.StatusDegraded}
	for _, c := range []struct {
		trig  model.WorkflowTrigger
		holds bool
		known bool
	}{
		{model.WorkflowTrigger{Metric: "status_code", Op: "ge", Threshold: 500}, true, true},
		{model.WorkflowTrigger{Metric: "status_code", Op: "eq", Threshold: 404}, false, true},
		{model.WorkflowTrigger{Metric: "cert_days", Op: "lt", Threshold: 14}, true, true},
		{model.WorkflowTrigger{Metric: "cert_days", Op: "lt", Threshold: 3}, false, true},
		{model.WorkflowTrigger{Metric: "status", Op: "eq", StatusValue: "DEGRADED"}, true, true},
		{model.WorkflowTrigger{Metric: "status", Op: "ne", StatusValue: "DEGRADED"}, false, true},
		{model.WorkflowTrigger{Metric: "latency_ms", Op: "gt", Threshold: 1}, false, false},  // no latency in this round
		{model.WorkflowTrigger{Metric: "uptime_24h", Op: "lt", Threshold: 99}, false, false}, // no history yet
	} {
		holds, known, _ := v.e.evalCondition(ctx, c.trig, m, rr, st)
		if holds != c.holds || known != c.known {
			t.Errorf("%s: holds=%v known=%v, want %v %v", DescribeCondition(c.trig), holds, known, c.holds, c.known)
		}
	}
	maint := *rr
	maint.Verdict = model.VerdictMaintenance
	if _, known, _ := v.e.evalCondition(ctx, model.WorkflowTrigger{Metric: "status_code", Op: "ge", Threshold: 500}, m, &maint, st); known {
		t.Error("conditions must not be evaluated during maintenance")
	}
}
