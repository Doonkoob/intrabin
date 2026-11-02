package api

import (
	"context"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

func f64(v float64) *float64 { return &v }

func TestNodeLatencyCertificatesStorageAndRetention(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	ctx := context.Background()
	var m struct {
		Monitor model.Monitor `json:"monitor"`
	}
	if code := owner.do("POST", "/api/v1/monitors", map[string]any{"name": "shop", "type": "http", "target": "https://shop.example", "interval_seconds": 60, "enabled": false}, &m); code != 201 {
		t.Fatalf("monitor: %d", code)
	}
	now := time.Now().UTC()
	for i, lat := range []float64{100, 300} {
		rr := &model.RoundResult{MonitorID: m.Monitor.ID, RoundStart: now.Add(-time.Duration(i+1) * time.Minute), EvaluatedAt: now, Verdict: model.VerdictPartial,
			NodeStats: []model.NodeStat{{NodeID: "node-a", OK: true, LatencyMs: f64(lat)}, {NodeID: "node-b", OK: false}}}
		if err := e.store.RecordRound(ctx, rr, &model.MonitorState{MonitorID: m.Monitor.ID, Status: model.StatusDegraded, StatusSince: now, UpdatedAt: now}, 60); err != nil {
			t.Fatal(err)
		}
	}
	var lat struct {
		Nodes []struct {
			ID           string   `json:"id"`
			Probes       int      `json:"probes"`
			Failures     int      `json:"failures"`
			AvgLatencyMs *float64 `json:"avg_latency_ms"`
		} `json:"nodes"`
		Monitors []struct {
			ID    string `json:"id"`
			Nodes map[string]struct {
				AvgLatencyMs *float64 `json:"avg_latency_ms"`
			} `json:"nodes"`
		} `json:"monitors"`
	}
	if code := owner.do("GET", "/api/v1/overview/node-latency?window=24h", nil, &lat); code != 200 || len(lat.Nodes) != 2 {
		t.Fatalf("node latency: %d %+v", code, lat)
	}
	a := lat.Nodes[0]
	if a.ID != "node-a" || a.Probes != 2 || a.AvgLatencyMs == nil || *a.AvgLatencyMs != 200 || lat.Nodes[1].Failures != 2 || lat.Nodes[1].AvgLatencyMs != nil {
		t.Fatalf("node totals: %+v", lat.Nodes)
	}
	if c := lat.Monitors[0].Nodes["node-a"]; c.AvgLatencyMs == nil || *c.AvgLatencyMs != 200 {
		t.Fatalf("per monitor: %+v", lat.Monitors)
	}

	exp := now.Add(10 * 24 * time.Hour)
	if _, err := e.store.InsertResults(ctx, []*model.CheckResult{{ID: "r1", MonitorID: m.Monitor.ID, NodeID: "node-a", ConfigVersion: 1, RoundStart: now, ProbeAt: now, ReceivedAt: now, Success: true, CertExpiresAt: &exp}}); err != nil {
		t.Fatal(err)
	}
	var certs struct {
		Certificates []struct {
			ID       string   `json:"id"`
			DaysLeft *float64 `json:"days_left"`
		} `json:"certificates"`
	}
	if code := owner.do("GET", "/api/v1/overview/certificates", nil, &certs); code != 200 || len(certs.Certificates) != 1 || certs.Certificates[0].DaysLeft == nil || *certs.Certificates[0].DaysLeft < 9.9 {
		t.Fatalf("certificates: %+v", certs)
	}

	var st struct {
		Usage struct {
			TotalBytes int64 `json:"total_bytes"`
			Tables     []struct {
				Name string `json:"name"`
				Rows int64  `json:"rows"`
			} `json:"tables"`
		} `json:"usage"`
		Retention map[string]int `json:"retention"`
	}
	if code := owner.do("GET", "/api/v1/system/storage", nil, &st); code != 200 || st.Usage.TotalBytes == 0 || len(st.Usage.Tables) == 0 || st.Retention["audit_days"] != 365 {
		t.Fatalf("storage: %d %+v", code, st)
	}
	var errResp struct {
		Error APIError `json:"error"`
	}
	if code := owner.do("PATCH", "/api/v1/settings", map[string]any{"log_retention_days": 45}, &errResp); code != 422 || errResp.Error.Fields["log_retention_days"] == "" {
		t.Fatalf("bad preset accepted: %d", code)
	}
	if code := owner.do("PATCH", "/api/v1/settings", map[string]any{"log_retention_days": 15}, nil); code != 200 {
		t.Fatalf("preset: %d", code)
	}
	if r := e.store.Retention(ctx); r.LogsDays != 15 || r.Audit != 15*24*time.Hour || r.Raw != 15*24*time.Hour || r.Hourly != 15*24*time.Hour {
		t.Fatalf("retention: %+v", r)
	}
	if code := owner.do("PATCH", "/api/v1/settings", map[string]any{"log_retention_days": 365}, nil); code != 200 {
		t.Fatal("365")
	}
	if r := e.store.Retention(ctx); r.Raw != 90*24*time.Hour || r.Rounds != 365*24*time.Hour {
		t.Fatalf("365-day retention: %+v", r)
	}

	// Condition-triggered workflows are validated.
	wf := map[string]any{"name": "slow shop", "trigger": map[string]any{"type": "condition", "monitor_ids": []string{m.Monitor.ID}, "metric": "latency_ms", "op": "gt", "threshold": 800},
		"steps": []map[string]any{{"name": "wait", "type": "wait", "seconds": 1}}}
	var created struct {
		Workflow model.Workflow `json:"workflow"`
	}
	if code := owner.do("POST", "/api/v1/workflows", wf, &created); code != 201 || created.Workflow.Trigger.RepeatMinutes != 60 {
		t.Fatalf("condition workflow: %d %+v", code, created.Workflow.Trigger)
	}
	wf["trigger"] = map[string]any{"type": "condition", "metric": "status", "op": "gt", "status_value": "sideways"}
	if code := owner.do("POST", "/api/v1/workflows", wf, &errResp); code != 422 || errResp.Error.Fields["trigger.op"] == "" || errResp.Error.Fields["trigger.status_value"] == "" {
		t.Fatalf("invalid condition accepted: %d %v", code, errResp.Error.Fields)
	}
}
