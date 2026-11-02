package api

import (
	"context"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

func TestDRPlanAPI(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	ctx := context.Background()
	e.dns.AddZone("cloudflare", "example.com")
	e.dns.AddRecord("cloudflare", "example.com", "A", "app", "192.0.2.1", 60, false)
	var prov struct {
		Provider map[string]any `json:"provider"`
	}
	if code := owner.do("POST", "/api/v1/dns/providers", map[string]any{"name": "CF", "type": "cloudflare", "api_token": "cf-token-123"}, &prov); code != 201 {
		t.Fatalf("provider: %d", code)
	}
	pid := prov.Provider["id"].(string)
	mons := map[string]string{}
	for _, n := range []string{"primary", "secondary"} {
		var m struct {
			Monitor model.Monitor `json:"monitor"`
		}
		if code := owner.do("POST", "/api/v1/monitors", map[string]any{"name": n, "type": "tcp", "target": "127.0.0.1:1", "interval_seconds": 3600, "enabled": false}, &m); code != 201 {
			t.Fatalf("monitor %s: %d", n, code)
		}
		mons[n] = m.Monitor.ID
		now := time.Now().UTC()
		if err := e.store.SaveMonitorState(ctx, &model.MonitorState{MonitorID: m.Monitor.ID, Status: model.StatusUp, StatusSince: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	var errResp struct {
		Error APIError `json:"error"`
	}
	bad := map[string]any{"name": "x", "config": map[string]any{"monitor_ids": []string{}, "provider_id": "nope", "zone_name": "example.com", "record_type": "TXT",
		"record_name": "app", "primary_value": "1.1.1.1", "secondary_value": "1.1.1.1", "automatic": true, "pre_steps": []map[string]any{{"type": "dns"}}}}
	if code := owner.do("POST", "/api/v1/dr-plans", bad, &errResp); code != 422 {
		t.Fatalf("invalid plan accepted: %d", code)
	}
	for _, k := range []string{"config.monitor_ids", "config.provider_id", "config.record_type", "config.pre_steps.0.type", "config.secondary_monitor_id"} {
		if errResp.Error.Fields[k] == "" {
			t.Errorf("missing error for %s: %v", k, errResp.Error.Fields)
		}
	}
	plan := map[string]any{"name": "App DR", "config": map[string]any{"monitor_ids": []string{mons["primary"]}, "secondary_monitor_id": mons["secondary"],
		"provider_id": pid, "zone_name": "example.com", "record_type": "A", "record_name": "app", "primary_value": "192.0.2.1", "secondary_value": "198.51.100.9",
		"post_steps": []map[string]any{{"name": "pause", "type": "wait", "seconds": 1}}}}
	var created struct {
		Plan model.DRPlan `json:"plan"`
	}
	if code := owner.do("POST", "/api/v1/dr-plans", plan, &created); code != 201 || created.Plan.State != "primary" || created.Plan.Config.Automatic || created.Plan.Config.RecordName != "app.example.com" {
		t.Fatalf("create: %d %+v", code, created.Plan)
	}
	id := created.Plan.ID
	// DR plans are not listed or editable as workflows.
	var wfs struct {
		Workflows []any `json:"workflows"`
	}
	owner.do("GET", "/api/v1/workflows", nil, &wfs)
	if len(wfs.Workflows) != 0 || owner.do("GET", "/api/v1/workflows/"+id, nil, nil) != 404 {
		t.Fatal("DR plan exposed as a workflow")
	}
	var prev struct {
		Result struct {
			Checks []map[string]any `json:"checks"`
			Steps  []string         `json:"steps"`
		} `json:"result"`
	}
	if code := owner.do("POST", "/api/v1/dr-plans/"+id+"/preview", map[string]any{"direction": "failover"}, &prev); code != 200 || len(prev.Result.Steps) != 2 {
		t.Fatalf("preview: %d %+v", code, prev.Result)
	}
	// Failback is refused from primary.
	if code := owner.do("POST", "/api/v1/dr-plans/"+id+"/failback", map[string]any{}, &errResp); code != 409 {
		t.Fatalf("failback from primary: %d", code)
	}
	if code := owner.do("POST", "/api/v1/dr-plans/"+id+"/failover", map[string]any{"force": true}, &errResp); code != 422 {
		t.Fatalf("force without note: %d", code)
	}
	if code := owner.do("POST", "/api/v1/dr-plans/"+id+"/failover", map[string]any{"note": "drill"}, nil); code != 200 {
		t.Fatalf("failover: %d", code)
	}
	if code := owner.do("PUT", "/api/v1/dr-plans/"+id, plan, &errResp); code != 409 || errResp.Error.Code != "dr_busy" {
		t.Fatalf("edit while failing over: %d", code)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var got struct {
			Plan model.DRPlan `json:"plan"`
		}
		owner.do("GET", "/api/v1/dr-plans/"+id, nil, &got)
		if got.Plan.State == model.DRFailedOver {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stuck in %s", got.Plan.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, r := range e.dns.Records("cloudflare", "example.com") {
		if r["name"] == "app.example.com" && r["content"] != "198.51.100.9" {
			t.Fatalf("record %v", r["content"])
		}
	}
	// The integration is in use and cannot be deleted.
	if code := owner.do("DELETE", "/api/v1/dns/providers/"+pid, nil, &errResp); code != 409 {
		t.Fatalf("delete provider in use: %d", code)
	}
	if code := owner.do("POST", "/api/v1/dr-plans/"+id+"/resolve", map[string]any{"state": "primary", "note": "x"}, &errResp); code != 409 {
		t.Fatalf("resolve outside attention: %d", code)
	}
	if code := owner.do("DELETE", "/api/v1/dr-plans/"+id, nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}
