package cluster

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/raft"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

// Errors returned to the API layer.
var (
	ErrAlreadyClustered = errors.New("this node is already part of a cluster")
	ErrNotClustered     = errors.New("this node is not part of a cluster")
	ErrDataPresent      = errors.New("this node has local users or monitors; choose to export or discard them before joining")
	ErrBadRequest       = errors.New("invalid request")
)

func validAdvertise(a string) error {
	host, port, err := net.SplitHostPort(a)
	if err != nil || host == "" {
		return fmt.Errorf("%w: advertise address must be host:port", ErrBadRequest)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%w: invalid port", ErrBadRequest)
	}
	if host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("%w: advertise address must be reachable by peers, not a wildcard", ErrBadRequest)
	}
	return nil
}

func (m *Manager) resolveAdvertise(a string) (string, error) {
	if a == "" {
		a = m.opt.Advertise
	}
	if a == "" {
		return "", fmt.Errorf("%w: an advertise address (host:port) is required", ErrBadRequest)
	}
	return a, validAdvertise(a)
}

// Init founds a new single-voter cluster from this standalone node. Existing data becomes the
// cluster's initial state. Secrets are re-encrypted with a new cluster data key.
func (m *Manager) Init(ctx context.Context, advertise string) error {
	if m.Enabled() {
		return ErrAlreadyClustered
	}
	adv, err := m.resolveAdvertise(advertise)
	if err != nil {
		return err
	}
	clusterID := uuid.NewString()
	caPEM, caKeyPEM, err := NewCA(clusterID)
	if err != nil {
		return err
	}
	keyPEM, csrPEM, err := NewNodeKey(m.opt.Node.Identity.ID)
	if err != nil {
		return err
	}
	host, _, _ := net.SplitHostPort(adv)
	certPEM, err := SignNode(caPEM, caKeyPEM, csrPEM, m.opt.Node.Identity.ID, []string{host})
	if err != nil {
		return err
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return err
	}
	newBox, _ := security.NewSecretBox(dataKey)
	// Re-encrypt existing monitor secrets from the node master key to the cluster data key.
	if err := m.opt.Store.ReplaceSecretBlobs(ctx, func(id string, blob []byte) ([]byte, error) {
		aad := []byte("monitor:" + id)
		pt, err := m.opt.DataBox.Open(blob, aad)
		if err != nil {
			return nil, fmt.Errorf("re-encrypt monitor %s: %w", id, err)
		}
		return newBox.Seal(pt, aad)
	}); err != nil {
		return err
	}
	sealedCAKey, err := newBox.Seal(caKeyPEM, []byte("cluster:ca-key"))
	if err != nil {
		return err
	}
	for k, v := range map[string]string{keyCID: clusterID, keyCACert: string(caPEM), keyCAKey: string(sealedCAKey)} {
		if err := m.opt.Store.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	if err := m.writeTLS(caPEM, certPEM, keyPEM); err != nil {
		return err
	}
	if err := m.persistLocal(ctx, clusterID, adv, dataKey); err != nil {
		return err
	}
	if err := m.useDataKey(dataKey); err != nil {
		return err
	}
	m.clusterID, m.advertise = clusterID, adv
	dump, err := m.opt.Store.DumpReplicated(ctx)
	if err != nil {
		return err
	}
	if err := m.opt.Store.SetSetting(ctx, keyMode, "1"); err != nil {
		return err
	}
	if err := m.start(true); err != nil {
		return err
	}
	if err := m.waitLeader(ctx, 20*time.Second); err != nil {
		return err
	}
	// Seed the replicated log with the full initial state so that every future member, which
	// replays the log from the beginning, starts from the same data.
	if err := m.opt.CP.RestoreState(ctx, dump); err != nil {
		return fmt.Errorf("seed cluster state: %w", err)
	}
	return m.opt.CP.UpsertClusterNode(ctx, m.selfMeta())
}

func (m *Manager) selfMeta() *model.ClusterNode {
	return &model.ClusterNode{ID: m.opt.Node.Identity.ID, Name: m.opt.Node.Identity.Name, ClusterAddr: m.advertise,
		APIURL: m.opt.APIURL, Location: m.opt.Location, Version: node.Version}
}

func (m *Manager) waitLeader(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if r := m.raftNode(); r != nil && r.State() == raft.Leader && m.GateOK() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("timed out waiting for leadership")
}

// ---- tokens ----

// CreateToken issues a single-use join token valid for ttl.
func (m *Manager) CreateToken(ctx context.Context, ttl time.Duration, description, createdBy string) (string, *model.EnrollmentToken, error) {
	if !m.Enabled() {
		return "", nil, ErrNotClustered
	}
	if ttl < 5*time.Minute || ttl > 24*time.Hour {
		return "", nil, fmt.Errorf("%w: token lifetime must be between 5 minutes and 24 hours", ErrBadRequest)
	}
	fp, err := Fingerprint(m.caPEM)
	if err != nil {
		return "", nil, err
	}
	secret := security.RandomToken(32)
	now := time.Now().UTC()
	t := &model.EnrollmentToken{ID: uuid.NewString(), SecretHash: security.HashToken(secret), Description: strings.TrimSpace(description),
		CreatedBy: createdBy, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	if err := m.opt.CP.CreateEnrollmentToken(ctx, t); err != nil {
		return "", nil, err
	}
	addrs := []string{m.advertise}
	for _, s := range m.servers() {
		if s.Suffrage == raft.Voter && string(s.Address) != m.advertise {
			addrs = append(addrs, string(s.Address))
		}
	}
	jt := &JoinToken{ClusterID: m.clusterID, TokenID: t.ID, Secret: secret, CAFingerprint: fp, Addrs: addrs}
	return jt.String(), t, nil
}

// ---- join (leader side) ----

type joinRequest struct {
	TokenID   string `json:"token_id"`
	Secret    string `json:"secret"`
	NodeID    string `json:"node_id"`
	NodeName  string `json:"node_name"`
	Advertise string `json:"advertise"`
	APIURL    string `json:"api_url"`
	Location  string `json:"location"`
	Version   string `json:"version"`
	CSR       []byte `json:"csr"`
	KeyPub    []byte `json:"key_pub"`
}

type joinResponse struct {
	ClusterID  string      `json:"cluster_id"`
	CACert     []byte      `json:"ca_cert"`
	NodeCert   []byte      `json:"node_cert"`
	WrappedKey *WrappedKey `json:"wrapped_key"`
	LeaderID   string      `json:"leader_id"`
}

func keyInfo(clusterID string) string { return "intrabin-cluster-data-key:" + clusterID }

func (m *Manager) handleJoin(w http.ResponseWriter, r *http.Request) {
	if !m.joinLimit.allow(remoteIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, rpcError{Code: "rate_limited", Message: "too many join attempts"})
		return
	}
	var req joinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError{Code: "bad_request", Message: "invalid join request"})
		return
	}
	if rn := m.raftNode(); rn == nil || rn.State() != raft.Leader {
		m.notLeader(w)
		return
	}
	if _, err := uuid.Parse(req.NodeID); err != nil || validAdvertise(req.Advertise) != nil || len(req.NodeName) > 100 || len(req.APIURL) > 300 || len(req.Location) > 100 {
		writeJSON(w, http.StatusBadRequest, rpcError{Code: "bad_request", Message: "invalid node id, name or advertise address"})
		return
	}
	if _, ok := m.isMember(req.NodeID); ok {
		writeJSON(w, http.StatusConflict, rpcError{Code: "already_member", Message: "a node with this ID is already a member"})
		return
	}
	ctx := r.Context()
	if err := m.opt.CP.ConsumeEnrollmentToken(ctx, req.TokenID, security.HashToken(req.Secret), req.NodeID); err != nil {
		code, status := "token_invalid", http.StatusUnauthorized
		switch {
		case errors.Is(err, storage.ErrTokenUsed):
			code = "token_used"
		case errors.Is(err, storage.ErrTokenExpired):
			code = "token_expired"
		case errors.Is(err, storage.ErrTokenInvalid):
		default:
			code, status = "unavailable", http.StatusServiceUnavailable
		}
		m.log.Warn("join rejected", "remote", remoteIP(r), "node_id", req.NodeID, "reason", code)
		writeJSON(w, status, rpcError{Code: code, Message: "join rejected: " + strings.ReplaceAll(code, "_", " ")})
		return
	}
	caKeyPEM, err := m.caKey(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, rpcError{Code: "internal", Message: "CA unavailable"})
		return
	}
	host, _, _ := net.SplitHostPort(req.Advertise)
	certPEM, err := SignNode(m.caPEM, caKeyPEM, req.CSR, req.NodeID, []string{host})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError{Code: "bad_csr", Message: "invalid certificate request"})
		return
	}
	wk, err := WrapKey(req.KeyPub, m.dataKey, keyInfo(m.clusterID))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, rpcError{Code: "bad_key", Message: "invalid key exchange public key"})
		return
	}
	// New members always start as non-voters: they cannot affect quorum until an operator
	// promotes them after they have caught up and proven connectivity.
	if err := m.raftNode().AddNonvoter(raft.ServerID(req.NodeID), raft.ServerAddress(req.Advertise), 0, 10*time.Second).Error(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, rpcError{Code: "unavailable", Message: "could not add member: " + err.Error()})
		return
	}
	_ = m.opt.CP.UpsertClusterNode(ctx, &model.ClusterNode{ID: req.NodeID, Name: req.NodeName, ClusterAddr: req.Advertise, APIURL: req.APIURL, Location: req.Location, Version: req.Version})
	_, lid := m.leaderAddr()
	m.log.Info("node joined as non-voter", "node_id", req.NodeID, "name", req.NodeName, "addr", req.Advertise)
	writeJSON(w, http.StatusOK, joinResponse{ClusterID: m.clusterID, CACert: m.caPEM, NodeCert: certPEM, WrappedKey: wk, LeaderID: lid})
}

