package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
)

func versionString() string { return node.Version }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	LeaderAddr string `json:"leader_addr,omitempty"`
}

func (m *Manager) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cluster/v1/join", m.handleJoin)
	mux.HandleFunc("POST /cluster/v1/ping", m.peerOnly(m.handlePing))
	mux.HandleFunc("POST /cluster/v1/apply", m.peerOnly(m.handleForwardApply))
	mux.HandleFunc("POST /cluster/v1/results", m.peerOnly(m.handleResults))
	return mux
}

// peerOnly requires a verified cluster client certificate whose subject is a current member.
func (m *Manager) peerOnly(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			writeJSON(w, http.StatusUnauthorized, rpcError{Code: "unauthenticated", Message: "cluster client certificate required"})
			return
		}
		id := r.TLS.VerifiedChains[0][0].Subject.CommonName
		if _, ok := m.isMember(id); !ok {
			writeJSON(w, http.StatusForbidden, rpcError{Code: "not_member", Message: "node is not a cluster member"})
			return
		}
		h(w, r, id)
	}
}

func (m *Manager) notLeader(w http.ResponseWriter) {
	addr, _ := m.leaderAddr()
	writeJSON(w, http.StatusMisdirectedRequest, rpcError{Code: "not_leader", Message: "this node is not the leader", LeaderAddr: addr})
}

// ---- writes ----

// Apply commits a command through Raft (controlplane.Applier). On followers, operations listed
// in controlplane.ForwardableOps are forwarded to the leader; other writes are rejected.
func (m *Manager) Apply(ctx context.Context, cmd *controlplane.Command) error {
	r := m.raftNode()
	if r == nil {
		return controlplane.ErrNoQuorum
	}
	if r.State() == raft.Leader {
		_, err := m.applyLocal(cmd)
		return err
	}
	if !controlplane.ForwardableOps[cmd.Op] {
		return controlplane.ErrNotLeader
	}
	return m.forward(ctx, cmd)
}

func (m *Manager) applyLocal(cmd *controlplane.Command) (uint64, error) {
	r := m.raftNode()
	if r == nil || r.State() != raft.Leader {
		return 0, controlplane.ErrNotLeader
	}
	if !m.GateOK() {
		return 0, controlplane.ErrNoQuorum
	}
	b, err := controlplane.Encode(cmd)
	if err != nil {
		return 0, err
	}
	f := r.Apply(b, 5*time.Second)
	if err := f.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return 0, controlplane.ErrNotLeader
		}
		return 0, fmt.Errorf("%w: %v", controlplane.ErrNoQuorum, err)
	}
	if res, ok := f.Response().(applyResult); ok && res.Err != nil {
		return f.Index(), res.Err
	}
	return f.Index(), nil
}

type forwardReply struct {
	Index uint64 `json:"index"`
	Code  string `json:"code"`
}

func (m *Manager) forward(ctx context.Context, cmd *controlplane.Command) error {
	addr, _ := m.leaderAddr()
	if addr == "" {
		return controlplane.ErrNoQuorum
	}
	var rep forwardReply
	if err := m.postPeer(ctx, addr, "/cluster/v1/apply", cmd, &rep); err != nil {
		return fmt.Errorf("%w: %v", controlplane.ErrNoQuorum, err)
	}
	// Read-your-writes: wait until this node has applied the entry.
	deadline := time.Now().Add(5 * time.Second)
	for r := m.raftNode(); r != nil && r.AppliedIndex() < rep.Index && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	return controlplane.CodeError(rep.Code)
}

func (m *Manager) handleForwardApply(w http.ResponseWriter, r *http.Request, peer string) {
	var cmd controlplane.Command
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&cmd); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError{Code: "bad_request", Message: "invalid command"})
		return
	}
	if !controlplane.ForwardableOps[cmd.Op] {
		writeJSON(w, http.StatusForbidden, rpcError{Code: "forbidden", Message: "operation cannot be forwarded"})
		return
	}
	if rn := m.raftNode(); rn == nil || rn.State() != raft.Leader {
		m.notLeader(w)
		return
	}
	// Timestamps come from the forwarding node; bound skew so a peer cannot back/forward-date records.
	if d := time.Since(cmd.At); d > time.Minute || d < -time.Minute {
		cmd.At = time.Now().UTC()
	}
	idx, err := m.applyLocal(&cmd)
	if errors.Is(err, controlplane.ErrNotLeader) || errors.Is(err, controlplane.ErrNoQuorum) {
		writeJSON(w, http.StatusServiceUnavailable, rpcError{Code: controlplane.ErrorCode(err), Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, forwardReply{Index: idx, Code: controlplane.ErrorCode(err)})
}

func (m *Manager) postPeer(ctx context.Context, addr, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.peerHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e rpcError
		_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<16)).Decode(&e)
		return fmt.Errorf("%s: %s (%d)", path, e.Code, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(out)
	}
	return nil
}

