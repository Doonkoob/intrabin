package automation

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/dns/dnsmock"
	"github.com/intrabin/intrabin/internal/model"
)

func (v *env) withDNS() (*dnsmock.Server, string) {
	v.t.Helper()
	m := dnsmock.New("cf-token", "arvan-key")
	ts := httptest.NewServer(m)
	v.t.Cleanup(ts.Close)
	m.AddZone("cloudflare", "example.com")
	m.AddRecord("cloudflare", "example.com", "A", "app", "192.0.2.1", 60, false)
	v.e.DNS = &dns.Service{Store: v.st, CP: v.cp, Box: v.e.Box, Fence: dns.Fencer(v.e.Fence), VerifyDelay: 10 * time.Millisecond,
		APIBase: map[string]string{"cloudflare": ts.URL + "/cloudflare/client/v4"}}
	p := &model.DNSProvider{ID: uuid.NewString(), Name: "cf", Type: "cloudflare"}
	var err error
	if p.CredentialBlob, err = dns.SealCredentials(v.e.Box, p.ID, dns.Credentials{APIToken: "cf-token"}); err != nil {
		v.t.Fatal(err)
	}
	if err := v.cp.CreateDNSProvider(context.Background(), p); err != nil {
		v.t.Fatal(err)
	}
	return m, p.ID
}

func failoverStep(pid string) model.WorkflowStep {
	return model.WorkflowStep{Name: "point app to DR", Type: "dns", ProviderID: pid, ZoneName: "example.com", DNSAction: "upsert",
		RecordType: "A", RecordName: "app.example.com", Content: "{{dr_ip}}", TTL: 60}
}

func appIP(m *dnsmock.Server) string {
	for _, r := range m.Records("cloudflare", "example.com") {
		if r["name"] == "app.example.com" {
			return r["content"].(string)
		}
	}
	return ""
}

func TestDNSStep(t *testing.T) {
	v := newEnv(t)
	m, pid := v.withDNS()
	wf := v.workflow([]model.WorkflowStep{failoverStep(pid), failoverStep(pid)}, nil, model.WorkflowTrigger{})
	run, err := v.e.TriggerManual(context.Background(), wf, "tester", map[string]string{"dr_ip": "198.51.100.50"})
	if err != nil {
		t.Fatal(err)
	}
	r := v.settle(run.ID, model.RunSucceeded, model.RunFailed)
	acts := v.actions(run.ID)
	if r.Status != model.RunSucceeded || len(acts) != 2 {
		t.Fatalf("run %s %s", r.Status, r.Error)
	}
	if appIP(m) != "198.51.100.50" || m.Writes()["cloudflare PUT"] != 1 {
		t.Fatalf("ip %s writes %v", appIP(m), m.Writes())
	}
	if !strings.Contains(acts[0].Output, "192.0.2.1 → 198.51.100.50") || !strings.Contains(acts[0].Output, "Verified") {
		t.Fatalf("output %q", acts[0].Output)
	}
	if !strings.Contains(acts[1].Output, "already had") {
		t.Fatalf("second step %q", acts[1].Output)
	}
	ch, err := v.st.GetDNSChange(context.Background(), acts[0].ID)
	if err != nil || ch.SnapshotID == "" || ch.FenceTerm != 7 {
		t.Fatalf("change %+v %v", ch, err)
	}
}

func TestDNSStepUncertainOutcome(t *testing.T) {
	v := newEnv(t)
	m, pid := v.withDNS()
	wf := v.workflow([]model.WorkflowStep{failoverStep(pid)}, nil, model.WorkflowTrigger{})
	// 502 after applying: resolved by read-back, not retried
	m.Fail("cloudflare", "PUT", 502, true)
	run, _ := v.e.TriggerManual(context.Background(), wf, "tester", map[string]string{"dr_ip": "198.51.100.60"})
	r := v.settle(run.ID, model.RunSucceeded, model.RunFailed, model.RunUnknownOutcome)
	if r.Status != model.RunSucceeded || appIP(m) != "198.51.100.60" || m.Writes()["cloudflare PUT"] != 1 {
		t.Fatalf("run %s %s writes %v", r.Status, r.Error, m.Writes())
	}
}

// A leader that dies after committing the dispatch record: the next leader reads the zone back.
func TestDNSStepReconciledAfterFailover(t *testing.T) {
	for _, applied := range []bool{true, false} {
		v := newEnv(t)
		m, pid := v.withDNS()
		wf := v.workflow([]model.WorkflowStep{failoverStep(pid)}, nil, model.WorkflowTrigger{})
		ctx := context.Background()
		run, _ := v.e.TriggerManual(ctx, wf, "tester", map[string]string{"dr_ip": "198.51.100.70"})
		v.e.Active = func() bool { return false }
		// Simulate the old leader: dispatch record + DNS change record committed, then the write
		// either reached the provider or not, and the node vanished.
		r, _ := v.st.GetRun(ctx, run.ID)
		a := &model.WorkflowAction{ID: uuid.NewString(), RunID: r.ID, StepIndex: 0, StepType: "dns", Attempt: 1, Status: model.ActionDispatching, StartedAt: time.Now().UTC()}
		r.Status = model.RunRunning
		if err := v.cp.ProgressRun(ctx, r, a); err != nil {
			t.Fatal(err)
		}
		_, rs, _ := v.e.DNS.Records(ctx, pid, "example.com")
		cur := dns.FindByName(rs, "A", "app.example.com")[0]
		ch := &model.DNSChange{ID: a.ID, ProviderID: pid, ZoneID: cur.ID, ZoneName: "example.com", Action: "update", RecordID: cur.ID, RecordType: "A", RecordName: cur.Name,
			Before: &model.DNSRecord{ID: cur.ID, Type: "A", Name: cur.Name, Content: cur.Content, TTL: cur.TTL},
			After:  &model.DNSRecord{Type: "A", Name: cur.Name, Content: "198.51.100.70", TTL: 60}, Status: model.DNSChangeDispatching, CreatedAt: time.Now().UTC()}
		zs, _ := v.e.DNS.Zones(ctx, pid, false)
		ch.ZoneID = zs[0].ID
		if err := v.cp.SaveDNSChange(ctx, ch); err != nil {
			t.Fatal(err)
		}
		if applied {
			m.AddRecord("cloudflare", "example.com", "A", "tmp", "192.0.2.99", 60, false) // unrelated noise
			c, _ := v.e.DNS.Client(mustProvider(t, v, pid))
			if _, err := c.Update(ctx, zs[0], &cur, dns.RecordInput{Type: "A", Name: cur.Name, Content: "198.51.100.70", TTL: 60}); err != nil {
				t.Fatal(err)
			}
		}
		writes := m.Writes()["cloudflare PUT"]
		v.e.Active = func() bool { return true }
		want := model.RunSucceeded
		if !applied {
			want = model.RunFailed
		}
		got := v.settle(run.ID, model.RunSucceeded, model.RunFailed, model.RunUnknownOutcome)
		acts := v.actions(run.ID)
		if got.Status != want || acts[0].ReconciledBy != "provider" || m.Writes()["cloudflare PUT"] != writes {
			t.Fatalf("applied=%v: run %s (%s), action %s by %q, writes %d→%d", applied, got.Status, got.Error, acts[0].Status, acts[0].ReconciledBy, writes, m.Writes()["cloudflare PUT"])
		}
	}
}

func mustProvider(t *testing.T, v *env, id string) *model.DNSProvider {
	p, err := v.st.GetDNSProvider(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
