package api

import (
	"strings"
	"testing"

	"github.com/intrabin/intrabin/internal/model"
)

func TestDNSAPI(t *testing.T) {
	e := newEnv(t)
	owner := e.owner()
	e.dns.AddZone("cloudflare", "example.com")
	e.dns.AddRecord("cloudflare", "example.com", "A", "www", "192.0.2.10", 300, true)
	e.dns.AddRecord("cloudflare", "example.com", "MX", "@", "mail.example.com", 300, false)

	var errResp struct {
		Error APIError `json:"error"`
	}
	// wrong credentials are rejected before anything is stored
	if code := owner.do("POST", "/api/v1/dns/providers", map[string]any{"name": "CF", "type": "cloudflare", "api_token": "wrong"}, &errResp); code != 422 || errResp.Error.Code != "dns_auth" {
		t.Fatalf("bad creds: %d %+v", code, errResp.Error)
	}
	var created struct {
		Provider map[string]any `json:"provider"`
	}
	if code := owner.do("POST", "/api/v1/dns/providers", map[string]any{"name": "CF", "type": "cloudflare", "api_token": "cf-token-123"}, &created); code != 201 {
		t.Fatalf("create: %d", code)
	}
	pid := created.Provider["id"].(string)
	var raw map[string]any
	owner.do("GET", "/api/v1/dns/providers", nil, &raw)
	if body := mustJSON(raw); strings.Contains(body, "cf-token-123") || strings.Contains(body, "credential") {
		t.Fatalf("credentials leaked: %s", body)
	}
	var zones struct {
		Zones []map[string]any `json:"zones"`
	}
	if code := owner.do("GET", "/api/v1/dns/providers/"+pid+"/zones", nil, &zones); code != 200 || len(zones.Zones) != 1 {
		t.Fatalf("zones: %d %v", code, zones)
	}
	zid := zones.Zones[0]["id"].(string)
	var recs struct {
		Records []map[string]any `json:"records"`
	}
	owner.do("GET", "/api/v1/dns/providers/"+pid+"/zones/"+zid+"/records", nil, &recs)
	if len(recs.Records) != 2 {
		t.Fatalf("records %v", recs)
	}

	change := map[string]any{"provider_id": pid, "zone": zid, "action": "upsert", "record": map[string]any{"type": "A", "name": "www.example.com", "content": "198.51.100.1", "ttl": 1, "proxied": true}}
	// applying without a reviewed plan is refused
	if code := owner.do("POST", "/api/v1/dns/changes", map[string]any{"change": change}, nil); code != 422 {
		t.Fatalf("apply without plan: %d", code)
	}
	var plan struct {
		Plan map[string]any `json:"plan"`
	}
	if code := owner.do("POST", "/api/v1/dns/changes/plan", change, &plan); code != 200 || plan.Plan["action"] != "update" {
		t.Fatalf("plan: %d %v", code, plan)
	}
	if e.dns.Writes()["cloudflare PUT"] != 0 {
		t.Fatal("plan wrote to the provider")
	}
	// the record changes behind our back: the confirmed plan is stale
	e.dns.AddRecord("cloudflare", "example.com", "A", "other", "192.0.2.50", 300, false)
	var applied struct {
		Change model.DNSChange `json:"change"`
	}
	if code := owner.do("POST", "/api/v1/dns/changes", map[string]any{"change": change, "plan": plan.Plan}, &applied); code != 200 || applied.Change.Status != "applied" || !applied.Change.Verified {
		t.Fatalf("apply: %d %+v", code, applied.Change)
	}
	if code := owner.do("POST", "/api/v1/dns/changes", map[string]any{"change": change, "plan": plan.Plan}, &errResp); code != 409 || errResp.Error.Code != "dns_conflict" {
		t.Fatalf("stale plan: %d %+v", code, errResp.Error)
	}
	// revert: dry run, then apply
	var rplan struct {
		Plan map[string]any `json:"plan"`
	}
	owner.do("POST", "/api/v1/dns/changes/"+applied.Change.ID+"/revert", map[string]any{"dry_run": true}, &rplan)
	if rplan.Plan["action"] != "update" {
		t.Fatalf("revert plan %v", rplan)
	}
	var rv struct {
		Change model.DNSChange `json:"change"`
	}
	if code := owner.do("POST", "/api/v1/dns/changes/"+applied.Change.ID+"/revert", map[string]any{"plan": rplan.Plan}, &rv); code != 200 || rv.Change.After.Content != "192.0.2.10" {
		t.Fatalf("revert: %d %+v", code, rv.Change)
	}
	// history and snapshots
	var hist struct {
		Changes []model.DNSChange `json:"changes"`
	}
	owner.do("GET", "/api/v1/dns/changes?provider_id="+pid, nil, &hist)
	if len(hist.Changes) != 2 {
		t.Fatalf("history %d", len(hist.Changes))
	}
	var sns struct {
		Snapshots []model.DNSSnapshot `json:"snapshots"`
	}
	owner.do("GET", "/api/v1/dns/snapshots?provider_id="+pid, nil, &sns)
	if len(sns.Snapshots) != 2 {
		t.Fatalf("snapshots %d", len(sns.Snapshots))
	}
	// restore the first snapshot including deletions: a record added since is removed
	e.dns.AddRecord("cloudflare", "example.com", "TXT", "added-later", "x", 300, false)
	first := sns.Snapshots[len(sns.Snapshots)-1]
	var rp struct {
		Plans []map[string]any `json:"plans"`
	}
	owner.do("POST", "/api/v1/dns/snapshots/"+first.ID+"/restore", map[string]any{"dry_run": true, "include_deletes": true}, &rp)
	if len(rp.Plans) != 1 || rp.Plans[0]["action"] != "delete" {
		t.Fatalf("restore plan %v", rp.Plans)
	}
	var res struct {
		Changes []model.DNSChange `json:"changes"`
	}
	if code := owner.do("POST", "/api/v1/dns/snapshots/"+first.ID+"/restore", map[string]any{"include_deletes": true, "plans": rp.Plans}, &res); code != 200 || len(res.Changes) != 1 || res.Changes[0].Status != "applied" {
		t.Fatalf("restore: %d %+v", code, res)
	}
	// a workflow DNS step is validated against the integration
	wf := map[string]any{"name": "DR", "trigger": map[string]any{"type": "manual"}, "steps": []map[string]any{
		{"type": "dns", "provider_id": pid, "zone_name": "example.com", "dns_action": "upsert", "record_type": "A", "record_name": "www", "content": "not-an-ip"},
		{"type": "dns", "provider_id": "nope", "zone_name": "example.com", "dns_action": "upsert", "record_type": "MX", "record_name": "www", "content": "x"}}}
	if code := owner.do("POST", "/api/v1/workflows", wf, &errResp); code != 422 || errResp.Error.Fields["steps.0.content"] == "" || errResp.Error.Fields["steps.1.provider_id"] == "" || errResp.Error.Fields["steps.1.record_type"] == "" {
		t.Fatalf("dns step validation: %d %v", code, errResp.Error.Fields)
	}
	// an integration used by a workflow cannot be deleted
	wf["steps"] = []map[string]any{{"type": "dns", "provider_id": pid, "zone_name": "example.com", "dns_action": "upsert", "record_type": "A", "record_name": "www", "content": "{{dr_ip}}"}}
	if code := owner.do("POST", "/api/v1/workflows", wf, nil); code != 201 {
		t.Fatalf("workflow: %d", code)
	}
	if code := owner.do("DELETE", "/api/v1/dns/providers/"+pid, nil, &errResp); code != 409 || errResp.Error.Code != "dns_provider_in_use" {
		t.Fatalf("delete in use: %d", code)
	}
}
