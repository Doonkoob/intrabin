// Package controlplane is the single write path for replicated (control-plane) state.
//
// Every mutation is encoded as a Command with an explicit timestamp. In standalone mode the
// command is executed directly against the local SQLite store; in cluster mode it is committed
// through Raft and executed by the FSM on every node. Because execution only depends on the
// command payload (never on the local clock or random sources), all replicas converge.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// Command is one replicated mutation.
type Command struct {
	Op   string          `json:"op"`
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// Applier commits a command (locally or through consensus) and returns its execution error.
type Applier interface {
	Apply(ctx context.Context, cmd *Command) error
}

// ErrNotLeader is returned when a write is attempted on a node that cannot commit it.
var ErrNotLeader = errors.New("this node cannot accept writes (not the leader)")

// ErrNoQuorum is returned when the cluster cannot commit writes.
var ErrNoQuorum = errors.New("cluster has no quorum; writes are unavailable")

// Executor applies commands to the local store. It is shared by the local applier and the FSM.
type Executor struct {
	Store *storage.Store
	mu    sync.Mutex
	hooks []func(op string)
}

// OnApplied registers a callback invoked after each successfully applied command.
func (x *Executor) OnApplied(f func(op string)) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.hooks = append(x.hooks, f)
}

func (x *Executor) notify(op string) {
	x.mu.Lock()
	hs := append([]func(string){}, x.hooks...)
	x.mu.Unlock()
	for _, h := range hs {
		h(op)
	}
}

// Local executes commands directly (standalone mode).
type Local struct{ X *Executor }

func (l Local) Apply(ctx context.Context, cmd *Command) error { return l.X.Execute(ctx, cmd) }

// CP is the typed facade used by the API, incident manager and engine.
type CP struct {
	st *storage.Store
	X  *Executor
	ap atomic.Pointer[applierBox]
}

type applierBox struct{ a Applier }

// New creates a control plane executing locally until SetApplier is called.
func New(st *storage.Store) *CP {
	x := &Executor{Store: st}
	c := &CP{st: st, X: x}
	c.SetApplier(Local{X: x})
	return c
}

// SetApplier switches the commit path (e.g. to Raft when the node enters cluster mode).
func (c *CP) SetApplier(a Applier) { c.ap.Store(&applierBox{a: a}) }

func (c *CP) apply(ctx context.Context, op string, v any) (time.Time, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return time.Time{}, err
	}
	cmd := &Command{Op: op, At: time.Now().UTC().Truncate(time.Millisecond), Data: data}
	return cmd.At, c.ap.Load().a.Apply(ctx, cmd)
}

// Encode/Decode helpers used by the consensus layer.
func Encode(cmd *Command) ([]byte, error) { return json.Marshal(cmd) }
func Decode(b []byte) (*Command, error) {
	var c Command
	err := json.Unmarshal(b, &c)
	return &c, err
}

// ---- error codes (errors cross process boundaries when writes are forwarded) ----

var codeErrors = map[string]error{
	"not_found":     storage.ErrNotFound,
	"conflict":      storage.ErrConflict,
	"token_invalid": storage.ErrTokenInvalid,
	"token_used":    storage.ErrTokenUsed,
	"token_expired": storage.ErrTokenExpired,
	"not_leader":    ErrNotLeader,
	"no_quorum":     ErrNoQuorum,
}

// ErrorCode maps an execution error to a stable code.
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	for code, e := range codeErrors {
		if errors.Is(err, e) {
			return code
		}
	}
	return "error:" + err.Error()
}

// CodeError reverses ErrorCode.
func CodeError(code string) error {
	if code == "" {
		return nil
	}
	if e, ok := codeErrors[code]; ok {
		return e
	}
	return errors.New(strings.TrimPrefix(code, "error:"))
}

// ---- payloads ----

type userWire struct {
	model.User
	PasswordHash string `json:"password_hash"`
}

type monitorWire struct {
	model.Monitor
	SecretBlob []byte `json:"secret_blob"`
}

type idPayload struct {
	ID string `json:"id"`
}

type settingPayload struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type passwordPayload struct {
	UserID string `json:"user_id"`
	Hash   string `json:"hash"`
}

