package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/intrabin/intrabin/internal/audit"
	"github.com/intrabin/intrabin/internal/cluster"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
)

func (s *Server) readOnlyErr(r *http.Request) *APIError {
	e := *errReadOnlyNode
	if s.cluster != nil && s.cluster.Enabled() {
		e.LeaderURL = s.cluster.LeaderAPIURL(r.Context())
		if s.node.Role() == node.RoleNoQuorum {
			e.Message = "The cluster has no quorum. This node is read-only until a majority of voters can reach each other."
		}
	}
	return &e
}

func (s *Server) clusterOrErr(w http.ResponseWriter, r *http.Request) bool {
	if s.cluster == nil {
		s.writeErr(w, r, &APIError{Status: http.StatusNotImplemented, Code: "cluster_unavailable", Message: "Clustering is not configured on this node."})
		return false
	}
	return true
}

func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if s.cluster == nil {
		writeJSON(w, 200, map[string]any{"cluster": &cluster.Status{NodeID: s.node.Identity.ID, Role: s.node.Role(), Members: []cluster.MemberStatus{}, Warnings: []string{}}})
		return
	}
	writeJSON(w, 200, map[string]any{"cluster": s.cluster.Status(r.Context())})
}

type clusterInitReq struct {
	Advertise string `json:"advertise"`
}

func (s *Server) handleClusterInit(w http.ResponseWriter, r *http.Request) {
	if !s.clusterOrErr(w, r) {
		return
	}
	var req clusterInitReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p := principal(r)
	if err := s.cluster.Init(r.Context(), strings.TrimSpace(req.Advertise)); err != nil {
		s.audit.Record(r.Context(), p.actor(), "cluster.init", "cluster", "", "failure", map[string]any{"error": err.Error()})
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "cluster.init", "cluster", "", "", map[string]any{"advertise": req.Advertise})
	writeJSON(w, 200, map[string]any{"cluster": s.cluster.Status(r.Context())})
}

type clusterJoinReq struct {
	Token      string `json:"token"`
	Advertise  string `json:"advertise"`
	DataAction string `json:"data_action"`
	SetupToken string `json:"setup_token,omitempty"`
}

func (s *Server) doJoin(w http.ResponseWriter, r *http.Request, req clusterJoinReq, actor audit.Actor) {
	switch req.DataAction {
	case "", "export", "discard":
	default:
		s.writeErr(w, r, errValidation(map[string]string{"data_action": "Must be export or discard."}))
		return
	}
	if strings.TrimSpace(req.Token) == "" {
		s.writeErr(w, r, errValidation(map[string]string{"token": "Paste the join token created on the cluster leader."}))
		return
	}
	// Record intent locally before the data is replaced; the entry is part of the export.
	s.log.Info("cluster join requested", "actor", actor.Name, "data_action", req.DataAction)
	res, err := s.cluster.Join(r.Context(), req.Token, strings.TrimSpace(req.Advertise), cluster.DataAction(req.DataAction))
	if err != nil {
		s.log.Warn("cluster join failed", "err", err)
		if strings.Contains(err.Error(), "join rejected") || strings.Contains(err.Error(), "could not reach") || strings.Contains(err.Error(), "CA does not match") {
			s.writeErr(w, r, &APIError{Status: http.StatusBadGateway, Code: "join_failed", Message: err.Error()})
			return
		}
		s.writeErr(w, r, err)
		return
	}
	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.isSecureRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	writeJSON(w, 200, map[string]any{"joined": true, "cluster_id": res.ClusterID, "backup_path": res.BackupPath})
}

func (s *Server) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if !s.clusterOrErr(w, r) {
		return
	}
	var req clusterJoinReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	p := principal(r)
	if p.User.Role != "owner" {
		s.writeErr(w, r, &APIError{Status: http.StatusForbidden, Code: "forbidden", Message: "Only the owner can join this node to a cluster, because its local data will be replaced."})
		return
	}
	s.doJoin(w, r, req, p.actor())
}

