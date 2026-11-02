package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const resultCols = `id,monitor_id,node_id,config_version,round_start,probe_at,received_at,success,latency_ms,http_status,error_category,error_message,attempts,cert_expires_at,slow`

// InsertResults stores probe results idempotently (duplicate IDs are ignored).
// It returns the number of newly inserted rows.
func (s *Store) InsertResults(ctx context.Context, results []*model.CheckResult) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO check_results(`+resultCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	inserted := 0
	for _, r := range results {
		var lat, code any
		if r.LatencyMs != nil {
			lat = *r.LatencyMs
		}
		if r.HTTPStatus != nil {
			code = *r.HTTPStatus
		}
		res, err := stmt.ExecContext(ctx, r.ID, r.MonitorID, r.NodeID, r.ConfigVersion, ms(r.RoundStart), ms(r.ProbeAt), ms(r.ReceivedAt),
			boolInt(r.Success), lat, code, string(r.ErrorCategory), r.ErrorMessage, r.Attempts, nullMs(r.CertExpiresAt), boolInt(r.Slow))
		if err != nil {
			return inserted, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

func scanResult(sc interface{ Scan(...any) error }) (*model.CheckResult, error) {
	var r model.CheckResult
	var round, probe, recv int64
	var success, slow int
	var lat sql.NullFloat64
	var code, cert sql.NullInt64
	var cat string
	if err := sc.Scan(&r.ID, &r.MonitorID, &r.NodeID, &r.ConfigVersion, &round, &probe, &recv, &success, &lat, &code, &cat, &r.ErrorMessage, &r.Attempts, &cert, &slow); err != nil {
		return nil, err
	}
	r.RoundStart, r.ProbeAt, r.ReceivedAt = fromMs(round), fromMs(probe), fromMs(recv)
	r.Success, r.Slow = success == 1, slow == 1
	r.ErrorCategory = model.ErrorCategory(cat)
	if lat.Valid {
		v := lat.Float64
		r.LatencyMs = &v
	}
	if code.Valid {
		v := int(code.Int64)
		r.HTTPStatus = &v
	}
	r.CertExpiresAt = ptrTime(cert)
	return &r, nil
}

func (s *Store) queryResults(ctx context.Context, q string, args ...any) ([]*model.CheckResult, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.CheckResult{}
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResultsForRound returns all probe results for a monitor round.
func (s *Store) ResultsForRound(ctx context.Context, monitorID string, roundStart time.Time) ([]*model.CheckResult, error) {
	return s.queryResults(ctx, `SELECT `+resultCols+` FROM check_results WHERE monitor_id=? AND round_start=?`, monitorID, ms(roundStart))
}

// ListResults returns recent results for a monitor, newest first.
func (s *Store) ListResults(ctx context.Context, monitorID string, since time.Time, nodeID string, limit int) ([]*model.CheckResult, error) {
	if limit <= 0 || limit > 5000 {
		limit = 100
	}
	q := `SELECT ` + resultCols + ` FROM check_results WHERE monitor_id=? AND probe_at>=?`
	args := []any{monitorID, ms(since)}
	if nodeID != "" {
		q += ` AND node_id=?`
		args = append(args, nodeID)
	}
	q += ` ORDER BY probe_at DESC LIMIT ?`
	args = append(args, limit)
	return s.queryResults(ctx, q, args...)
}

// LatestResultPerNode returns the most recent probe of each node for a monitor.
func (s *Store) LatestResultPerNode(ctx context.Context, monitorID string) ([]*model.CheckResult, error) {
	return s.queryResults(ctx, `SELECT `+resultCols+` FROM check_results c WHERE monitor_id=? AND probe_at = (SELECT MAX(probe_at) FROM check_results WHERE monitor_id=c.monitor_id AND node_id=c.node_id) ORDER BY node_id`, monitorID)
}

// LatestCertExpiry returns the latest observed certificate expiry for a monitor.
func (s *Store) LatestCertExpiry(ctx context.Context, monitorID string) (*time.Time, error) {
	var v sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT cert_expires_at FROM check_results WHERE monitor_id=? AND cert_expires_at IS NOT NULL ORDER BY probe_at DESC LIMIT 1`, monitorID).Scan(&v)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ptrTime(v), nil
}

