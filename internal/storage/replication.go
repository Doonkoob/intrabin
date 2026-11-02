package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"strings"
	"time"
)

// Settings whose keys start with one of these prefixes are node-local and never replicated.
var localSettingPrefixes = []string{"node.", "local."}

// IsLocalSetting reports whether a settings key is node-local.
func IsLocalSetting(key string) bool {
	for _, p := range localSettingPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

type tableSpec struct {
	name  string
	where func(now time.Time) (string, []any) // optional row filter for dumps
	merge bool                                // merge (upsert) on restore instead of replace
}

// replicatedTables lists control-plane tables in dependency order (parents first).
// Round verdicts and hourly rollups are replicated through the log as they are produced;
// snapshots include only a recent window of them to bound snapshot size (see ADR-0008).
var replicatedTables = []tableSpec{
	{name: "settings", where: func(time.Time) (string, []any) {
		return "key NOT LIKE 'node.%' AND key NOT LIKE 'local.%'", nil
	}},
	{name: "users"},
	{name: "sessions"},
	{name: "monitor_groups"},
	{name: "monitors"},
	{name: "monitor_state"},
	{name: "maintenance_windows"},
	{name: "incidents"},
	{name: "incident_events"},
	{name: "audit_logs"},
	{name: "enrollment_tokens"},
	{name: "cluster_nodes"},
	{name: "workflows"},
	{name: "workflow_runs"},
	{name: "workflow_actions"},
	{name: "round_results", merge: true, where: func(now time.Time) (string, []any) {
		return "round_start >= ?", []any{ms(now.Add(-48 * time.Hour))}
	}},
	{name: "monitor_hourly", merge: true, where: func(now time.Time) (string, []any) {
		return "hour >= ?", []any{ms(now.Add(-35 * 24 * time.Hour))}
	}},
}

// Cell is one typed SQLite value in a dump.
type Cell struct {
	K byte // 0 null, 1 int, 2 float, 3 text, 4 blob
	I int64
	F float64
	S string
	B []byte
}

// TableDump holds the rows of one table.
type TableDump struct {
	Name    string
	Columns []string
	Rows    [][]Cell
}

// StateDump is a point-in-time copy of all replicated state.
type StateDump struct {
	FormatVersion int
	CreatedAt     int64
	Tables        []TableDump
}

const dumpFormat = 1

// DumpReplicated serialises all replicated tables (gzip+gob).
func (s *Store) DumpReplicated(ctx context.Context) ([]byte, error) {
	now := Now(ctx)
	d := StateDump{FormatVersion: dumpFormat, CreatedAt: ms(now)}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, t := range replicatedTables {
		q := "SELECT * FROM " + t.name
		var args []any
		if t.where != nil {
			w, a := t.where(now)
			q += " WHERE " + w
			args = a
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("dump %s: %w", t.name, err)
		}
		cols, _ := rows.Columns()
		td := TableDump{Name: t.name, Columns: cols}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			row := make([]Cell, len(cols))
			for i, v := range vals {
				switch x := v.(type) {
				case nil:
				case int64:
					row[i] = Cell{K: 1, I: x}
				case float64:
					row[i] = Cell{K: 2, F: x}
				case string:
					row[i] = Cell{K: 3, S: x}
				case []byte:
					row[i] = Cell{K: 4, B: append([]byte(nil), x...)}
				case bool:
					row[i] = Cell{K: 1, I: int64(boolInt(x))}
				case time.Time:
					row[i] = Cell{K: 1, I: ms(x)}
				default:
					rows.Close()
					return nil, fmt.Errorf("dump %s: unsupported type %T", t.name, v)
				}
			}
			td.Rows = append(td.Rows, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		d.Tables = append(d.Tables, td)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := gob.NewEncoder(zw).Encode(&d); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RestoreReplicated replaces all replicated state with the dump. Node-local data (local settings,
// raw probe results of monitors that still exist) is preserved.
func (s *Store) RestoreReplicated(ctx context.Context, data []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	var d StateDump
	if err := gob.NewDecoder(zr).Decode(&d); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if d.FormatVersion != dumpFormat {
		return fmt.Errorf("restore: unsupported dump format %d", d.FormatVersion)
	}
	byName := map[string]*TableDump{}
	for i := range d.Tables {
		byName[d.Tables[i].Name] = &d.Tables[i]
	}
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "PRAGMA foreign_keys=ON") //nolint:errcheck
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := len(replicatedTables) - 1; i >= 0; i-- {
		t := replicatedTables[i]
		if t.merge {
			continue
		}
		q := "DELETE FROM " + t.name
		if t.name == "settings" {
			w, _ := t.where(time.Time{})
			q += " WHERE " + w
		}
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("restore clear %s: %w", t.name, err)
		}
	}
	for _, t := range replicatedTables {
		td := byName[t.name]
		if td == nil || len(td.Rows) == 0 {
			continue
		}
		verb := "INSERT"
		if t.merge || t.name == "settings" {
			verb = "INSERT OR REPLACE"
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(td.Columns)), ",")
		stmt, err := tx.PrepareContext(ctx, verb+" INTO "+t.name+"("+strings.Join(td.Columns, ",")+") VALUES("+ph+")")
		if err != nil {
			return fmt.Errorf("restore %s: %w", t.name, err)
		}
		for _, row := range td.Rows {
			if t.name == "settings" && len(row) > 0 && IsLocalSetting(row[0].S) {
				continue
			}
			args := make([]any, len(row))
			for i, c := range row {
				switch c.K {
				case 1:
					args[i] = c.I
				case 2:
					args[i] = c.F
				case 3:
					args[i] = c.S
				case 4:
					args[i] = c.B
				}
			}
			if _, err := stmt.ExecContext(ctx, args...); err != nil {
				stmt.Close()
				return fmt.Errorf("restore %s: %w", t.name, err)
			}
		}
		stmt.Close()
	}
	// Drop node-local monitoring data that refers to monitors that no longer exist.
	for _, q := range []string{
		`DELETE FROM check_results WHERE monitor_id NOT IN (SELECT id FROM monitors)`,
		`DELETE FROM round_results WHERE monitor_id NOT IN (SELECT id FROM monitors)`,
		`DELETE FROM monitor_hourly WHERE monitor_id NOT IN (SELECT id FROM monitors)`,
		`DELETE FROM result_outbox WHERE result_id NOT IN (SELECT id FROM check_results)`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WipeReplicated deletes all replicated state and node-local monitoring data. Used when a
// standalone node joins an existing cluster (after the operator chose to export or discard).
func (s *Store) WipeReplicated(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"result_outbox", "check_results", "round_results", "monitor_hourly"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+q); err != nil {
			return err
		}
	}
	for i := len(replicatedTables) - 1; i >= 0; i-- {
		t := replicatedTables[i]
		q := "DELETE FROM " + t.name
		if t.name == "settings" {
			w, _ := t.where(time.Time{})
			q += " WHERE " + w
		}
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HasUserData reports whether this node holds users or monitors (used to prevent silent data loss on join).
func (s *Store) HasUserData(ctx context.Context) (users, monitors int, err error) {
	if err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		return
	}
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitors`).Scan(&monitors)
	return
}
