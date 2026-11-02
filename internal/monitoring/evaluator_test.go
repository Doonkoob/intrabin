package monitoring

import (
	"fmt"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

func mon(p model.Policy) *model.Monitor {
	return &model.Monitor{ID: "m1", Version: 1, IntervalSeconds: 30, Policy: p}
}

func res(node string, ok bool, round time.Time) *model.CheckResult {
	lat := 10.0
	r := &model.CheckResult{ID: fmt.Sprintf("%s-%d", node, round.Unix()), MonitorID: "m1", NodeID: node, ConfigVersion: 1, RoundStart: round, ProbeAt: round.Add(time.Second), Success: ok, LatencyMs: &lat}
	if !ok {
		r.ErrorCategory = model.ErrTimeout
	}
	return r
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func policy3Nodes() model.Policy {
	return model.Policy{ConfirmationsRequired: 2, MinLocations: 2, MinValidProbes: 2, FailMode: model.FailConsecutive, FailureRounds: 6, RecoveryRounds: 2}
}

func TestVerdictRequiresIndependentConfirmation(t *testing.T) {
	m := mon(policy3Nodes())
	// One isolated agent fails: partial, never failed.
	rr := RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0), res("b", true, t0), res("c", true, t0)}})
	if rr.Verdict != model.VerdictPartial {
		t.Fatalf("got %s", rr.Verdict)
	}
	rr = RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0), res("b", false, t0), res("c", true, t0)}})
	if rr.Verdict != model.VerdictFailed || rr.FailingProbes != 2 {
		t.Fatalf("got %s", rr.Verdict)
	}
}

func TestMinLocations(t *testing.T) {
	m := mon(policy3Nodes())
	loc := func(id string) string {
		if id == "a" || id == "b" {
			return "dc1"
		}
		return "dc2"
	}
	// Two failing nodes but same location → not enough independent locations.
	rr := RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Location: loc, Results: []*model.CheckResult{res("a", false, t0), res("b", false, t0), res("c", true, t0)}})
	if rr.Verdict != model.VerdictPartial {
		t.Fatalf("got %s", rr.Verdict)
	}
}

func TestMissingDataIsInconclusiveNotDown(t *testing.T) {
	m := mon(policy3Nodes())
	rr := RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0)}})
	if rr.Verdict != model.VerdictInconclusive {
		t.Fatalf("got %s", rr.Verdict)
	}
	st := model.MonitorState{Status: model.StatusUp, ConsecutiveFailed: 2}
	in := RoundInput{Monitor: m, Now: t0}
	for i := 0; i < 2; i++ {
		st = NextState(st, rr, in)
		if st.Status != model.StatusUp || st.ConsecutiveFailed != 2 {
			t.Fatalf("round %d: inconclusive changed state: %+v", i, st)
		}
	}
	st = NextState(st, rr, in)
	if st.Status != model.StatusUnknown {
		t.Fatalf("expected UNKNOWN after 3 inconclusive, got %s", st.Status)
	}
}

func TestStaleConfigVersionIgnored(t *testing.T) {
	m := mon(model.DefaultPolicy())
	m.Version = 2
	rr := RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0)}})
	if rr.Verdict != model.VerdictInconclusive {
		t.Fatalf("stale version counted: %s", rr.Verdict)
	}
}

func TestOneResultPerNodePerRound(t *testing.T) {
	m := mon(model.Policy{ConfirmationsRequired: 2, MinLocations: 1, MinValidProbes: 1, FailureRounds: 1, RecoveryRounds: 1})
	r1 := res("a", false, t0)
	r2 := res("a", false, t0)
	r2.ID = "dup"
	r2.ProbeAt = t0.Add(5 * time.Second)
	rr := RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{r1, r2}})
	if rr.FailingProbes != 1 || rr.Verdict != model.VerdictPartial {
		t.Fatalf("same node counted twice: %+v", rr)
	}
}

// Six probes from three agents across two rounds must NOT count as six consecutive failures.
func TestConsecutiveRoundsNotProbes(t *testing.T) {
	m := mon(policy3Nodes())
	st := model.MonitorState{Status: model.StatusUp}
	for round := 0; round < 2; round++ {
		rs := t0.Add(time.Duration(round*30) * time.Second)
		in := RoundInput{Monitor: m, RoundStart: rs, Now: rs, Results: []*model.CheckResult{res("a", false, rs), res("b", false, rs), res("c", false, rs)}}
		st = NextState(st, RoundVerdict(in), in)
	}
	if st.Status != model.StatusSuspectedDown || st.ConsecutiveFailed != 2 {
		t.Fatalf("expected SUSPECTED_DOWN after 2 rounds, got %s (%d)", st.Status, st.ConsecutiveFailed)
	}
	for round := 2; round < 6; round++ {
		rs := t0.Add(time.Duration(round*30) * time.Second)
		in := RoundInput{Monitor: m, RoundStart: rs, Now: rs, Results: []*model.CheckResult{res("a", false, rs), res("b", false, rs), res("c", true, rs)}}
		st = NextState(st, RoundVerdict(in), in)
		if round < 5 && st.Status != model.StatusSuspectedDown {
			t.Fatalf("round %d: premature %s", round, st.Status)
		}
	}
	if st.Status != model.StatusDown {
		t.Fatalf("expected DOWN after 6 failed rounds, got %s", st.Status)
	}
}

