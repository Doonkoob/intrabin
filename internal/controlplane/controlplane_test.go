package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

func open(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(context.Background(), t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// recorder captures commands so they can be replayed on another replica.
type recorder struct {
	x    *Executor
	cmds []*Command
}

func (r *recorder) Apply(ctx context.Context, c *Command) error {
	r.cmds = append(r.cmds, c)
	return r.x.Execute(ctx, c)
}

func TestReplicasConverge(t *testing.T) {
	ctx := context.Background()
	a, b := open(t), open(t)
	cp := New(a)
	rec := &recorder{x: cp.X}
	cp.SetApplier(rec)

	now := time.Now().UTC()
	u := &model.User{ID: "u1", Username: "owner", PasswordHash: "h", Role: model.RoleOwner, Language: "en", CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now}
	must(t, cp.CreateFirstUser(ctx, u))
	if err := cp.CreateFirstUser(ctx, &model.User{ID: "u2", Username: "x", PasswordHash: "h", Role: model.RoleOwner, CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("second first-user must conflict, got %v", err)
	}
	must(t, cp.SetSetting(ctx, "app.instance_name", "Prod"))
	must(t, cp.SetSetting(ctx, "local.only", "here")) // node-local: not a command
	must(t, cp.CreateGroup(ctx, &model.MonitorGroup{ID: "g1", Name: "web", CreatedAt: now, UpdatedAt: now}))
	m := &model.Monitor{ID: "m1", Name: "site", Type: model.MonitorHTTP, Target: "https://example.test", IntervalSeconds: 30, TimeoutSeconds: 5,
		Enabled: true, Policy: model.DefaultPolicy(), Version: 1, CreatedAt: now, UpdatedAt: now, SecretBlob: []byte{1, 2, 3}}
	must(t, cp.CreateMonitor(ctx, m))
	lat := 10.0
	rr := &model.RoundResult{MonitorID: "m1", RoundStart: now.Truncate(30 * time.Second), Verdict: model.VerdictOK, ValidProbes: 1, AvgLatencyMs: &lat, EvaluatedAt: now}
	stt := &model.MonitorState{MonitorID: "m1", Status: model.StatusUp, ConsecutiveOK: 1, LastRound: rr.RoundStart.UnixMilli(), StatusSince: now, UpdatedAt: now}
	must(t, cp.RecordRound(ctx, rr, stt, 30))
	must(t, cp.RecordRound(ctx, rr, stt, 30)) // idempotent
	must(t, cp.UpdateUser(ctx, &model.User{ID: "u1", Username: "owner", DisplayName: "Owner", Role: model.RoleOwner, Language: "fa"}))
	must(t, cp.TouchLogin(ctx, "u1"))
	must(t, cp.InsertAudit(ctx, &model.AuditEntry{ID: "a1", At: now, ActorName: "owner", Action: "x"}))

	// Replay on replica b (with JSON round trip, as through the Raft log).
	xb := &Executor{Store: b}
	for _, c := range rec.cmds {
		raw, _ := Encode(c)
		dc, err := Decode(raw)
		must(t, err)
		_ = xb.Execute(ctx, dc) // errors (e.g. conflicts) are deterministic outcomes
	}
	if v, _ := b.GetSetting(ctx, "local.only"); v != "" {
		t.Fatal("local setting must not replicate")
	}
	da, db := dumpJSON(t, a), dumpJSON(t, b)
	if da != db {
		t.Fatalf("replicas diverged:\nA=%s\nB=%s", da, db)
	}
	u2, err := b.Uptime(ctx, "m1", now.Add(-2*time.Hour))
	must(t, err)
	if u2.OK != 1 || u2.Expected != 1 {
		t.Fatalf("hourly rollup must count the round once: %+v", u2)
	}
}

func dumpJSON(t *testing.T, st *storage.Store) string {
	ctx := storage.WithNow(context.Background(), time.Unix(1700000000, 0))
	raw, err := st.DumpReplicated(ctx)
	must(t, err)
	// Restore into a fresh store and compare logical content via JSON of the decoded dump.
	fresh := open(t)
	must(t, fresh.RestoreReplicated(context.Background(), raw))
	u, _ := fresh.ListUsers(context.Background())
	ms, _ := fresh.ListMonitors(context.Background())
	s, _ := fresh.AllSettings(context.Background(), "")
	out, _ := json.Marshal(map[string]any{"users": u, "monitors": ms, "settings": s})
	return string(out)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestErrorCodesRoundTrip(t *testing.T) {
	for _, e := range []error{storage.ErrNotFound, storage.ErrConflict, storage.ErrTokenUsed, ErrNotLeader, ErrNoQuorum} {
		if got := CodeError(ErrorCode(e)); !errors.Is(got, e) {
			t.Fatalf("%v did not round trip (got %v)", e, got)
		}
	}
	if CodeError(ErrorCode(nil)) != nil {
		t.Fatal("nil must round trip")
	}
}
