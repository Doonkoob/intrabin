package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// RolloutKey is replicated before any server is restarted. A new leader resumes the same plan.
const RolloutKey = "cluster.update.rollout"

var ErrClusterInactive = errors.New("start cluster updates on the leader with an open quorum gate")

// UpdatePeer is a live member, not cached enrollment metadata.
type UpdatePeer struct {
	ID        string
	Name      string
	Voter     bool
	Reachable bool
}

type UpdateCluster interface {
	UpdateMembers(context.Context) (self string, active bool, peers []UpdatePeer)
	PeerUpdateState(context.Context, string) (*State, error)
	RequestPeerUpdate(context.Context, string, string) error
}

type RolloutNode struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	State             string    `json:"state"` // queued | updating | succeeded | failed
	Version           string    `json:"version"`
	Error             string    `json:"error,omitempty"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	PreviousStartedAt string    `json:"previous_started_at,omitempty"`
	Progress          *int      `json:"progress,omitempty"`
	Stage             string    `json:"stage,omitempty"`
	Service           string    `json:"service,omitempty"`
}

type RolloutPlan struct {
	Version   string        `json:"version"`
	State     string        `json:"state"` // running | succeeded | failed
	StartedAt time.Time     `json:"started_at"`
	Nodes     []RolloutNode `json:"nodes"`
}

// Rollout updates the initiating leader first, then one member at a time. Membership and quorum
// must be healthy before dispatch. The durable active node fences retries across leader changes.
type Rollout struct {
	Service *Service
	Cluster UpdateCluster
	Store   Settings
	Write   func(context.Context, string, string) error
	mu      sync.Mutex
}

func (c *Rollout) State(ctx context.Context) *RolloutPlan {
	b, err := c.Store.GetSetting(ctx, RolloutKey)
	if err != nil || b == "" {
		return nil
	}
	var p RolloutPlan
	if json.Unmarshal([]byte(b), &p) != nil {
		return nil
	}
	return &p
}
func (c *Rollout) save(ctx context.Context, p *RolloutPlan) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return c.Write(ctx, RolloutKey, string(b))
}

func (c *Rollout) Start(ctx context.Context, version, actor string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	self, active, peers := c.Cluster.UpdateMembers(ctx)
	if !active {
		return ErrClusterInactive
	}
	if p := c.State(ctx); p != nil && p.State == "running" {
		return ErrBusy
	}
	st := c.Service.State(ctx)
	if !st.Agent.Available {
		return ErrNoAgent
	}
	if st.Latest == nil || st.Latest.Version != version || !st.Available {
		return ErrNotAvailable
	}
	if st.Pending || (st.Status != nil && st.Status.State == "running") {
		return ErrBusy
	}
	for _, peer := range peers {
		if !peer.Reachable {
			return errors.New("all cluster members must be reachable before updating the leader")
		}
	}
	// Finish the leader before starting the rest; non-voters precede the other voters.
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Voter != peers[j].Voter {
			return !peers[i].Voter
		}
		return peers[i].ID < peers[j].ID
	})
	p := &RolloutPlan{Version: version, State: "running", StartedAt: time.Now().UTC(), Nodes: []RolloutNode{}}
	for _, peer := range peers {
		if peer.ID == self {
			n := RolloutNode{ID: self, Name: peer.Name, State: "updating", Version: st.Current, StartedAt: p.StartedAt}
			if st.Status != nil {
				n.PreviousStartedAt = st.Status.StartedAt
			}
			p.Nodes = append(p.Nodes, n)
		}
	}
	if len(p.Nodes) == 0 {
		return errors.New("leader is missing from cluster membership")
	}
	for _, peer := range peers {
		if peer.ID != self {
			p.Nodes = append(p.Nodes, RolloutNode{ID: peer.ID, Name: peer.Name, State: "queued"})
		}
	}
	if err := c.save(ctx, p); err != nil {
		return err
	}
	if err := c.Service.Request(ctx, version, actor); err != nil {
		p.State, p.Nodes[0].State, p.Nodes[0].Error = "failed", "failed", err.Error()
		_ = c.save(ctx, p)
		return err
	}
	return nil
}

func (c *Rollout) Retry(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, active, _ := c.Cluster.UpdateMembers(ctx)
	if !active {
		return errors.New("retry on the active cluster leader")
	}
	p := c.State(ctx)
	if p == nil || p.State != "failed" {
		return errors.New("there is no failed cluster update")
	}
	p.State = "running"
	for i := range p.Nodes {
		if p.Nodes[i].State == "failed" {
			p.Nodes[i].State, p.Nodes[i].Error = "queued", ""
		}
	}
	return c.save(ctx, p)
}

func (c *Rollout) Run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Step(ctx)
		}
	}
}

func (c *Rollout) Step(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	self, active, peers := c.Cluster.UpdateMembers(ctx)
	if !active {
		return
	}
	p := c.State(ctx)
	if p == nil || p.State != "running" {
		return
	}
	members := map[string]UpdatePeer{}
	for _, m := range peers {
		members[m.ID] = m
	}
	fail := func(n *RolloutNode, err error) {
		n.State, n.Error, p.State = "failed", err.Error(), "failed"
		_ = c.save(ctx, p)
	}
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if n.State == "succeeded" {
			continue
		}
		member, exists := members[n.ID]
		if !exists {
			fail(n, errors.New("member was removed; start a new cluster update"))
			return
		}
		if !member.Reachable {
			if n.State == "updating" && time.Since(n.StartedAt) > 30*time.Minute {
				fail(n, errors.New("node has not returned within 30 minutes"))
			}
			return
		}
		var st *State
		var err error
		if n.ID == self {
			st = c.Service.State(ctx)
		} else {
			st, err = c.Cluster.PeerUpdateState(ctx, n.ID)
		}
		if err != nil {
			fail(n, fmt.Errorf("cannot query update agent; older nodes need a one-time manual upgrade: %w", err))
			return
		}
		n.Version = st.Current
		if st.Status != nil && st.Status.Target == p.Version && st.Status.StartedAt != n.PreviousStartedAt {
			status := st.Status
			changed := (n.Progress == nil) != (status.Progress == nil) || n.Stage != status.Stage || n.Service != status.Service
			if n.Progress != nil && status.Progress != nil && *n.Progress != *status.Progress {
				changed = true
			}
			if changed {
				n.Progress, n.Stage, n.Service = status.Progress, status.Stage, status.Service
				if c.save(ctx, p) != nil {
					return
				}
			}
		}
		if st.Pending && st.PendingVersion != "" && st.PendingVersion != p.Version {
			fail(n, errors.New("another version is queued on this node"))
			return
		}
		if st.Status != nil && st.Status.State == "running" && st.Status.Target != p.Version {
			fail(n, errors.New("another version is being installed on this node"))
			return
		}
		if st.Current == p.Version {
			// Wait for its host agent to finish before restarting any other node.
			if st.Pending || (st.Status != nil && st.Status.State == "running") {
				return
			}
			n.State, n.Error = "succeeded", ""
			complete := 100
			n.Progress, n.Stage, n.Service = &complete, "complete", ""
			_ = c.save(ctx, p)
			return
		}
		if v, ok := parseSemver(st.Current); ok && v.pre == "" && compareSemver(st.Current, p.Version) > 0 {
			fail(n, errors.New("node runs a newer version; refusing a downgrade"))
			return
		}
		if n.State == "updating" && !st.Pending && st.Status != nil && st.Status.Target == p.Version && st.Status.State == "failed" && st.Status.StartedAt != n.PreviousStartedAt {
			at, _ := time.Parse(time.RFC3339, st.Status.StartedAt)
			if !at.Before(n.StartedAt.Truncate(time.Second)) {
				fail(n, errors.New(st.Status.Message))
				return
			}
		}
		if n.State == "updating" && time.Since(n.StartedAt) > 30*time.Minute {
			fail(n, errors.New("update did not complete within 30 minutes"))
			return
		}
		if !st.Agent.Available {
			fail(n, ErrNoAgent)
			return
		}
		// Do not restart a member while any other member is unreachable. This also protects
		// two-voter clusters: the plan pauses through the temporary loss of quorum.
		for _, peer := range peers {
			if !peer.Reachable {
				return
			}
		}
		if n.State != "updating" {
			n.State, n.StartedAt = "updating", time.Now().UTC()
			n.Progress, n.Stage, n.Service = nil, "queued", ""
			if st.Status != nil {
				n.PreviousStartedAt = st.Status.StartedAt
			}
			if c.save(ctx, p) != nil {
				return
			}
		}
		if n.ID == self {
			err = c.Service.RequestVersion(ctx, p.Version, "cluster leader")
		} else {
			err = c.Cluster.RequestPeerUpdate(ctx, n.ID, p.Version)
		}
		// Pending/running is an idempotent acknowledgement of an earlier, possibly lost reply.
		if err != nil && !errors.Is(err, ErrBusy) {
			fail(n, err)
		}
		return
	}
	p.State = "succeeded"
	_ = c.save(ctx, p)
}