// RecordRound atomically stores a round verdict, the new monitor state and the hourly rollup.
func (s *Store) RecordRound(ctx context.Context, rr *model.RoundResult, st *model.MonitorState, intervalSeconds int) error {
	nodes, _ := json.Marshal(nonNil(rr.FailingNodes))
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lat any
	if rr.AvgLatencyMs != nil {
		lat = *rr.AvgLatencyMs
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO round_results(monitor_id,round_start,verdict,valid_probes,failing_probes,failing_nodes,avg_latency_ms,evaluated_at) VALUES(?,?,?,?,?,?,?,?)`,
		rr.MonitorID, ms(rr.RoundStart), string(rr.Verdict), rr.ValidProbes, rr.FailingProbes, string(nodes), lat, ms(rr.EvaluatedAt))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		hour := rr.RoundStart.UTC().Truncate(time.Hour)
		var ok, partial, failed, inc, maint int
		switch rr.Verdict {
		case model.VerdictOK:
			ok = 1
		case model.VerdictPartial:
			partial = 1
		case model.VerdictFailed:
			failed = 1
		case model.VerdictMaintenance:
			maint = 1
		default:
			inc = 1
		}
		var latSum float64
		latCount := 0
		if rr.AvgLatencyMs != nil && (rr.Verdict == model.VerdictOK || rr.Verdict == model.VerdictPartial) {
			latSum, latCount = *rr.AvgLatencyMs, 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_hourly(monitor_id,hour,ok_rounds,partial_rounds,failed_rounds,inconclusive_rounds,maintenance_rounds,latency_sum,latency_count,expected_rounds)
			VALUES(?,?,?,?,?,?,?,?,?,1) ON CONFLICT(monitor_id,hour) DO UPDATE SET ok_rounds=ok_rounds+excluded.ok_rounds, partial_rounds=partial_rounds+excluded.partial_rounds,
			failed_rounds=failed_rounds+excluded.failed_rounds, inconclusive_rounds=inconclusive_rounds+excluded.inconclusive_rounds, maintenance_rounds=maintenance_rounds+excluded.maintenance_rounds,
			latency_sum=latency_sum+excluded.latency_sum, latency_count=latency_count+excluded.latency_count, expected_rounds=expected_rounds+1`,
			rr.MonitorID, ms(hour), ok, partial, failed, inc, maint, latSum, latCount); err != nil {
			return err
		}
		// Per-node probe statistics are network measurements, not uptime verdicts, so they are
		// kept during maintenance windows too (uptime above excludes them).
		for _, ns := range rr.NodeStats {
			fail, lsum, lcount := 0, 0.0, 0
			if !ns.OK {
				fail = 1
			} else if ns.LatencyMs != nil {
				lsum, lcount = *ns.LatencyMs, 1
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_node_hourly(monitor_id,node_id,hour,probes,failures,latency_sum,latency_count) VALUES(?,?,?,1,?,?,?)
				ON CONFLICT(monitor_id,node_id,hour) DO UPDATE SET probes=probes+1, failures=failures+excluded.failures,
				latency_sum=latency_sum+excluded.latency_sum, latency_count=latency_count+excluded.latency_count`,
				rr.MonitorID, ns.NodeID, ms(hour), fail, lsum, lcount); err != nil {
				return err
			}
		}

	}
	if err := saveState(ctx, tx, st); err != nil {
		return err
	}
	return tx.Commit()
}

// ListRounds returns round verdicts since the given time, oldest first.
func (s *Store) ListRounds(ctx context.Context, monitorID string, since time.Time, limit int) ([]*model.RoundResult, error) {
	if limit <= 0 || limit > 20000 {
		limit = 2000
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT monitor_id,round_start,verdict,valid_probes,failing_probes,failing_nodes,avg_latency_ms,evaluated_at FROM round_results WHERE monitor_id=? AND round_start>=? ORDER BY round_start DESC LIMIT ?`, monitorID, ms(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.RoundResult
	for rows.Next() {
		var r model.RoundResult
		var rs, ev int64
		var verdict, nodes string
		var lat sql.NullFloat64
		if err := rows.Scan(&r.MonitorID, &rs, &verdict, &r.ValidProbes, &r.FailingProbes, &nodes, &lat, &ev); err != nil {
			return nil, err
		}
		r.RoundStart, r.EvaluatedAt, r.Verdict = fromMs(rs), fromMs(ev), model.Verdict(verdict)
		_ = json.Unmarshal([]byte(nodes), &r.FailingNodes)
		if lat.Valid {
			v := lat.Float64
			r.AvgLatencyMs = &v
		}
		out = append(out, &r)
	}
	// reverse to oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// UptimeStats aggregates round verdicts over a period.
type UptimeStats struct {
	OK           int      `json:"ok_rounds"`
	Partial      int      `json:"partial_rounds"`
	Failed       int      `json:"failed_rounds"`
	Inconclusive int      `json:"inconclusive_rounds"`
	Maintenance  int      `json:"maintenance_rounds"`
	Expected     int      `json:"expected_rounds"`
	AvgLatencyMs *float64 `json:"avg_latency_ms,omitempty"`
	// Availability = (ok+partial)/(ok+partial+failed). nil when no conclusive data.
	Availability *float64 `json:"availability"`
	// Coverage = conclusive rounds / expected rounds.
	Coverage *float64 `json:"coverage"`
}

func finishStats(u *UptimeStats, latSum float64, latCount int) {
	conclusive := u.OK + u.Partial + u.Failed
	if conclusive > 0 {
		a := float64(u.OK+u.Partial) / float64(conclusive) * 100
		u.Availability = &a
	}
	if u.Expected > 0 {
		c := float64(conclusive+u.Maintenance) / float64(u.Expected) * 100
		if c > 100 {
			c = 100
		}
		u.Coverage = &c
	}
	if latCount > 0 {
		l := latSum / float64(latCount)
		u.AvgLatencyMs = &l
	}
}

// Uptime returns aggregated statistics for a monitor since the given time.
func (s *Store) Uptime(ctx context.Context, monitorID string, since time.Time) (*UptimeStats, error) {
	var u UptimeStats
	var latSum float64
	var latCount int
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(ok_rounds),0),COALESCE(SUM(partial_rounds),0),COALESCE(SUM(failed_rounds),0),COALESCE(SUM(inconclusive_rounds),0),
		COALESCE(SUM(maintenance_rounds),0),COALESCE(SUM(expected_rounds),0),COALESCE(SUM(latency_sum),0),COALESCE(SUM(latency_count),0) FROM monitor_hourly WHERE monitor_id=? AND hour>=?`,
		monitorID, ms(since.Truncate(time.Hour))).Scan(&u.OK, &u.Partial, &u.Failed, &u.Inconclusive, &u.Maintenance, &u.Expected, &latSum, &latCount)
	if err != nil {
		return nil, err
	}
	finishStats(&u, latSum, latCount)
	return &u, nil
}

// DailyBucket is one day of uptime history.
type DailyBucket struct {
	Day string `json:"day"` // YYYY-MM-DD (UTC)
	UptimeStats
}

// DailyUptime returns per-day statistics for the last n days (UTC days).
func (s *Store) DailyUptime(ctx context.Context, monitorID string, days int, now time.Time) ([]DailyBucket, error) {
	start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	rows, err := s.DB.QueryContext(ctx, `SELECT hour, ok_rounds, partial_rounds, failed_rounds, inconclusive_rounds, maintenance_rounds, expected_rounds, latency_sum, latency_count FROM monitor_hourly WHERE monitor_id=? AND hour>=?`, monitorID, ms(start))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		u        UptimeStats
		sum      float64
		cnt      int
		hasValue bool
	}
	buckets := make([]acc, days)
	for rows.Next() {
		var h int64
		var a UptimeStats
		var sum float64
		var cnt int
		if err := rows.Scan(&h, &a.OK, &a.Partial, &a.Failed, &a.Inconclusive, &a.Maintenance, &a.Expected, &sum, &cnt); err != nil {
			return nil, err
		}
		idx := int(fromMs(h).Sub(start) / (24 * time.Hour))
		if idx < 0 || idx >= days {
			continue
		}
		b := &buckets[idx]
		b.u.OK += a.OK
		b.u.Partial += a.Partial
		b.u.Failed += a.Failed
		b.u.Inconclusive += a.Inconclusive
		b.u.Maintenance += a.Maintenance
		b.u.Expected += a.Expected
		b.sum += sum
		b.cnt += cnt
	}
	out := make([]DailyBucket, days)
	for i := range buckets {
		finishStats(&buckets[i].u, buckets[i].sum, buckets[i].cnt)
		out[i] = DailyBucket{Day: start.AddDate(0, 0, i).Format("2006-01-02"), UptimeStats: buckets[i].u}
	}
	return out, rows.Err()
}

// ResultBreakdown counts results by status code, error category and node.
type ResultBreakdown struct {
	StatusCodes     map[string]int `json:"status_codes"`
	ErrorCategories map[string]int `json:"error_categories"`
	Nodes           []NodeSummary  `json:"nodes"`
}

type NodeSummary struct {
	NodeID       string    `json:"node_id"`
	Total        int       `json:"total"`
	Failures     int       `json:"failures"`
	AvgLatencyMs *float64  `json:"avg_latency_ms,omitempty"`
	LastProbeAt  time.Time `json:"last_probe_at"`
	LastSuccess  bool      `json:"last_success"`
}

func (s *Store) Breakdown(ctx context.Context, monitorID string, since time.Time) (*ResultBreakdown, error) {
	b := &ResultBreakdown{StatusCodes: map[string]int{}, ErrorCategories: map[string]int{}, Nodes: []NodeSummary{}}
	rows, err := s.DB.QueryContext(ctx, `SELECT COALESCE(CAST(http_status AS TEXT),'none'), COUNT(*) FROM check_results WHERE monitor_id=? AND probe_at>=? GROUP BY 1`, monitorID, ms(since))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return nil, err
		}
		b.StatusCodes[k] = n
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT error_category, COUNT(*) FROM check_results WHERE monitor_id=? AND probe_at>=? AND error_category<>'' GROUP BY 1`, monitorID, ms(since))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return nil, err
		}
		b.ErrorCategories[k] = n
	}
	rows.Close()
	rows, err = s.DB.QueryContext(ctx, `SELECT node_id, COUNT(*), SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), AVG(latency_ms), MAX(probe_at),
		(SELECT success FROM check_results c2 WHERE c2.monitor_id=c.monitor_id AND c2.node_id=c.node_id ORDER BY probe_at DESC LIMIT 1)
		FROM check_results c WHERE monitor_id=? AND probe_at>=? GROUP BY node_id ORDER BY node_id`, monitorID, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n NodeSummary
		var avg sql.NullFloat64
		var last int64
		var lastOK int
		if err := rows.Scan(&n.NodeID, &n.Total, &n.Failures, &avg, &last, &lastOK); err != nil {
			return nil, err
		}
		if avg.Valid {
			v := avg.Float64
			n.AvgLatencyMs = &v
		}
		n.LastProbeAt = fromMs(last)
		n.LastSuccess = lastOK == 1
		b.Nodes = append(b.Nodes, n)
	}
	return b, rows.Err()
}

