package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const incidentCols = `i.id,i.monitor_id,COALESCE(m.name,''),i.status,i.title,i.started_at,i.detected_at,i.confirmed_at,i.acknowledged_at,COALESCE(i.acknowledged_by,''),i.resolved_at,i.resolution,i.confirmation_sources,i.last_error_category,i.updated_at`

func scanIncident(sc interface{ Scan(...any) error }) (*model.Incident, error) {
	var in model.Incident
	var status, sources string
	var started, detected, updated int64
	var confirmed, acked, resolved sql.NullInt64
	if err := sc.Scan(&in.ID, &in.MonitorID, &in.MonitorName, &status, &in.Title, &started, &detected, &confirmed, &acked, &in.AcknowledgedBy, &resolved, &in.Resolution, &sources, &in.LastErrorCategory, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	in.Status = model.IncidentStatus(status)
	in.StartedAt, in.DetectedAt, in.UpdatedAt = fromMs(started), fromMs(detected), fromMs(updated)
	in.ConfirmedAt, in.AcknowledgedAt, in.ResolvedAt = ptrTime(confirmed), ptrTime(acked), ptrTime(resolved)
	_ = json.Unmarshal([]byte(sources), &in.ConfirmationSources)
	if in.ConfirmationSources == nil {
		in.ConfirmationSources = []string{}
	}
	return &in, nil
}

// SaveIncident upserts an incident and appends events in one transaction.
func (s *Store) SaveIncident(ctx context.Context, in *model.Incident, events ...*model.IncidentEvent) error {
	sources, _ := json.Marshal(nonNil(in.ConfirmationSources))
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ackBy any
	if in.AcknowledgedBy != "" {
		ackBy = in.AcknowledgedBy
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO incidents(id,monitor_id,status,title,started_at,detected_at,confirmed_at,acknowledged_at,acknowledged_by,resolved_at,resolution,confirmation_sources,last_error_category,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status, title=excluded.title, confirmed_at=excluded.confirmed_at,
		acknowledged_at=excluded.acknowledged_at, acknowledged_by=excluded.acknowledged_by, resolved_at=excluded.resolved_at, resolution=excluded.resolution,
		confirmation_sources=excluded.confirmation_sources, last_error_category=excluded.last_error_category, updated_at=excluded.updated_at`,
		in.ID, in.MonitorID, string(in.Status), in.Title, ms(in.StartedAt), ms(in.DetectedAt), nullMs(in.ConfirmedAt), nullMs(in.AcknowledgedAt), ackBy,
		nullMs(in.ResolvedAt), in.Resolution, string(sources), in.LastErrorCategory, ms(in.UpdatedAt))
	if err != nil {
		return err
	}
	for _, e := range events {
		details, _ := json.Marshal(e.Details)
		if e.Details == nil {
			details = []byte("{}")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO incident_events(id,incident_id,at,type,from_status,to_status,actor,message,details) VALUES(?,?,?,?,?,?,?,?,?)`,
			e.ID, e.IncidentID, ms(e.At), e.Type, string(e.FromStatus), string(e.ToStatus), e.Actor, e.Message, string(details)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetIncident(ctx context.Context, id string) (*model.Incident, error) {
	return scanIncident(s.DB.QueryRowContext(ctx, `SELECT `+incidentCols+` FROM incidents i LEFT JOIN monitors m ON m.id=i.monitor_id WHERE i.id=?`, id))
}

// OpenIncidentForMonitor returns the unresolved incident for a monitor, if any.
func (s *Store) OpenIncidentForMonitor(ctx context.Context, monitorID string) (*model.Incident, error) {
	return scanIncident(s.DB.QueryRowContext(ctx, `SELECT `+incidentCols+` FROM incidents i LEFT JOIN monitors m ON m.id=i.monitor_id WHERE i.monitor_id=? AND i.status<>'RESOLVED' ORDER BY i.started_at DESC LIMIT 1`, monitorID))
}

// LastResolvedIncident returns the most recently resolved incident for a monitor.
func (s *Store) LastResolvedIncident(ctx context.Context, monitorID string) (*model.Incident, error) {
	return scanIncident(s.DB.QueryRowContext(ctx, `SELECT `+incidentCols+` FROM incidents i LEFT JOIN monitors m ON m.id=i.monitor_id WHERE i.monitor_id=? AND i.status='RESOLVED' ORDER BY i.resolved_at DESC LIMIT 1`, monitorID))
}

type IncidentFilter struct {
	MonitorID string
	Statuses  []string
	OpenOnly  bool
	Limit     int
	Before    *time.Time
}

func (s *Store) ListIncidents(ctx context.Context, f IncidentFilter) ([]*model.Incident, error) {
	q := `SELECT ` + incidentCols + ` FROM incidents i LEFT JOIN monitors m ON m.id=i.monitor_id WHERE 1=1`
	var args []any
	if f.MonitorID != "" {
		q += ` AND i.monitor_id=?`
		args = append(args, f.MonitorID)
	}
	if f.OpenOnly {
		q += ` AND i.status<>'RESOLVED'`
	}
	if len(f.Statuses) > 0 {
		q += ` AND i.status IN (?` + strings.Repeat(",?", len(f.Statuses)-1) + `)`
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.Before != nil {
		q += ` AND i.started_at < ?`
		args = append(args, ms(*f.Before))
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY i.started_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Incident{}
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) ListIncidentEvents(ctx context.Context, incidentID string) ([]*model.IncidentEvent, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,incident_id,at,type,from_status,to_status,actor,message,details FROM incident_events WHERE incident_id=? ORDER BY at, rowid`, incidentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.IncidentEvent{}
	for rows.Next() {
		var e model.IncidentEvent
		var at int64
		var from, to, details string
		if err := rows.Scan(&e.ID, &e.IncidentID, &at, &e.Type, &from, &to, &e.Actor, &e.Message, &details); err != nil {
			return nil, err
		}
		e.At = fromMs(at)
		e.FromStatus, e.ToStatus = model.IncidentStatus(from), model.IncidentStatus(to)
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// Maintenance windows

func (s *Store) CreateMaintenance(ctx context.Context, w *model.MaintenanceWindow) error {
	var mon any
	if w.MonitorID != "" {
		mon = w.MonitorID
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO maintenance_windows(id,monitor_id,title,starts_at,ends_at,created_by,created_at) VALUES(?,?,?,?,?,?,?)`,
		w.ID, mon, w.Title, ms(w.StartsAt), ms(w.EndsAt), w.CreatedBy, ms(w.CreatedAt))
	return err
}

func (s *Store) DeleteMaintenance(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM maintenance_windows WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListMaintenance lists windows that end after `after`.
func (s *Store) ListMaintenance(ctx context.Context, after time.Time) ([]*model.MaintenanceWindow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,COALESCE(monitor_id,''),title,starts_at,ends_at,created_by,created_at FROM maintenance_windows WHERE ends_at > ? ORDER BY starts_at`, ms(after))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.MaintenanceWindow{}
	for rows.Next() {
		var w model.MaintenanceWindow
		var st, en, cr int64
		if err := rows.Scan(&w.ID, &w.MonitorID, &w.Title, &st, &en, &w.CreatedBy, &cr); err != nil {
			return nil, err
		}
		w.StartsAt, w.EndsAt, w.CreatedAt = fromMs(st), fromMs(en), fromMs(cr)
		out = append(out, &w)
	}
	return out, rows.Err()
}

// InMaintenance reports whether monitorID is covered by an active window at t.
func (s *Store) InMaintenance(ctx context.Context, monitorID string, t time.Time) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_windows WHERE starts_at <= ? AND ends_at > ? AND (monitor_id IS NULL OR monitor_id=?)`, ms(t), ms(t), monitorID).Scan(&n)
	return n > 0, err
}

// Audit

func (s *Store) InsertAudit(ctx context.Context, a *model.AuditEntry) error {
	details := []byte("{}")
	if a.Details != nil {
		details, _ = json.Marshal(a.Details)
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_logs(id,at,actor_id,actor_name,action,target_type,target_id,ip,outcome,details) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		a.ID, ms(a.At), a.ActorID, a.ActorName, a.Action, a.TargetType, a.TargetID, a.IP, a.Outcome, string(details))
	return err
}

type AuditFilter struct {
	Action string
	Actor  string
	Target string
	Query  string
	Since  *time.Time
	Before *time.Time
	Limit  int
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]*model.AuditEntry, error) {
	q := `SELECT id,at,actor_id,actor_name,action,target_type,target_id,ip,outcome,details FROM audit_logs WHERE 1=1`
	var args []any
	if f.Action != "" {
		q += ` AND action LIKE ? || '%'`
		args = append(args, f.Action)
	}
	if f.Actor != "" {
		q += ` AND actor_name=?`
		args = append(args, f.Actor)
	}
	if f.Target != "" {
		q += ` AND target_id=?`
		args = append(args, f.Target)
	}
	if f.Query != "" {
		q += ` AND (action LIKE '%' || ? || '%' OR actor_name LIKE '%' || ? || '%' OR target_id LIKE '%' || ? || '%' OR details LIKE '%' || ? || '%')`
		args = append(args, f.Query, f.Query, f.Query, f.Query)
	}
	if f.Since != nil {
		q += ` AND at >= ?`
		args = append(args, ms(*f.Since))
	}
	if f.Before != nil {
		q += ` AND at < ?`
		args = append(args, ms(*f.Before))
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.AuditEntry{}
	for rows.Next() {
		var a model.AuditEntry
		var at int64
		var details string
		if err := rows.Scan(&a.ID, &at, &a.ActorID, &a.ActorName, &a.Action, &a.TargetType, &a.TargetID, &a.IP, &a.Outcome, &details); err != nil {
			return nil, err
		}
		a.At = fromMs(at)
		_ = json.Unmarshal([]byte(details), &a.Details)
		out = append(out, &a)
	}
	return out, rows.Err()
}
