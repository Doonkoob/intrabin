package dns

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/dns/dnsmock"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

type svcEnv struct {
	m   *dnsmock.Server
	s   *Service
	st  *storage.Store
	ids map[string]string // provider kind -> integration ID
}

// mock provider names differ from Intrabin type names
var mockName = map[string]string{"cloudflare": "cloudflare", "arvancloud": "arvan"}

func newSvc(t *testing.T) *svcEnv {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := dnsmock.New("cf-token", "arvan-key")
	ts := httptest.NewServer(m)
	t.Cleanup(ts.Close)
	box, _ := security.NewSecretBox(make([]byte, 32))
	s := &Service{Store: st, CP: controlplane.New(st), Box: box, VerifyDelay: 10 * time.Millisecond,
		APIBase: map[string]string{"cloudflare": ts.URL + "/cloudflare/client/v4", "arvancloud": ts.URL + "/arvan/cdn/4.0"}}
	v := &svcEnv{m: m, s: s, st: st, ids: map[string]string{}}
	for kind, mk := range mockName {
		m.AddZone(mk, "example.com")
		m.AddRecord(mk, "example.com", "A", "www", "192.0.2.10", 300, false)
		m.AddRecord(mk, "example.com", "TXT", "keep", "hello", 300, false)
		p := &model.DNSProvider{ID: "p-" + kind, Name: kind, Type: kind}
		c := Credentials{APIToken: "cf-token"}
		if kind == "arvancloud" {
			c = Credentials{APIKey: "arvan-key"}
		}
		if p.CredentialBlob, err = SealCredentials(box, p.ID, c); err != nil {
			t.Fatal(err)
		}
		if err := s.CP.CreateDNSProvider(ctx, p); err != nil {
			t.Fatal(err)
		}
		v.ids[kind] = p.ID
	}
	return v
}

func upsertWWW(pid, ip string) ChangeRequest {
	return ChangeRequest{ProviderID: pid, Zone: "example.com", Action: "upsert", Record: RecordInput{Type: "A", Name: "www.example.com", Content: ip, TTL: 300}}
}

