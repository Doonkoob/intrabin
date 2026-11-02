package cluster

import (
	"context"
	"sort"
	"time"

	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/node"
)

// MemberStatus describes one member as seen from this node.
type MemberStatus struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Address      string `json:"address"`
	APIURL       string `json:"api_url"`
	Location     string `json:"location"`
	Version      string `json:"version"`
	Suffrage     string `json:"suffrage"` // voter | nonvoter
	Self         bool   `json:"self"`
	Leader       bool   `json:"leader"`
	Reachable    bool   `json:"reachable"`    // fresh bidirectional proof (always true for self)
	LastSeenMs   *int64 `json:"last_seen_ms"` // ms since last proof
	AppliedIndex uint64 `json:"applied_index"`
	RaftState    string `json:"raft_state"`
	Outbox       int    `json:"outbox"`
}

// Status is the cluster view served to the UI.
type Status struct {
	Enabled      bool                       `json:"enabled"`
	ClusterID    string                     `json:"cluster_id,omitempty"`
	NodeID       string                     `json:"node_id"`
	Role         node.Role                  `json:"role"`
	RaftState    string                     `json:"raft_state,omitempty"`
	Term         string                     `json:"term,omitempty"`
	LeaderID     string                     `json:"leader_id,omitempty"`
	LeaderAPIURL string                     `json:"leader_api_url,omitempty"`
	CommitIndex  string                     `json:"commit_index,omitempty"`
	AppliedIndex uint64                     `json:"applied_index,omitempty"`
	LastContact  *int64                     `json:"last_contact_ms,omitempty"`
	Advertise    string                     `json:"advertise,omitempty"`
	Gate         *GateStatus                `json:"gate,omitempty"`
	Members      []MemberStatus             `json:"members"`
	Mesh         map[string]map[string]bool `json:"mesh,omitempty"` // from → to → reachable
	Warnings     []string                   `json:"warnings"`
	Defaults     map[string]string          `json:"defaults"`
}

// GateStatus explains the mesh safety gate.
type GateStatus struct {
	Open      bool `json:"open"`
	Voters    int  `json:"voters"`
	Reachable int  `json:"reachable"`
	Required  int  `json:"required"`
}

// Status returns the current cluster status.
func (m *Manager) Status(ctx context.Context) *Status {
	st := &Status{NodeID: m.opt.Node.Identity.ID, Role: m.opt.Node.Role(), Members: []MemberStatus{}, Warnings: []string{},
		Defaults: map[string]string{"advertise": m.opt.Advertise, "listen": m.opt.Listen}}
	r := m.raftNode()
	if r == nil {
		return st
	}
	st.Enabled = true
	st.ClusterID = m.clusterID
	st.Advertise = m.advertise
	stats := r.Stats()
	st.RaftState = r.State().String()
	st.Term = stats["term"]
	st.CommitIndex = stats["commit_index"]
	st.AppliedIndex = r.AppliedIndex()
	if r.State() != raft.Leader {
		if lc := r.LastContact(); !lc.IsZero() {
			v := time.Since(lc).Milliseconds()
			st.LastContact = &v
		}
	}
	_, lid := r.LeaderWithID()
	st.LeaderID = string(lid)
	st.LeaderAPIURL = m.LeaderAPIURL(ctx)
	ok, have, need := m.gate()
	servers := m.servers()
	voters := 0
	for _, s := range servers {
		if s.Suffrage == raft.Voter {
			voters++
		}
	}
	st.Gate = &GateStatus{Open: ok, Voters: voters, Reachable: have, Required: need}

	meta := map[string]MemberStatus{}
	if nodes, err := m.opt.Store.ListClusterNodes(ctx); err == nil {
		for _, n := range nodes {
			meta[n.ID] = MemberStatus{Name: n.Name, APIURL: n.APIURL, Location: n.Location, Version: n.Version}
		}
	}
	m.mesh.mu.Lock()
	views := map[string]peerView{}
	for k, v := range m.mesh.views {
		views[k] = v
	}
	m.mesh.mu.Unlock()
	self := m.opt.Node.Identity.ID
	st.Mesh = map[string]map[string]bool{}
	for _, s := range servers {
		id := string(s.ID)
		ms := meta[id]
		ms.ID, ms.Address, ms.Self, ms.Leader = id, string(s.Address), id == self, s.ID == lid
		if ms.Name == "" {
			ms.Name = id[:min(8, len(id))]
		}
		ms.Suffrage = "nonvoter"
		if s.Suffrage == raft.Voter {
			ms.Suffrage = "voter"
		}
		if ms.Self {
			ms.Reachable = true
			ms.AppliedIndex = r.AppliedIndex()
			ms.RaftState = r.State().String()
			z := int64(0)
			ms.LastSeenMs = &z
			ms.Outbox, _ = m.opt.Store.OutboxSize(ctx)
			st.Mesh[id] = map[string]bool{}
			for p := range m.mesh.heard(m.opt.MeshFreshness) {
				st.Mesh[id][p] = true
			}
		} else {
			ms.Reachable = m.mesh.bidirectional(id, m.opt.MeshFreshness)
			if v, ok := views[id]; ok {
				ago := time.Since(v.At).Milliseconds()
				ms.LastSeenMs = &ago
				ms.AppliedIndex, ms.RaftState, ms.Outbox = v.Applied, v.State, v.Outbox
				if ms.Version == "" {
					ms.Version = v.Version
				}
				if time.Since(v.At) <= 3*m.opt.MeshFreshness {
					st.Mesh[id] = map[string]bool{}
					for p := range v.Heard {
						st.Mesh[id][p] = true
					}
				}
			}
		}
		st.Members = append(st.Members, ms)
	}
	sort.Slice(st.Members, func(i, j int) bool { return st.Members[i].Name < st.Members[j].Name })
	switch {
	case voters == 1 && len(servers) > 1:
		st.Warnings = append(st.Warnings, "single_voter")
	case voters == 2:
		st.Warnings = append(st.Warnings, "two_voters")
	case voters%2 == 0 && voters > 0:
		st.Warnings = append(st.Warnings, "even_voters")
	}
	if !ok {
		st.Warnings = append(st.Warnings, "gate_closed")
	}
	return st
}

// LocationOf returns a node's location label (falls back to the node ID), cached for 30 s.
func (m *Manager) LocationOf(id string) string {
	m.locMu.Lock()
	defer m.locMu.Unlock()
	if time.Since(m.locAt) > 30*time.Second {
		m.locs = map[string]string{}
		if nodes, err := m.opt.Store.ListClusterNodes(context.Background()); err == nil {
			for _, n := range nodes {
				if n.Location != "" {
					m.locs[n.ID] = n.Location
				}
			}
		}
		m.locAt = time.Now()
	}
	if l, ok := m.locs[id]; ok {
		return l
	}
	return id
}
