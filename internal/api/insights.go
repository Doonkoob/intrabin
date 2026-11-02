package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

var latencyWindows = map[string]time.Duration{"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

type nodeCell struct {
	Probes       int      `json:"probes"`
	Failures     int      `json:"failures"`
	AvgLatencyMs *float64 `json:"avg_latency_ms"`
}

// handleNodeLatency returns average latency per monitoring node, overall and per monitor, from the
// stored hourly per-node aggregates. ?window=1h|24h|7d|30d (default 24h).
func (s *Server) handleNodeLatency(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	win := r.URL.Query().Get("window")
	d, ok := latencyWindows[win]
	if !ok {
		win, d = "24h", 24*time.Hour
	}
	since := time.Now().UTC().Add(-d)
	totals, err := s.store.NodeLatencies(ctx, since, false)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	per, err := s.store.NodeLatencies(ctx, since, true)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	names := s.nodeNames()
	type nodeRow struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		nodeCell
	}
	nodes := []nodeRow{}
	for _, t := range totals {
		name := names[t.NodeID]
		if name == "" {
			name = t.NodeID
		}
		nodes = append(nodes, nodeRow{ID: t.NodeID, Name: name, nodeCell: nodeCell{t.Probes, t.Failures, t.AvgLatencyMs}})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	ms, err := s.store.ListMonitors(ctx)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	type monRow struct {
		ID     string              `json:"id"`
		Name   string              `json:"name"`
		Target string              `json:"target"`
		Nodes  map[string]nodeCell `json:"nodes"`
	}
	byID := map[string]*monRow{}
	monitors := []*monRow{}
	for _, m := range ms {
		row := &monRow{ID: m.ID, Name: m.Name, Target: m.Target, Nodes: map[string]nodeCell{}}
		byID[m.ID] = row
		monitors = append(monitors, row)
	}
	for _, p := range per {
		if row := byID[p.MonitorID]; row != nil {
			row.Nodes[p.NodeID] = nodeCell{p.Probes, p.Failures, p.AvgLatencyMs}
		}
	}
	sort.Slice(monitors, func(i, j int) bool { return monitors[i].Name < monitors[j].Name })
	writeJSON(w, 200, map[string]any{"window": win, "nodes": nodes, "monitors": monitors})
}

// handleCertificates lists the TLS certificate expiry seen for each HTTPS/TLS monitor.
func (s *Server) handleCertificates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ms, err := s.store.ListMonitors(ctx)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	type certRow struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		Target    string     `json:"target"`
		ExpiresAt *time.Time `json:"expires_at"`
		DaysLeft  *float64   `json:"days_left"`
		WarnDays  int        `json:"warn_days"`
		FailDays  int        `json:"fail_days"`
	}
	now := time.Now().UTC()
	out := []certRow{}
	for _, m := range ms {
		if m.Type != model.MonitorHTTP && m.Type != model.MonitorTLS {
			continue
		}
		row := certRow{ID: m.ID, Name: m.Name, Target: m.Target, WarnDays: m.Settings.CertExpiryWarnDays, FailDays: m.Settings.CertExpiryFailDays}
		if exp, err := s.store.LatestCertExpiry(ctx, m.ID); err == nil && exp != nil {
			row.ExpiresAt = exp
			d := exp.Sub(now).Hours() / 24
			row.DaysLeft = &d
		}
		if row.ExpiresAt == nil && m.Type == model.MonitorHTTP && len(m.Target) > 5 && m.Target[:5] == "http:" {
			continue // plain HTTP: no certificate
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].DaysLeft, out[j].DaysLeft
		switch {
		case a == nil:
			return false
		case b == nil:
			return true
		}
		return *a < *b
	})
	writeJSON(w, 200, map[string]any{"certificates": out})
}

// handleStorage reports the database size on this node and the effective retention.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.DiskUsage(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	ret := s.store.Retention(r.Context())
	days := func(d time.Duration) int { return int(d / (24 * time.Hour)) }
	writeJSON(w, 200, map[string]any{"usage": u, "node": s.node.Identity.Name, "retention": map[string]int{
		"logs_days": ret.LogsDays, "raw_days": days(ret.Raw), "rounds_days": days(ret.Rounds), "hourly_days": days(ret.Hourly),
		"audit_days": days(ret.Audit), "runs_days": days(ret.Runs)}, "max_raw_days": storage.MaxRawRetentionDays})
}
