package dr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/automation"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/dns"
	"github.com/intrabin/intrabin/internal/dns/dnsmock"
	"github.com/intrabin/intrabin/internal/incidents"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

type env struct {
	t      *testing.T
	st     *storage.Store
	cp     *controlplane.CP
	m      *Manager
	e      *automation.Engine
	mock   *dnsmock.Server
	active atomic.Bool
	pid    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cp := controlplane.New(st)
	box, _ := security.NewSecretBox(make([]byte, 32))
	mock := dnsmock.New("tok", "key")
	ts := httptest.NewServer(mock)
	t.Cleanup(ts.Close)
	mock.AddZone("cloudflare", "example.com")
	mock.AddRecord("cloudflare", "example.com", "A", "app", "192.0.2.1", 60, false)
	v := &env{t: t, st: st, cp: cp, mock: mock}
	v.active.Store(true)
	fence := func(context.Context) (uint64, string, error) {
		if !v.active.Load() {
			return 0, "", controlplane.ErrNotLeader
		}
		return 3, "node-a", nil
	}
	svc := &dns.Service{Store: st, CP: cp, Box: box, Fence: fence, VerifyDelay: 10 * time.Millisecond,
		APIBase: map[string]string{"cloudflare": ts.URL + "/cloudflare/client/v4"}}
	inc := incidents.NewManager(controlplane.IncidentStore{Store: st, CP: cp}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	v.e = &automation.Engine{Store: st, CP: cp, Incidents: inc, Box: box, DNS: svc, Fence: fence, Active: v.active.Load}
	v.m = &Manager{Store: st, CP: cp, Engine: v.e, DNS: svc, Fence: fence, Active: v.active.Load}
	p := &model.DNSProvider{ID: "prov", Name: "cf", Type: "cloudflare"}
	if p.CredentialBlob, err = dns.SealCredentials(box, p.ID, dns.Credentials{APIToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if err := cp.CreateDNSProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	v.pid = p.ID
	now := time.Now().UTC()
	for _, id := range []string{"primary", "secondary"} {
		mon := &model.Monitor{ID: id, Name: id, Type: model.MonitorHTTP, Target: "https://" + id + ".test", IntervalSeconds: 30, TimeoutSeconds: 5, Enabled: true,
			Policy: model.DefaultPolicy(), Version: 1, CreatedAt: now, UpdatedAt: now}
		if err := cp.CreateMonitor(ctx, mon); err != nil {
			t.Fatal(err)
		}
		v.status(id, model.StatusUp)
	}
	return v
}

func (v *env) status(id string, s model.MonitorStatus) {
	v.t.Helper()
	if err := v.cp.SaveMonitorState(context.Background(), &model.MonitorState{MonitorID: id, Status: s, StatusSince: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		v.t.Fatal(err)
	}
}

func (v *env) incident(id string, s model.IncidentStatus) {
	v.t.Helper()
	now := time.Now().UTC()
	in := &model.Incident{ID: id, MonitorID: "primary", Status: s, Title: "primary is down", StartedAt: now, DetectedAt: now, UpdatedAt: now, ConfirmedAt: &now}
	if err := v.cp.SaveIncident(context.Background(), in); err != nil {
		v.t.Fatal(err)
	}
}

func (v *env) plan(auto bool) *model.DRPlan {
	v.t.Helper()
	now := time.Now().UTC()
	w := &model.Workflow{ID: "plan-1", Name: "App DR", Trigger: model.WorkflowTrigger{Type: "manual"}, SecretKeys: []string{}, Version: 1, CreatedAt: now, UpdatedAt: now}
	p := &model.DRPlan{ID: w.ID, State: model.DRPrimary, LastTransitionAt: now, UpdatedAt: now, Config: model.DRConfig{
		MonitorIDs: []string{"primary"}, SecondaryMonitorID: "secondary", Automatic: auto, ProviderID: v.pid, ZoneName: "example.com",
		RecordType: "A", RecordName: "app.example.com", PrimaryValue: "192.0.2.1", SecondaryValue: "198.51.100.1", TTL: 60}}
	if err := v.cp.SaveDRPlan(context.Background(), w, p, true); err != nil {
		v.t.Fatal(err)
	}
	return p
}

// drive ticks the engine and the manager until the plan reaches want.
func (v *env) drive(want string) *model.DRPlan {
	v.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		v.e.Tick(context.Background())
		time.Sleep(30 * time.Millisecond)
		v.m.Tick(context.Background())
		p, err := v.st.GetDRPlan(context.Background(), "plan-1")
		if err != nil {
			v.t.Fatal(err)
		}
		if p.State == want {
			return p
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("plan stuck in %s (%s), want %s", p.State, p.StateNote, want)
		}
	}
}

func (v *env) appIP() string {
	for _, r := range v.mock.Records("cloudflare", "example.com") {
		if r["name"] == "app.example.com" {
			return r["content"].(string)
		}
	}
	return ""
}

func (v *env) runs() []*model.WorkflowRun {
	rs, _ := v.st.ListRuns(context.Background(), storage.RunFilter{WorkflowID: "plan-1"})
	return rs
}

func check(cs []Check, id string) Check {
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	return Check{}
}

func TestManualFailoverAndFailback(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	v.plan(false)

	pre, err := v.m.Preview(ctx, "plan-1", Failover, false)
	if err != nil || pre.DNS == nil || pre.DNS.Before.Content != "192.0.2.1" || !check(pre.Checks, "dns").OK {
		t.Fatalf("preview %+v %v", pre, err)
	}
	if v.mock.Writes()["cloudflare PUT"] != 0 {
		t.Fatal("preview wrote")
	}
	// The secondary must be healthy; forcing needs a note.
	v.status("secondary", model.StatusDown)
	if _, err := v.m.Start(ctx, "plan-1", Failover, StartOptions{Actor: "alice"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("secondary down: %v", err)
	}
	if _, err := v.m.Start(ctx, "plan-1", Failover, StartOptions{Actor: "alice", Force: true}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("force without note: %v", err)
	}
	v.status("secondary", model.StatusUp)
	res, err := v.m.Start(ctx, "plan-1", Failover, StartOptions{Actor: "alice", Note: "planned maintenance"})
	if err != nil || res.Run == nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := v.m.Start(ctx, "plan-1", Failover, StartOptions{Actor: "bob"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("second failover while failing over: %v", err)
	}
	p := v.drive(model.DRFailedOver)
	if v.appIP() != "198.51.100.1" || p.LastFailoverAt == nil || p.Direction != Failover {
		t.Fatalf("after failover ip=%s plan=%+v", v.appIP(), p)
	}
	// Failback needs the primary UP (overridable with a note).
	v.status("primary", model.StatusDown)
	if _, err := v.m.Start(ctx, "plan-1", Failback, StartOptions{Actor: "alice"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("failback with primary down: %v", err)
	}
	v.status("primary", model.StatusUp)
	if _, err := v.m.Start(ctx, "plan-1", Failback, StartOptions{Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	v.drive(model.DRPrimary)
	if v.appIP() != "192.0.2.1" || v.mock.Writes()["cloudflare PUT"] != 2 || len(v.runs()) != 2 {
		t.Fatalf("after failback ip=%s writes=%v runs=%d", v.appIP(), v.mock.Writes(), len(v.runs()))
	}
}

func TestAutomaticFailoverIsOptInGatedAndOnce(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	v.plan(false)
	v.status("primary", model.StatusDown)
	v.incident("inc-1", model.IncidentConfirmed)
	v.m.Tick(ctx)
	if len(v.runs()) != 0 {
		t.Fatal("a manual plan failed over automatically")
	}
	// Opt in.
	p, _ := v.st.GetDRPlan(ctx, "plan-1")
	p.Config.Automatic = true
	w, _ := v.st.GetWorkflow(ctx, "plan-1")
	if err := v.cp.SaveDRPlan(ctx, w, p, false); err != nil {
		t.Fatal(err)
	}
	// Not the active controller: nothing happens.
	v.active.Store(false)
	v.m.Tick(ctx)
	v.active.Store(true)
	// Secondary unhealthy: waits, with a reason.
	v.status("secondary", model.StatusDown)
	v.m.Tick(ctx)
	if len(v.runs()) != 0 || v.m.WaitingReason("plan-1") == "" {
		t.Fatalf("failed over to an unhealthy secondary (runs %d, reason %q)", len(v.runs()), v.m.WaitingReason("plan-1"))
	}
	v.status("secondary", model.StatusUp)
	var wg sync.WaitGroup
	for range 4 { // concurrent evaluations start exactly one switch
		wg.Add(1)
		go func() { defer wg.Done(); v.m.Tick(ctx) }()
	}
	wg.Wait()
	if n := len(v.runs()); n != 1 {
		t.Fatalf("want one failover run, got %d", n)
	}
	v.drive(model.DRFailedOver)
	in, _ := v.st.GetIncident(ctx, "inc-1")
	if v.appIP() != "198.51.100.1" || in.Status != model.IncidentFailedOver || v.runs()[0].Context["incident_id"] != "inc-1" {
		t.Fatalf("ip=%s incident=%s", v.appIP(), in.Status)
	}
	// Automatic failback never happens; a manual failback resolves the incident.
	v.status("primary", model.StatusUp)
	v.m.Tick(ctx)
	if p, _ := v.st.GetDRPlan(ctx, "plan-1"); p.State != model.DRFailedOver {
		t.Fatal("failed back automatically")
	}
	if _, err := v.m.Start(ctx, "plan-1", Failback, StartOptions{Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	v.drive(model.DRPrimary)
	if in, _ := v.st.GetIncident(ctx, "inc-1"); in.Status != model.IncidentResolved {
		t.Fatalf("incident %s", in.Status)
	}
	// A new incident within the cooldown does not fail over again.
	v.status("primary", model.StatusDown)
	v.incident("inc-2", model.IncidentConfirmed)
	v.m.Tick(ctx)
	if len(v.runs()) != 2 || v.m.WaitingReason("plan-1") == "" {
		t.Fatalf("cooldown ignored: runs %d", len(v.runs()))
	}
}

func TestFailedSwitchNeedsAttention(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	v.plan(false)
	v.mock.Fail("cloudflare", "PUT", 400, false)
	v.mock.Fail("cloudflare", "PUT", 400, false) // the DNS step retries once after a definite failure
	if _, err := v.m.Start(ctx, "plan-1", Failover, StartOptions{Actor: "alice"}); err != nil {
		t.Fatal(err)
	}
	// Shorten the retry timer.
	go func() {
		for range 400 {
			for _, r := range v.runs() {
				if r.Status == model.RunWaiting {
					past := time.Now().Add(-time.Second)
					r.NextAt = &past
					_ = v.st.ProgressRun(ctx, r, nil)
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	p := v.drive(model.DRAttention)
	if v.appIP() != "192.0.2.1" || p.StateNote == "" {
		t.Fatalf("ip %s note %q", v.appIP(), p.StateNote)
	}
	if _, err := v.m.Start(ctx, "plan-1", Failover, StartOptions{Actor: "alice"}); !errors.Is(err, ErrBlocked) {
		t.Fatal("started from attention without force")
	}
	if _, err := v.m.Resolve(ctx, "plan-1", model.DRPrimary, "alice", "checked: still on primary"); err != nil {
		t.Fatal(err)
	}
	if p, _ := v.st.GetDRPlan(ctx, "plan-1"); p.State != model.DRPrimary {
		t.Fatal(p.State)
	}
}
