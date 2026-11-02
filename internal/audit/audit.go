// Package audit records security-relevant actions.
package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/model"
)

type Store interface {
	InsertAudit(ctx context.Context, a *model.AuditEntry) error
}

type Logger struct {
	store Store
	log   *slog.Logger
}

func New(store Store, log *slog.Logger) *Logger { return &Logger{store: store, log: log} }

// Actor identifies who performed an action.
type Actor struct {
	ID   string
	Name string
	IP   string
}

// System is the actor for automated actions.
var System = Actor{Name: "system"}

// Record persists an audit entry. Failures are logged but never silently dropped.
func (l *Logger) Record(ctx context.Context, actor Actor, action, targetType, targetID, outcome string, details map[string]any) {
	if outcome == "" {
		outcome = "success"
	}
	e := &model.AuditEntry{ID: uuid.NewString(), At: time.Now().UTC(), ActorID: actor.ID, ActorName: actor.Name, Action: action,
		TargetType: targetType, TargetID: targetID, IP: actor.IP, Outcome: outcome, Details: details}
	if err := l.store.InsertAudit(context.WithoutCancel(ctx), e); err != nil {
		l.log.Error("audit write failed", "action", action, "err", err)
	}
	l.log.Info("audit", "action", action, "actor", actor.Name, "target_type", targetType, "target_id", targetID, "outcome", outcome)
}
