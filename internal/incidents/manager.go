package incidents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/intrabin/intrabin/internal/model"
	"github.com/intrabin/intrabin/internal/storage"
)

// Store is the persistence required by the manager.
type Store interface {
	OpenIncidentForMonitor(ctx context.Context, monitorID string) (*model.Incident, error)
	LastResolvedIncident(ctx context.Context, monitorID string) (*model.Incident, error)
	GetIncident(ctx context.Context, id string) (*model.Incident, error)
	SaveIncident(ctx context.Context, in *model.Incident, events ...*model.IncidentEvent) error
}

// Listener is notified after a committed incident transition (used by automation in Phase 3).
type Listener func(ctx context.Context, in *model.Incident, from, to model.IncidentStatus)

// Manager drives incident lifecycle from monitor status transitions and user actions.
type Manager struct {
	store     Store
	log       *slog.Logger
	now       func() time.Time
	mu        sync.Mutex // serialises transitions
	listeners []Listener
}

func NewManager(store Store, log *slog.Logger) *Manager {
	return &Manager{store: store, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// OnTransition registers a listener.
func (m *Manager) OnTransition(l Listener) { m.listeners = append(m.listeners, l) }

func (m *Manager) notify(ctx context.Context, in *model.Incident, from, to model.IncidentStatus) {
	for _, l := range m.listeners {
		l(ctx, in, from, to)
	}
}

func newEvent(in *model.Incident, typ string, from, to model.IncidentStatus, actor, msg string, at time.Time, details map[string]any) *model.IncidentEvent {
	return &model.IncidentEvent{ID: uuid.NewString(), IncidentID: in.ID, At: at, Type: typ, FromStatus: from, ToStatus: to, Actor: actor, Message: msg, Details: details}
}

// MonitorChange carries the context of a monitor status transition.
type MonitorChange struct {
	Monitor      *model.Monitor
	From, To     model.MonitorStatus
	Round        *model.RoundResult
	FirstFailure time.Time // start of the first failed round in this streak
	Category     string
}

// HandleMonitorChange applies incident logic for a monitor status change.
func (m *Manager) HandleMonitorChange(ctx context.Context, c MonitorChange) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	open, err := m.store.OpenIncidentForMonitor(ctx, c.Monitor.ID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	details := map[string]any{"monitor_status": c.To}
	if c.Round != nil {
		details["round_start"] = c.Round.RoundStart
		details["failing_nodes"] = c.Round.FailingNodes
		details["valid_probes"] = c.Round.ValidProbes
		details["failing_probes"] = c.Round.FailingProbes
	}
	switch c.To {
	case model.StatusSuspectedDown, model.StatusDown:
		target := model.IncidentSuspected
		if c.To == model.StatusDown {
			target = model.IncidentConfirmed
		}
		if open == nil {
			// Deduplicate: reopen a recently resolved incident within the cooldown.
			last, err := m.store.LastResolvedIncident(ctx, c.Monitor.ID)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			cooldown := time.Duration(c.Monitor.Policy.CooldownSeconds) * time.Second
			if last != nil && last.ResolvedAt != nil && cooldown > 0 && now.Sub(*last.ResolvedAt) < cooldown {
				from := last.Status
				last.Status = target
				last.ResolvedAt = nil
				last.Resolution = ""
				last.UpdatedAt = now
				if target == model.IncidentConfirmed && last.ConfirmedAt == nil {
					t := now
					last.ConfirmedAt = &t
				}
				m.applySources(last, c)
				if err := m.store.SaveIncident(ctx, last, newEvent(last, "reopened", from, target, "system", "Failure recurred within cooldown period; incident reopened", now, details)); err != nil {
					return err
				}
				m.log.Info("incident reopened", "incident_id", last.ID, "monitor_id", c.Monitor.ID)
				m.notify(ctx, last, from, target)
				return nil
			}
			started := c.FirstFailure
			if started.IsZero() {
				started = now
			}
			in := &model.Incident{ID: uuid.NewString(), MonitorID: c.Monitor.ID, Status: target, Title: fmt.Sprintf("%s is down", c.Monitor.Name),
				StartedAt: started, DetectedAt: now, UpdatedAt: now, LastErrorCategory: c.Category}
			if target == model.IncidentConfirmed {
				t := now
				in.ConfirmedAt = &t
			}
			m.applySources(in, c)
			evs := []*model.IncidentEvent{newEvent(in, "opened", "", model.IncidentSuspected, "system", "Failure detected", now, details)}
			if target == model.IncidentConfirmed {
				evs[0].ToStatus = target
				evs[0].Message = "Failure detected and confirmed"
			}
			if err := m.store.SaveIncident(ctx, in, evs...); err != nil {
				return err
			}
			m.log.Info("incident opened", "incident_id", in.ID, "monitor_id", c.Monitor.ID, "status", in.Status)
			m.notify(ctx, in, "", target)
			return nil
		}
		m.applySources(open, c)
		open.LastErrorCategory = c.Category
		if target == model.IncidentConfirmed && !IsConfirmedOrLater(open.Status) {
			return m.transitionLocked(ctx, open, model.IncidentConfirmed, "system", "Failure confirmed by policy threshold", details)
		}
		open.UpdatedAt = now
		return m.store.SaveIncident(ctx, open)
	case model.StatusUp, model.StatusDegraded:
		if open == nil {
			return nil
		}
		resolution := "recovered"
		msg := "Monitor recovered (recovery threshold met)"
		if open.Status == model.IncidentSuspected {
			resolution = "cleared_before_confirmation"
			msg = "Failure cleared before confirmation"
		}
		if open.Status == model.IncidentRecovering || open.Status == model.IncidentRecoveryPending || open.Status == model.IncidentFailedOver || open.Status == model.IncidentManualIntervention {
			// Recovery workflows own these states; record the observation only.
			return m.store.SaveIncident(ctx, open, newEvent(open, "monitor_recovered", open.Status, open.Status, "system", "Monitor reports healthy; incident remains under recovery management", now, details))
		}
		open.Resolution = resolution
		return m.transitionLocked(ctx, open, model.IncidentResolved, "system", msg, details)
	}
	return nil
}

func (m *Manager) applySources(in *model.Incident, c MonitorChange) {
	if c.Round == nil {
		return
	}
	seen := map[string]bool{}
	for _, s := range in.ConfirmationSources {
		seen[s] = true
	}
	for _, n := range c.Round.FailingNodes {
		if !seen[n] {
			in.ConfirmationSources = append(in.ConfirmationSources, n)
			seen[n] = true
		}
	}
}

func (m *Manager) transitionLocked(ctx context.Context, in *model.Incident, to model.IncidentStatus, actor, msg string, details map[string]any) error {
	from := in.Status
	if !CanTransition(from, to) {
		return ErrInvalidTransition{From: from, To: to}
	}
	now := m.now()
	in.Status = to
	in.UpdatedAt = now
	switch to {
	case model.IncidentConfirmed:
		if in.ConfirmedAt == nil {
			in.ConfirmedAt = &now
		}
	case model.IncidentAcknowledged:
		in.AcknowledgedAt = &now
		in.AcknowledgedBy = actor
	case model.IncidentResolved:
		in.ResolvedAt = &now
	}
	if err := m.store.SaveIncident(ctx, in, newEvent(in, "transition", from, to, actor, msg, now, details)); err != nil {
		return err
	}
	m.log.Info("incident transition", "incident_id", in.ID, "from", from, "to", to, "actor", actor)
	m.notify(ctx, in, from, to)
	return nil
}

// Acknowledge marks an incident acknowledged by a user.
func (m *Manager) Acknowledge(ctx context.Context, id, actor, note string) (*model.Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.store.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	msg := "Acknowledged"
	if note != "" {
		msg = note
	}
	return in, m.transitionLocked(ctx, in, model.IncidentAcknowledged, actor, msg, nil)
}

// Resolve manually resolves an incident.
func (m *Manager) Resolve(ctx context.Context, id, actor, note string) (*model.Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.store.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	in.Resolution = "manual"
	if note != "" {
		in.Resolution = "manual: " + note
	}
	msg := "Manually resolved"
	if note != "" {
		msg += ": " + note
	}
	return in, m.transitionLocked(ctx, in, model.IncidentResolved, actor, msg, nil)
}

// AddNote appends a user note to the incident timeline.
func (m *Manager) AddNote(ctx context.Context, id, actor, note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.store.GetIncident(ctx, id)
	if err != nil {
		return err
	}
	in.UpdatedAt = m.now()
	return m.store.SaveIncident(ctx, in, newEvent(in, "note", in.Status, in.Status, actor, note, in.UpdatedAt, nil))
}

// SetStatus moves an incident to a recovery-related status (used by workflows and operators).
// The transition must be permitted by the state machine.
func (m *Manager) SetStatus(ctx context.Context, id string, to model.IncidentStatus, actor, note string) (*model.Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	in, err := m.store.GetIncident(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Status == to {
		return in, nil
	}
	msg := note
	if msg == "" {
		msg = fmt.Sprintf("Status set to %s", to)
	}
	return in, m.transitionLocked(ctx, in, to, actor, msg, nil)
}
