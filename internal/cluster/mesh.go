package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// mesh tracks reachability between this node and its peers. A bidirectional proof with peer P
// means: our ping to P returned (we → P) and P's ping to us arrived (P → us), both recently.
type mesh struct {
	mu       sync.Mutex
	outbound map[string]time.Time // last successful ping reply from peer
	inbound  map[string]time.Time // last ping received from peer
	views    map[string]peerView
}

type peerView struct {
	At       time.Time        `json:"-"`
	Applied  uint64           `json:"applied"`
	State    string           `json:"state"`
	LeaderID string           `json:"leader_id"`
	Heard    map[string]int64 `json:"heard"` // peer id → unix ms of last bidirectional proof
	Version  string           `json:"version"`
	Outbox   int              `json:"outbox"`
}

func newMesh() *mesh {
	return &mesh{outbound: map[string]time.Time{}, inbound: map[string]time.Time{}, views: map[string]peerView{}}
}

func (s *mesh) bidirectional(id string, fresh time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	return now.Sub(s.outbound[id]) <= fresh && now.Sub(s.inbound[id]) <= fresh
}

func (s *mesh) heard(fresh time.Duration) map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int64{}
	now := time.Now()
	for id, o := range s.outbound {
		in := s.inbound[id]
		if now.Sub(o) <= fresh && now.Sub(in) <= fresh {
			t := o
			if in.Before(t) {
				t = in
			}
			out[id] = t.UnixMilli()
		}
	}
	return out
}

type pingMsg struct {
	From string `json:"from"`
	peerView
}

func (m *Manager) selfView() peerView {
	r := m.raftNode()
	v := peerView{Heard: m.mesh.heard(m.opt.MeshFreshness), Version: versionString()}
	if r != nil {
		v.Applied = r.AppliedIndex()
		v.State = r.State().String()
		_, id := r.LeaderWithID()
		v.LeaderID = string(id)
	}
	if n, err := m.opt.Store.OutboxSize(context.Background()); err == nil {
		v.Outbox = n
	}
	return v
}

func (m *Manager) meshLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			self := m.opt.Node.Identity.ID
			var wg sync.WaitGroup
			for _, s := range m.servers() {
				if string(s.ID) == self {
					continue
				}
				wg.Add(1)
				go func(s raft.Server) {
					defer wg.Done()
					m.ping(ctx, s)
				}(s)
			}
			wg.Wait()
		}
	}
}

func (m *Manager) ping(ctx context.Context, s raft.Server) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(pingMsg{From: m.opt.Node.Identity.ID, peerView: m.selfView()})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+string(s.Address)+"/cluster/v1/ping", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.peerHTTP.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var reply pingMsg
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&reply); err != nil || reply.From != string(s.ID) {
		return
	}
	reply.peerView.At = time.Now()
	m.mesh.mu.Lock()
	m.mesh.outbound[string(s.ID)] = time.Now()
	m.mesh.views[string(s.ID)] = reply.peerView
	m.mesh.mu.Unlock()
}

func (m *Manager) handlePing(w http.ResponseWriter, r *http.Request, peer string) {
	var msg pingMsg
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&msg); err != nil || msg.From != peer {
		http.Error(w, "bad ping", http.StatusBadRequest)
		return
	}
	m.mesh.mu.Lock()
	m.mesh.inbound[peer] = time.Now()
	m.mesh.mu.Unlock()
	writeJSON(w, http.StatusOK, pingMsg{From: m.opt.Node.Identity.ID, peerView: m.selfView()})
}
