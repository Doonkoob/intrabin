package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
)

// serveWeb exposes the node's tunnel endpoint the way the public API does and returns its URL.
func (tn *tnode) serveWeb(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != TunnelPath {
			http.NotFound(w, r)
			return
		}
		tn.m.ServeTunnel(w, r, "127.0.0.1")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func suffrage(m *Manager, id string) (raft.ServerSuffrage, string, bool) {
	s, ok := m.isMember(id)
	return s.Suffrage, string(s.Address), ok
}

func TestClusterOverWebTunnelAndLeaderHandover(t *testing.T) {
	ctx := context.Background()
	n1, n2 := newTNode(t, "w1"), newTNode(t, "w2")
	defer n1.close()
	defer n2.close()
	u1, u2 := n1.serveWeb(t), n2.serveWeb(t)

	if err := n1.m.Init(ctx, u1+"/"); err != nil {
		t.Fatalf("init with URL address: %v", err)
	}
	waitFor(t, "leader", 5*time.Second, func() bool { return n1.m.computeRole() == node.RoleLeader })
	if got := n1.m.advertiseAddr(); got != u1 {
		t.Fatalf("advertise not normalised: %q", got)
	}
	tok, _, err := n1.m.CreateToken(ctx, 10*time.Minute, "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if jt, _ := ParseJoinToken(tok); jt.Addrs[0] != u1 {
		t.Fatalf("token should carry the URL, got %v", jt.Addrs)
	}
	if _, err := n2.m.Join(ctx, tok, u2, ""); err != nil {
		t.Fatalf("join through the tunnel: %v", err)
	}
	id2 := n2.n.Identity.ID
	waitFor(t, "mesh proof", 10*time.Second, func() bool { return n1.m.mesh.bidirectional(id2, 3*time.Second) })
	gid := uuid.NewString()
	if err := n1.cp.CreateGroup(ctx, &model.MonitorGroup{ID: gid, Name: "via-tunnel"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "replicated through tunnel", 10*time.Second, func() bool { _, err := n2.store.GetGroup(ctx, gid); return err == nil })
	if len(n1.m.tunnelSlots) == 0 || len(n2.m.tunnelSlots) == 0 {
		t.Fatalf("expected tunnelled connections in both directions (n1=%d n2=%d)", len(n1.m.tunnelSlots), len(n2.m.tunnelSlots))
	}

	// The leader hands over to the non-voter (promoted on the way) and becomes a worker.
	if err := n1.m.TransferLeadership(ctx, n1.n.Identity.ID, false); err == nil {
		t.Fatal("transfer to itself must be refused")
	}
	if err := n1.m.TransferLeadership(ctx, id2, true); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	waitFor(t, "n2 leader", 10*time.Second, func() bool { return n2.m.computeRole() == node.RoleLeader })
	waitFor(t, "n1 non-voter", 10*time.Second, func() bool {
		s, _, ok := suffrage(n2.m, n1.n.Identity.ID)
		return ok && s == raft.Nonvoter
	})
	waitFor(t, "n1 follower", 10*time.Second, func() bool { return n1.m.computeRole() == node.RoleFollower })
	gid2 := uuid.NewString()
	if err := n2.cp.CreateGroup(ctx, &model.MonitorGroup{ID: gid2, Name: "after-handover"}); err != nil {
		t.Fatalf("write on the new leader: %v", err)
	}
	waitFor(t, "replicated to the worker", 10*time.Second, func() bool { _, err := n1.store.GetGroup(ctx, gid2); return err == nil })
}

func TestSetAddressMovesMembersToURLs(t *testing.T) {
	ctx := context.Background()
	n1, n2 := newTNode(t, "s1"), newTNode(t, "s2")
	defer n1.close()
	defer n2.close()
	u1, u2 := n1.serveWeb(t), n2.serveWeb(t)
	if err := n1.m.Init(ctx, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "leader", 5*time.Second, func() bool { return n1.m.computeRole() == node.RoleLeader })
	tok, _, _ := n1.m.CreateToken(ctx, 10*time.Minute, "", "x")
	if _, err := n2.m.Join(ctx, tok, "", ""); err != nil {
		t.Fatal(err)
	}
	id1, id2 := n1.n.Identity.ID, n2.n.Identity.ID
	waitFor(t, "promote", 10*time.Second, func() bool { return n1.m.Promote(ctx, id2) == nil })

	if err := n1.m.SetAddress(ctx, id2, "http://127.0.0.1:1"); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("unreachable address must be refused, got %v", err)
	}
	if err := n1.m.SetAddress(ctx, id2, u1); err == nil || !strings.Contains(err.Error(), "different node") {
		t.Fatalf("address of another node must be refused, got %v", err)
	}
	if err := n1.m.SetAddress(ctx, id2, u2); err != nil {
		t.Fatalf("move n2 to its URL: %v", err)
	}
	if err := n1.m.SetAddress(ctx, id1, u1); err != nil {
		t.Fatalf("move the leader to its URL: %v", err)
	}
	if s, a, _ := suffrage(n1.m, id2); s != raft.Voter || a != u2 {
		t.Fatalf("n2 should stay a voter at %s, got %v %s", u2, s, a)
	}
	waitFor(t, "n2 learns its new address", 10*time.Second, func() bool { return n2.m.advertiseAddr() == u2 })
	waitFor(t, "mesh over tunnels", 10*time.Second, func() bool {
		return n1.m.GateOK() && len(n1.m.tunnelSlots) > 0 && len(n2.m.tunnelSlots) > 0
	})
	gid := uuid.NewString()
	if err := n1.cp.CreateGroup(ctx, &model.MonitorGroup{ID: gid, Name: "after-move"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "replicated after the move", 10*time.Second, func() bool { _, err := n2.store.GetGroup(ctx, gid); return err == nil })
	if v, _ := n2.store.GetSetting(ctx, keyAdvertise); v != u2 {
		t.Fatalf("n2 should persist its new address, got %q", v)
	}
}

func TestWebSocketTunnelCarriesBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := acceptWebSocket(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}))
	defer srv.Close()
	c, err := dialWebSocket(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := make([]byte, 300<<10) // spans several frames, all three length encodings
	_, _ = rand.Read(payload)
	go func() { _, _ = c.Write(payload[:10]); _, _ = c.Write(payload[10:1000]); _, _ = c.Write(payload[1000:]) }()
	got := make([]byte, len(payload))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("echoed bytes differ")
	}

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("plain GET should be refused, got %d", resp.StatusCode)
	}
}

func TestValidAdvertise(t *testing.T) {
	for _, a := range []string{"10.0.0.1:7443", "node1.example.com:7443", "[2001:db8::1]:7443", "https://ib.example.com", "http://203.0.113.5:8080", "https://ib.example.com/intrabin"} {
		if err := validAdvertise(a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	for _, a := range []string{"", "10.0.0.1", "0.0.0.0:7443", "https://", "https://user:pw@ib.example.com", "https://ib.example.com/?x=1", "http://0.0.0.0:8080", "10.0.0.1:99999"} {
		if err := validAdvertise(a); err == nil {
			t.Errorf("%s: expected an error", a)
		}
	}
	if got := normalizeAdvertise(" HTTPS://IB.Example.com/ "); got != "https://ib.example.com" {
		t.Errorf("normalize: %q", got)
	}
	if got := advertiseHost("https://ib.example.com:8443/x"); got != "ib.example.com" {
		t.Errorf("host: %q", got)
	}
}
