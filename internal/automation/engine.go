// Package automation implements the durable workflow engine.
//
// Workflow definitions, runs, durable timers and action records are replicated control-plane
// state. Only the active controller (the standalone node, or the cluster leader while the strict
// mesh gate is open) executes runs. Every external action (script or HTTP step) follows the
// fencing protocol:
//
//  1. Fence: prove leadership (Raft VerifyLeader + mesh gate) and read the term.
//  2. Commit a "dispatching" action record carrying (term, leader) through Raft.
//  3. Fence again; if leadership or the term changed, do not dispatch.
//  4. Dispatch the action with the action ID as its idempotency key.
//  5. Commit the result together with the run's next state.
//
// If the node loses leadership between 2 and 5, the record stays "dispatching". The next leader
// never re-dispatches such an action: it asks the runner for its record of the action ID, and if
// the outcome cannot be established it marks the action and the run UNKNOWN_OUTCOME for an
// operator to reconcile. Execution is therefore at-most-once per action record; exactly-once
// external execution is not claimed.
package automation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/runner"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

// Fencer proves the right to dispatch and returns (term, leader ID).
type Fencer func(ctx context.Context) (uint64, string, error)

// Engine executes workflow runs on the active controller.
type Engine struct {
	Store     *storage.Store
	CP        *controlplane.CP
	Incidents *incidents.Manager
	Fence     Fencer
	Active    func() bool
	Box       *security.SecretBox
	Runner    Runner // nil when no runner is configured
	DNS       *dns.Service
	Log       *slog.Logger
	Interval  time.Duration
	// HTTPClient overrides the SSRF-guarded client (tests only).
	HTTPClient func(allowPrivate bool, timeout time.Duration) *http.Client

	kick        chan struct{}
	mu          sync.Mutex
	inflight    map[string]bool      // run IDs being processed by this node
	nextAttempt map[string]time.Time // action ID -> earliest next reconciliation attempt
	once        sync.Once
}

const (
	maxStoredOutput = 16 << 10
	notDispatched   = "not dispatched: leadership could not be re-verified"
	reconcileAfter  = 10 * time.Minute // give up waiting for an unreachable runner after timeout + this
)

func (e *Engine) init() {
	e.once.Do(func() {
		e.kick = make(chan struct{}, 1)
		e.inflight = map[string]bool{}
		e.nextAttempt = map[string]time.Time{}
		if e.Interval <= 0 {
			e.Interval = time.Second
		}
		if e.Log == nil {
			e.Log = slog.Default()
		}
		if e.HTTPClient == nil {
			e.HTTPClient = guardedClient
		}
		if e.Fence == nil {
			e.Fence = func(context.Context) (uint64, string, error) { return 0, "", nil }
		}
		if e.Active == nil {
			e.Active = func() bool { return true }
		}
	})
}

// Kick schedules an immediate evaluation.
func (e *Engine) Kick() {
	e.init()
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run drives the engine until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.init()
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		}
		e.Tick(ctx)
	}
}

// Tick performs one evaluation pass (exported for tests).
func (e *Engine) Tick(ctx context.Context) {
	e.init()
	if !e.Active() {
		return
	}
	e.fireSchedules(ctx)
	now := time.Now().UTC()
	runs, err := e.Store.ListRuns(ctx, storage.RunFilter{Statuses: []string{model.RunPending, model.RunRunning, model.RunWaiting}, Limit: 500})
	if err != nil {
		e.Log.Error("automation: list runs", "err", err)
		return
	}
	ids := map[string]bool{}
	for _, r := range runs {
		if r.Status == model.RunWaiting && r.NextAt != nil && r.NextAt.After(now) {
			continue
		}
		ids[r.ID] = true
	}
	// Actions left dispatching under runs that are no longer active (e.g. cancelled) still need
	// their outcome recorded.
	if acts, err := e.Store.DispatchingActions(ctx); err == nil {
		for _, a := range acts {
			ids[a.RunID] = true
		}
	}
	for id := range ids {
		e.mu.Lock()
		busy := e.inflight[id]
		if !busy {
			e.inflight[id] = true
		}
		e.mu.Unlock()
		if !busy {
			go e.processRun(ctx, id)
		}
	}
}