// LatencySeries returns time-bucketed average latency for successful probes.
type LatencyPoint struct {
	At       time.Time `json:"at"`
	AvgMs    float64   `json:"avg_ms"`
	MaxMs    float64   `json:"max_ms"`
	Failures int       `json:"failures"`
	Total    int       `json:"total"`
}

func (s *Store) LatencySeries(ctx context.Context, monitorID string, since time.Time, bucket time.Duration) ([]LatencyPoint, error) {
	bms := bucket.Milliseconds()
	if bms <= 0 {
		bms = 60000
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT (probe_at / ?) * ? AS b, COALESCE(AVG(CASE WHEN success=1 THEN latency_ms END),0), COALESCE(MAX(CASE WHEN success=1 THEN latency_ms END),0),
		SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), COUNT(*) FROM check_results WHERE monitor_id=? AND probe_at>=? GROUP BY b ORDER BY b`, bms, bms, monitorID, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencyPoint{}
	for rows.Next() {
		var p LatencyPoint
		var b int64
		if err := rows.Scan(&b, &p.AvgMs, &p.MaxMs, &p.Failures, &p.Total); err != nil {
			return nil, err
		}
		p.At = fromMs(b)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Prune deletes data older than the retention thresholds.
func (s *Store) Prune(ctx context.Context, rawBefore, roundsBefore, hourlyBefore time.Time) (int64, error) {
	var total int64
	for _, q := range []struct {
		sql string
		t   time.Time
	}{
		{`DELETE FROM check_results WHERE probe_at < ?`, rawBefore},
		{`DELETE FROM round_results WHERE round_start < ?`, roundsBefore},
		{`DELETE FROM monitor_hourly WHERE hour < ?`, hourlyBefore},
		{`DELETE FROM monitor_node_hourly WHERE hour < ?`, hourlyBefore},
	} {
		res, err := s.DB.ExecContext(ctx, q.sql, ms(q.t))
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// CountFailedRounds counts failed rounds in [from, to).
func (s *Store) CountFailedRounds(ctx context.Context, monitorID string, from, to time.Time) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM round_results WHERE monitor_id=? AND verdict='failed' AND round_start>=? AND round_start<?`, monitorID, ms(from), ms(to)).Scan(&n)
	return n, err
}

