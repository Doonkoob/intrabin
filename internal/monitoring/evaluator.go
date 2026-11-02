package monitoring

import (
	"sort"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

// RoundInput is everything needed to evaluate one round deterministically.
type RoundInput struct {
	Monitor    *model.Monitor
	RoundStart time.Time
	Results    []*model.CheckResult
	// Location returns the location label of a node (defaults to node id).
	Location func(nodeID string) string
	// RecentFailedRounds is the number of failed rounds within the policy window
	// strictly before RoundStart (used only by window mode).
	RecentFailedRounds int
	InMaintenance      bool
	Now                time.Time
}

// RoundVerdict computes the verdict for a round from per-node probe results.
// At most one result per node is considered (the latest probe in the round).
// Results for a different configuration version are ignored (stale).
func RoundVerdict(in RoundInput) *model.RoundResult {
	m := in.Monitor
	p := m.Policy
	loc := in.Location
	if loc == nil {
		loc = func(id string) string { return id }
	}
	latest := map[string]*model.CheckResult{}
	for _, r := range in.Results {
		if r.ConfigVersion != m.Version || r.ErrorCategory == model.ErrInvalidConfig {
			continue
		}
		if cur, ok := latest[r.NodeID]; !ok || r.ProbeAt.After(cur.ProbeAt) {
			latest[r.NodeID] = r
		}
	}
	rr := &model.RoundResult{MonitorID: m.ID, RoundStart: in.RoundStart, EvaluatedAt: in.Now, FailingNodes: []string{}}
	var latSum float64
	latN, slowN, okN := 0, 0, 0
	failLocs := map[string]bool{}
	for nodeID, r := range latest {
		rr.ValidProbes++
		if r.Success {
			okN++
			if r.Slow {
				slowN++
			}
			if r.LatencyMs != nil {
				latSum += *r.LatencyMs
				latN++
			}
		} else {
			rr.FailingProbes++
			rr.FailingNodes = append(rr.FailingNodes, nodeID)
			failLocs[loc(nodeID)] = true
		}
	}
	sort.Strings(rr.FailingNodes)
	if latN > 0 {
		avg := latSum / float64(latN)
		rr.AvgLatencyMs = &avg
	}
	minValid := max(p.MinValidProbes, 1)
	confirm := max(p.ConfirmationsRequired, 1)
	minLocs := max(p.MinLocations, 1)
	switch {
	case in.InMaintenance:
		rr.Verdict = model.VerdictMaintenance
	case rr.ValidProbes < minValid:
		rr.Verdict = model.VerdictInconclusive
	case rr.FailingProbes >= confirm && len(failLocs) >= minLocs:
		rr.Verdict = model.VerdictFailed
	case rr.FailingProbes > 0:
		rr.Verdict = model.VerdictPartial
	default:
		rr.Verdict = model.VerdictOK
	}
	rr.Slow = okN > 0 && slowN*2 > okN
	return rr
}

// UnknownAfterInconclusive is the number of inconclusive rounds after which status becomes UNKNOWN.
const UnknownAfterInconclusive = 3

// NextState applies a round verdict to the previous monitor state.
func NextState(prev model.MonitorState, rr *model.RoundResult, in RoundInput) model.MonitorState {
	p := in.Monitor.Policy
	st := prev
	st.LastRound = rr.RoundStart.UnixMilli()
	st.LastVerdict = rr.Verdict
	st.UpdatedAt = in.Now
	if rr.AvgLatencyMs != nil {
		v := *rr.AvgLatencyMs
		st.LastLatencyMs = &v
	}
	failureRounds := max(p.FailureRounds, 1)
	recoveryRounds := max(p.RecoveryRounds, 1)
	healthy := func() model.MonitorStatus {
		if rr.Verdict == model.VerdictPartial || rr.Slow {
			return model.StatusDegraded
		}
		return model.StatusUp
	}
	var next model.MonitorStatus
	switch rr.Verdict {
	case model.VerdictMaintenance:
		next = model.StatusMaintenance
	case model.VerdictFailed:
		st.ConsecutiveFailed++
		st.ConsecutiveOK = 0
		st.ConsecutiveInconclusive = 0
		thresholdMet := false
		if p.FailMode == model.FailWindow {
			thresholdMet = in.RecentFailedRounds+1 >= failureRounds
		} else {
			thresholdMet = st.ConsecutiveFailed >= failureRounds
		}
		if thresholdMet || prev.Status == model.StatusDown {
			next = model.StatusDown
		} else {
			next = model.StatusSuspectedDown
		}
	case model.VerdictOK, model.VerdictPartial:
		st.ConsecutiveOK++
		st.ConsecutiveFailed = 0
		st.ConsecutiveInconclusive = 0
		if prev.Status == model.StatusDown && st.ConsecutiveOK < recoveryRounds {
			next = model.StatusDown // recovery not yet confirmed
		} else {
			next = healthy()
		}
	default: // inconclusive: missing data is not downtime; counters are frozen
		st.ConsecutiveInconclusive++
		switch {
		case prev.Status == model.StatusDown:
			next = model.StatusDown
		case st.ConsecutiveInconclusive >= UnknownAfterInconclusive:
			next = model.StatusUnknown
		case prev.Status == model.StatusPaused || prev.Status == model.StatusMaintenance || prev.Status == "":
			next = model.StatusUnknown
		default:
			next = prev.Status
		}
	}
	if next != prev.Status {
		st.StatusSince = in.Now
	}
	st.Status = next
	return st
}