func (e *Engine) processRun(ctx context.Context, id string) {
	defer func() {
		e.mu.Lock()
		delete(e.inflight, id)
		e.mu.Unlock()
	}()
	for i := 0; i < 1000 && e.Active() && ctx.Err() == nil; i++ {
		run, err := e.Store.GetRun(ctx, id)
		if err != nil {
			return
		}
		if !e.step(ctx, run) {
			return
		}
	}
}

func terminal(status string) bool {
	return status == model.RunSucceeded || status == model.RunFailed || status == model.RunCancelled
}

func (e *Engine) stepActions(ctx context.Context, run *model.WorkflowRun, idx int) ([]*model.WorkflowAction, error) {
	all, err := e.Store.RunActions(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	var out []*model.WorkflowAction
	for _, a := range all {
		if a.StepIndex == idx {
			out = append(out, a)
		}
	}
	return out, nil
}

// step advances a run by at most one step. It returns true if processing should continue
// immediately.
func (e *Engine) step(ctx context.Context, run *model.WorkflowRun) bool {
	// An action whose result was never recorded must be resolved before anything else happens.
	all, err := e.Store.RunActions(ctx, run.ID)
	if err != nil {
		return false
	}
	for _, a := range all {
		if a.Status == model.ActionDispatching {
			return e.reconcile(ctx, run, a)
		}
	}
	if terminal(run.Status) || run.Status == model.RunUnknownOutcome {
		return false
	}
	now := time.Now().UTC()
	if run.StartedAt == nil {
		run.StartedAt = &now
	}
	waited := false
	if run.NextAt != nil {
		if run.NextAt.After(now) {
			return false
		}
		run.NextAt, waited = nil, true
	}
	if run.CurrentStep >= len(run.Steps) {
		run.Status, run.FinishedAt = model.RunSucceeded, &now
		return e.save(ctx, run, nil) && false
	}
	st := run.Steps[run.CurrentStep]
	switch st.Type {
	case "wait":
		if waited {
			run.CurrentStep++
			run.Status = model.RunRunning
		} else {
			t := now.Add(time.Duration(st.Seconds) * time.Second)
			run.Status, run.NextAt = model.RunWaiting, &t
		}
		run.UpdatedAt = now
		return e.save(ctx, run, nil) && run.Status == model.RunRunning
	case "incident_status":
		return e.incidentStep(ctx, run, st)
	case "script", "http", "dns":
		return e.external(ctx, run, st)
	default:
		return e.finish(ctx, run, st, nil, outcome{status: model.ActionFailed, err: "unknown step type " + st.Type})
	}
}

func (e *Engine) save(ctx context.Context, run *model.WorkflowRun, a *model.WorkflowAction) bool {
	run.UpdatedAt = time.Now().UTC()
	if err := e.CP.ProgressRun(ctx, run, a); err != nil {
		e.Log.Warn("automation: could not record run progress", "run_id", run.ID, "err", err)
		return false
	}
	return true
}

func (e *Engine) incidentStep(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep) bool {
	attempt := 1
	if prev, err := e.stepActions(ctx, run, run.CurrentStep); err == nil {
		attempt = len(prev) + 1
	}
	a := &model.WorkflowAction{ID: uuid.NewString(), RunID: run.ID, StepIndex: run.CurrentStep, StepType: st.Type, Attempt: attempt, StartedAt: time.Now().UTC()}
	id := run.Context["incident_id"]
	if id == "" || e.Incidents == nil {
		return e.finish(ctx, run, st, a, outcome{status: model.ActionFailed, err: "this run has no incident to update"})
	}
	in, err := e.Incidents.SetStatus(ctx, id, st.Status, "workflow:"+run.WorkflowID, expand(st.Note, run, nil))
	if err != nil {
		if errors.Is(err, controlplane.ErrNotLeader) || errors.Is(err, controlplane.ErrNoQuorum) {
			return false
		}
		return e.finish(ctx, run, st, a, outcome{status: model.ActionFailed, err: "could not set incident status: " + safeErr(err)})
	}
	return e.finish(ctx, run, st, a, outcome{status: model.ActionSucceeded, output: "incident " + in.ID + " is now " + string(in.Status)})
}

type outcome struct {
	status   string
	exitCode *int
	output   string
	err      string
	by       string
}

func safeNetErr(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "name resolution failed"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return "connection timed out"
	}
	return "network error"
}