type touchPayload struct {
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

type expirePayload struct {
	Now  time.Time     `json:"now"`
	Idle time.Duration `json:"idle"`
}

type incidentPayload struct {
	Incident *model.Incident        `json:"incident"`
	Events   []*model.IncidentEvent `json:"events"`
}

type roundPayload struct {
	Round    *model.RoundResult  `json:"round"`
	State    *model.MonitorState `json:"state"`
	Interval int                 `json:"interval"`
}

type tokenPayload struct {
	Token      *model.EnrollmentToken `json:"token"`
	SecretHash string                 `json:"secret_hash"`
}

type consumePayload struct {
	ID         string `json:"id"`
	SecretHash string `json:"secret_hash"`
	NodeID     string `json:"node_id"`
}

type restorePayload struct {
	Dump []byte `json:"dump"`
}

// Execute runs one command against the local store.
func (x *Executor) Execute(ctx context.Context, cmd *Command) error {
	ctx = storage.WithNow(ctx, cmd.At)
	st := x.Store
	var err error
	dec := func(v any) bool {
		if e := json.Unmarshal(cmd.Data, v); e != nil {
			err = fmt.Errorf("decode %s: %w", cmd.Op, e)
			return false
		}
		return true
	}
	switch cmd.Op {
	case "setting.set":
		var p settingPayload
		if dec(&p) {
			if storage.IsLocalSetting(p.Key) {
				err = errors.New("local settings are not replicated")
			} else {
				err = st.SetSetting(ctx, p.Key, p.Value)
			}
		}
	case "user.create", "user.create_first":
		var p userWire
		if dec(&p) {
			u := p.User
			u.PasswordHash = p.PasswordHash
			if cmd.Op == "user.create" {
				err = st.CreateUser(ctx, &u)
			} else {
				err = st.CreateFirstUser(ctx, &u)
			}
		}
	case "user.update":
		var p userWire
		if dec(&p) {
			err = st.UpdateUser(ctx, &p.User)
		}
	case "user.set_password":
		var p passwordPayload
		if dec(&p) {
			err = st.SetPassword(ctx, p.UserID, p.Hash)
		}
	case "user.touch_login":
		var p idPayload
		if dec(&p) {
			err = st.TouchLogin(ctx, p.ID)
		}
	case "user.delete":
		var p idPayload
		if dec(&p) {
			err = st.DeleteUser(ctx, p.ID)
		}
	case "session.create":
		var p model.Session
		if dec(&p) {
			err = st.CreateSession(ctx, &p)
		}
	case "session.touch":
		var p touchPayload
		if dec(&p) {
			err = st.TouchSession(ctx, p.Hash, p.At)
		}
	case "session.delete":
		var p idPayload
		if dec(&p) {
			err = st.DeleteSession(ctx, p.ID)
		}
	case "session.delete_user":
		var p idPayload
		if dec(&p) {
			err = st.DeleteUserSessions(ctx, p.ID)
		}
	case "session.delete_expired":
		var p expirePayload
		if dec(&p) {
			_, err = st.DeleteExpiredSessions(ctx, p.Now, p.Idle)
		}
	case "monitor.create", "monitor.update":
		var p monitorWire
		if dec(&p) {
			m := p.Monitor
			m.SecretBlob = p.SecretBlob
			if cmd.Op == "monitor.create" {
				err = st.CreateMonitor(ctx, &m)
			} else {
				err = st.UpdateMonitor(ctx, &m)
			}
		}
	case "monitor.delete":
		var p idPayload
		if dec(&p) {
			err = st.DeleteMonitor(ctx, p.ID)
		}
	case "monitor.state":
		var p model.MonitorState
		if dec(&p) {
			err = st.SaveMonitorState(ctx, &p)
		}
	case "monitor.round":
		var p roundPayload
		if dec(&p) {
			err = st.RecordRound(ctx, p.Round, p.State, p.Interval)
		}
	case "group.create", "group.update":
		var p model.MonitorGroup
		if dec(&p) {
			if cmd.Op == "group.create" {
				err = st.CreateGroup(ctx, &p)
			} else {
				err = st.UpdateGroup(ctx, &p)
			}
		}
	case "group.delete":
		var p idPayload
		if dec(&p) {
			err = st.DeleteGroup(ctx, p.ID)
		}
	case "maintenance.create":
		var p model.MaintenanceWindow
		if dec(&p) {
			err = st.CreateMaintenance(ctx, &p)
		}
	case "maintenance.delete":
		var p idPayload
		if dec(&p) {
			err = st.DeleteMaintenance(ctx, p.ID)
		}
	case "incident.save":
		var p incidentPayload
		if dec(&p) {
			err = st.SaveIncident(ctx, p.Incident, p.Events...)
		}
	case "audit.insert":
		var p model.AuditEntry
		if dec(&p) {
			err = st.InsertAudit(ctx, &p)
		}
	case "token.create":
		var p tokenPayload
		if dec(&p) {
			p.Token.SecretHash = p.SecretHash
			err = st.CreateEnrollmentToken(ctx, p.Token)
		}
	case "token.consume":
		var p consumePayload
		if dec(&p) {
			err = st.ConsumeEnrollmentToken(ctx, p.ID, p.SecretHash, p.NodeID)
		}
	case "token.revoke":
		var p idPayload
		if dec(&p) {
			err = st.RevokeEnrollmentToken(ctx, p.ID)
		}
	case "node.upsert":
		var p model.ClusterNode
		if dec(&p) {
			err = st.UpsertClusterNode(ctx, &p)
		}
	case "node.delete":
		var p idPayload
		if dec(&p) {
			err = st.DeleteClusterNode(ctx, p.ID)
		}
	case "state.restore":
		var p restorePayload
		if dec(&p) {
			err = st.RestoreReplicated(ctx, p.Dump)
		}
	default:
		err = fmt.Errorf("unknown control-plane operation %q", cmd.Op)
	}
	if err == nil {
		x.notify(cmd.Op)
	}
	return err
}

// ForwardableOps may be submitted by a follower on behalf of its local users (login, logout,
// session activity, audit). Everything else must be issued on the leader.
var ForwardableOps = map[string]bool{
	"session.create": true, "session.touch": true, "session.delete": true,
	"user.touch_login": true, "audit.insert": true,
}

// NotifyExternal runs the applied hooks for a state change not made through Execute (snapshot restore).
func (x *Executor) NotifyExternal(op string) { x.notify(op) }
