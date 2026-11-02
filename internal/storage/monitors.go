package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const monitorCols = `id, name, description, type, target, group_id, tags, interval_seconds, timeout_seconds, retries, enabled, settings, secret_settings, policy, node_selector, version, created_at, updated_at`

func scanMonitor(sc interface{ Scan(...any) error }) (*model.Monitor, error) {
	var m model.Monitor
	var typ, tags, settings, policy, nodes string
	var group sql.NullString
	var enabled int
	var created, updated int64
	if err := sc.Scan(&m.ID, &m.Name, &m.Description, &typ, &m.Target, &group, &tags, &m.IntervalSeconds, &m.TimeoutSeconds, &m.Retries, &enabled, &settings, &m.SecretBlob, &policy, &nodes, &m.Version, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.Type = model.MonitorType(typ)
	m.GroupID = group.String
	m.Enabled = enabled == 1
	m.CreatedAt, m.UpdatedAt = fromMs(created), fromMs(updated)
	if err := json.Unmarshal([]byte(tags), &m.Tags); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(settings), &m.Settings); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(policy), &m.Policy); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(nodes), &m.NodeSelector); err != nil {
		return nil, err
	}
	if m.Tags == nil {
		m.Tags = []string{}
	}
	if m.NodeSelector == nil {
		m.NodeSelector = []string{}
	}
	return &m, nil
}

