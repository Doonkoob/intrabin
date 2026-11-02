package monitoring

import (
	"context"
	"errors"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/metrics"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// RoundStart returns the start of the interval-aligned round containing t.
func RoundStart(t time.Time, interval time.Duration) time.Time {
	return t.UTC().Truncate(interval)
}

// ProbeOffset spreads probes of different monitors within the round (deterministic per monitor).
func ProbeOffset(monitorID string, interval time.Duration) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(monitorID))
	maxOff := interval / 4
	if maxOff > 10*time.Second {
		maxOff = 10 * time.Second
	}
	if maxOff <= 0 {
		return 0
	}
	return time.Duration(h.Sum32()) % maxOff
}

// EvalGrace is how long after a round ends the evaluator waits for late results.
const EvalGrace = 3 * time.Second

// MaxCatchUpRounds bounds evaluation work after a long process outage.
const MaxCatchUpRounds = 500

// Engine schedules probes for this node and evaluates rounds (when it is the evaluator).
type Engine struct {
	Store *storage.Store
	// CP is the replicated write path for round verdicts and monitor state.
	CP        *controlplane.CP
	Prober    *Prober
	Incidents *incidents.Manager
	Log       *slog.Logger
	NodeID    string
	// Decrypt populates m.Secrets from m.SecretBlob.
	Decrypt func(m *model.Monitor) error
	// IsEvaluator reports whether this node evaluates rounds (standalone or leader).
	IsEvaluator func() bool
	// Location returns a node's location label.
	Location func(nodeID string) string
	// ResultSink receives probe results; defaults to local storage insert.
	ResultSink func(ctx context.Context, rs []*model.CheckResult) error
	// Retention returns raw, round and hourly retention durations.
	Retention func(ctx context.Context) (raw, rounds, hourly time.Duration)
	// OnRound is called after a round verdict was recorded (condition triggers).
	OnRound func(ctx context.Context, m *model.Monitor, rr *model.RoundResult, st model.MonitorState)
	Workers int
	Now     func() time.Time

	reload    chan struct{}
	mu        sync.Mutex
	monitors  []*model.Monitor
	lastProbe map[string]int64 // monitorID → round start (ms) last dispatched
	inflight  map[string]bool
	sem       chan struct{}
	wg        sync.WaitGroup
}

func (e *Engine) init() {
	if e.Now == nil {
		e.Now = func() time.Time { return time.Now().UTC() }
	}
	if e.Workers <= 0 {
		e.Workers = 64
	}
	if e.IsEvaluator == nil {
		e.IsEvaluator = func() bool { return true }
	}
	if e.CP == nil {
		e.CP = controlplane.New(e.Store)
	}
	if e.ResultSink == nil {
		e.ResultSink = func(ctx context.Context, rs []*model.CheckResult) error {
			_, err := e.Store.InsertResults(ctx, rs)
			return err
		}
	}
	e.reload = make(chan struct{}, 1)
	e.lastProbe = map[string]int64{}
	e.inflight = map[string]bool{}
	e.sem = make(chan struct{}, e.Workers)
}

// Reload asks the engine to reload monitor definitions.
func (e *Engine) Reload() {
	select {
	case e.reload <- struct{}{}:
	default:
	}
}

func (e *Engine) loadMonitors(ctx context.Context) {
	ms, err := e.Store.ListMonitors(ctx)
	if err != nil {
		e.Log.Error("load monitors", "err", err)
		return
	}
	e.mu.Lock()
	e.monitors = ms
	e.mu.Unlock()
}

func (e *Engine) assigned(m *model.Monitor) bool {
	if len(m.NodeSelector) == 0 {
		return true
	}
	for _, id := range m.NodeSelector {
		if id == e.NodeID {
			return true
		}
	}
	return false
}

// Run blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.init()
	e.loadMonitors(ctx)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	refresh := time.NewTicker(30 * time.Second)
	defer refresh.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	e.prune(ctx)
	lastEval := time.Time{}
	for {
		select {
		case <-ctx.Done():
			e.wg.Wait()
			return
		case <-e.reload:
			e.loadMonitors(ctx)
		case <-refresh.C:
			e.loadMonitors(ctx)
		case <-prune.C:
			e.prune(ctx)
		case <-tick.C:
			e.dispatchDue(ctx)
			if now := e.Now(); now.Sub(lastEval) >= time.Second {
				lastEval = now
				if e.IsEvaluator() {
					e.EvaluateDue(ctx)
				}
			}
		}
	}
}