// handleSetupJoin lets a fresh node (no users yet) join a cluster instead of running setup.
// It requires the node's one-time setup token, like setup itself.
func (s *Server) handleSetupJoin(w http.ResponseWriter, r *http.Request) {
	if !s.clusterOrErr(w, r) {
		return
	}
	ip := s.clientIP(r)
	if ok, retry := s.limiter.Allowed("setup:" + ip); !ok {
		w.Header().Set("Retry-After", itoa(int(retry.Seconds())))
		s.writeErr(w, r, &APIError{Status: 429, Code: "rate_limited", Message: "Too many attempts. Try again later."})
		return
	}
	var req clusterJoinReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.setupMu.Lock()
	tok := s.setupToken
	s.setupMu.Unlock()
	need, err := s.needsSetup(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if !need || tok == "" {
		s.writeErr(w, r, &APIError{Status: 409, Code: "already_initialized", Message: "Initial setup has already been completed."})
		return
	}
	if !security.ConstantTimeEqual(strings.TrimSpace(req.SetupToken), tok) {
		s.limiter.Fail("setup:" + ip)
		s.writeErr(w, r, errValidation(map[string]string{"setup_token": "Invalid setup token. Find it in the server logs."}))
		return
	}
	req.DataAction = ""
	s.doJoin(w, r, req, audit.Actor{Name: "setup", IP: ip})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.ListEnrollmentTokens(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"tokens": ts})
}

type tokenReq struct {
	Description string `json:"description"`
	TTLMinutes  int    `json:"ttl_minutes"`
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if !s.clusterOrErr(w, r) {
		return
	}
	var req tokenReq
	if err := decode(r, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if req.TTLMinutes == 0 {
		req.TTLMinutes = 30
	}
	if len(req.Description) > 200 {
		s.writeErr(w, r, errValidation(map[string]string{"description": "At most 200 characters."}))
		return
	}
	p := principal(r)
	tok, t, err := s.cluster.CreateToken(r.Context(), time.Duration(req.TTLMinutes)*time.Minute, req.Description, p.User.Username)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), p.actor(), "cluster.token_created", "enrollment_token", t.ID, "", map[string]any{"ttl_minutes": req.TTLMinutes})
	// The token secret is returned exactly once and is never retrievable again.
	writeJSON(w, 201, map[string]any{"token": tok, "info": t})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.cp.RevokeEnrollmentToken(r.Context(), id); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.audit.Record(r.Context(), principal(r).actor(), "cluster.token_revoked", "enrollment_token", id, "", nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) memberAction(action string, f func(*cluster.Manager, *http.Request, string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.clusterOrErr(w, r) {
			return
		}
		id := r.PathValue("id")
		p := principal(r)
		if err := f(s.cluster, r, id); err != nil {
			s.audit.Record(r.Context(), p.actor(), "cluster.node_"+action, "node", id, "failure", map[string]any{"error": err.Error()})
			if !isKnownClusterErr(err) {
				err = &APIError{Status: http.StatusConflict, Code: "cluster_change_failed", Message: err.Error()}
			}
			s.writeErr(w, r, err)
			return
		}
		s.audit.Record(r.Context(), p.actor(), "cluster.node_"+action, "node", id, "", nil)
		writeJSON(w, 200, map[string]any{"cluster": s.cluster.Status(r.Context())})
	}
}

func (s *Server) handlePromoteNode(w http.ResponseWriter, r *http.Request) {
	s.memberAction("promoted", func(m *cluster.Manager, r *http.Request, id string) error { return m.Promote(r.Context(), id) })(w, r)
}

func (s *Server) handleDemoteNode(w http.ResponseWriter, r *http.Request) {
	s.memberAction("demoted", func(m *cluster.Manager, r *http.Request, id string) error { return m.Demote(r.Context(), id) })(w, r)
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	s.memberAction("removed", func(m *cluster.Manager, r *http.Request, id string) error { return m.Remove(r.Context(), id) })(w, r)
}