func safeErr(err error) string {
	var re *rejectedError
	switch {
	case errors.As(err, &re):
		return re.Error()
	case errors.As(err, new(incidents.ErrInvalidTransition)):
		return err.Error()
	case errors.Is(err, storage.ErrNotFound):
		return "not found"
	}
	return "internal error"
}

func (e *Engine) external(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep) bool {
	prev, err := e.stepActions(ctx, run, run.CurrentStep)
	if err != nil {
		return false
	}
	term, leader, err := e.Fence(ctx)
	if err != nil {
		return false
	}
	now := time.Now().UTC()
	a := &model.WorkflowAction{ID: uuid.NewString(), RunID: run.ID, StepIndex: run.CurrentStep, StepType: st.Type, Attempt: len(prev) + 1,
		Status: model.ActionDispatching, FenceTerm: term, FenceLeader: leader, StartedAt: now}
	run.Status = model.RunRunning
	if !e.save(ctx, run, a) {
		return false
	}
	e.mu.Lock()
	e.nextAttempt[a.ID] = time.Now().Add(time.Hour) // owned by this goroutine
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.nextAttempt, a.ID)
		e.mu.Unlock()
	}()
	if term2, leader2, err := e.Fence(ctx); err != nil || term2 != term || leader2 != leader {
		// Nothing was dispatched. Try to record that; if leadership is really gone the write fails
		// and the next leader treats the record as an unknown outcome.
		a.Status, a.Error = model.ActionFailed, notDispatched
		fin := time.Now().UTC()
		a.FinishedAt = &fin
		e.save(ctx, run, a)
		return false
	}
	secrets, err := e.secrets(ctx, run.WorkflowID)
	if err != nil {
		return e.finish(ctx, run, st, a, outcome{status: model.ActionFailed, err: "cannot decrypt workflow secrets"})
	}
	var o outcome
	switch st.Type {
	case "script":
		o = e.dispatchScript(ctx, run, st, a, secrets)
	case "dns":
		o = e.dispatchDNS(ctx, run, st, a, secrets)
	default:
		o = e.dispatchHTTP(ctx, run, st, secrets)
	}
	if ctx.Err() != nil {
		// This node is shutting down: leave the record "dispatching" so the next leader
		// establishes the outcome (from the runner's record for scripts) instead of guessing here.
		e.Log.Warn("automation: shutting down with an action in flight", "run_id", run.ID, "action_id", a.ID)
		return false
	}
	o.output = redact(o.output, secrets)
	o.err = redact(o.err, secrets)
	return e.finish(ctx, run, st, a, o)
}

