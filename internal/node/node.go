// Package node manages the persistent node identity and the effective node role.
package node

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/storage"
)

// Role is the effective runtime role of this installation.
type Role string

const (
	RoleStandalone Role = "STANDALONE"
	RoleFollower   Role = "FOLLOWER"
	RoleLeader     Role = "LEADER"
	RoleNoQuorum   Role = "NO_QUORUM"
)

// Version is set at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// Identity is the persistent identity of this node.
type Identity struct {
	ID        string    `json:"node_id"`
	Name      string    `json:"node_name"`
	CreatedAt time.Time `json:"created_at"`
}

// Node tracks identity and role.
type Node struct {
	Identity  Identity
	StartedAt time.Time
	role      atomic.Value
}

// LoadIdentity loads or creates the node identity stored in the database.
func LoadIdentity(ctx context.Context, st *storage.Store, nameOverride string) (*Node, error) {
	id, err := st.GetSetting(ctx, "node.id")
	if errors.Is(err, storage.ErrNotFound) {
		id = uuid.NewString()
		if err := st.SetSetting(ctx, "node.id", id); err != nil {
			return nil, err
		}
		if err := st.SetSetting(ctx, "node.created_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	name := nameOverride
	if name == "" {
		name, _ = st.GetSetting(ctx, "node.name")
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "intrabin-" + id[:8]
	}
	created, _ := st.GetSetting(ctx, "node.created_at")
	ct, _ := time.Parse(time.RFC3339, created)
	n := &Node{Identity: Identity{ID: id, Name: name, CreatedAt: ct}, StartedAt: time.Now().UTC()}
	n.role.Store(RoleStandalone)
	return n, nil
}

func (n *Node) Role() Role     { return n.role.Load().(Role) }
func (n *Node) SetRole(r Role) { n.role.Store(r) }

// CanManage reports whether the full management UI/API is active on this node.
func (n *Node) CanManage() bool {
	r := n.Role()
	return r == RoleStandalone || r == RoleLeader
}