func monitorArgs(m *model.Monitor) ([]any, error) {
	tags, err := json.Marshal(nonNil(m.Tags))
	if err != nil {
		return nil, err
	}
	settings, err := json.Marshal(m.Settings)
	if err != nil {
		return nil, err
	}
	policy, err := json.Marshal(m.Policy)
	if err != nil {
		return nil, err
	}
	nodes, err := json.Marshal(nonNil(m.NodeSelector))
	if err != nil {
		return nil, err
	}
	var group any
	if m.GroupID != "" {
		group = m.GroupID
	}
	return []any{m.ID, m.Name, m.Description, string(m.Type), m.Target, group, string(tags), m.IntervalSeconds, m.TimeoutSeconds, m.Retries, boolInt(m.Enabled), string(settings), m.SecretBlob, string(policy), string(nodes), m.Version, ms(m.CreatedAt), ms(m.UpdatedAt)}, nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// CreateMonitor inserts a monitor and its initial state.
func (s *Store) CreateMonitor(ctx context.Context, m *model.Monitor) error {
	args, err := monitorArgs(m)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitors(`+monitorCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...); err != nil {
		return err
	}
	status := model.StatusUnknown
	if !m.Enabled {
		status = model.StatusPaused
	}
	now := ms(m.CreatedAt)
	if m.CreatedAt.IsZero() {
		now = ms(time.Now())
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_state(monitor_id,status,status_since,updated_at) VALUES(?,?,?,?)`, m.ID, string(status), now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateMonitor replaces a monitor definition (version must already be incremented).
func (s *Store) UpdateMonitor(ctx context.Context, m *model.Monitor) error {
	args, err := monitorArgs(m)
	if err != nil {
		return err
	}
	// reorder: SET fields then WHERE id
	res, err := s.DB.ExecContext(ctx, `UPDATE monitors SET name=?, description=?, type=?, target=?, group_id=?, tags=?, interval_seconds=?, timeout_seconds=?, retries=?, enabled=?, settings=?, secret_settings=?, policy=?, node_selector=?, version=?, updated_at=? WHERE id=?`,
		append(args[1:16], args[17], args[0])...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetMonitor(ctx context.Context, id string) (*model.Monitor, error) {
	return scanMonitor(s.DB.QueryRowContext(ctx, `SELECT `+monitorCols+` FROM monitors WHERE id=?`, id))
}

func (s *Store) ListMonitors(ctx context.Context) ([]*model.Monitor, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+monitorCols+` FROM monitors ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMonitor(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM monitors WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Monitor state

func (s *Store) GetMonitorState(ctx context.Context, id string) (*model.MonitorState, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT monitor_id,status,consecutive_failed,consecutive_ok,consecutive_inconclusive,last_round,last_verdict,last_latency_ms,status_since,updated_at FROM monitor_state WHERE monitor_id=?`, id)
	return scanState(row)
}

func scanState(sc interface{ Scan(...any) error }) (*model.MonitorState, error) {
	var st model.MonitorState
	var status, verdict string
	var lat sql.NullFloat64
	var since, upd int64
	if err := sc.Scan(&st.MonitorID, &status, &st.ConsecutiveFailed, &st.ConsecutiveOK, &st.ConsecutiveInconclusive, &st.LastRound, &verdict, &lat, &since, &upd); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	st.Status = model.MonitorStatus(status)
	st.LastVerdict = model.Verdict(verdict)
	if lat.Valid {
		v := lat.Float64
		st.LastLatencyMs = &v
	}
	st.StatusSince, st.UpdatedAt = fromMs(since), fromMs(upd)
	return &st, nil
}

func (s *Store) ListMonitorStates(ctx context.Context) (map[string]*model.MonitorState, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT monitor_id,status,consecutive_failed,consecutive_ok,consecutive_inconclusive,last_round,last_verdict,last_latency_ms,status_since,updated_at FROM monitor_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.MonitorState{}
	for rows.Next() {
		st, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		out[st.MonitorID] = st
	}
	return out, rows.Err()
}

func (s *Store) SaveMonitorState(ctx context.Context, st *model.MonitorState) error {
	return saveState(ctx, s.DB, st)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func saveState(ctx context.Context, db execer, st *model.MonitorState) error {
	var lat any
	if st.LastLatencyMs != nil {
		lat = *st.LastLatencyMs
	}
	_, err := db.ExecContext(ctx, `INSERT INTO monitor_state(monitor_id,status,consecutive_failed,consecutive_ok,consecutive_inconclusive,last_round,last_verdict,last_latency_ms,status_since,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(monitor_id) DO UPDATE SET status=excluded.status, consecutive_failed=excluded.consecutive_failed, consecutive_ok=excluded.consecutive_ok,
		consecutive_inconclusive=excluded.consecutive_inconclusive, last_round=excluded.last_round, last_verdict=excluded.last_verdict, last_latency_ms=excluded.last_latency_ms,
		status_since=excluded.status_since, updated_at=excluded.updated_at`,
		st.MonitorID, string(st.Status), st.ConsecutiveFailed, st.ConsecutiveOK, st.ConsecutiveInconclusive, st.LastRound, string(st.LastVerdict), lat, ms(st.StatusSince), ms(st.UpdatedAt))
	return err
}

// Groups

func (s *Store) ListGroups(ctx context.Context) ([]*model.MonitorGroup, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,description,created_at,updated_at FROM monitor_groups ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.MonitorGroup
	for rows.Next() {
		var g model.MonitorGroup
		var c, u int64
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &c, &u); err != nil {
			return nil, err
		}
		g.CreatedAt, g.UpdatedAt = fromMs(c), fromMs(u)
		out = append(out, &g)
	}
	return out, rows.Err()
}

func (s *Store) GetGroup(ctx context.Context, id string) (*model.MonitorGroup, error) {
	var g model.MonitorGroup
	var c, u int64
	err := s.DB.QueryRowContext(ctx, `SELECT id,name,description,created_at,updated_at FROM monitor_groups WHERE id=?`, id).Scan(&g.ID, &g.Name, &g.Description, &c, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt, g.UpdatedAt = fromMs(c), fromMs(u)
	return &g, nil
}

func (s *Store) CreateGroup(ctx context.Context, g *model.MonitorGroup) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO monitor_groups(id,name,description,created_at,updated_at) VALUES(?,?,?,?,?)`, g.ID, g.Name, g.Description, ms(g.CreatedAt), ms(g.UpdatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdateGroup(ctx context.Context, g *model.MonitorGroup) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE monitor_groups SET name=?, description=?, updated_at=? WHERE id=?`, g.Name, g.Description, ms(g.UpdatedAt), g.ID)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM monitor_groups WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
