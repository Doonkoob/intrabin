// Package cluster implements Intrabin clustering: Raft consensus (hashicorp/raft) for the
// control plane, a cluster CA with per-node mTLS, single-use enrollment tokens, a strict mesh
// safety gate, follower result shipping, and write forwarding for session/audit operations.
package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/intrabin/intrabin/internal/controlplane"
	"github.com/intrabin/intrabin/internal/node"
	"github.com/intrabin/intrabin/internal/security"
	"github.com/intrabin/intrabin/internal/storage"
)

// Local (non-replicated) settings.
const (
	keyMode      = "local.cluster_mode"
	keyID        = "local.cluster_id"
	keyAdvertise = "local.cluster_advertise"
	keyDataKey   = "local.cluster_data_key" // sealed with the node master key
)

// Replicated settings.
const (
	keyCACert = "cluster.ca_cert"
	keyCAKey  = "cluster.ca_key" // sealed with the cluster data key
	keyCID    = "cluster.id"
)

// Options configures a Manager.
type Options struct {
	DataDir   string
	Listen    string // cluster port bind address, e.g. ":7443"
	Advertise string // default advertise address (host:port reachable by peers)
	APIURL    string // public URL of this node's UI/API (shown to users on other nodes)
	Location  string
	Store     *storage.Store
	CP        *controlplane.CP
	DataBox   *security.SecretBox // encrypts replicated secrets; switched to the cluster data key
	MasterBox *security.SecretBox // node master key; wraps the cluster data key at rest
	Node      *node.Node
	Log       *slog.Logger

	// MeshFreshness is how recent a bidirectional reachability proof must be (default 5s).
	MeshFreshness time.Duration
	// Fast shortens Raft timeouts (tests only).
	Fast bool
}

// Manager owns this node's cluster participation.
type Manager struct {
	opt Options
	log *slog.Logger

	mu        sync.Mutex
	enabled   atomic.Bool
	r         *raft.Raft
	trans     *raft.NetworkTransport
	bolt      *raftboltdb.BoltStore
	mux       *mux
	httpSrv   *http.Server
	cert      tls.Certificate
	caPool    *x509.CertPool
	caPEM     []byte
	clusterID string
	dataKey   []byte
	advertise string
	fsm       *fsm
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	mesh      *mesh
	shipKick  chan struct{}
	peerHTTP  *http.Client
	joinLimit *rateWindow
	roleHooks []func(node.Role)

	locMu sync.Mutex
	locs  map[string]string
	locAt time.Time
}

