// Package dr implements disaster-recovery plans: switching a service's DNS record between a
// primary and a secondary, manually or (opt-in) automatically on a confirmed incident.
//
// A plan is replicated control-plane state with its own state machine:
//
//	primary ──failover──▶ failing_over ──run succeeded──▶ failed_over
//	failed_over ──failback (manual only)──▶ failing_back ──run succeeded──▶ primary
//	failing_* ──run failed/cancelled──▶ attention ──operator decision──▶ primary | failed_over
//
// Starting a failover or failback is one atomic, Raft-committed compare-and-set that also creates
// the workflow run doing the work, so two nodes (or two clicks) can never start two switches. The
// run itself goes through the workflow engine: every external step is fenced and an uncertain DNS
// write is settled by reading the zone back, never by sending it again. Automatic failover only
// happens on the active controller (Raft leader with the mesh gate open), only for plans that opted
// in, only for an incident the detection policy confirmed, never during the cooldown after a
// previous failover, and never when the secondary's monitor is not UP. Failback is always manual.
package dr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/automation"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// Directions.
const (
	Failover = "failover"
	Failback = "failback"
)

// DefaultCooldown is the minimum time between two failovers of a plan.
const DefaultCooldown = 30 * time.Minute

var (
	// ErrBlocked means a safety check failed; the checks explain why.
	ErrBlocked = errors.New("a safety check failed")
	// ErrState means the plan is not in a state that allows the operation.
	ErrState = errors.New("the plan is not in a state that allows this")
)

// Manager evaluates and executes DR plans on the active controller.
type Manager struct {
	Store  *storage.Store
	CP     *controlplane.CP
	Engine *automation.Engine
	DNS    *dns.Service
	Fence  automation.Fencer
	Active func() bool
	Log    *slog.Logger
	// Interval between evaluation passes (default 5 s).
	Interval time.Duration
	// now is replaceable in tests.
	now func() time.Time

	mu      sync.Mutex
	waiting map[string]string // plan ID -> why automatic failover is not happening now
	once    sync.Once
	kick    chan struct{}
}

func (m *Manager) init() {
	m.once.Do(func() {
		m.waiting = map[string]string{}
		m.kick = make(chan struct{}, 1)
		if m.Interval <= 0 {
			m.Interval = 5 * time.Second
		}
		if m.Log == nil {
			m.Log = slog.Default()
		}
		if m.now == nil {
			m.now = func() time.Time { return time.Now().UTC() }
		}
		if m.Active == nil {
			m.Active = func() bool { return true }
		}
		if m.Fence == nil {
			m.Fence = func(context.Context) (uint64, string, error) { return 0, "", nil }
		}
	})
}

// Check is one safety check shown before (and recorded with) a switch.
type Check struct {
	ID string `json:"id"` // controller | state | primary | secondary | cooldown | dns
	OK bool   `json:"ok"`
	// Blocking checks stop the switch unless Overridable and the operator forces it with a note.
	Blocking    bool   `json:"blocking"`
	Overridable bool   `json:"overridable"`
	Detail      string `json:"detail"`
}

func blocked(cs []Check, force bool) bool {
	for _, c := range cs {
		if !c.OK && c.Blocking && (!force || !c.Overridable) {
			return true
		}
	}
	return false
}

func (m *Manager) monitorStatus(ctx context.Context, id string) (string, model.MonitorStatus) {
	mon, err := m.Store.GetMonitor(ctx, id)
	if err != nil {
		return id, model.StatusUnknown
	}
	st, err := m.Store.GetMonitorState(ctx, id)
	if err != nil || st == nil {
		return mon.Name, model.StatusUnknown
	}
	return mon.Name, st.Status
}

// confirmedIncident returns an open incident on one of the plan's monitors that the detection
// policy has confirmed and that no one has taken over manually.
func (m *Manager) confirmedIncident(ctx context.Context, p *model.DRPlan) *model.Incident {
	for _, id := range p.Config.MonitorIDs {
		in, err := m.Store.OpenIncidentForMonitor(ctx, id)
		if err == nil && in != nil && (in.Status == model.IncidentConfirmed || in.Status == model.IncidentAcknowledged) {
			return in
		}
	}
	return nil
}

