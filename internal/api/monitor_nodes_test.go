package api

import (
	"context"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

func TestMonitorNodeSeriesAndResultsFilter(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	ctx := context.Background()
	var m struct {
		Monitor model.Monitor `json:"monitor"`
	}
	if code := owner.do("POST", "/api/v1/monitors", map[string]any{"name": "nodes", "type": "http", "target": "https://example.com", "interval_seconds": 30, "enabled": false}, &m); code != 201 {
		t.Fatal(code)
	}
	now := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Minute)
	rs := []*model.CheckResult{}
	for i, x := range []struct {
		id, node string
		lat      float64
		ok       bool
	}{{"a1", "node-a", 100, true}, {"a2", "node-a", 300, true}, {"b1", "node-b", 900, true}, {"b2", "node-b", 0, false}} {
		at := now.Add(time.Duration(i) * time.Second)
		rs = append(rs, &model.CheckResult{ID: x.id, NodeID: x.node, MonitorID: m.Monitor.ID, ProbeAt: at, RoundStart: now, ReceivedAt: at, Success: x.ok, LatencyMs: f64(x.lat), ConfigVersion: 1})
	}
	if _, err := e.store.InsertResults(ctx, rs); err != nil {
		t.Fatal(err)
	}
	var stats struct {
		Nodes []storage.NodeSeries `json:"node_latency"`
	}
	path := "/api/v1/monitors/" + m.Monitor.ID
	if code := owner.do("GET", path+"/stats?range=24h", nil, &stats); code != 200 || len(stats.Nodes) != 2 {
		t.Fatalf("raw: %d %+v", code, stats)
	}
	a, b := stats.Nodes[0].Points[0], stats.Nodes[1].Points[0]
	if a.AvgMs != 200 || a.MaxMs != 300 || a.Total != 2 || b.AvgMs != 900 || b.Failures != 1 || b.Total != 2 {
		t.Fatalf("raw stats: %+v", stats)
	}
	// Replicated hourly aggregates survive raw-result retention. Weighted averages must not
	// average hourly means; a failed-only node must remain visible without a success latency.
	for i, lat := range []float64{100, 300, 500} {
		at := now.Add(-time.Duration(i+1) * time.Hour)
		rr := &model.RoundResult{MonitorID: m.Monitor.ID, RoundStart: at, EvaluatedAt: at, Verdict: model.VerdictPartial, NodeStats: []model.NodeStat{{NodeID: "node-a", OK: true, LatencyMs: f64(lat)}, {NodeID: "node-b", OK: false}}}
		if err := e.store.RecordRound(ctx, rr, &model.MonitorState{MonitorID: m.Monitor.ID, Status: model.StatusDegraded, StatusSince: at, UpdatedAt: at}, 30); err != nil {
			t.Fatal(err)
		}
	}
	if code := owner.do("GET", path+"/stats?range=7d", nil, &stats); code != 200 || len(stats.Nodes) != 2 {
		t.Fatalf("hourly: %d %+v", code, stats)
	}
	count, failures := 0, 0
	for _, n := range stats.Nodes {
		for _, p := range n.Points {
			count += p.Total
			failures += p.Failures
			if p.MaxMs != 0 {
				t.Fatal("aggregates have no maximum")
			}
		}
	}
	if count != 6 || failures != 3 {
		t.Fatalf("hourly totals %d/%d", count, failures)
	}
	var results struct {
		Results []model.CheckResult `json:"results"`
	}
	if code := owner.do("GET", path+"/results?node_id=node-b", nil, &results); code != 200 || len(results.Results) != 2 {
		t.Fatalf("filtered: %d %+v", code, results)
	}
	for _, r := range results.Results {
		if r.NodeID != "node-b" {
			t.Fatal("another node leaked into filter")
		}
	}
	if code := owner.do("GET", path+"/results?node_id=absent", nil, &results); code != 200 || len(results.Results) != 0 {
		t.Fatal("empty filter")
	}
}