// New creates a disabled manager. Call Resume at startup to rejoin an existing cluster.
func New(opt Options) *Manager {
	if opt.MeshFreshness <= 0 {
		opt.MeshFreshness = 5 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Manager{opt: opt, log: opt.Log.With("component", "cluster"), mesh: newMesh(), shipKick: make(chan struct{}, 1), joinLimit: newRateWindow(20, time.Minute)}
}

// OnRoleChange registers a callback for effective role changes.
func (m *Manager) OnRoleChange(f func(node.Role)) { m.roleHooks = append(m.roleHooks, f) }

// Enabled reports whether this node is part of a cluster.
func (m *Manager) Enabled() bool { return m.enabled.Load() }

func (m *Manager) dir(parts ...string) string {
	return filepath.Join(append([]string{m.opt.DataDir}, parts...)...)
}

// Resume restarts cluster participation after a process restart.
func (m *Manager) Resume(ctx context.Context) error {
	mode, _ := m.opt.Store.GetSetting(ctx, keyMode)
	if mode != "1" {
		return nil
	}
	sealed, err := m.opt.Store.GetSetting(ctx, keyDataKey)
	if err != nil {
		return fmt.Errorf("cluster data key missing: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return err
	}
	key, err := m.opt.MasterBox.Open(raw, []byte("cluster-data-key"))
	if err != nil {
		return fmt.Errorf("unseal cluster data key (wrong master key?): %w", err)
	}
	m.clusterID, _ = m.opt.Store.GetSetting(ctx, keyID)
	m.advertise, _ = m.opt.Store.GetSetting(ctx, keyAdvertise)
	if err := m.useDataKey(key); err != nil {
		return err
	}
	return m.start(false)
}

func (m *Manager) useDataKey(key []byte) error {
	if err := m.opt.DataBox.SetKey(key); err != nil {
		return err
	}
	m.dataKey = key
	return nil
}

func (m *Manager) persistLocal(ctx context.Context, clusterID, advertise string, key []byte) error {
	sealed, err := m.opt.MasterBox.Seal(key, []byte("cluster-data-key"))
	if err != nil {
		return err
	}
	for k, v := range map[string]string{keyDataKey: base64.StdEncoding.EncodeToString(sealed), keyID: clusterID, keyAdvertise: advertise} {
		if err := m.opt.Store.SetSetting(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) loadTLS() error {
	cert, err := tls.LoadX509KeyPair(m.dir("cluster", "node.crt"), m.dir("cluster", "node.key"))
	if err != nil {
		return fmt.Errorf("load node certificate: %w", err)
	}
	caPEM, err := os.ReadFile(m.dir("cluster", "ca.crt"))
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("invalid cluster CA certificate")
	}
	m.cert, m.caPool, m.caPEM = cert, pool, caPEM
	return nil
}

func (m *Manager) writeTLS(caPEM, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(m.dir("cluster"), 0o700); err != nil {
		return err
	}
	chain := append(append([]byte{}, certPEM...), caPEM...)
	if err := os.WriteFile(m.dir("cluster", "node.key"), keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(m.dir("cluster", "node.crt"), chain, 0o644); err != nil {
		return err
	}
	return os.WriteFile(m.dir("cluster", "ca.crt"), caPEM, 0o644)
}

func (m *Manager) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{m.cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    m.caPool,
		NextProtos:   []string{alpnRaft, alpnHTTP},
		MinVersion:   tls.VersionTLS13,
	}
}

func (m *Manager) clientTLS(proto string) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{m.cert},
		RootCAs:      m.caPool,
		ServerName:   PeerServerName,
		NextProtos:   []string{proto},
		MinVersion:   tls.VersionTLS13,
	}
}

type strAddr string

func (a strAddr) Network() string { return "tcp" }
func (a strAddr) String() string  { return string(a) }

func (m *Manager) hclogger() hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{Name: "raft", Level: hclog.Warn, Output: os.Stderr, JSONFormat: true})
}

// start brings up the listener, transport and Raft. bootstrap is true only when founding a cluster.
func (m *Manager) start(bootstrap bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.r != nil {
		return errors.New("cluster already running")
	}
	if err := m.loadTLS(); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", m.opt.Listen)
	if err != nil {
		return fmt.Errorf("cluster listen %s: %w", m.opt.Listen, err)
	}
	m.mux = newMux(ln, m.serverTLS(), strAddr(m.advertise), m.log)
	stream := &streamLayer{chanListener: m.mux.raft, client: m.clientTLS}
	m.trans = raft.NewNetworkTransportWithLogger(stream, 3, 10*time.Second, m.hclogger())

	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(m.opt.Node.Identity.ID)
	cfg.Logger = m.hclogger()
	cfg.SnapshotThreshold = 4096
	cfg.TrailingLogs = 4096
	cfg.HeartbeatTimeout = 1000 * time.Millisecond
	cfg.ElectionTimeout = 1000 * time.Millisecond
	cfg.LeaderLeaseTimeout = 500 * time.Millisecond
	cfg.CommitTimeout = 50 * time.Millisecond
	if m.opt.Fast {
		cfg.HeartbeatTimeout = 300 * time.Millisecond
		cfg.ElectionTimeout = 300 * time.Millisecond
		cfg.LeaderLeaseTimeout = 200 * time.Millisecond
	}
	if err := os.MkdirAll(m.dir("raft"), 0o700); err != nil {
		return err
	}
	m.bolt, err = raftboltdb.NewBoltStore(m.dir("raft", "raft.db"))
	if err != nil {
		return err
	}
	snaps, err := raft.NewFileSnapshotStoreWithLogger(m.dir("raft"), 2, m.hclogger())
	if err != nil {
		return err
	}
	m.fsm = newFSM(m.opt.CP.X, m.opt.Store, m.log)
	r, err := raft.NewRaft(cfg, m.fsm, m.bolt, m.bolt, snaps, m.trans)
	if err != nil {
		m.bolt.Close()
		m.mux.Close()
		return err
	}
	if bootstrap {
		f := r.BootstrapCluster(raft.Configuration{Servers: []raft.Server{{Suffrage: raft.Voter, ID: cfg.LocalID, Address: raft.ServerAddress(m.advertise)}}})
		if err := f.Error(); err != nil && !errors.Is(err, raft.ErrCantBootstrap) {
			return err
		}
	}
	m.r = r
	m.peerHTTP = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: m.clientTLS(alpnHTTP), MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}}
	m.httpSrv = &http.Server{Handler: m.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go m.httpSrv.Serve(m.mux.http) //nolint:errcheck
	m.opt.CP.SetApplier(m)
	m.enabled.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(3)
	go m.roleLoop(ctx)
	go m.meshLoop(ctx)
	go m.shipLoop(ctx)
	m.log.Info("cluster started", "cluster_id", m.clusterID, "advertise", m.advertise, "listen", m.opt.Listen, "bootstrap", bootstrap)
	return nil
}