func (e *Engine) secrets(ctx context.Context, workflowID string) (map[string]string, error) {
	wf, err := e.Store.GetWorkflow(ctx, workflowID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	return OpenSecrets(e.Box, wf)
}

func runnerOutcome(res *runner.Result) outcome {
	o := outcome{exitCode: res.ExitCode, output: res.Output, err: res.Error}
	if res.Truncated {
		o.output += "\n[output truncated by the runner]"
	}
	switch res.Status {
	case "succeeded":
		o.status = model.ActionSucceeded
	case "failed", "timeout":
		o.status = model.ActionFailed
		if o.err == "" && res.ExitCode != nil {
			o.err = fmt.Sprintf("exit code %d", *res.ExitCode)
		}
	default: // interrupted or anything unexpected
		o.status = model.ActionUnknownOutcome
		if o.err == "" {
			o.err = "the runner could not establish the outcome"
		}
	}
	return o
}

func (e *Engine) dispatchScript(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep, a *model.WorkflowAction, secrets map[string]string) outcome {
	if e.Runner == nil {
		return outcome{status: model.ActionFailed, err: "no Action Runner is configured on this node (INTRABIN_RUNNER_URL)"}
	}
	env := map[string]string{}
	for k, v := range run.Context {
		env["INTRABIN_"+strings.ToUpper(k)] = v
	}
	env["INTRABIN_RUN_ID"], env["INTRABIN_ACTION_ID"], env["INTRABIN_WORKFLOW_ID"] = run.ID, a.ID, run.WorkflowID
	for k, v := range st.Env {
		env[k] = expand(v, run, secrets)
	}
	for k, v := range secrets {
		env[k] = v
	}
	req := &runner.Request{ID: a.ID, Language: st.Language, Script: st.Script, Env: env, TimeoutSeconds: st.TimeoutSeconds}
	res, err := e.Runner.Execute(ctx, req)
	if err == nil {
		return runnerOutcome(res)
	}
	var re *rejectedError
	if errors.As(err, &re) {
		return outcome{status: model.ActionFailed, err: re.Error()}
	}
	// Transport error: the request may or may not have reached the runner. Ask the runner for its
	// record (the action ID is the idempotency key) before concluding anything.
	for i := 0; i < 3 && ctx.Err() == nil; i++ {
		time.Sleep(time.Duration(i+1) * time.Second)
		if r, found, gerr := e.Runner.Get(ctx, a.ID); gerr == nil && found && r.Status != "running" {
			return runnerOutcome(r)
		}
	}
	return outcome{status: model.ActionUnknownOutcome, err: "lost contact with the Action Runner during execution; the script may or may not have run"}
}

func expectedStatus(codes []string, got int) bool {
	if len(codes) == 0 {
		codes = []string{"2xx"}
	}
	for _, c := range codes {
		c = strings.ToLower(strings.TrimSpace(c))
		if len(c) == 3 && strings.HasSuffix(c, "xx") && c[0] >= '1' && c[0] <= '5' {
			if got/100 == int(c[0]-'0') {
				return true
			}
		} else if n, err := strconv.Atoi(c); err == nil && n == got {
			return true
		}
	}
	return false
}

func (e *Engine) dispatchHTTP(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep, secrets map[string]string) outcome {
	timeout := time.Duration(st.TimeoutSeconds) * time.Second
	method := st.Method
	if method == "" {
		method = http.MethodPost
	}
	var body io.Reader
	if st.Body != "" {
		body = strings.NewReader(expand(st.Body, run, secrets))
	}
	req, err := http.NewRequestWithContext(ctx, method, expand(st.URL, run, secrets), body)
	if err != nil {
		return outcome{status: model.ActionFailed, err: "invalid request"}
	}
	req.Header.Set("User-Agent", "Intrabin-Workflow/1")
	for k, v := range st.Headers {
		req.Header.Set(k, expand(v, run, secrets))
	}
	resp, err := e.HTTPClient(st.AllowPrivateNetwork, timeout).Do(req)
	if err != nil {
		if errors.Is(err, ErrBlockedAddress) {
			return outcome{status: model.ActionFailed, err: "blocked: the destination resolves to a private or reserved address (enable allow_private_network to permit private ranges)"}
		}
		var dnsErr *net.DNSError
		var opErr *net.OpError
		if errors.As(err, &dnsErr) || (errors.As(err, &opErr) && opErr.Op == "dial") {
			// No connection was established, so the request was never sent.
			return outcome{status: model.ActionFailed, err: "could not connect: " + safeNetErr(err)}
		}
		// The connection may have failed before or after the server acted on the request.
		return outcome{status: model.ActionUnknownOutcome, err: "the HTTP request failed before a response was received; the receiving system may or may not have acted on it"}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxStoredOutput))
	code := resp.StatusCode
	o := outcome{exitCode: &code, output: fmt.Sprintf("HTTP %d\n%s", code, b)}
	if expectedStatus(st.ExpectedStatus, code) {
		o.status = model.ActionSucceeded
	} else {
		o.status, o.err = model.ActionFailed, fmt.Sprintf("unexpected HTTP status %d", code)
	}
	return o
}