func TestServicePlanApplyRevert(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	for kind, pid := range v.ids {
		t.Run(kind, func(t *testing.T) {
			p, err := v.s.PlanChange(ctx, upsertWWW(pid, "198.51.100.7"))
			if err != nil || p.Action != "update" || p.Before.Content != "192.0.2.10" || p.NoChange {
				t.Fatalf("plan %+v %v", p, err)
			}
			if v.m.Writes()[mockName[kind]+" PUT"] != 0 {
				t.Fatal("plan must not write")
			}
			ch, err := v.s.Apply(ctx, upsertWWW(pid, "198.51.100.7"), p, "alice")
			if err != nil || ch.Status != model.DNSChangeApplied || !ch.Verified || ch.SnapshotID == "" {
				t.Fatalf("apply %+v %v", ch, err)
			}
			sn, err := v.st.GetDNSSnapshot(ctx, ch.SnapshotID)
			if err != nil || sn.RecordCount != 2 {
				t.Fatalf("snapshot %+v %v", sn, err)
			}
			// applying the same plan again is a conflict (the record changed)
			if _, err := v.s.Apply(ctx, upsertWWW(pid, "198.51.100.8"), p, "alice"); !errors.Is(err, ErrConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
			if _, err := v.s.Apply(ctx, upsertWWW(pid, "198.51.100.7"), nil, "alice"); !errors.Is(err, ErrNoChange) {
				t.Fatalf("want no change, got %v", err)
			}
			// revert
			rr, err := RevertRequest(ch)
			if err != nil {
				t.Fatal(err)
			}
			rv, err := v.s.Apply(ctx, rr, nil, "alice")
			if err != nil || rv.Status != model.DNSChangeApplied {
				t.Fatalf("revert %+v %v", rv, err)
			}
			_, rs, _ := v.s.Records(ctx, pid, "example.com")
			if r := FindByName(rs, "A", "www.example.com"); len(r) != 1 || r[0].Content != "192.0.2.10" {
				t.Fatalf("after revert %+v", r)
			}
			// create, then delete via revert
			cr, err := v.s.Apply(ctx, ChangeRequest{ProviderID: pid, Zone: "example.com", Action: "upsert", Record: RecordInput{Type: "CNAME", Name: "app.example.com", Content: "www.example.com", TTL: 300}}, nil, "alice")
			if err != nil || cr.Action != "create" || cr.Status != model.DNSChangeApplied || cr.RecordID == "" {
				t.Fatalf("create %+v %v", cr, err)
			}
			rr, _ = RevertRequest(cr)
			if d, err := v.s.Apply(ctx, rr, nil, "alice"); err != nil || d.Action != "delete" || d.Status != model.DNSChangeApplied {
				t.Fatalf("delete %+v %v", d, err)
			}
		})
	}
}

func TestServiceUncertainOutcomes(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	pid := v.ids["cloudflare"]
	// 1. definite rejection: failed, nothing written
	v.m.Fail("cloudflare", "PUT", 400, false)
	ch, err := v.s.Apply(ctx, upsertWWW(pid, "198.51.100.1"), nil, "a")
	if err != nil || ch.Status != model.DNSChangeFailed {
		t.Fatalf("400: %+v %v", ch, err)
	}
	// 2. 502 after the write was applied: unknown -> read back -> reconciled_applied
	v.m.Fail("cloudflare", "PUT", 502, true)
	ch, err = v.s.Apply(ctx, upsertWWW(pid, "198.51.100.2"), nil, "a")
	if err != nil || ch.Status != model.DNSChangeReconciledApplied || !ch.Verified {
		t.Fatalf("502 applied: %+v %v", ch, err)
	}
	// 3. 503 without applying: unknown -> read back -> reconciled_not_applied; never retried
	before := v.m.Writes()["cloudflare PUT"]
	v.m.Fail("cloudflare", "PUT", 503, false)
	ch, err = v.s.Apply(ctx, upsertWWW(pid, "198.51.100.3"), nil, "a")
	if err != nil || ch.Status != model.DNSChangeReconciledNotApplied {
		t.Fatalf("503: %+v %v", ch, err)
	}
	if v.m.Writes()["cloudflare PUT"] != before {
		t.Fatal("a write was retried")
	}
	// 4. response lost (timeout) after apply
	old := RequestTimeout
	RequestTimeout = 300 * time.Millisecond
	defer func() { RequestTimeout = old }()
	v.m.Hang("cloudflare", "PUT", time.Second)
	ch, err = v.s.Apply(ctx, upsertWWW(pid, "198.51.100.4"), nil, "a")
	if err != nil || ch.Status != model.DNSChangeReconciledApplied {
		t.Fatalf("timeout: %+v %v", ch, err)
	}
	// 5. fence lost before dispatch: not dispatched
	v.s.Fence = func(context.Context) (uint64, string, error) { return 0, "", controlplane.ErrNotLeader }
	if _, err := v.s.Apply(ctx, upsertWWW(pid, "198.51.100.5"), nil, "a"); !errors.Is(err, controlplane.ErrNotLeader) {
		t.Fatalf("fence: %v", err)
	}
	v.s.Fence = nil
	// idempotent by change ID
	req := upsertWWW(pid, "198.51.100.6")
	req.ExpectID = "fixed-id"
	a, _ := v.s.Apply(ctx, req, nil, "a")
	n := v.m.Writes()["cloudflare PUT"]
	b, _ := v.s.Apply(ctx, req, nil, "a")
	if a.ID != b.ID || v.m.Writes()["cloudflare PUT"] != n {
		t.Fatal("re-applying the same change ID wrote again")
	}
}

func TestServiceReconcilePending(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	pid := v.ids["arvancloud"]
	_, rs, _ := v.s.Records(ctx, pid, "example.com")
	www := FindByName(rs, "A", "www.example.com")[0]
	mk := func(id, ip string) *model.DNSChange {
		ch := &model.DNSChange{ID: id, ProviderID: pid, ZoneID: "example.com", ZoneName: "example.com", Action: "update", RecordID: www.ID,
			RecordType: "A", RecordName: www.Name, Before: toModel(&www), After: &model.DNSRecord{Type: "A", Name: www.Name, Content: ip, TTL: 300},
			Status: model.DNSChangeDispatching, CreatedAt: time.Now().Add(-time.Minute)}
		if err := v.s.CP.SaveDNSChange(ctx, ch); err != nil {
			t.Fatal(err)
		}
		return ch
	}
	mk("c1", "198.51.100.9") // never sent
	if _, err := v.s.Apply(ctx, upsertWWW(pid, "198.51.100.10"), nil, "a"); err != nil {
		t.Fatal(err)
	}
	mk("c2", "198.51.100.10") // the zone now has this value
	v.s.ReconcilePending(ctx, 30*time.Second)
	c1, _ := v.st.GetDNSChange(ctx, "c1")
	c2, _ := v.st.GetDNSChange(ctx, "c2")
	if c1.Status != model.DNSChangeUnknown { // record no longer matches "before" nor "after"
		t.Fatalf("c1 %s", c1.Status)
	}
	if c2.Status != model.DNSChangeReconciledApplied {
		t.Fatalf("c2 %s", c2.Status)
	}
}

func TestServiceRestore(t *testing.T) {
	v := newSvc(t)
	ctx := context.Background()
	pid := v.ids["cloudflare"]
	sn, err := v.s.Snapshot(ctx, pid, "example.com", "test", "a")
	if err != nil {
		t.Fatal(err)
	}
	v.s.Apply(ctx, upsertWWW(pid, "198.51.100.20"), nil, "a")
	_, rs, _ := v.s.Records(ctx, pid, "example.com")
	keep := FindByName(rs, "TXT", "keep.example.com")[0]
	v.s.Apply(ctx, ChangeRequest{ProviderID: pid, Zone: "example.com", Action: "delete", RecordID: keep.ID}, nil, "a")
	v.s.Apply(ctx, ChangeRequest{ProviderID: pid, Zone: "example.com", Action: "upsert", Record: RecordInput{Type: "TXT", Name: "extra.example.com", Content: "x", TTL: 300}}, nil, "a")

	reqs, plans, err := v.s.RestorePlan(ctx, sn, false)
	if err != nil || len(reqs) != 2 {
		t.Fatalf("plan %d %v %+v", len(reqs), err, plans)
	}
	reqs2, _, _ := v.s.RestorePlan(ctx, sn, true)
	if len(reqs2) != 3 {
		t.Fatalf("with deletes: %d", len(reqs2))
	}
	for _, r := range reqs2 {
		if _, err := v.s.Apply(ctx, r, nil, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if again, _, _ := v.s.RestorePlan(ctx, sn, true); len(again) != 0 {
		t.Fatalf("not converged: %+v", again)
	}
}