func (m *Manager) caKey(ctx context.Context) ([]byte, error) {
	v, err := m.opt.Store.GetSetting(ctx, keyCAKey)
	if err != nil {
		return nil, err
	}
	return m.opt.DataBox.Open([]byte(v), []byte("cluster:ca-key"))
}

// ---- join (joining node side) ----

// DataAction says what to do with existing local data when joining.
type DataAction string

const (
	DataNone    DataAction = ""        // only allowed when the node holds no users or monitors
	DataExport  DataAction = "export"  // write a backup file, then replace with cluster state
	DataDiscard DataAction = "discard" // replace with cluster state without a backup
)

// JoinResult reports what happened during a join.
type JoinResult struct {
	ClusterID  string `json:"cluster_id"`
	BackupPath string `json:"backup_path,omitempty"`
}

// Join enrolls this standalone node into an existing cluster.
func (m *Manager) Join(ctx context.Context, token, advertise string, action DataAction) (*JoinResult, error) {
	if m.Enabled() {
		return nil, ErrAlreadyClustered
	}
	jt, err := ParseJoinToken(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	adv, err := m.resolveAdvertise(advertise)
	if err != nil {
		return nil, err
	}
	users, monitors, err := m.opt.Store.HasUserData(ctx)
	if err != nil {
		return nil, err
	}
	hasData := users > 0 || monitors > 0
	if hasData && action != DataExport && action != DataDiscard {
		return nil, ErrDataPresent
	}
	keyPEM, csrPEM, err := NewNodeKey(m.opt.Node.Identity.ID)
	if err != nil {
		return nil, err
	}
	xpriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	req := joinRequest{TokenID: jt.TokenID, Secret: jt.Secret, NodeID: m.opt.Node.Identity.ID, NodeName: m.opt.Node.Identity.Name,
		Advertise: adv, APIURL: m.opt.APIURL, Location: m.opt.Location, Version: node.Version, CSR: csrPEM, KeyPub: xpriv.PublicKey().Bytes()}
	resp, err := m.sendJoin(ctx, jt, req)
	if err != nil {
		return nil, err
	}
	if fp, _ := Fingerprint(resp.CACert); fp != jt.CAFingerprint {
		return nil, errors.New("cluster CA does not match the token fingerprint")
	}
	dataKey, err := UnwrapKey(xpriv, resp.WrappedKey, keyInfo(resp.ClusterID))
	if err != nil {
		return nil, fmt.Errorf("unwrap cluster key: %w", err)
	}
	res := &JoinResult{ClusterID: resp.ClusterID}
	if hasData && action == DataExport {
		res.BackupPath = filepath.Join(m.opt.DataDir, "pre-join-backup-"+time.Now().UTC().Format("20060102T150405Z")+".db")
		if err := m.opt.Store.Backup(ctx, res.BackupPath); err != nil {
			return nil, fmt.Errorf("export before join failed (node is a non-voting member but has not switched; fix and retry with a new token): %w", err)
		}
	}
	if err := m.writeTLS(resp.CACert, resp.NodeCert, keyPEM); err != nil {
		return nil, err
	}
	if err := m.persistLocal(ctx, resp.ClusterID, adv, dataKey); err != nil {
		return nil, err
	}
	if err := m.opt.Store.WipeReplicated(ctx); err != nil {
		return nil, err
	}
	_ = m.opt.Store.SetSetting(ctx, "local.raft_applied_index", "0")
	if err := m.useDataKey(dataKey); err != nil {
		return nil, err
	}
	m.clusterID, m.advertise = resp.ClusterID, adv
	if err := m.opt.Store.SetSetting(ctx, keyMode, "1"); err != nil {
		return nil, err
	}
	m.opt.CP.X.NotifyExternal("state.restore")
	if err := m.start(false); err != nil {
		return nil, err
	}
	m.log.Info("joined cluster as non-voter", "cluster_id", resp.ClusterID, "export", res.BackupPath)
	return res, nil
}

func (m *Manager) sendJoin(ctx context.Context, jt *JoinToken, req joinRequest) (*joinResponse, error) {
	verify := func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		pool := x509.NewCertPool()
		var leaf *x509.Certificate
		for i, raw := range rawCerts {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return err
			}
			if i == 0 {
				leaf = c
			}
			if fp, _ := Fingerprint(pemEncode("CERTIFICATE", raw)); fp == jt.CAFingerprint {
				pool.AddCert(c)
			}
		}
		if leaf == nil {
			return errors.New("no server certificate")
		}
		_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: PeerServerName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if err != nil {
			return fmt.Errorf("server is not part of the expected cluster: %w", err)
		}
		return nil
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify:    true, // verification is done by VerifyPeerCertificate against the pinned CA fingerprint
		VerifyPeerCertificate: verify, NextProtos: []string{alpnHTTP}, MinVersion: tls.VersionTLS13,
	}}}
	body, _ := json.Marshal(req)
	addrs := append([]string{}, jt.Addrs...)
	var lastErr error
	tried := map[string]bool{}
	for len(addrs) > 0 {
		addr := addrs[0]
		addrs = addrs[1:]
		if tried[addr] {
			continue
		}
		tried[addr] = true
		hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/cluster/v1/join", bytes.NewReader(body))
		hreq.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(hreq)
		if err != nil {
			lastErr = err
			continue
		}
		var out joinResponse
		var e rpcError
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&out)
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			return &out, nil
		}
		_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<16)).Decode(&e)
		resp.Body.Close()
		if e.Code == "not_leader" && e.LeaderAddr != "" {
			addrs = append([]string{e.LeaderAddr}, addrs...)
			lastErr = errors.New("contacted a follower; retrying the leader")
			continue
		}
		return nil, fmt.Errorf("join rejected by cluster: %s", e.Message)
	}
	if lastErr == nil {
		lastErr = errors.New("no reachable cluster address in token")
	}
	return nil, fmt.Errorf("could not reach the cluster: %w", lastErr)
}