func (e *Engine) dispatchDue(ctx context.Context) {
	now := e.Now()
	e.mu.Lock()
	ms := e.monitors
	e.mu.Unlock()
	for _, m := range ms {
		if !m.Enabled || !e.assigned(m) {
			continue
		}
		interval := time.Duration(m.IntervalSeconds) * time.Second
		rs := RoundStart(now, interval)
		key := m.ID
		e.mu.Lock()
		due := e.lastProbe[key] != rs.UnixMilli() && !e.inflight[key] && now.Sub(rs) >= ProbeOffset(m.ID, interval)
		if due {
			e.lastProbe[key] = rs.UnixMilli()
		}
		e.mu.Unlock()
		if !due {
			continue
		}
		select {
		case e.sem <- struct{}{}:
		default:
			metrics.ProbeQueueDropped.Inc()
			e.Log.Warn("probe skipped: worker pool saturated", "monitor_id", m.ID)
			continue
		}
		e.mu.Lock()
		e.inflight[key] = true
		e.mu.Unlock()
		e.wg.Add(1)
		go func(m *model.Monitor, rs time.Time) {
			defer func() {
				<-e.sem
				e.mu.Lock()
				delete(e.inflight, m.ID)
				e.mu.Unlock()
				e.wg.Done()
			}()
			r := e.RunProbe(ctx, m, rs)
			if r == nil {
				return
			}
			if err := e.ResultSink(context.WithoutCancel(ctx), []*model.CheckResult{r}); err != nil {
				e.Log.Error("store probe result", "monitor_id", m.ID, "err", err)
			}
		}(m, rs)
	}
}

// RunProbe executes one probe for a monitor and returns the result record.
func (e *Engine) RunProbe(ctx context.Context, m *model.Monitor, roundStart time.Time) *model.CheckResult {
	mm := *m
	if e.Decrypt != nil && len(m.SecretBlob) > 0 {
		if err := e.Decrypt(&mm); err != nil {
			e.Log.Error("decrypt monitor secrets", "monitor_id", m.ID, "err", err)
			return &model.CheckResult{ID: uuid.NewString(), MonitorID: m.ID, NodeID: e.NodeID, ConfigVersion: m.Version, RoundStart: roundStart,
				ProbeAt: e.Now(), ReceivedAt: e.Now(), ErrorCategory: model.ErrInvalidConfig, ErrorMessage: "cannot decrypt monitor secrets", Attempts: 0}
		}
	}
	start := e.Now()
	out, attempts := e.Prober.Probe(ctx, &mm)
	if ctx.Err() != nil {
		return nil // shutting down: do not record a fabricated failure
	}
	r := &model.CheckResult{ID: uuid.NewString(), MonitorID: m.ID, NodeID: e.NodeID, ConfigVersion: m.Version, RoundStart: roundStart,
		ProbeAt: start, ReceivedAt: e.Now(), Success: out.Success, Slow: out.Slow, HTTPStatus: out.HTTPStatus, ErrorCategory: out.Category,
		ErrorMessage: out.Message, Attempts: attempts, CertExpiresAt: out.CertExpiresAt}
	if out.Latency > 0 {
		l := float64(out.Latency.Microseconds()) / 1000
		r.LatencyMs = &l
	}
	outcome := "success"
	if !out.Success {
		outcome = "failure"
		metrics.CheckErrors.WithLabelValues(string(out.Category)).Inc()
	}
	metrics.ChecksTotal.WithLabelValues(string(m.Type), outcome).Inc()
	metrics.CheckDuration.WithLabelValues(string(m.Type)).Observe(out.Latency.Seconds())
	e.Log.Debug("probe", "monitor_id", m.ID, "check_id", r.ID, "success", out.Success, "category", out.Category, "latency_ms", r.LatencyMs)
	return r
}

// EvaluateDue evaluates all closed, unevaluated rounds for every enabled monitor.
func (e *Engine) EvaluateDue(ctx context.Context) {
	e.mu.Lock()
	ms := e.monitors
	e.mu.Unlock()
	for _, m := range ms {
		if !m.Enabled {
			continue
		}
		if err := e.evaluateMonitor(ctx, m); err != nil && !errors.Is(err, context.Canceled) {
			e.Log.Error("evaluate monitor", "monitor_id", m.ID, "err", err)
		}
	}
}