// Checks evaluates the safety checks for a switch in the given direction.
func (m *Manager) Checks(ctx context.Context, p *model.DRPlan, direction string, auto bool) []Check {
	m.init()
	var cs []Check
	ctrl := Check{ID: "controller", Blocking: true, OK: m.Active()}
	if ctrl.OK {
		if _, _, err := m.Fence(ctx); err != nil {
			ctrl.OK = false
		}
	}
	if ctrl.OK {
		ctrl.Detail = "This node is the active controller and a majority of voters confirms it."
	} else {
		ctrl.Detail = "This node is not the active controller (not the leader, or the cluster has no quorum)."
	}
	cs = append(cs, ctrl)

	want := model.DRPrimary
	if direction == Failback {
		want = model.DRFailedOver
	}
	stc := Check{ID: "state", Blocking: true, OK: p.State == want, Overridable: p.State == model.DRAttention && !auto}
	if stc.OK {
		stc.Detail = "The plan is " + p.State + "."
	} else {
		stc.Detail = fmt.Sprintf("The plan is %s; a %s starts from %s.", p.State, direction, want)
	}
	cs = append(cs, stc)

	var down, notUp []string
	for _, id := range p.Config.MonitorIDs {
		name, st := m.monitorStatus(ctx, id)
		if st == model.StatusDown {
			down = append(down, name)
		}
		if st != model.StatusUp && st != model.StatusDegraded {
			notUp = append(notUp, fmt.Sprintf("%s (%s)", name, st))
		}
	}
	if direction == Failover {
		in := m.confirmedIncident(ctx, p)
		pc := Check{ID: "primary", OK: in != nil, Blocking: auto}
		switch {
		case in != nil:
			pc.Detail = "Confirmed incident: " + in.Title
		case len(down) > 0:
			pc.Detail = "Down without a confirmed incident: " + strings.Join(down, ", ")
		default:
			pc.Detail = "No confirmed incident on the primary's monitors (planned failover)."
		}
		cs = append(cs, pc)
		if p.Config.SecondaryMonitorID != "" {
			name, st := m.monitorStatus(ctx, p.Config.SecondaryMonitorID)
			sc := Check{ID: "secondary", Blocking: true, Overridable: !auto, OK: st == model.StatusUp || st == model.StatusDegraded}
			sc.Detail = fmt.Sprintf("%s is %s.", name, st)
			cs = append(cs, sc)
		} else if auto {
			// Automatic failover never moves traffic to a secondary whose health is unknown.
			cs = append(cs, Check{ID: "secondary", Blocking: true, Detail: "No monitor is configured for the secondary."})
		}
		cd := p.Config.CooldownSeconds
		if cd <= 0 {
			cd = int(DefaultCooldown / time.Second)
		}
		cc := Check{ID: "cooldown", Blocking: true, Overridable: !auto, OK: true, Detail: "No recent failover."}
		if p.LastFailoverAt != nil {
			if until := p.LastFailoverAt.Add(time.Duration(cd) * time.Second); m.now().Before(until) {
				cc.OK = false
				cc.Detail = "The last failover was less than the cooldown ago; next allowed at " + until.Format(time.RFC3339) + "."
			} else {
				cc.Detail = "Last failover at " + p.LastFailoverAt.Format(time.RFC3339) + "."
			}
		}
		cs = append(cs, cc)
	} else {
		pc := Check{ID: "primary", Blocking: true, Overridable: true, OK: len(notUp) == 0 && len(p.Config.MonitorIDs) > 0}
		if pc.OK {
			pc.Detail = "All primary monitors are UP."
		} else {
			pc.Detail = "Not UP: " + strings.Join(notUp, ", ")
		}
		cs = append(cs, pc)
	}
	return cs
}

