package monitoring

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

func newTestEngine(t *testing.T) (*Engine, *storage.Store, *time.Time) {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	e := &Engine{Store: st, Prober: NewProber("t"), Incidents: incidents.NewManager(st, log), Log: log, NodeID: "n1"}
	e.init()
	e.Now = func() time.Time { return now }
	return e, st, &now
}

func addResult(t *testing.T, st *storage.Store, m *model.Monitor, node string, rs time.Time, ok bool) {
	t.Helper()
	r := res(node, ok, rs)
	r.ID = node + rs.String()
	r.MonitorID = m.ID
	r.ReceivedAt = rs
	if _, err := st.InsertResults(context.Background(), []*model.CheckResult{r}); err != nil {
		t.Fatal(err)
	}
}

func TestEngineIncidentLifecycle(t *testing.T) {
	e, st, now := newTestEngine(t)
	ctx := context.Background()
	m := &model.Monitor{ID: "m1", Name: "Site", Type: model.MonitorHTTP, Target: "http://x", IntervalSeconds: 30, TimeoutSeconds: 5, Enabled: true, Version: 1,
		Policy:    model.Policy{ConfirmationsRequired: 1, MinLocations: 1, MinValidProbes: 1, FailMode: model.FailConsecutive, FailureRounds: 3, RecoveryRounds: 2, CooldownSeconds: 300},
		CreatedAt: *now, UpdatedAt: *now}
	if err := st.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}
	e.loadMonitors(ctx)
	interval := 30 * time.Second
	r0 := RoundStart(*now, interval)
	step := func(round int, ok bool) {
		rs := r0.Add(time.Duration(round) * interval)
		addResult(t, st, m, "n1", rs, ok)
		*now = rs.Add(interval).Add(EvalGrace)
		e.EvaluateDue(ctx)
	}
	step(0, true)
	s, _ := st.GetMonitorState(ctx, "m1")
	if s.Status != model.StatusUp {
		t.Fatalf("expected UP got %s", s.Status)
	}
	step(1, false)
	s, _ = st.GetMonitorState(ctx, "m1")
	if s.Status != model.StatusSuspectedDown {
		t.Fatalf("expected SUSPECTED got %s", s.Status)
	}
	in, err := st.OpenIncidentForMonitor(ctx, "m1")
	if err != nil || in.Status != model.IncidentSuspected {
		t.Fatalf("expected suspected incident: %v %+v", err, in)
	}
	// Missing round (agent produced nothing) is inconclusive — must not count as failure.
	*now = r0.Add(3 * interval).Add(EvalGrace)
	e.EvaluateDue(ctx)
	s, _ = st.GetMonitorState(ctx, "m1")
	if s.ConsecutiveFailed != 1 || s.Status != model.StatusSuspectedDown {
		t.Fatalf("missing data changed counters: %+v", s)
	}
	step(3, false)
	step(4, false)
	s, _ = st.GetMonitorState(ctx, "m1")
	if s.Status != model.StatusDown {
		t.Fatalf("expected DOWN got %s (%d)", s.Status, s.ConsecutiveFailed)
	}
	in, _ = st.OpenIncidentForMonitor(ctx, "m1")
	if in.Status != model.IncidentConfirmed || in.ConfirmedAt == nil {
		t.Fatalf("incident not confirmed: %+v", in)
	}
	if !in.StartedAt.Equal(r0.Add(interval)) {
		t.Errorf("incident start should be first failed round %v, got %v", r0.Add(interval), in.StartedAt)
	}
	incID := in.ID
	step(5, true)
	s, _ = st.GetMonitorState(ctx, "m1")
	if s.Status != model.StatusDown {
		t.Fatalf("recovered too early: %s", s.Status)
	}
	step(6, true)
	s, _ = st.GetMonitorState(ctx, "m1")
	if s.Status != model.StatusUp {
		t.Fatalf("expected UP got %s", s.Status)
	}
	in, _ = st.GetIncident(ctx, incID)
	if in.Status != model.IncidentResolved || in.Resolution != "recovered" {
		t.Fatalf("incident not resolved: %+v", in)
	}
	// Failure within cooldown reopens the same incident (deduplication).
	step(7, false)
	in2, err := st.OpenIncidentForMonitor(ctx, "m1")
	if err != nil || in2.ID != incID {
		t.Fatalf("expected reopened incident %s, got %+v %v", incID, in2, err)
	}
	evs, _ := st.ListIncidentEvents(ctx, incID)
	if len(evs) < 4 {
		t.Fatalf("expected audit trail events, got %d", len(evs))
	}
	// Uptime accounting: verdicts recorded per round.
	u, _ := st.Uptime(ctx, "m1", r0.Add(-time.Hour))
	if u.Failed != 4 || u.Inconclusive != 1 || u.OK != 3 {
		t.Fatalf("uptime stats: %+v", u)
	}
	if *u.Availability > 43 || *u.Availability < 42 {
		t.Fatalf("availability %v", *u.Availability)
	}
	if *u.Coverage > 88 || *u.Coverage < 87 {
		t.Fatalf("coverage %v", *u.Coverage)
	}
}

func TestEngineRunProbeAndDuplicateResult(t *testing.T) {
	e, st, _ := newTestEngine(t)
	ctx := context.Background()
	m := &model.Monitor{ID: "m2", Name: "TCP", Type: model.MonitorTCP, Target: "127.0.0.1:1", IntervalSeconds: 30, TimeoutSeconds: 1, Enabled: true, Version: 1, Policy: model.DefaultPolicy()}
	if err := st.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}
	r := e.RunProbe(ctx, m, RoundStart(e.Now(), 30*time.Second))
	if r.Success || r.ErrorCategory != model.ErrConnectionRefused {
		t.Fatalf("%+v", r)
	}
	n, err := st.InsertResults(ctx, []*model.CheckResult{r, r})
	if err != nil || n != 1 {
		t.Fatalf("duplicate not ignored: %d %v", n, err)
	}
	n, _ = st.InsertResults(ctx, []*model.CheckResult{r})
	if n != 0 {
		t.Fatal("re-delivery inserted twice")
	}
}

func TestMaintenanceSuppressesIncidents(t *testing.T) {
	e, st, now := newTestEngine(t)
	ctx := context.Background()
	m := &model.Monitor{ID: "m3", Name: "M", Type: model.MonitorHTTP, Target: "http://x", IntervalSeconds: 30, TimeoutSeconds: 5, Enabled: true, Version: 1,
		Policy: model.Policy{ConfirmationsRequired: 1, MinValidProbes: 1, MinLocations: 1, FailureRounds: 1, RecoveryRounds: 1}, CreatedAt: *now}
	st.CreateMonitor(ctx, m)
	st.CreateMaintenance(ctx, &model.MaintenanceWindow{ID: "w", MonitorID: "m3", Title: "upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour), CreatedAt: *now})
	e.loadMonitors(ctx)
	rs := RoundStart(*now, 30*time.Second)
	addResult(t, st, m, "n1", rs, false)
	*now = rs.Add(33 * time.Second)
	e.EvaluateDue(ctx)
	s, _ := st.GetMonitorState(ctx, "m3")
	if s.Status != model.StatusMaintenance {
		t.Fatalf("got %s", s.Status)
	}
	if _, err := st.OpenIncidentForMonitor(ctx, "m3"); err != storage.ErrNotFound {
		t.Fatal("incident opened during maintenance")
	}
}