func (e *Engine) evaluateMonitor(ctx context.Context, m *model.Monitor) error {
	interval := time.Duration(m.IntervalSeconds) * time.Second
	now := e.Now()
	st, err := e.Store.GetMonitorState(ctx, m.ID)
	if errors.Is(err, storage.ErrNotFound) {
		st = &model.MonitorState{MonitorID: m.ID, Status: model.StatusUnknown, StatusSince: now}
	} else if err != nil {
		return err
	}
	// Fresh (or resumed) monitors start at the round containing the time they became active.
	next := RoundStart(st.StatusSince, interval)
	if st.StatusSince.IsZero() || st.StatusSince.After(now) {
		next = RoundStart(now, interval)
	}
	if st.LastRound != 0 && st.Status != model.StatusPaused {
		next = time.UnixMilli(st.LastRound).UTC().Add(interval)
	}
	// If the definition changed alignment (interval change), re-align.
	if !next.Equal(RoundStart(next, interval)) {
		next = RoundStart(now, interval)
	}
	if behind := int(now.Sub(next) / interval); behind > MaxCatchUpRounds {
		e.Log.Warn("evaluation far behind; skipping rounds", "monitor_id", m.ID, "rounds", behind-MaxCatchUpRounds)
		next = next.Add(time.Duration(behind-MaxCatchUpRounds) * interval)
	}
	for !next.Add(interval).Add(EvalGrace).After(now) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.evaluateRound(ctx, m, st, next); err != nil {
			return err
		}
		next = next.Add(interval)
	}
	return nil
}

func (e *Engine) evaluateRound(ctx context.Context, m *model.Monitor, st *model.MonitorState, rs time.Time) error {
	results, err := e.Store.ResultsForRound(ctx, m.ID, rs)
	if err != nil {
		return err
	}
	inMaint, err := e.Store.InMaintenance(ctx, m.ID, rs)
	if err != nil {
		return err
	}
	in := RoundInput{Monitor: m, RoundStart: rs, Results: results, Location: e.Location, InMaintenance: inMaint, Now: e.Now()}
	if m.Policy.FailMode == model.FailWindow && m.Policy.WindowSeconds > 0 {
		n, err := e.Store.CountFailedRounds(ctx, m.ID, rs.Add(-time.Duration(m.Policy.WindowSeconds)*time.Second), rs)
		if err != nil {
			return err
		}
		in.RecentFailedRounds = n
	}
	rr := RoundVerdict(in)
	prev := *st
	if prev.Status == model.StatusPaused {
		prev.Status = model.StatusUnknown
	}
	ns := NextState(prev, rr, in)
	if err := e.CP.RecordRound(ctx, rr, &ns, m.IntervalSeconds); err != nil {
		return err
	}
	metrics.RoundsTotal.WithLabelValues(string(rr.Verdict)).Inc()
	*st = ns
	if e.OnRound != nil {
		e.OnRound(ctx, m, rr, ns)
	}
	if ns.Status != prev.Status && e.Incidents != nil {
		cat := ""
		for _, r := range results {
			if !r.Success && r.ErrorCategory != "" {
				cat = string(r.ErrorCategory)
				break
			}
		}
		first := rs.Add(-time.Duration(max(ns.ConsecutiveFailed-1, 0)) * time.Duration(m.IntervalSeconds) * time.Second)
		e.Log.Info("monitor status changed", "monitor_id", m.ID, "from", prev.Status, "to", ns.Status, "round_start", rs, "verdict", rr.Verdict)
		if err := e.Incidents.HandleMonitorChange(ctx, incidents.MonitorChange{Monitor: m, From: prev.Status, To: ns.Status, Round: rr, FirstFailure: first, Category: cat}); err != nil {
			e.Log.Error("incident handling", "monitor_id", m.ID, "err", err)
		}
	}
	return nil
}

func (e *Engine) prune(ctx context.Context) {
	if e.Retention == nil {
		return
	}
	raw, rounds, hourly := e.Retention(ctx)
	now := e.Now()
	n, err := e.Store.Prune(ctx, now.Add(-raw), now.Add(-rounds), now.Add(-hourly))
	if err != nil {
		e.Log.Error("retention prune", "err", err)
		return
	}
	if n > 0 {
		e.Log.Info("retention prune", "rows", n)
	}
}