// DNSPlan previews the record change of a switch.
func (m *Manager) DNSPlan(ctx context.Context, p *model.DRPlan, direction string) (*dns.Plan, Check) {
	c := Check{ID: "dns", Blocking: true}
	if m.DNS == nil {
		c.Detail = "DNS integrations are not available on this node."
		return nil, c
	}
	plan, err := m.DNS.PlanChange(ctx, changeRequest(p, direction))
	if err != nil {
		c.Detail = dns.SafeError(err)
		if errors.Is(err, dns.ErrAmbiguous) || errors.Is(err, dns.ErrInvalid) || errors.Is(err, dns.ErrNotFound) {
			c.Detail = err.Error()
		}
		return nil, c
	}
	c.OK = true
	switch {
	case plan.NoChange:
		c.Detail = "The record already has the target value."
	case plan.Before != nil:
		c.Detail = fmt.Sprintf("%s %s: %s → %s", plan.After.Type, plan.After.Name, plan.Before.Content, plan.After.Content)
	default:
		c.Detail = fmt.Sprintf("%s %s will be created with %s", plan.After.Type, plan.After.Name, plan.After.Content)
	}
	return plan, c
}

func target(p *model.DRPlan, direction string) string {
	if direction == Failback {
		return p.Config.PrimaryValue
	}
	return p.Config.SecondaryValue
}

func changeRequest(p *model.DRPlan, direction string) dns.ChangeRequest {
	return dns.ChangeRequest{ProviderID: p.Config.ProviderID, Zone: p.Config.ZoneName, Action: "upsert",
		Record: dns.RecordInput{Type: p.Config.RecordType, Name: p.Config.RecordName, Content: target(p, direction), TTL: p.Config.TTL, Proxied: p.Config.Proxied}}
}

// Steps builds the run definition of a switch.
func Steps(p *model.DRPlan, direction, incidentID string) []model.WorkflowStep {
	c := p.Config
	dnsStep := model.WorkflowStep{Type: "dns", OnFailure: "stop", Retries: 1, ProviderID: c.ProviderID, ZoneName: c.ZoneName, DNSAction: "upsert",
		RecordType: c.RecordType, RecordName: c.RecordName, Content: target(p, direction), TTL: c.TTL, Proxied: c.Proxied}
	var out []model.WorkflowStep
	if direction == Failback {
		dnsStep.Name = "Point " + c.RecordName + " back to the primary"
		out = append(out, dnsStep)
		if incidentID != "" {
			out = append(out, model.WorkflowStep{Name: "Resolve the incident", Type: "incident_status", Status: model.IncidentResolved, OnFailure: "continue",
				Note: "Failed back to the primary by DR plan " + p.Name})
		}
		return out
	}
	dnsStep.Name = "Point " + c.RecordName + " to the secondary"
	if incidentID != "" {
		out = append(out, model.WorkflowStep{Name: "Mark the incident as recovering", Type: "incident_status", Status: model.IncidentRecovering, OnFailure: "continue",
			Note: "Failover started by DR plan " + p.Name})
	}
	out = append(out, c.PreSteps...)
	out = append(out, dnsStep)
	out = append(out, c.PostSteps...)
	if incidentID != "" {
		out = append(out, model.WorkflowStep{Name: "Mark the incident as failed over", Type: "incident_status", Status: model.IncidentFailedOver, OnFailure: "continue",
			Note: "Traffic moved to the secondary by DR plan " + p.Name})
	}
	return out
}

// StartOptions describe who starts a switch and why.
type StartOptions struct {
	Actor string
	Note  string
	// Force overrides overridable checks (operator only; requires a note).
	Force bool
	Auto  bool
}

// Result is returned by Start and Preview.
type Result struct {
	Plan   *model.DRPlan      `json:"plan"`
	Checks []Check            `json:"checks"`
	DNS    *dns.Plan          `json:"dns_plan,omitempty"`
	Steps  []string           `json:"steps"`
	Run    *model.WorkflowRun `json:"run,omitempty"`
}

func (m *Manager) incidentFor(ctx context.Context, p *model.DRPlan, direction string) *model.Incident {
	if direction == Failover {
		return m.confirmedIncident(ctx, p)
	}
	for _, id := range p.Config.MonitorIDs {
		if in, err := m.Store.OpenIncidentForMonitor(ctx, id); err == nil && in != nil && in.Status == model.IncidentFailedOver {
			return in
		}
	}
	return nil
}

// Preview runs every check and the DNS dry run without changing anything.
func (m *Manager) Preview(ctx context.Context, id, direction string, auto bool) (*Result, error) {
	m.init()
	p, err := m.Store.GetDRPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: p, Checks: m.Checks(ctx, p, direction, auto)}
	plan, dc := m.DNSPlan(ctx, p, direction)
	res.DNS = plan
	res.Checks = append(res.Checks, dc)
	inc := ""
	if in := m.incidentFor(ctx, p, direction); in != nil {
		inc = in.ID
	}
	for _, s := range Steps(p, direction, inc) {
		res.Steps = append(res.Steps, s.Name)
	}
	return res, nil
}

