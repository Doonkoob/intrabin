package cluster

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

type tnode struct {
	dir   string
	addr  string
	store *storage.Store
	cp    *controlplane.CP
	box   *security.SecretBox
	n     *node.Node
	m     *Manager
	key   []byte
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func newTNode(t *testing.T, name string) *tnode {
	t.Helper()
	tn := &tnode{dir: t.TempDir(), addr: freeAddr(t)}
	tn.key = make([]byte, 32)
	copy(tn.key, []byte(name+"-master-key-0123456789abcdefghijkl"))
	tn.open(t, name)
	return tn
}

func (tn *tnode) open(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	st, err := storage.Open(ctx, tn.dir+"/intrabin.db")
	if err != nil {
		t.Fatal(err)
	}
	tn.store = st
	tn.cp = controlplane.New(st)
	tn.box, _ = security.NewSecretBox(tn.key)
	master, _ := security.NewSecretBox(tn.key)
	tn.n, err = node.LoadIdentity(ctx, st, name)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tn.m = New(Options{DataDir: tn.dir, Listen: tn.addr, Advertise: tn.addr, APIURL: "http://" + name, Store: st, CP: tn.cp,
		DataBox: tn.box, MasterBox: master, Node: tn.n, Log: log, Fast: true, MeshFreshness: 3 * time.Second})
}

func (tn *tnode) close() {
	tn.m.Shutdown()
	tn.store.Close()
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mkUser(name string) *model.User {
	now := time.Now().UTC()
	return &model.User{ID: uuid.NewString(), Username: name, PasswordHash: "x", Role: model.RoleAdmin, Language: "en", CreatedAt: now, UpdatedAt: now, PasswordChangedAt: now}
}

func TestClusterLifecycle(t *testing.T) {
	ctx := context.Background()
	n1, n2, n3 := newTNode(t, "n1"), newTNode(t, "n2"), newTNode(t, "n3")
	defer func() {
		for _, n := range []*tnode{n1, n2, n3} {
			n.close()
		}
	}()

	// Founder has existing data, including an encrypted monitor secret.
	if err := n1.cp.CreateFirstUser(ctx, mkUser("owner")); err != nil {
		t.Fatal(err)
	}
	mon := &model.Monitor{ID: uuid.NewString(), Name: "web", Type: model.MonitorHTTP, Target: "http://127.0.0.1:1/", IntervalSeconds: 30, TimeoutSeconds: 5,
		Enabled: true, Policy: model.DefaultPolicy(), Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	blob, _ := n1.box.Seal([]byte(`{"headers":{"Authorization":"Bearer s3cret"}}`), []byte("monitor:"+mon.ID))
	mon.SecretBlob = blob
	if err := n1.cp.CreateMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	if err := n1.m.Init(ctx, ""); err != nil {
		t.Fatalf("init: %v", err)
	}
	waitFor(t, "n1 leader", 5*time.Second, func() bool { return n1.m.computeRole() == node.RoleLeader })

	tok, _, err := n1.m.CreateToken(ctx, 10*time.Minute, "n2", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n2.m.Join(ctx, tok, "", ""); err != nil {
		t.Fatalf("join n2: %v", err)
	}
	// Token is single-use.
	if _, err := n3.m.Join(ctx, tok, "", DataDiscard); err == nil || !strings.Contains(err.Error(), "token used") {
		t.Fatalf("expected token used error, got %v", err)
	}
	// n3 has local data; joining without an explicit choice is refused (no silent data loss).
	if err := n3.cp.CreateFirstUser(ctx, mkUser("local")); err != nil {
		t.Fatal(err)
	}
	tok3, _, _ := n1.m.CreateToken(ctx, 10*time.Minute, "n3", "owner")
	if _, err := n3.m.Join(ctx, tok3, "", ""); !errors.Is(err, ErrDataPresent) {
		t.Fatalf("expected ErrDataPresent, got %v", err)
	}
	res, err := n3.m.Join(ctx, tok3, "", DataExport)
	if err != nil {
		t.Fatalf("join n3: %v", err)
	}
	if res.BackupPath == "" {
		t.Fatal("expected a backup path")
	}

	// Replication of pre-existing state (seed) reaches the new members, and secrets decrypt.
	for _, n := range []*tnode{n2, n3} {
		waitFor(t, "replicated monitor", 10*time.Second, func() bool {
			m, err := n.store.GetMonitor(ctx, mon.ID)
			if err != nil {
				return false
			}
			pt, err := n.box.Open(m.SecretBlob, []byte("monitor:"+m.ID))
			return err == nil && strings.Contains(string(pt), "s3cret")
		})
		if _, err := n.store.GetUserByUsername(ctx, "local"); err == nil {
			t.Fatal("n3 local user should have been replaced by cluster state")
		}
	}

	// Promote both after they are reachable.
	for _, n := range []*tnode{n2, n3} {
		id := n.n.Identity.ID
		waitFor(t, "mesh proof", 10*time.Second, func() bool { return n1.m.mesh.bidirectional(id, 3*time.Second) })
		waitFor(t, "promotion", 10*time.Second, func() bool { return n1.m.Promote(ctx, id) == nil })
	}
	waitFor(t, "followers", 10*time.Second, func() bool {
		return n2.m.computeRole() == node.RoleFollower && n3.m.computeRole() == node.RoleFollower
	})
	st := n1.m.Status(ctx)
	if !st.Gate.Open || st.Gate.Voters != 3 {
		t.Fatalf("unexpected gate: %+v", st.Gate)
	}

	// Followers reject management writes but forward session writes.
	if err := n2.cp.CreateGroup(ctx, &model.MonitorGroup{ID: uuid.NewString(), Name: "g"}); !errors.Is(err, controlplane.ErrNotLeader) {
		t.Fatalf("expected ErrNotLeader, got %v", err)
	}
	owner, _ := n2.store.GetUserByUsername(ctx, "owner")
	se := &model.Session{TokenHash: "h-" + uuid.NewString(), UserID: owner.ID, CSRFToken: "c", CreatedAt: time.Now(), LastSeenAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err := n2.cp.CreateSession(ctx, se); err != nil {
		t.Fatalf("forwarded session: %v", err)
	}
	if _, err := n2.store.GetSession(ctx, se.TokenHash); err != nil {
		t.Fatalf("read-your-writes after forward: %v", err)
	}

	// Result shipping from a follower to the leader.
	n2.m.opt.Node.SetRole(node.RoleFollower)
	lat := 12.0
	r := &model.CheckResult{ID: uuid.NewString(), MonitorID: mon.ID, NodeID: n2.n.Identity.ID, ConfigVersion: 1, RoundStart: time.Now().Truncate(30 * time.Second),
		ProbeAt: time.Now(), ReceivedAt: time.Now(), Success: true, LatencyMs: &lat, Attempts: 1}
	if err := n2.m.ResultSink()(ctx, []*model.CheckResult{r}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "shipped result", 10*time.Second, func() bool {
		rs, _ := n1.store.ResultsForRound(ctx, mon.ID, r.RoundStart)
		return len(rs) == 1
	})
	waitFor(t, "outbox drained", 5*time.Second, func() bool { n, _ := n2.store.OutboxSize(ctx); return n == 0 })

	// Leader failure: a new leader is elected among the remaining voters and accepts writes.
	n1.close()
	var leader *tnode
	waitFor(t, "new leader", 15*time.Second, func() bool {
		for _, n := range []*tnode{n2, n3} {
			if n.m.computeRole() == node.RoleLeader {
				leader = n
				return true
			}
		}
		return false
	})
	gid := uuid.NewString()
	if err := leader.cp.CreateGroup(ctx, &model.MonitorGroup{ID: gid, Name: "after-failover"}); err != nil {
		t.Fatalf("write on new leader: %v", err)
	}

	// The old leader restarts, resumes as a follower and catches up; it does not take over.
	n1.open(t, "n1")
	if err := n1.m.Resume(ctx); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitFor(t, "old leader catches up", 15*time.Second, func() bool {
		_, err := n1.store.GetGroup(ctx, gid)
		return err == nil
	})
	waitFor(t, "old leader is follower", 10*time.Second, func() bool { return n1.m.computeRole() == node.RoleFollower })
	if pt, err := n1.box.Open(mustMonitor(t, n1.store, mon.ID).SecretBlob, []byte("monitor:"+mon.ID)); err != nil || !strings.Contains(string(pt), "s3cret") {
		t.Fatalf("secret after resume: %v", err)
	}
}

func mustMonitor(t *testing.T, st *storage.Store, id string) *model.Monitor {
	m, err := st.GetMonitor(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestJoinRejectsWrongCluster(t *testing.T) {
	ctx := context.Background()
	n1, n2 := newTNode(t, "a1"), newTNode(t, "a2")
	defer n1.close()
	defer n2.close()
	if err := n1.m.Init(ctx, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "leader", 5*time.Second, func() bool { return n1.m.computeRole() == node.RoleLeader })
	tok, _, err := n1.m.CreateToken(ctx, 10*time.Minute, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	jt, _ := ParseJoinToken(tok)
	jt.CAFingerprint = strings.Repeat("0", 64) // attacker-controlled or wrong cluster
	if _, err := n2.m.Join(ctx, jt.String(), "", ""); err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Fatalf("expected TLS pinning failure, got %v", err)
	}
	jt, _ = ParseJoinToken(tok)
	jt.Secret = strings.Repeat("A", 43)
	if _, err := n2.m.Join(ctx, jt.String(), "", ""); err == nil || !strings.Contains(err.Error(), "token invalid") {
		t.Fatalf("expected invalid token, got %v", err)
	}
	if n2.m.Enabled() {
		t.Fatal("node must remain standalone after a failed join")
	}
}

func threeVoters(t *testing.T) []*tnode {
	t.Helper()
	ctx := context.Background()
	ns := []*tnode{newTNode(t, "q1"), newTNode(t, "q2"), newTNode(t, "q3")}
	if err := ns[0].m.Init(ctx, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "leader", 5*time.Second, func() bool { return ns[0].m.computeRole() == node.RoleLeader })
	for _, n := range ns[1:] {
		tok, _, err := ns[0].m.CreateToken(ctx, 10*time.Minute, "", "x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.m.Join(ctx, tok, "", ""); err != nil {
			t.Fatal(err)
		}
		id := n.n.Identity.ID
		waitFor(t, "promote", 10*time.Second, func() bool { return ns[0].m.Promote(ctx, id) == nil })
	}
	return ns
}

func TestQuorumLossStopsWrites(t *testing.T) {
	ctx := context.Background()
	ns := threeVoters(t)
	defer ns[0].close()
	waitFor(t, "followers", 10*time.Second, func() bool {
		return ns[1].m.computeRole() == node.RoleFollower && ns[2].m.computeRole() == node.RoleFollower
	})
	// Lose both followers: the leader must stop accepting writes (NO_QUORUM), never continue alone.
	ns[1].close()
	ns[2].close()
	waitFor(t, "NO_QUORUM", 10*time.Second, func() bool { return ns[0].m.computeRole() == node.RoleNoQuorum })
	err := ns[0].cp.CreateGroup(ctx, &model.MonitorGroup{ID: uuid.NewString(), Name: "x"})
	if !errors.Is(err, controlplane.ErrNoQuorum) && !errors.Is(err, controlplane.ErrNotLeader) {
		t.Fatalf("expected write to fail without quorum, got %v", err)
	}
	waitFor(t, "gate closed", 10*time.Second, func() bool { return !ns[0].m.Status(ctx).Gate.Open })
}