// ---- membership changes (leader only) ----

func (m *Manager) requireLeader() (*raft.Raft, error) {
	r := m.raftNode()
	if r == nil {
		return nil, ErrNotClustered
	}
	if r.State() != raft.Leader {
		return nil, errors.New("membership changes must be made on the leader")
	}
	if !m.GateOK() {
		return nil, errors.New("mesh safety gate is closed; membership changes are blocked")
	}
	return r, nil
}

// Promote makes a caught-up, reachable non-voter a voter.
func (m *Manager) Promote(ctx context.Context, id string) error {
	r, err := m.requireLeader()
	if err != nil {
		return err
	}
	s, ok := m.isMember(id)
	if !ok {
		return storage.ErrNotFound
	}
	if s.Suffrage == raft.Voter {
		return fmt.Errorf("%w: node is already a voter", ErrBadRequest)
	}
	if !m.mesh.bidirectional(id, m.opt.MeshFreshness) {
		return fmt.Errorf("%w: node is not reachable in both directions; it cannot be promoted", ErrBadRequest)
	}
	m.mesh.mu.Lock()
	v := m.mesh.views[id]
	m.mesh.mu.Unlock()
	if lag := int64(r.AppliedIndex()) - int64(v.Applied); lag > 50 {
		return fmt.Errorf("%w: node is still catching up (%d entries behind)", ErrBadRequest, lag)
	}
	return r.AddVoter(s.ID, s.Address, 0, 10*time.Second).Error()
}