func TestPartialResetsConsecutive(t *testing.T) {
	m := mon(policy3Nodes())
	st := model.MonitorState{Status: model.StatusUp}
	in := RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0), res("b", false, t0), res("c", false, t0)}}
	st = NextState(st, RoundVerdict(in), in)
	in2 := RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0), res("b", true, t0), res("c", true, t0)}}
	st = NextState(st, RoundVerdict(in2), in2)
	if st.Status != model.StatusDegraded || st.ConsecutiveFailed != 0 {
		t.Fatalf("got %+v", st)
	}
}

func TestWindowMode(t *testing.T) {
	p := model.Policy{ConfirmationsRequired: 1, MinLocations: 1, MinValidProbes: 1, FailMode: model.FailWindow, FailureRounds: 4, WindowSeconds: 180, RecoveryRounds: 2}
	m := mon(p)
	st := model.MonitorState{Status: model.StatusUp}
	fail := RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", false, t0)}}
	// 3 earlier failures (non-consecutive) inside the window plus this one → DOWN.
	fail.RecentFailedRounds = 3
	st = NextState(st, RoundVerdict(fail), fail)
	if st.Status != model.StatusDown {
		t.Fatalf("window mode: got %s", st.Status)
	}
	st2 := NextState(model.MonitorState{Status: model.StatusUp}, RoundVerdict(RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: fail.Results, RecentFailedRounds: 1}), RoundInput{Monitor: m, Now: t0, RecentFailedRounds: 1})
	if st2.Status != model.StatusSuspectedDown {
		t.Fatalf("window mode below threshold: got %s", st2.Status)
	}
}

func TestRecoveryThreshold(t *testing.T) {
	m := mon(model.Policy{ConfirmationsRequired: 1, MinLocations: 1, MinValidProbes: 1, FailureRounds: 1, RecoveryRounds: 3})
	st := model.MonitorState{Status: model.StatusDown, ConsecutiveFailed: 4}
	ok := RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{res("a", true, t0)}}
	for i := 1; i <= 3; i++ {
		st = NextState(st, RoundVerdict(ok), ok)
		if i < 3 && st.Status != model.StatusDown {
			t.Fatalf("recovered after %d rounds", i)
		}
	}
	if st.Status != model.StatusUp {
		t.Fatalf("expected UP, got %s", st.Status)
	}
	// While DOWN, inconclusive rounds do not imply recovery.
	st = model.MonitorState{Status: model.StatusDown}
	inc := RoundInput{Monitor: m, RoundStart: t0, Now: t0}
	for i := 0; i < 5; i++ {
		st = NextState(st, RoundVerdict(inc), inc)
	}
	if st.Status != model.StatusDown {
		t.Fatalf("inconclusive changed DOWN to %s", st.Status)
	}
}

func TestMaintenance(t *testing.T) {
	m := mon(model.DefaultPolicy())
	in := RoundInput{Monitor: m, RoundStart: t0, Now: t0, InMaintenance: true, Results: []*model.CheckResult{res("a", false, t0)}}
	rr := RoundVerdict(in)
	st := NextState(model.MonitorState{Status: model.StatusUp, ConsecutiveFailed: 1}, rr, in)
	if rr.Verdict != model.VerdictMaintenance || st.Status != model.StatusMaintenance || st.ConsecutiveFailed != 1 {
		t.Fatalf("maintenance: %s %+v", rr.Verdict, st)
	}
}

func TestSlowIsDegraded(t *testing.T) {
	m := mon(model.DefaultPolicy())
	r := res("a", true, t0)
	r.Slow = true
	in := RoundInput{Monitor: m, RoundStart: t0, Now: t0, Results: []*model.CheckResult{r}}
	st := NextState(model.MonitorState{Status: model.StatusUp}, RoundVerdict(in), in)
	if st.Status != model.StatusDegraded {
		t.Fatalf("got %s", st.Status)
	}
}