// finish records an action's outcome and advances the run in one replicated write. It returns
// true if the run can continue immediately.
func (e *Engine) finish(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep, a *model.WorkflowAction, o outcome) bool {
	e.advance(ctx, run, st, a, o)
	return e.save(ctx, run, a) && run.Status == model.RunRunning
}

// advance applies an action outcome to the action record and the run (in memory).
func (e *Engine) advance(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep, a *model.WorkflowAction, o outcome) {
	now := time.Now().UTC()
	if a != nil {
		a.Status, a.ExitCode, a.Error, a.FinishedAt = o.status, o.exitCode, o.err, &now
		a.Output = truncate(o.output, maxStoredOutput)
		if o.by != "" {
			a.ReconciledBy = o.by
		}
	}
	if terminal(run.Status) {
		return
	}
	label := fmt.Sprintf("Step %d (%s)", run.CurrentStep+1, st.Name)
	switch o.status {
	case model.ActionSucceeded, model.ActionReconciledSucceeded:
		run.CurrentStep++
		run.Status = model.RunRunning
	case model.ActionUnknownOutcome:
		run.Status = model.RunUnknownOutcome
		run.Error = label + ": outcome unknown. " + o.err + " Reconcile the action to continue."
	default:
		failures := 1
		if prev, err := e.stepActions(ctx, run, run.CurrentStep); err == nil {
			for _, p := range prev {
				if (p.Status == model.ActionFailed && p.Error != notDispatched) && (a == nil || p.ID != a.ID) {
					failures++
				}
			}
		}
		switch {
		case o.status == model.ActionFailed && failures <= st.Retries:
			t := now.Add(time.Duration(min(failures*5, 60)) * time.Second)
			run.Status, run.NextAt = model.RunWaiting, &t
		case st.OnFailure == "continue":
			run.CurrentStep++
			run.Status = model.RunRunning
			run.Error = strings.TrimSpace(run.Error + " " + label + " failed; continuing.")
		default:
			run.Status, run.FinishedAt = model.RunFailed, &now
			run.Error = label + " failed: " + o.err
		}
	}
	if run.Status == model.RunRunning && run.CurrentStep >= len(run.Steps) {
		run.Status, run.FinishedAt = model.RunSucceeded, &now
	}
}

// reconcile resolves an action left "dispatching" (its result was never committed).
func (e *Engine) reconcile(ctx context.Context, run *model.WorkflowRun, a *model.WorkflowAction) bool {
	e.mu.Lock()
	if t, ok := e.nextAttempt[a.ID]; ok && time.Now().Before(t) {
		e.mu.Unlock()
		return false
	}
	e.nextAttempt[a.ID] = time.Now().Add(5 * time.Second)
	e.mu.Unlock()
	if _, _, err := e.Fence(ctx); err != nil {
		return false
	}
	st := model.WorkflowStep{Name: a.StepType}
	if a.StepIndex < len(run.Steps) {
		st = run.Steps[a.StepIndex]
	}
	unknown := func(msg string) bool {
		e.Log.Warn("automation: action outcome unknown", "run_id", run.ID, "action_id", a.ID, "reason", msg)
		return e.finish(ctx, run, st, a, outcome{status: model.ActionUnknownOutcome, err: msg, by: "engine"})
	}
	if a.StepType == "dns" {
		return e.reconcileDNS(ctx, run, st, a, unknown)
	}
	if a.StepType != "script" {
		return unknown("leadership changed before the result of this " + a.StepType + " action was recorded; it may or may not have been executed.")
	}
	if e.Runner == nil {
		return unknown("no Action Runner is configured on the current leader, so the outcome cannot be checked.")
	}
	res, found, err := e.Runner.Get(ctx, a.ID)
	switch {
	case err != nil:
		if time.Since(a.StartedAt) > time.Duration(st.TimeoutSeconds)*time.Second+reconcileAfter {
			return unknown("the Action Runner could not be reached to establish the outcome.")
		}
		return false
	case !found:
		return unknown("the Action Runner has no record of this action; the dispatch may still be in flight or may never have arrived.")
	case res.Status == "running":
		return false
	}
	o := runnerOutcome(res)
	o.by = "runner"
	if o.status == model.ActionSucceeded {
		o.status = model.ActionReconciledSucceeded
	} else if o.status == model.ActionFailed {
		o.status = model.ActionReconciledFailed
	}
	if sec, err := e.secrets(ctx, run.WorkflowID); err == nil {
		o.output, o.err = redact(o.output, sec), redact(o.err, sec)
	}
	return e.finish(ctx, run, st, a, o)
}

