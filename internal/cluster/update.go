package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/update"
)

func (m *Manager) UpdateMembers(ctx context.Context) (string, bool, []update.UpdatePeer) {
	st := m.Status(ctx)
	peers := []update.UpdatePeer{}
	for _, n := range st.Members {
		peers = append(peers, update.UpdatePeer{ID: n.ID, Name: n.Name, Voter: n.Suffrage == "voter", Reachable: n.Reachable})
	}
	return st.NodeID, st.Enabled && st.RaftState == raft.Leader.String() && st.Gate != nil && st.Gate.Open, peers
}
func (m *Manager) PeerUpdateState(ctx context.Context, id string) (*update.State, error) {
	n, ok := m.isMember(id)
	if !ok {
		return nil, errors.New("not a cluster member")
	}
	var state update.State
	err := m.postPeer(ctx, string(n.Address), "/cluster/v1/update/state", struct{}{}, &state)
	return &state, err
}
func (m *Manager) RequestPeerUpdate(ctx context.Context, id, version string) error {
	n, ok := m.isMember(id)
	if !ok {
		return errors.New("not a cluster member")
	}
	return m.postPeer(ctx, string(n.Address), "/cluster/v1/update/apply", map[string]string{"version": version}, nil)
}
func (m *Manager) handlePeerUpdateState(w http.ResponseWriter, r *http.Request, peer string) {
	if m.opt.Update == nil {
		writeJSON(w, 501, rpcError{Code: "updates_unavailable"})
		return
	}
	// Do not send update logs or release history to peers.
	s := m.opt.Update.State(r.Context())
	writeJSON(w, 200, &update.State{Current: s.Current, Agent: s.Agent, Pending: s.Pending, PendingVersion: s.PendingVersion, Status: s.Status})
}
func (m *Manager) handlePeerUpdateApply(w http.ResponseWriter, r *http.Request, peer string) {
	rn := m.raftNode()
	if rn == nil || m.opt.Update == nil {
		writeJSON(w, 503, rpcError{Code: "updates_unavailable"})
		return
	}
	_, leader := rn.LeaderWithID()
	if string(leader) != peer || !m.GateOK() {
		writeJSON(w, 403, rpcError{Code: "not_leader"})
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil {
		writeJSON(w, 400, rpcError{Code: "bad_request"})
		return
	}
	// The dispatching leader may have just committed this plan. Wait for local replication,
	// then require this exact node and version to be the plan's sole active update.
	authorized := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		b, _ := m.opt.Store.GetSetting(r.Context(), update.RolloutKey)
		var p update.RolloutPlan
		if json.Unmarshal([]byte(b), &p) == nil && p.State == "running" && p.Version == req.Version {
			for _, n := range p.Nodes {
				if n.ID == m.opt.Node.Identity.ID && n.State == "updating" {
					authorized = true
				}
			}
		}
		if authorized {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	_, leader = rn.LeaderWithID()
	if !authorized || string(leader) != peer || !m.GateOK() {
		writeJSON(w, 403, rpcError{Code: "unconfirmed_update"})
		return
	}
	err := m.opt.Update.RequestVersion(r.Context(), req.Version, "cluster leader "+peer)
	if errors.Is(err, update.ErrBusy) {
		st := m.opt.Update.State(r.Context())
		if !((st.Pending && st.PendingVersion == req.Version) || (st.Status != nil && st.Status.Target == req.Version && st.Status.State == "running")) {
			writeJSON(w, 409, rpcError{Code: "another_update_running", Message: err.Error()})
			return
		}
	}
	if err != nil && !errors.Is(err, update.ErrBusy) {
		writeJSON(w, 409, rpcError{Code: "update_failed", Message: err.Error()})
		return
	}
	m.log.Warn("cluster update accepted", "target", req.Version, "leader", peer)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