// Demote turns a voter into a non-voter.
func (m *Manager) Demote(ctx context.Context, id string) error {
	r, err := m.requireLeader()
	if err != nil {
		return err
	}
	if id == m.opt.Node.Identity.ID {
		return fmt.Errorf("%w: the leader cannot demote itself", ErrBadRequest)
	}
	s, ok := m.isMember(id)
	if !ok {
		return storage.ErrNotFound
	}
	if s.Suffrage != raft.Voter {
		return fmt.Errorf("%w: node is not a voter", ErrBadRequest)
	}
	return r.DemoteVoter(s.ID, 0, 10*time.Second).Error()
}

// Remove removes a member. Removing a member never happens automatically.
func (m *Manager) Remove(ctx context.Context, id string) error {
	r, err := m.requireLeader()
	if err != nil {
		return err
	}
	if id == m.opt.Node.Identity.ID {
		return fmt.Errorf("%w: the leader cannot remove itself", ErrBadRequest)
	}
	if _, ok := m.isMember(id); !ok {
		return storage.ErrNotFound
	}
	if err := r.RemoveServer(raft.ServerID(id), 0, 10*time.Second).Error(); err != nil {
		return err
	}
	return m.opt.CP.DeleteClusterNode(ctx, id)
}

// LeaderAPIURL returns the leader's public UI/API URL, if known.
func (m *Manager) LeaderAPIURL(ctx context.Context) string {
	_, id := m.leaderAddr()
	if id == "" {
		return ""
	}
	nodes, _ := m.opt.Store.ListClusterNodes(ctx)
	for _, n := range nodes {
		if n.ID == id {
			return n.APIURL
		}
	}
	return ""
}