// GlobalLatencySeries aggregates latency across all monitors.
func (s *Store) GlobalLatencySeries(ctx context.Context, since time.Time, bucket time.Duration) ([]LatencyPoint, error) {
	bms := bucket.Milliseconds()
	rows, err := s.DB.QueryContext(ctx, `SELECT (probe_at / ?) * ? AS b, COALESCE(AVG(CASE WHEN success=1 THEN latency_ms END),0), COALESCE(MAX(CASE WHEN success=1 THEN latency_ms END),0),
		SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), COUNT(*) FROM check_results WHERE probe_at>=? GROUP BY b ORDER BY b`, bms, bms, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LatencyPoint{}
	for rows.Next() {
		var p LatencyPoint
		var b int64
		if err := rows.Scan(&b, &p.AvgMs, &p.MaxMs, &p.Failures, &p.Total); err != nil {
			return nil, err
		}
		p.At = fromMs(b)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GlobalDailyUptime aggregates daily availability across all monitors.
func (s *Store) GlobalDailyUptime(ctx context.Context, days int, now time.Time) ([]DailyBucket, error) {
	start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	rows, err := s.DB.QueryContext(ctx, `SELECT (hour - ?) / 86400000, SUM(ok_rounds), SUM(partial_rounds), SUM(failed_rounds), SUM(inconclusive_rounds), SUM(maintenance_rounds), SUM(expected_rounds), SUM(latency_sum), SUM(latency_count)
		FROM monitor_hourly WHERE hour>=? GROUP BY 1`, ms(start), ms(start))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DailyBucket, days)
	for i := range out {
		out[i].Day = start.AddDate(0, 0, i).Format("2006-01-02")
	}
	sums := make([]float64, days)
	cnts := make([]int, days)
	for rows.Next() {
		var idx int
		var u UptimeStats
		var sum float64
		var cnt int
		if err := rows.Scan(&idx, &u.OK, &u.Partial, &u.Failed, &u.Inconclusive, &u.Maintenance, &u.Expected, &sum, &cnt); err != nil {
			return nil, err
		}
		if idx < 0 || idx >= days {
			continue
		}
		out[idx].UptimeStats = u
		sums[idx], cnts[idx] = sum, cnt
	}
	for i := range out {
		finishStats(&out[i].UptimeStats, sums[i], cnts[i])
	}
	return out, rows.Err()
}

// NodeLatency is the average latency and failure count of one monitor (or all) from one node.
type NodeLatency struct {
	MonitorID    string   `json:"monitor_id"`
	NodeID       string   `json:"node_id"`
	Probes       int      `json:"probes"`
	Failures     int      `json:"failures"`
	AvgLatencyMs *float64 `json:"avg_latency_ms"`
}

// NodeLatencies aggregates per-node probe statistics since the given hour. With byMonitor false
// the rows are per node across all monitors (MonitorID empty).
func (s *Store) NodeLatencies(ctx context.Context, since time.Time, byMonitor bool) ([]NodeLatency, error) {
	group, sel := "node_id", "''"
	if byMonitor {
		group, sel = "monitor_id, node_id", "monitor_id"
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+sel+`, node_id, SUM(probes), SUM(failures), SUM(latency_sum), SUM(latency_count)
		FROM monitor_node_hourly WHERE hour >= ? GROUP BY `+group+` ORDER BY 1, 2`, ms(since.UTC().Truncate(time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeLatency{}
	for rows.Next() {
		var n NodeLatency
		var sum float64
		var cnt int
		if err := rows.Scan(&n.MonitorID, &n.NodeID, &n.Probes, &n.Failures, &sum, &cnt); err != nil {
			return nil, err
		}
		if cnt > 0 {
			avg := sum / float64(cnt)
			n.AvgLatencyMs = &avg
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