// ---- result shipping ----

// ResultSink returns the engine's sink: results are stored locally and, on followers, queued in
// a durable outbox until the leader acknowledges them (at-least-once; the leader dedups by ID).
func (m *Manager) ResultSink() func(ctx context.Context, rs []*model.CheckResult) error {
	return func(ctx context.Context, rs []*model.CheckResult) error {
		if _, err := m.opt.Store.InsertResults(ctx, rs); err != nil {
			return err
		}
		if !m.Enabled() || m.opt.Node.Role() == node.RoleLeader {
			return nil
		}
		ids := make([]string, len(rs))
		for i, r := range rs {
			ids[i] = r.ID
		}
		if err := m.opt.Store.EnqueueOutbox(ctx, ids); err != nil {
			return err
		}
		m.kickShip()
		return nil
	}
}

func (m *Manager) kickShip() {
	select {
	case m.shipKick <- struct{}{}:
	default:
	}
}

func (m *Manager) shipLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.shipKick:
		}
		for m.shipOnce(ctx) {
		}
	}
}

// shipOnce sends one batch; it returns true if more may be pending.
func (m *Manager) shipOnce(ctx context.Context) bool {
	batch, err := m.opt.Store.PendingOutbox(ctx, 500)
	if err != nil || len(batch) == 0 {
		return false
	}
	ids := make([]string, len(batch))
	for i, r := range batch {
		ids[i] = r.ID
	}
	if rn := m.raftNode(); rn != nil && rn.State() == raft.Leader {
		// Results are already in the local table the evaluator reads.
		_ = m.opt.Store.AckOutbox(ctx, ids)
		return len(batch) == 500
	}
	addr, _ := m.leaderAddr()
	if addr == "" || m.opt.Node.Role() != node.RoleFollower {
		return false
	}
	if err := m.postPeer(ctx, addr, "/cluster/v1/results", batch, nil); err != nil {
		m.log.Debug("result shipping failed; will retry", "err", err, "pending", len(batch))
		return false
	}
	if err := m.opt.Store.AckOutbox(ctx, ids); err != nil {
		m.log.Error("outbox ack", "err", err)
		return false
	}
	return len(batch) == 500
}

func (m *Manager) handleResults(w http.ResponseWriter, r *http.Request, peer string) {
	if rn := m.raftNode(); rn == nil || rn.State() != raft.Leader {
		m.notLeader(w)
		return
	}
	var rs []*model.CheckResult
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&rs); err != nil || len(rs) > 1000 {
		writeJSON(w, http.StatusBadRequest, rpcError{Code: "bad_request", Message: "invalid results batch"})
		return
	}
	now := time.Now().UTC()
	for _, x := range rs {
		if x.NodeID != peer { // a node may only submit its own observations
			writeJSON(w, http.StatusForbidden, rpcError{Code: "forbidden", Message: "result node_id does not match certificate"})
			return
		}
		x.ReceivedAt = now
	}
	if _, err := m.opt.Store.InsertResults(r.Context(), rs); err != nil {
		writeJSON(w, http.StatusInternalServerError, rpcError{Code: "internal", Message: "store failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": len(rs)})
}

// ---- small helpers ----

type rateWindow struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateWindow(max int, window time.Duration) *rateWindow {
	return &rateWindow{max: max, window: window, hits: map[string][]time.Time{}}
}

func (r *rateWindow) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	h := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if now.Sub(t) < r.window {
			h = append(h, t)
		}
	}
	if len(h) >= r.max {
		r.hits[key] = h
		return false
	}
	r.hits[key] = append(h, now)
	return true
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}