// Start performs a failover or failback. The returned Result carries the checks even on ErrBlocked.
func (m *Manager) Start(ctx context.Context, id, direction string, o StartOptions) (*Result, error) {
	m.init()
	if direction != Failover && direction != Failback {
		return nil, ErrState
	}
	if direction == Failback && o.Auto {
		return nil, ErrState // failback is never automatic
	}
	if o.Force && (o.Auto || strings.TrimSpace(o.Note) == "") {
		return nil, ErrBlocked
	}
	res, err := m.Preview(ctx, id, direction, o.Auto)
	if err != nil {
		return nil, err
	}
	if blocked(res.Checks, o.Force) {
		return res, ErrBlocked
	}
	p := res.Plan
	inc := m.incidentFor(ctx, p, direction)
	w, err := m.Store.GetWorkflow(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	now := m.now()
	cx := map[string]string{"workflow_name": p.Name, "dr_plan_id": p.ID, "dr_direction": direction,
		"primary_value": p.Config.PrimaryValue, "secondary_value": p.Config.SecondaryValue}
	trigger, ref := "manual", ""
	if o.Auto {
		trigger = "incident"
	}
	if inc != nil {
		cx["incident_id"], cx["incident_title"], cx["monitor_id"] = inc.ID, inc.Title, inc.MonitorID
		if o.Auto {
			ref = "dr:" + direction + ":" + inc.ID // at most one automatic failover per incident
		}
	}
	run := &model.WorkflowRun{ID: uuid.NewString(), WorkflowID: p.ID, WorkflowVersion: w.Version, Steps: Steps(p, direction, cx["incident_id"]),
		TriggerType: trigger, TriggerRef: ref, Context: cx, Status: model.RunPending, CreatedBy: o.Actor, CreatedAt: now, UpdatedAt: now}
	from := []string{model.DRPrimary}
	next := model.DRFailingOver
	if direction == Failback {
		from, next = []string{model.DRFailedOver}, model.DRFailingBack
	}
	if o.Force {
		from = append(from, model.DRAttention)
	}
	note := o.Note
	if o.Auto {
		note = "Automatic failover"
		if inc != nil {
			note += " for incident: " + inc.Title
		}
	}
	if _, _, err := m.Fence(ctx); err != nil {
		return res, err
	}
	err = m.CP.TransitionDRPlan(ctx, storage.DRTransition{PlanID: p.ID, Expect: from, State: next, Direction: direction, Note: note, Run: run, At: now})
	if errors.Is(err, storage.ErrStateChanged) || errors.Is(err, storage.ErrConflict) {
		return res, ErrState
	}
	if err != nil {
		return res, err
	}
	res.Run = run
	m.Log.Info("dr: switch started", "plan_id", p.ID, "direction", direction, "run_id", run.ID, "actor", o.Actor, "forced", o.Force)
	if m.Engine != nil {
		m.Engine.Kick()
	}
	return res, nil
}

// Resolve settles a plan in "attention" after an operator checked where traffic goes.
func (m *Manager) Resolve(ctx context.Context, id, state, actor, note string) (*model.DRPlan, error) {
	if state != model.DRPrimary && state != model.DRFailedOver {
		return nil, ErrState
	}
	p, err := m.Store.GetDRPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.ActiveRunID != "" {
		if r, err := m.Store.GetRun(ctx, p.ActiveRunID); err == nil && !runFinished(r.Status) {
			return nil, ErrState // the run is still working (or waiting for its own reconciliation)
		}
	}
	err = m.CP.TransitionDRPlan(ctx, storage.DRTransition{PlanID: id, Expect: []string{model.DRAttention}, State: state,
		Note: "Set by " + actor + ": " + note, At: time.Now().UTC()})
	if errors.Is(err, storage.ErrStateChanged) {
		return nil, ErrState
	}
	if err != nil {
		return nil, err
	}
	return m.Store.GetDRPlan(ctx, id)
}

func runFinished(s string) bool {
	return s == model.RunSucceeded || s == model.RunFailed || s == model.RunCancelled
}

// WaitingReason explains why an automatic plan is not failing over right now ("" if not waiting).
func (m *Manager) WaitingReason(id string) string {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.waiting[id]
}

// Kick schedules an immediate evaluation (called on incident transitions and replicated changes).
func (m *Manager) Kick() {
	m.init()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Run evaluates plans until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.init()
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
		m.Tick(ctx)
	}
}

