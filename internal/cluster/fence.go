package cluster

import (
	"context"
	"strconv"

	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/controlplane"
)

// Fence proves, at the moment of the call, that this node may dispatch external actions: it is
// the Raft leader, a majority of voters confirms that leadership (VerifyLeader round trip), and the
// strict mesh gate is open. It returns the leader term and node ID, which are recorded in the
// action's dispatch record. In standalone mode it returns term 0 and this node's ID.
//
// A fence is not a lease: leadership can still be lost immediately after the call returns. The
// automation engine therefore commits a "dispatching" record through Raft before dispatching and
// treats any dispatch whose result was not committed as an UNKNOWN_OUTCOME, never replaying it.
func (m *Manager) Fence(ctx context.Context) (uint64, string, error) {
	self := m.opt.Node.Identity.ID
	r := m.raftNode()
	if r == nil {
		return 0, self, nil
	}
	if r.State() != raft.Leader {
		return 0, "", controlplane.ErrNotLeader
	}
	term := m.term(r)
	if !m.GateOK() {
		return 0, "", controlplane.ErrNoQuorum
	}
	done := make(chan error, 1)
	go func() { done <- r.VerifyLeader().Error() }()
	select {
	case err := <-done:
		if err != nil {
			return 0, "", controlplane.ErrNotLeader
		}
	case <-ctx.Done():
		return 0, "", ctx.Err()
	}
	if r.State() != raft.Leader || m.term(r) != term || !m.GateOK() {
		return 0, "", controlplane.ErrNotLeader
	}
	return term, self, nil
}

func (m *Manager) term(r *raft.Raft) uint64 {
	t, _ := strconv.ParseUint(r.Stats()["term"], 10, 64)
	return t
}
