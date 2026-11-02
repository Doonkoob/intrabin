package storage

import (
	"context"
	"os"
	"time"
)

// TableUsage is the on-disk size of one table (with its indexes) and its row count.
type TableUsage struct {
	Name  string `json:"name"`
	Rows  int64  `json:"rows"`
	Bytes int64  `json:"bytes"`
}

// Usage describes how much disk the database uses.
type Usage struct {
	DatabaseBytes int64        `json:"database_bytes"`
	WALBytes      int64        `json:"wal_bytes"`
	TotalBytes    int64        `json:"total_bytes"`
	Tables        []TableUsage `json:"tables"`
}

// usageTables are reported individually ("logs" and history); the rest is summed as "other".
var usageTables = []string{"check_results", "round_results", "monitor_hourly", "monitor_node_hourly", "audit_logs",
	"workflow_runs", "workflow_actions", "incidents", "incident_events", "dns_changes", "dns_snapshots"}

// DiskUsage measures the database files and the size of the main tables.
func (s *Store) DiskUsage(ctx context.Context) (*Usage, error) {
	u := &Usage{Tables: []TableUsage{}}
	if st, err := os.Stat(s.path); err == nil {
		u.DatabaseBytes = st.Size()
	}
	if st, err := os.Stat(s.path + "-wal"); err == nil {
		u.WALBytes = st.Size()
	}
	u.TotalBytes = u.DatabaseBytes + u.WALBytes
	// Size per table including its indexes (dbstat reports pages per b-tree).
	rows, err := s.DB.QueryContext(ctx, `SELECT COALESCE(m.tbl_name, d.name), SUM(d.pgsize) FROM dbstat d LEFT JOIN sqlite_schema m ON m.name = d.name GROUP BY 1`)
	if err != nil {
		return u, nil // dbstat unavailable: report file sizes only
	}
	sizes := map[string]int64{}
	for rows.Next() {
		var name string
		var b int64
		if rows.Scan(&name, &b) == nil {
			sizes[name] = b
		}
	}
	rows.Close()
	var known int64
	for _, t := range usageTables {
		tu := TableUsage{Name: t, Bytes: sizes[t]}
		_ = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&tu.Rows)
		known += tu.Bytes
		u.Tables = append(u.Tables, tu)
	}
	var all int64
	for _, b := range sizes {
		all += b
	}
	u.Tables = append(u.Tables, TableUsage{Name: "other", Bytes: max(all-known, 0)})
	return u, nil
}

// PruneAudit deletes audit entries older than before.
func (s *Store) PruneAudit(ctx context.Context, before time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM audit_logs WHERE at < ?`, ms(before))
	return err
}

// LogRetentionChoices are the retention presets offered in the UI (days).
var LogRetentionChoices = []int{7, 15, 30, 90, 180, 365}

// MaxRawRetentionDays caps individual probe results; longer periods keep hourly aggregates.
const MaxRawRetentionDays = 90

// RetentionPolicy is the effective retention of each kind of data.
type RetentionPolicy struct {
	LogsDays int // 0 when no preset is chosen (separate raw/round/hourly settings apply)
	Raw      time.Duration
	Rounds   time.Duration
	Hourly   time.Duration
	Audit    time.Duration
	Runs     time.Duration
}

// Retention reads the retention settings. With a log retention preset (retention.logs_days) every
// kind of history is kept for that many days (individual probe results at most 90 days).
func (s *Store) Retention(ctx context.Context) RetentionPolicy {
	day := 24 * time.Hour
	get := func(k string, def int) int {
		v, err := s.GetSetting(ctx, k)
		if err != nil {
			return def
		}
		n := 0
		for _, c := range v {
			if c < '0' || c > '9' {
				return def
			}
			n = n*10 + int(c-'0')
		}
		if n <= 0 {
			return def
		}
		return n
	}
	if d := get("retention.logs_days", 0); d > 0 {
		dd := time.Duration(d) * day
		return RetentionPolicy{LogsDays: d, Raw: time.Duration(min(d, MaxRawRetentionDays)) * day, Rounds: dd, Hourly: dd, Audit: dd, Runs: dd}
	}
	return RetentionPolicy{Raw: time.Duration(get("retention.raw_days", 14)) * day, Rounds: time.Duration(get("retention.rounds_days", 90)) * day,
		Hourly: time.Duration(get("retention.hourly_days", 400)) * day, Audit: 365 * day, Runs: 90 * day}
}