// ---- triggers and operator actions ----

// ErrInvalidState is returned for operations not permitted in the current state.
var ErrInvalidState = errors.New("invalid state")

func newRun(wf *model.Workflow, triggerType, ref, actor string, cx map[string]string) *model.WorkflowRun {
	now := time.Now().UTC()
	if cx == nil {
		cx = map[string]string{}
	}
	cx["workflow_name"] = wf.Name
	cx["trigger"] = triggerType
	return &model.WorkflowRun{ID: uuid.NewString(), WorkflowID: wf.ID, WorkflowVersion: wf.Version, Steps: wf.Steps, TriggerType: triggerType,
		TriggerRef: ref, Context: cx, Status: model.RunPending, CreatedBy: actor, CreatedAt: now, UpdatedAt: now}
}

// TriggerManual starts a run of wf now. Manual runs are allowed for disabled workflows (a
// disabled workflow only stops automatic triggers).
func (e *Engine) TriggerManual(ctx context.Context, wf *model.Workflow, actor string, cx map[string]string) (*model.WorkflowRun, error) {
	r := newRun(wf, "manual", "", actor, cx)
	if err := e.CP.CreateRun(ctx, r); err != nil {
		return nil, err
	}
	e.Kick()
	return r, nil
}

// OnIncidentTransition starts runs of enabled incident-triggered workflows that match.
func (e *Engine) OnIncidentTransition(ctx context.Context, in *model.Incident, from, to model.IncidentStatus) {
	e.init()
	if !e.Active() {
		return
	}
	wfs, err := e.Store.ListWorkflows(ctx)
	if err != nil {
		return
	}
	var mon *model.Monitor
	started := false
	for _, wf := range wfs {
		if !wf.Enabled || wf.Trigger.Type != "incident" {
			continue
		}
		statuses := wf.Trigger.OnStatuses
		if len(statuses) == 0 {
			statuses = []model.IncidentStatus{model.IncidentConfirmed}
		}
		if !slices.Contains(statuses, to) {
			continue
		}
		if mon == nil {
			if mon, err = e.Store.GetMonitor(ctx, in.MonitorID); err != nil {
				mon = &model.Monitor{ID: in.MonitorID}
			}
		}
		if !matchesMonitor(wf.Trigger, mon) {
			continue
		}
		ref := fmt.Sprintf("incident:%s:%s:%d", in.ID, to, in.UpdatedAt.UnixMilli())
		cx := map[string]string{"incident_id": in.ID, "incident_status": string(to), "previous_status": string(from), "incident_title": in.Title,
			"monitor_id": in.MonitorID, "monitor_name": mon.Name, "monitor_target": mon.Target}
		r := newRun(wf, "incident", ref, "system", cx)
		if err := e.CP.CreateRun(ctx, r); err != nil && !errors.Is(err, storage.ErrConflict) {
			e.Log.Error("automation: could not start incident workflow", "workflow_id", wf.ID, "incident_id", in.ID, "err", err)
			continue
		}
		started = true
	}
	if started {
		e.Kick()
	}
}

func matchesMonitor(t model.WorkflowTrigger, m *model.Monitor) bool {
	if len(t.MonitorIDs) == 0 && len(t.GroupIDs) == 0 && len(t.Tags) == 0 {
		return true
	}
	if slices.Contains(t.MonitorIDs, m.ID) || (m.GroupID != "" && slices.Contains(t.GroupIDs, m.GroupID)) {
		return true
	}
	for _, tag := range m.Tags {
		if slices.Contains(t.Tags, tag) {
			return true
		}
	}
	return false
}

