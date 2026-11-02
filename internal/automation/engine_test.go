package automation

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/runner"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "runner-exec" {
		os.Exit(runner.ExecWrapper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

const runnerToken = "engine-test-token-0123456789"

type countingRunner struct {
	Runner
	executes atomic.Int32
}

func (c *countingRunner) Execute(ctx context.Context, r *runner.Request) (*runner.Result, error) {
	c.executes.Add(1)
	return c.Runner.Execute(ctx, r)
}

type env struct {
	t      *testing.T
	st     *storage.Store
	cp     *controlplane.CP
	e      *Engine
	rs     *runner.Server
	runner *countingRunner
	fence  atomic.Value // error
	active atomic.Bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rs, err := runner.New(runner.Config{Token: runnerToken, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rs.Handler())
	t.Cleanup(ts.Close)
	box, _ := security.NewSecretBox(make([]byte, 32))
	cp := controlplane.New(st)
	inc := incidents.NewManager(controlplane.IncidentStore{Store: st, CP: cp}, nil)
	v := &env{t: t, st: st, cp: cp, rs: rs, runner: &countingRunner{Runner: &RunnerClient{URL: ts.URL, Token: runnerToken}}}
	v.fence.Store(errors.New(""))
	v.active.Store(true)
	v.e = &Engine{Store: st, CP: cp, Incidents: inc, Box: box, Runner: v.runner,
		Active: v.active.Load,
		Fence: func(context.Context) (uint64, string, error) {
			if err := v.fence.Load().(error); err.Error() != "" {
				return 0, "", err
			}
			return 7, "node-a", nil
		}}
	inc.OnTransition(v.e.OnIncidentTransition)
	return v
}

func (v *env) workflow(steps []model.WorkflowStep, secrets map[string]string, trig model.WorkflowTrigger) *model.Workflow {
	v.t.Helper()
	now := time.Now().UTC()
	if trig.Type == "" {
		trig.Type = "manual"
	}
	wf := &model.Workflow{ID: uuid.NewString(), Name: "wf", Enabled: true, Trigger: trig, Steps: steps, Version: 1, CreatedAt: now, UpdatedAt: now}
	blob, err := SealSecrets(v.e.Box, wf.ID, secrets)
	if err != nil {
		v.t.Fatal(err)
	}
	wf.SecretBlob = blob
	if err := v.cp.CreateWorkflow(context.Background(), wf); err != nil {
		v.t.Fatal(err)
	}
	return wf
}

// settle ticks the engine until the run reaches a status in want (or times out).
func (v *env) settle(runID string, want ...string) *model.WorkflowRun {
	v.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		v.e.Tick(context.Background())
		time.Sleep(50 * time.Millisecond)
		r, err := v.st.GetRun(context.Background(), runID)
		if err != nil {
			v.t.Fatal(err)
		}
		for _, w := range want {
			if r.Status == w {
				v.waitIdle()
				return r
			}
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("run stuck in %s (%s), want %v", r.Status, r.Error, want)
		}
	}
}

func (v *env) waitIdle() {
	for range 200 {
		v.e.mu.Lock()
		n := len(v.e.inflight)
		v.e.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (v *env) actions(runID string) []*model.WorkflowAction {
	a, err := v.st.RunActions(context.Background(), runID)
	if err != nil {
		v.t.Fatal(err)
	}
	return a
}

func TestScriptRunWithContextAndRedaction(t *testing.T) {
	v := newEnv(t)
	wf := v.workflow([]model.WorkflowStep{
		{Name: "say", Type: "script", Language: "bash", TimeoutSeconds: 10, Script: `echo "monitor=$INTRABIN_MONITOR_NAME token=$API_TOKEN extra=$EXTRA"`, Env: map[string]string{"EXTRA": "{{monitor_name}}!"}},
	}, map[string]string{"API_TOKEN": "s3cr3t-value"}, model.WorkflowTrigger{})
	run, err := v.e.TriggerManual(context.Background(), wf, "tester", map[string]string{"monitor_name": "web"})
	if err != nil {
		t.Fatal(err)
	}
	r := v.settle(run.ID, model.RunSucceeded, model.RunFailed)
	if r.Status != model.RunSucceeded {
		t.Fatalf("run %s: %s", r.Status, r.Error)
	}
	a := v.actions(run.ID)
	if len(a) != 1 || a[0].Status != model.ActionSucceeded || a[0].FenceTerm != 7 || a[0].FenceLeader != "node-a" {
		t.Fatalf("unexpected actions %+v", a[0])
	}
	if !strings.Contains(a[0].Output, "monitor=web") || !strings.Contains(a[0].Output, "extra=web!") || strings.Contains(a[0].Output, "s3cr3t-value") || !strings.Contains(a[0].Output, "[REDACTED]") {
		t.Fatalf("output %q", a[0].Output)
	}
}

func TestRetriesThenContinue(t *testing.T) {
	v := newEnv(t)
	wf := v.workflow([]model.WorkflowStep{
		{Name: "flaky", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "exit 1", Retries: 1, OnFailure: "continue"},
		{Name: "after", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "echo done"},
	}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	// The retry is delayed by a durable timer; shorten it by rewriting next_at as it appears.
	go func() {
		for range 400 {
			r, _ := v.st.GetRun(context.Background(), run.ID)
			if r != nil && r.Status == model.RunWaiting {
				past := time.Now().Add(-time.Second)
				r.NextAt = &past
				_ = v.st.ProgressRun(context.Background(), r, nil)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	r := v.settle(run.ID, model.RunSucceeded, model.RunFailed)
	acts := v.actions(run.ID)
	if r.Status != model.RunSucceeded || len(acts) != 3 || acts[0].Status != model.ActionFailed || acts[1].Status != model.ActionFailed || acts[2].Status != model.ActionSucceeded {
		t.Fatalf("status %s, %d actions", r.Status, len(acts))
	}
	if !strings.Contains(r.Error, "failed; continuing") {
		t.Fatalf("error %q", r.Error)
	}
}

func TestStopOnFailure(t *testing.T) {
	v := newEnv(t)
	wf := v.workflow([]model.WorkflowStep{
		{Name: "bad", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "echo nope; exit 4"},
		{Name: "never", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "echo never"},
	}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	r := v.settle(run.ID, model.RunFailed, model.RunSucceeded)
	if r.Status != model.RunFailed || len(v.actions(run.ID)) != 1 || !strings.Contains(r.Error, "exit code 4") {
		t.Fatalf("status %s error %q", r.Status, r.Error)
	}
}

func TestDurableWait(t *testing.T) {
	v := newEnv(t)
	wf := v.workflow([]model.WorkflowStep{{Name: "pause", Type: "wait", Seconds: 1}, {Name: "after", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "true"}}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	r := v.settle(run.ID, model.RunWaiting)
	if r.NextAt == nil || r.CurrentStep != 0 {
		t.Fatalf("expected a durable timer, got %+v", r)
	}
	// A "restart": a fresh engine over the same store picks the timer up.
	v.e = &Engine{Store: v.st, CP: v.cp, Box: v.e.Box, Runner: v.runner, Fence: v.e.Fence, Active: v.active.Load}
	r = v.settle(run.ID, model.RunSucceeded)
	if r.FinishedAt.Sub(*r.StartedAt) < time.Second {
		t.Fatal("wait step did not wait")
	}
}

func TestNoDispatchWithoutFence(t *testing.T) {
	v := newEnv(t)
	v.fence.Store(controlplane.ErrNotLeader)
	wf := v.workflow([]model.WorkflowStep{{Name: "s", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "true"}}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	for range 10 {
		v.e.Tick(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
	v.waitIdle()
	if n := v.runner.executes.Load(); n != 0 {
		t.Fatalf("dispatched %d times without a fence", n)
	}
	if len(v.actions(run.ID)) != 0 {
		t.Fatal("an action record was written without a fence")
	}
	v.fence.Store(errors.New(""))
	if r := v.settle(run.ID, model.RunSucceeded); r.Status != model.RunSucceeded {
		t.Fatal(r.Status)
	}
}

func TestInactiveNodeDoesNothing(t *testing.T) {
	v := newEnv(t)
	v.active.Store(false)
	wf := v.workflow([]model.WorkflowStep{{Name: "s", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "true"}}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	for range 5 {
		v.e.Tick(context.Background())
	}
	time.Sleep(100 * time.Millisecond)
	if r, _ := v.st.GetRun(context.Background(), run.ID); r.Status != model.RunPending || v.runner.executes.Load() != 0 {
		t.Fatalf("follower executed a run: %s", r.Status)
	}
}

// A new leader finds a script action left "dispatching" by a previous leader. The runner has a
// record of it, so the outcome is recovered without executing the script again.
func TestReconcileScriptFromRunnerRecord(t *testing.T) {
	v := newEnv(t)
	marker := filepath.Join(t.TempDir(), "count")
	steps := []model.WorkflowStep{{Name: "once", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "echo x >> " + marker}}
	wf := v.workflow(steps, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	// The old leader committed the dispatch record and dispatched, then died before committing the result.
	a := &model.WorkflowAction{ID: uuid.NewString(), RunID: run.ID, StepIndex: 0, StepType: "script", Attempt: 1, Status: model.ActionDispatching, FenceTerm: 3, FenceLeader: "old", StartedAt: time.Now().UTC()}
	run.Status = model.RunRunning
	if err := v.cp.ProgressRun(context.Background(), run, a); err != nil {
		t.Fatal(err)
	}
	v.rs.Execute(context.Background(), &runner.Request{ID: a.ID, Language: "bash", Script: steps[0].Script, TimeoutSeconds: 5})
	r := v.settle(run.ID, model.RunSucceeded, model.RunUnknownOutcome, model.RunFailed)
	got, _ := v.st.GetAction(context.Background(), a.ID)
	if r.Status != model.RunSucceeded || got.Status != model.ActionReconciledSucceeded || got.ReconciledBy != "runner" {
		t.Fatalf("run %s action %s", r.Status, got.Status)
	}
	b, _ := os.ReadFile(marker)
	if strings.Count(string(b), "x") != 1 || v.runner.executes.Load() != 0 {
		t.Fatal("the script was executed again during reconciliation")
	}
}

// An HTTP action left "dispatching" cannot be checked: it becomes UNKNOWN_OUTCOME and is never
// replayed; an operator reconciles it and the run continues.
func TestUnknownOutcomeAndOperatorReconcile(t *testing.T) {
	v := newEnv(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer ts.Close()
	wf := v.workflow([]model.WorkflowStep{
		{Name: "hook", Type: "http", Method: "POST", URL: ts.URL, TimeoutSeconds: 5, AllowPrivateNetwork: true},
		{Name: "after", Type: "script", Language: "bash", TimeoutSeconds: 5, Script: "true"},
	}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	a := &model.WorkflowAction{ID: uuid.NewString(), RunID: run.ID, StepIndex: 0, StepType: "http", Attempt: 1, Status: model.ActionDispatching, FenceTerm: 3, FenceLeader: "old", StartedAt: time.Now().UTC()}
	run.Status = model.RunRunning
	if err := v.cp.ProgressRun(context.Background(), run, a); err != nil {
		t.Fatal(err)
	}
	r := v.settle(run.ID, model.RunUnknownOutcome)
	if hits.Load() != 0 {
		t.Fatal("an action with an uncertain outcome was replayed")
	}
	if !strings.Contains(r.Error, "outcome unknown") {
		t.Fatalf("error %q", r.Error)
	}
	if _, err := v.e.Reconcile(context.Background(), a.ID, "maybe", "admin", "x"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("bad result accepted: %v", err)
	}
	if _, err := v.e.Reconcile(context.Background(), a.ID, "succeeded", "admin", "checked the receiver's log"); err != nil {
		t.Fatal(err)
	}
	r = v.settle(run.ID, model.RunSucceeded)
	got, _ := v.st.GetAction(context.Background(), a.ID)
	if got.Status != model.ActionReconciledSucceeded || got.ReconciledBy != "admin" || hits.Load() != 0 {
		t.Fatalf("action %s by %s, hits %d", got.Status, got.ReconciledBy, hits.Load())
	}
	if _, err := v.e.Reconcile(context.Background(), a.ID, "failed", "admin", "again"); !errors.Is(err, ErrInvalidState) {
		t.Fatal("an action was reconciled twice")
	}
}

func TestHTTPStepAndSSRFGuard(t *testing.T) {
	v := newEnv(t)
	var body atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 100)
		n, _ := r.Body.Read(b)
		body.Store(string(b[:n]) + "|" + r.Header.Get("Authorization"))
		w.WriteHeader(202)
	}))
	defer ts.Close()
	step := model.WorkflowStep{Name: "hook", Type: "http", Method: "POST", URL: ts.URL + "/hook?run={{run_id}}", Body: `{"incident":"{{incident_id}}"}`,
		Headers: map[string]string{"Authorization": "Bearer {{secret.HOOK_TOKEN}}"}, TimeoutSeconds: 5}
	blocked := v.workflow([]model.WorkflowStep{step}, map[string]string{"HOOK_TOKEN": "tok-abcdef"}, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), blocked, "tester", map[string]string{"incident_id": "inc-1"})
	r := v.settle(run.ID, model.RunFailed, model.RunSucceeded)
	if r.Status != model.RunFailed || !strings.Contains(r.Error, "blocked") || body.Load() != nil {
		t.Fatalf("loopback was reachable without allow_private_network: %s %q", r.Status, r.Error)
	}
	step.AllowPrivateNetwork = true
	allowed := v.workflow([]model.WorkflowStep{step}, map[string]string{"HOOK_TOKEN": "tok-abcdef"}, model.WorkflowTrigger{})
	run, _ = v.e.TriggerManual(context.Background(), allowed, "tester", map[string]string{"incident_id": "inc-1"})
	r = v.settle(run.ID, model.RunFailed, model.RunSucceeded)
	if r.Status != model.RunSucceeded || body.Load() != `{"incident":"inc-1"}|Bearer tok-abcdef` {
		t.Fatalf("status %s (%s), body %v", r.Status, r.Error, body.Load())
	}
}

func TestAddressPolicy(t *testing.T) {
	for _, c := range []struct {
		ip           string
		allowPrivate bool
		want         bool
	}{
		{"8.8.8.8", false, true}, {"127.0.0.1", false, false}, {"127.0.0.1", true, true}, {"10.1.2.3", false, false}, {"10.1.2.3", true, true},
		{"169.254.169.254", true, false}, {"::ffff:169.254.169.254", true, false}, {"100.64.1.1", false, false}, {"0.0.0.0", true, false},
		{"::1", false, false}, {"fe80::1", false, false}, {"fc00::1", false, false}, {"2606:4700::1111", false, true}, {"224.0.0.1", true, false},
	} {
		if got := AddressAllowed(netip.MustParseAddr(c.ip), c.allowPrivate); got != c.want {
			t.Errorf("%s private=%v: got %v", c.ip, c.allowPrivate, got)
		}
	}
}

func TestIncidentTriggerDeduplicatedAndCancel(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	mon := &model.Monitor{ID: "m1", Name: "api", Type: model.MonitorHTTP, Target: "https://x.test", IntervalSeconds: 30, TimeoutSeconds: 5, Enabled: true,
		Policy: model.DefaultPolicy(), Tags: []string{"prod"}, Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := v.cp.CreateMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	wf := v.workflow([]model.WorkflowStep{{Name: "w", Type: "wait", Seconds: 3600}}, nil,
		model.WorkflowTrigger{Type: "incident", OnStatuses: []model.IncidentStatus{model.IncidentConfirmed}, Tags: []string{"prod"}})
	other := v.workflow([]model.WorkflowStep{{Name: "w", Type: "wait", Seconds: 3600}}, nil,
		model.WorkflowTrigger{Type: "incident", Tags: []string{"staging"}})
	disabled := v.workflow([]model.WorkflowStep{{Name: "w", Type: "wait", Seconds: 3600}}, nil, model.WorkflowTrigger{Type: "incident"})
	disabled.Enabled = false
	if err := v.cp.UpdateWorkflow(ctx, disabled); err != nil {
		t.Fatal(err)
	}
	in := &model.Incident{ID: "inc-1", MonitorID: "m1", Status: model.IncidentConfirmed, Title: "api down", UpdatedAt: now}
	v.e.OnIncidentTransition(ctx, in, model.IncidentSuspected, model.IncidentConfirmed)
	v.e.OnIncidentTransition(ctx, in, model.IncidentSuspected, model.IncidentConfirmed) // duplicate notification
	v.e.OnIncidentTransition(ctx, in, model.IncidentConfirmed, model.IncidentAcknowledged)
	runs, _ := v.st.ListRuns(ctx, storage.RunFilter{})
	if len(runs) != 1 || runs[0].WorkflowID != wf.ID || runs[0].Context["incident_id"] != "inc-1" || runs[0].Context["monitor_name"] != "api" {
		t.Fatalf("want exactly one run of the matching workflow, got %d (other=%s)", len(runs), other.ID)
	}
	r := v.settle(runs[0].ID, model.RunWaiting)
	if _, err := v.e.Cancel(ctx, r.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.e.Cancel(ctx, r.ID, "admin"); !errors.Is(err, ErrInvalidState) {
		t.Fatal("cancelled twice")
	}
	got, _ := v.st.GetRun(ctx, r.ID)
	if got.Status != model.RunCancelled {
		t.Fatal(got.Status)
	}
}

func TestScheduleFiresOncePerSlot(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	wf := v.workflow([]model.WorkflowStep{{Name: "w", Type: "wait", Seconds: 3600}}, nil, model.WorkflowTrigger{Type: "schedule", IntervalSeconds: 60})
	v.e.Tick(ctx) // arms the schedule
	got, _ := v.st.GetWorkflow(ctx, wf.ID)
	if got.NextFireAt == nil {
		t.Fatal("schedule not armed")
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := v.cp.FireSchedule(ctx, wf.ID, past, nil); err != nil {
		t.Fatal(err)
	}
	v.e.Tick(ctx)
	v.e.Tick(ctx)
	v.waitIdle()
	runs, _ := v.st.ListRuns(ctx, storage.RunFilter{WorkflowID: wf.ID})
	if len(runs) != 1 || runs[0].TriggerType != "schedule" {
		t.Fatalf("want one scheduled run, got %d", len(runs))
	}
	got, _ = v.st.GetWorkflow(ctx, wf.ID)
	if !got.NextFireAt.After(time.Now()) {
		t.Fatal("next fire time not advanced")
	}
}

func TestExpectedStatus(t *testing.T) {
	if !expectedStatus(nil, 204) || expectedStatus(nil, 301) || !expectedStatus([]string{"301", "4xx"}, 404) || expectedStatus([]string{"200"}, 201) {
		t.Fatal("expectedStatus")
	}
}

func TestHTTPConnectFailureIsDefinite(t *testing.T) {
	v := newEnv(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close() // nothing listens here any more
	wf := v.workflow([]model.WorkflowStep{{Name: "hook", Type: "http", Method: "POST", URL: "http://" + addr + "/", TimeoutSeconds: 3, AllowPrivateNetwork: true}}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	r := v.settle(run.ID, model.RunFailed, model.RunUnknownOutcome, model.RunSucceeded)
	if r.Status != model.RunFailed || !strings.Contains(r.Error, "connection refused") {
		t.Fatalf("status %s error %q", r.Status, r.Error)
	}
}

func TestHTTPNoResponseIsUnknown(t *testing.T) {
	v := newEnv(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accept and never answer
		}
	}()
	wf := v.workflow([]model.WorkflowStep{{Name: "hook", Type: "http", Method: "POST", URL: "http://" + l.Addr().String() + "/", TimeoutSeconds: 1, AllowPrivateNetwork: true}}, nil, model.WorkflowTrigger{})
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", nil)
	if r := v.settle(run.ID, model.RunFailed, model.RunUnknownOutcome, model.RunSucceeded); r.Status != model.RunUnknownOutcome {
		t.Fatalf("status %s", r.Status)
	}
}