// Shutdown stops Raft and the cluster listener (the node keeps its cluster state on disk).
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.r == nil {
		return
	}
	m.cancel()
	_ = m.r.Shutdown().Error()
	m.trans.Close()
	_ = m.httpSrv.Close()
	m.mux.Close()
	m.wg.Wait()
	m.bolt.Close()
	m.r = nil
	m.enabled.Store(false)
}

func (m *Manager) raftNode() *raft.Raft {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.r
}

// ---- role ----

func (m *Manager) computeRole() node.Role {
	r := m.raftNode()
	if r == nil {
		return node.RoleStandalone
	}
	switch r.State() {
	case raft.Leader:
		if m.GateOK() {
			return node.RoleLeader
		}
		return node.RoleNoQuorum
	default:
		_, id := r.LeaderWithID()
		if id != "" && time.Since(r.LastContact()) < m.opt.MeshFreshness {
			return node.RoleFollower
		}
		return node.RoleNoQuorum
	}
}

func (m *Manager) roleLoop(ctx context.Context) {
	defer m.wg.Done()
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			nr := m.computeRole()
			if old := m.opt.Node.Role(); old != nr {
				m.opt.Node.SetRole(nr)
				m.log.Warn("cluster role changed", "from", old, "to", nr)
				for _, h := range m.roleHooks {
					h(nr)
				}
				if nr == node.RoleLeader {
					m.kickShip()
				}
			}
		}
	}
}

// servers returns the current Raft configuration.
func (m *Manager) servers() []raft.Server {
	r := m.raftNode()
	if r == nil {
		return nil
	}
	f := r.GetConfiguration()
	if f.Error() != nil {
		return nil
	}
	return f.Configuration().Servers
}

func (m *Manager) isMember(id string) (raft.Server, bool) {
	for _, s := range m.servers() {
		if string(s.ID) == id {
			return s, true
		}
	}
	return raft.Server{}, false
}

// GateOK is the strict mesh safety gate: the leader may evaluate and dispatch mutations only
// while it holds fresh bidirectional reachability proofs with a majority of configured voters
// (itself included). This is in addition to, never instead of, Raft's own majority rule.
func (m *Manager) GateOK() bool {
	ok, _, _ := m.gate()
	return ok
}

func (m *Manager) gate() (ok bool, have, need int) {
	self := m.opt.Node.Identity.ID
	voters := 0
	for _, s := range m.servers() {
		if s.Suffrage != raft.Voter {
			continue
		}
		voters++
		if string(s.ID) == self || m.mesh.bidirectional(string(s.ID), m.opt.MeshFreshness) {
			have++
		}
	}
	need = voters/2 + 1
	return voters > 0 && have >= need, have, need
}

// leaderAddr returns the current leader's cluster address and ID.
func (m *Manager) leaderAddr() (string, string) {
	r := m.raftNode()
	if r == nil {
		return "", ""
	}
	a, id := r.LeaderWithID()
	return string(a), string(id)
}