// Tick records the outcome of finished switch runs and evaluates automatic failover.
func (m *Manager) Tick(ctx context.Context) {
	m.init()
	if !m.Active() {
		return
	}
	plans, err := m.Store.ListDRPlans(ctx)
	if err != nil {
		return
	}
	for _, p := range plans {
		switch p.State {
		case model.DRFailingOver, model.DRFailingBack:
			m.settle(ctx, p)
		case model.DRPrimary:
			if p.Config.Automatic {
				m.auto(ctx, p)
			} else {
				m.setWaiting(p.ID, "")
			}
		default:
			m.setWaiting(p.ID, "")
		}
	}
}

func (m *Manager) setWaiting(id, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if why == "" {
		delete(m.waiting, id)
	} else {
		m.waiting[id] = why
	}
}

func (m *Manager) settle(ctx context.Context, p *model.DRPlan) {
	r, err := m.Store.GetRun(ctx, p.ActiveRunID)
	if err != nil || !runFinished(r.Status) {
		return
	}
	t := storage.DRTransition{PlanID: p.ID, Expect: []string{p.State}, ExpectRunID: r.ID, At: time.Now().UTC()}
	switch {
	case r.Status == model.RunSucceeded && p.State == model.DRFailingOver:
		t.State, t.Note = model.DRFailedOver, "Failover completed; traffic goes to the secondary."
	case r.Status == model.RunSucceeded:
		t.State, t.Note = model.DRPrimary, "Failback completed; traffic goes to the primary."
	default:
		t.State = model.DRAttention
		t.Note = fmt.Sprintf("The %s run ended %s: %s Check where traffic goes, then set the plan's state.", p.Direction, r.Status, r.Error)
	}
	if err := m.CP.TransitionDRPlan(ctx, t); err != nil && !errors.Is(err, storage.ErrStateChanged) {
		m.Log.Warn("dr: could not record the switch outcome", "plan_id", p.ID, "err", err)
		return
	}
	m.Log.Info("dr: switch finished", "plan_id", p.ID, "state", t.State, "run_id", r.ID)
}

func (m *Manager) auto(ctx context.Context, p *model.DRPlan) {
	if m.confirmedIncident(ctx, p) == nil {
		m.setWaiting(p.ID, "")
		return
	}
	res, err := m.Start(ctx, p.ID, Failover, StartOptions{Actor: "system", Auto: true})
	if err == nil {
		m.setWaiting(p.ID, "")
		return
	}
	why := err.Error()
	if errors.Is(err, ErrBlocked) && res != nil {
		var parts []string
		for _, c := range res.Checks {
			if !c.OK && c.Blocking {
				parts = append(parts, c.Detail)
			}
		}
		why = "Automatic failover is waiting: " + strings.Join(parts, " ")
	}
	if prev := m.WaitingReason(p.ID); prev != why {
		m.Log.Warn("dr: automatic failover not started", "plan_id", p.ID, "reason", why)
	}
	m.setWaiting(p.ID, why)
}

// OnIncidentTransition triggers an evaluation when an incident on a DR-watched monitor changes.
func (m *Manager) OnIncidentTransition(_ context.Context, in *model.Incident, _, to model.IncidentStatus) {
	if to == model.IncidentConfirmed || to == model.IncidentAcknowledged {
		m.Kick()
	}
}

// UsesProvider reports whether any plan uses the DNS integration.
func UsesProvider(plans []*model.DRPlan, providerID string) *model.DRPlan {
	for _, p := range plans {
		if p.Config.ProviderID == providerID {
			return p
		}
	}
	return nil
}

// WatchesMonitor reports whether a plan watches the monitor.
func WatchesMonitor(p *model.DRPlan, monitorID string) bool {
	return slices.Contains(p.Config.MonitorIDs, monitorID) || p.Config.SecondaryMonitorID == monitorID
}
