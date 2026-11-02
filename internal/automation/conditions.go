package automation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// Condition trigger metrics and operators.
var (
	ConditionMetrics = []string{"latency_ms", "status_code", "uptime_24h", "cert_days", "status"}
	ConditionOps     = []string{"gt", "ge", "lt", "le", "eq", "ne"}
)

// DefaultRepeat is the default minimum time between two condition runs for one monitor.
const DefaultRepeat = time.Hour

var opText = map[string]string{"gt": ">", "ge": "≥", "lt": "<", "le": "≤", "eq": "=", "ne": "≠"}

// DescribeCondition renders a condition for humans, e.g. "latency_ms > 800".
func DescribeCondition(t model.WorkflowTrigger) string {
	if t.Metric == "status" {
		return fmt.Sprintf("status %s %s", opText[t.Op], t.StatusValue)
	}
	return fmt.Sprintf("%s %s %s", t.Metric, opText[t.Op], strconv.FormatFloat(t.Threshold, 'f', -1, 64))
}

func compare(v float64, op string, thr float64) bool {
	switch op {
	case "gt":
		return v > thr
	case "ge":
		return v >= thr
	case "lt":
		return v < thr
	case "le":
		return v <= thr
	case "eq":
		return v == thr
	case "ne":
		return v != thr
	}
	return false
}

// evalCondition reports whether the condition holds for this round. known is false when the round
// has no data for the metric (the condition then keeps its previous state).
func (e *Engine) evalCondition(ctx context.Context, t model.WorkflowTrigger, m *model.Monitor, rr *model.RoundResult, st model.MonitorState) (holds, known bool, value string) {
	if rr.Verdict == model.VerdictMaintenance {
		return false, false, ""
	}
	switch t.Metric {
	case "latency_ms":
		if rr.AvgLatencyMs == nil {
			return false, false, ""
		}
		return compare(*rr.AvgLatencyMs, t.Op, t.Threshold), true, strconv.FormatFloat(*rr.AvgLatencyMs, 'f', 0, 64)
	case "status_code":
		if len(rr.StatusCodes) == 0 {
			return false, false, ""
		}
		for _, c := range rr.StatusCodes { // holds when any node saw a matching code
			if compare(float64(c), t.Op, t.Threshold) {
				return true, true, strconv.Itoa(c)
			}
		}
		return false, true, strconv.Itoa(rr.StatusCodes[0])
	case "cert_days":
		if rr.CertExpiresAt == nil {
			return false, false, ""
		}
		days := rr.CertExpiresAt.Sub(rr.EvaluatedAt).Hours() / 24
		return compare(days, t.Op, t.Threshold), true, strconv.FormatFloat(days, 'f', 1, 64)
	case "uptime_24h":
		u, err := e.Store.Uptime(ctx, m.ID, rr.EvaluatedAt.Add(-24*time.Hour))
		if err != nil || u.Availability == nil {
			return false, false, ""
		}
		return compare(*u.Availability, t.Op, t.Threshold), true, strconv.FormatFloat(*u.Availability, 'f', 2, 64)
	case "status":
		eq := string(st.Status) == t.StatusValue
		if t.Op == "ne" {
			return !eq, true, string(st.Status)
		}
		return eq, true, string(st.Status)
	}
	return false, false, ""
}

// OnRound evaluates condition-triggered workflows after a round of m was recorded. A workflow runs
// when its condition becomes true for a monitor (not on every round while it stays true), at most
// once per repeat window. The run's trigger reference includes the window, so a new leader that
// re-evaluates the same round cannot start a second run for it.
func (e *Engine) OnRound(ctx context.Context, m *model.Monitor, rr *model.RoundResult, st model.MonitorState) {
	e.init()
	if !e.Active() {
		return
	}
	wfs, err := e.Store.ListWorkflows(ctx)
	if err != nil {
		return
	}
	started := false
	for _, wf := range wfs {
		if !wf.Enabled || wf.Trigger.Type != "condition" || !matchesMonitor(wf.Trigger, m) {
			continue
		}
		holds, known, value := e.evalCondition(ctx, wf.Trigger, m, rr, st)
		if !known {
			continue
		}
		key := wf.ID + "/" + m.ID
		e.mu.Lock()
		was := e.condActive[key]
		e.condActive[key] = holds
		last := e.condFired[key]
		e.mu.Unlock()
		if !holds || was {
			continue
		}
		repeat := DefaultRepeat
		if wf.Trigger.RepeatMinutes > 0 {
			repeat = time.Duration(wf.Trigger.RepeatMinutes) * time.Minute
		}
		if !last.IsZero() && rr.RoundStart.Sub(last) < repeat {
			continue
		}
		ref := fmt.Sprintf("condition:%s:%s:%d", wf.ID, m.ID, rr.RoundStart.UnixMilli()/repeat.Milliseconds())
		cx := map[string]string{"monitor_id": m.ID, "monitor_name": m.Name, "monitor_target": m.Target, "monitor_status": string(st.Status),
			"condition": DescribeCondition(wf.Trigger), "metric": wf.Trigger.Metric, "value": value, "round_start": rr.RoundStart.Format(time.RFC3339)}
		r := newRun(wf, "condition", ref, "system", cx)
		if err := e.CP.CreateRun(ctx, r); err != nil && !errors.Is(err, storage.ErrConflict) {
			e.Log.Error("automation: could not start condition workflow", "workflow_id", wf.ID, "monitor_id", m.ID, "err", err)
			continue
		}
		e.mu.Lock()
		e.condFired[key] = rr.RoundStart
		e.mu.Unlock()
		started = true
	}
	if started {
		e.Kick()
	}
}