func (e *Engine) fireSchedules(ctx context.Context) {
	wfs, err := e.Store.ListWorkflows(ctx)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, wf := range wfs {
		if !wf.Enabled || wf.Trigger.Type != "schedule" || wf.Trigger.IntervalSeconds <= 0 {
			continue
		}
		every := time.Duration(wf.Trigger.IntervalSeconds) * time.Second
		if wf.NextFireAt == nil {
			if err := e.CP.FireSchedule(ctx, wf.ID, now.Add(every), nil); err != nil {
				return
			}
			continue
		}
		if wf.NextFireAt.After(now) {
			continue
		}
		next := wf.NextFireAt.Add(every)
		if !next.After(now) { // missed fires are skipped, not replayed
			next = now.Add(every)
		}
		r := newRun(wf, "schedule", fmt.Sprintf("schedule:%d", wf.NextFireAt.UnixMilli()), "system", nil)
		if err := e.CP.FireSchedule(ctx, wf.ID, next, r); err != nil {
			e.Log.Warn("automation: could not fire schedule", "workflow_id", wf.ID, "err", err)
			return
		}
	}
}

// Cancel stops a run. An action already executing is not interrupted; its result is still
// recorded but the run stays cancelled.
func (e *Engine) Cancel(ctx context.Context, runID, actor string) (*model.WorkflowRun, error) {
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if terminal(run.Status) {
		return nil, fmt.Errorf("%w: the run has already finished", ErrInvalidState)
	}
	now := time.Now().UTC()
	run.Status, run.FinishedAt, run.NextAt = model.RunCancelled, &now, nil
	run.Error = strings.TrimSpace(run.Error + " Cancelled by " + actor + ".")
	run.UpdatedAt = now
	if err := e.CP.ProgressRun(ctx, run, nil); err != nil {
		return nil, err
	}
	return run, nil
}

// Reconcile records an operator's decision about an action whose outcome is unknown and lets the
// run continue: "succeeded" moves to the next step, "failed" applies the step's failure policy
// (without automatic retry).
func (e *Engine) Reconcile(ctx context.Context, actionID, result, actor, note string) (*model.WorkflowRun, error) {
	a, err := e.Store.GetAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if a.Status != model.ActionUnknownOutcome {
		return nil, fmt.Errorf("%w: only actions with an unknown outcome can be reconciled", ErrInvalidState)
	}
	run, err := e.Store.GetRun(ctx, a.RunID)
	if err != nil {
		return nil, err
	}
	o := outcome{by: actor, output: a.Output, exitCode: a.ExitCode, err: strings.TrimSpace(a.Error + " Operator note: " + note)}
	switch result {
	case "succeeded":
		o.status = model.ActionReconciledSucceeded
	case "failed":
		o.status = model.ActionReconciledFailed
	default:
		return nil, fmt.Errorf("%w: result must be succeeded or failed", ErrInvalidState)
	}
	st := model.WorkflowStep{Name: a.StepType}
	if a.StepIndex < len(run.Steps) {
		st = run.Steps[a.StepIndex]
	}
	if run.Status == model.RunUnknownOutcome {
		run.Status = model.RunRunning
		run.Error = ""
		st.Retries = 0 // an operator-declared failure is never retried automatically
	}
	if run.CurrentStep != a.StepIndex && !terminal(run.Status) {
		return nil, fmt.Errorf("%w: the run is no longer at this action's step", ErrInvalidState)
	}
	e.advance(ctx, run, st, a, o)
	run.UpdatedAt = time.Now().UTC()
	if err := e.CP.ProgressRun(ctx, run, a); err != nil {
		return nil, err
	}
	e.Kick()
	return e.Store.GetRun(ctx, run.ID)
}

// RunnerHealth reports the configured runner's health.
func (e *Engine) RunnerHealth(ctx context.Context) (bool, map[string]any, string) {
	if e.Runner == nil {
		return false, nil, "not configured"
	}
	h, err := e.Runner.Health(ctx)
	if err != nil {
		return false, nil, "unreachable"
	}
	return true, h, ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}

// IsActive reports whether this node currently executes workflow runs.
func (e *Engine) IsActive() bool { e.init(); return e.Active() }

// ---- DNS steps ----

