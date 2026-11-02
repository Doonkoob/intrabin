package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const dnsProviderCols = `id,name,type,credentials,created_by,created_at,updated_at,last_verified_at,last_error`

func (s *Store) CreateDNSProvider(ctx context.Context, p *model.DNSProvider) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO dns_providers(`+dnsProviderCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Type, p.CredentialBlob, p.CreatedBy, ms(p.CreatedAt), ms(p.UpdatedAt), nullMs(p.LastVerifiedAt), p.LastError)
	return mapConflict(err)
}

func (s *Store) UpdateDNSProvider(ctx context.Context, p *model.DNSProvider) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE dns_providers SET name=?,credentials=?,updated_at=?,last_verified_at=?,last_error=? WHERE id=?`,
		p.Name, p.CredentialBlob, ms(p.UpdatedAt), nullMs(p.LastVerifiedAt), p.LastError, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteDNSProvider(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM dns_providers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDNSProvider(sc interface{ Scan(...any) error }) (*model.DNSProvider, error) {
	var p model.DNSProvider
	var c, u int64
	var v sql.NullInt64
	if err := sc.Scan(&p.ID, &p.Name, &p.Type, &p.CredentialBlob, &p.CreatedBy, &c, &u, &v, &p.LastError); err != nil {
		return nil, err
	}
	p.CreatedAt, p.UpdatedAt, p.LastVerifiedAt = fromMs(c), fromMs(u), ptrNull(v)
	return &p, nil
}

func (s *Store) GetDNSProvider(ctx context.Context, id string) (*model.DNSProvider, error) {
	p, err := scanDNSProvider(s.DB.QueryRowContext(ctx, `SELECT `+dnsProviderCols+` FROM dns_providers WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) ListDNSProviders(ctx context.Context) ([]*model.DNSProvider, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+dnsProviderCols+` FROM dns_providers ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.DNSProvider{}
	for rows.Next() {
		p, err := scanDNSProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- snapshots ----

func (s *Store) SaveDNSSnapshot(ctx context.Context, sn *model.DNSSnapshot) error {
	recs := sn.Records
	if recs == nil {
		recs = []model.DNSRecord{}
	}
	b, _ := json.Marshal(recs)
	_, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO dns_snapshots(id,provider_id,zone_id,zone_name,reason,records,created_by,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		sn.ID, sn.ProviderID, sn.ZoneID, sn.ZoneName, sn.Reason, string(b), sn.CreatedBy, ms(sn.CreatedAt))
	return err
}

func scanSnapshot(sc interface{ Scan(...any) error }) (*model.DNSSnapshot, error) {
	var sn model.DNSSnapshot
	var recs string
	var c int64
	if err := sc.Scan(&sn.ID, &sn.ProviderID, &sn.ZoneID, &sn.ZoneName, &sn.Reason, &recs, &sn.CreatedBy, &c); err != nil {
		return nil, err
	}
	sn.CreatedAt = fromMs(c)
	_ = json.Unmarshal([]byte(recs), &sn.Records)
	sn.RecordCount = len(sn.Records)
	return &sn, nil
}

func (s *Store) GetDNSSnapshot(ctx context.Context, id string) (*model.DNSSnapshot, error) {
	sn, err := scanSnapshot(s.DB.QueryRowContext(ctx, `SELECT id,provider_id,zone_id,zone_name,reason,records,created_by,created_at FROM dns_snapshots WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sn, err
}

// ListDNSSnapshots returns snapshots (without their records) newest first.
func (s *Store) ListDNSSnapshots(ctx context.Context, providerID, zoneID string, limit int) ([]*model.DNSSnapshot, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,provider_id,zone_id,zone_name,reason,records,created_by,created_at FROM dns_snapshots
		WHERE (?='' OR provider_id=?) AND (?='' OR zone_id=?) ORDER BY created_at DESC LIMIT ?`, providerID, providerID, zoneID, zoneID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.DNSSnapshot{}
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		sn.Records = nil
		out = append(out, sn)
	}
	return out, rows.Err()
}

// PruneDNSSnapshots deletes snapshots older than before, keeping the newest keep per zone.
func (s *Store) PruneDNSSnapshots(ctx context.Context, before time.Time, keep int) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM dns_snapshots WHERE created_at < ? AND id NOT IN (
		SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY provider_id, zone_id ORDER BY created_at DESC) AS rn FROM dns_snapshots) WHERE rn <= ?)`, ms(before), keep)
	return err
}

// ---- changes ----

const dnsChangeCols = `id,provider_id,zone_id,zone_name,action,record_type,record_name,record_id,before,after,status,verified,snapshot_id,source,fence_term,fence_leader,actor,error,created_at,finished_at`

func jsonOrNull(v *model.DNSRecord) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// SaveDNSChange inserts or updates a change record.
func (s *Store) SaveDNSChange(ctx context.Context, c *model.DNSChange) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO dns_changes(`+dnsChangeCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET record_id=excluded.record_id, status=excluded.status, verified=excluded.verified, error=excluded.error,
		finished_at=excluded.finished_at, after=excluded.after, before=excluded.before, snapshot_id=excluded.snapshot_id`,
		c.ID, c.ProviderID, c.ZoneID, c.ZoneName, c.Action, c.RecordType, c.RecordName, c.RecordID, jsonOrNull(c.Before), jsonOrNull(c.After),
		c.Status, boolInt(c.Verified), c.SnapshotID, c.Source, int64(c.FenceTerm), c.FenceLeader, c.Actor, c.Error, ms(c.CreatedAt), nullMs(c.FinishedAt))
	return err
}

func scanDNSChange(sc interface{ Scan(...any) error }) (*model.DNSChange, error) {
	var c model.DNSChange
	var before, after sql.NullString
	var verified int
	var term int64
	var created int64
	var fin sql.NullInt64
	if err := sc.Scan(&c.ID, &c.ProviderID, &c.ZoneID, &c.ZoneName, &c.Action, &c.RecordType, &c.RecordName, &c.RecordID, &before, &after,
		&c.Status, &verified, &c.SnapshotID, &c.Source, &term, &c.FenceLeader, &c.Actor, &c.Error, &created, &fin); err != nil {
		return nil, err
	}
	if before.Valid {
		c.Before = &model.DNSRecord{}
		_ = json.Unmarshal([]byte(before.String), c.Before)
	}
	if after.Valid {
		c.After = &model.DNSRecord{}
		_ = json.Unmarshal([]byte(after.String), c.After)
	}
	c.Verified, c.FenceTerm, c.CreatedAt, c.FinishedAt = verified == 1, uint64(term), fromMs(created), ptrNull(fin)
	return &c, nil
}

func (s *Store) GetDNSChange(ctx context.Context, id string) (*model.DNSChange, error) {
	c, err := scanDNSChange(s.DB.QueryRowContext(ctx, `SELECT `+dnsChangeCols+` FROM dns_changes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// DNSChangeFilter selects changes.
type DNSChangeFilter struct {
	ProviderID, ZoneID string
	Statuses           []string
	Limit              int
}

func (s *Store) ListDNSChanges(ctx context.Context, f DNSChangeFilter) ([]*model.DNSChange, error) {
	q := `SELECT ` + dnsChangeCols + ` FROM dns_changes WHERE 1=1`
	var args []any
	if f.ProviderID != "" {
		q += ` AND provider_id=?`
		args = append(args, f.ProviderID)
	}
	if f.ZoneID != "" {
		q += ` AND zone_id=?`
		args = append(args, f.ZoneID)
	}
	if len(f.Statuses) > 0 {
		q += ` AND status IN (?` + repeatComma(len(f.Statuses)-1) + `)`
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.DNSChange{}
	for rows.Next() {
		c, err := scanDNSChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