// DNSRequest builds the change request for a DNS step (templates expanded).
func DNSRequest(run *model.WorkflowRun, st model.WorkflowStep, secrets map[string]string) dns.ChangeRequest {
	zone := st.ZoneID
	if zone == "" {
		zone = st.ZoneName
	}
	return dns.ChangeRequest{ProviderID: st.ProviderID, Zone: zone, Action: st.DNSAction, Source: "workflow:" + run.WorkflowID,
		Record: dns.RecordInput{Type: st.RecordType, Name: expand(st.RecordName, run, secrets), Content: expand(st.Content, run, secrets), TTL: st.TTL, Proxied: st.Proxied}}
}

func (e *Engine) dispatchDNS(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep, a *model.WorkflowAction, secrets map[string]string) outcome {
	if e.DNS == nil {
		return outcome{status: model.ActionFailed, err: "DNS integrations are not available on this node"}
	}
	req := DNSRequest(run, st, secrets)
	req.ExpectID = a.ID // the change record shares the action ID, so the next leader can find it
	ch, err := e.DNS.Apply(ctx, req, nil, "workflow:"+run.WorkflowID)
	switch {
	case errors.Is(err, dns.ErrNoChange):
		return outcome{status: model.ActionSucceeded, output: "The record already had the requested value; nothing was changed."}
	case err != nil && ch == nil:
		return outcome{status: model.ActionFailed, err: dns.SafeError(err)}
	}
	return dnsOutcome(ch, false)
}

func describeChange(ch *model.DNSChange) string {
	s := fmt.Sprintf("%s %s %s in %s", ch.Action, ch.RecordType, ch.RecordName, ch.ZoneName)
	if ch.Before != nil && ch.After != nil {
		s += fmt.Sprintf(": %s → %s", ch.Before.Content, ch.After.Content)
	} else if ch.After != nil {
		s += ": " + ch.After.Content
	}
	return s + " (change " + ch.ID + ", snapshot " + ch.SnapshotID + ")"
}

func dnsOutcome(ch *model.DNSChange, byLeader bool) outcome {
	o := outcome{output: describeChange(ch), err: ch.Error}
	switch ch.Status {
	case model.DNSChangeApplied, model.DNSChangeReconciledApplied:
		o.status = model.ActionSucceeded
		if byLeader {
			o.status, o.by = model.ActionReconciledSucceeded, "provider"
		}
		if ch.Verified {
			o.output += "\nVerified by reading the record back."
		}
	case model.DNSChangeFailed, model.DNSChangeReconciledNotApplied:
		o.status = model.ActionFailed // definitely not applied, so the step's retries apply
		if byLeader {
			o.by = "provider"
		}
		if o.err == "" {
			o.err = "the change was not applied"
		}
	case model.DNSChangeDispatching:
		o.status = model.ActionUnknownOutcome
		o.err = "the provider state could not be read to establish the outcome"
	default:
		o.status = model.ActionUnknownOutcome
		if o.err == "" {
			o.err = "the DNS provider did not confirm the change and reading the zone back was inconclusive"
		}
	}
	return o
}

func (e *Engine) reconcileDNS(ctx context.Context, run *model.WorkflowRun, st model.WorkflowStep, a *model.WorkflowAction, unknown func(string) bool) bool {
	if e.DNS == nil {
		return unknown("DNS integrations are not available on the current leader, so the outcome cannot be checked.")
	}
	ch, found, err := e.DNS.ResolveChange(ctx, a.ID)
	switch {
	case err != nil:
		if time.Since(a.StartedAt) > reconcileAfter {
			return unknown("the DNS provider could not be reached to establish the outcome.")
		}
		return false
	case !found:
		// No change record was committed, so the provider was never called: safe to retry.
		return e.finish(ctx, run, st, a, outcome{status: model.ActionFailed, err: notDispatched, by: "engine"})
	case ch.Status == model.DNSChangeDispatching:
		if time.Since(a.StartedAt) > reconcileAfter {
			return unknown("the DNS provider could not be read to establish the outcome.")
		}
		return false
	}
	return e.finish(ctx, run, st, a, dnsOutcome(ch, true))
}
