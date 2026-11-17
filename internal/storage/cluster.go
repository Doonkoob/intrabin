package storage

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const NodeNameKeyPrefix = "cluster.node-name."

var customNodeName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// Empty names reset the display to the node's address. Names are URL-safe public aliases.
func ValidNodeName(name string) bool { return name == "" || customNodeName.MatchString(name) }

// SetNodeName enforces uniqueness inside the replicated setting.set command. Checking in
// the FSM transaction prevents concurrent rename requests from claiming the same alias.
func (s *Store) SetNodeName(ctx context.Context, id, name string) error {
	if !ValidNodeName(name) {
		return ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cluster_nodes WHERE id=?)`, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	if name != "" {
		var duplicate bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cluster_nodes n WHERE n.id<>? AND
			(lower(n.id)=lower(?) OR EXISTS(SELECT 1 FROM settings s WHERE s.key=?||n.id AND lower(s.value)=lower(?))))`, id, name, NodeNameKeyPrefix, name).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			return ErrConflict
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, NodeNameKeyPrefix+id, name, ms(Now(ctx))); err != nil {
		return err
	}
	return tx.Commit()
}

// Token errors.
var (
	ErrTokenInvalid = errors.New("enrollment token is invalid")
	ErrTokenUsed    = errors.New("enrollment token was already used")
	ErrTokenExpired = errors.New("enrollment token expired or was revoked")
)

func (s *Store) CreateEnrollmentToken(ctx context.Context, t *model.EnrollmentToken) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO enrollment_tokens(id,secret_hash,description,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?)`,
		t.ID, t.SecretHash, t.Description, t.CreatedBy, ms(t.CreatedAt), ms(t.ExpiresAt))
	return mapConflict(err)
}

// ConsumeEnrollmentToken atomically validates and marks a token used. It is deterministic given
// the pinned clock, so every replica reaches the same outcome.
func (s *Store) ConsumeEnrollmentToken(ctx context.Context, id, secretHash, nodeID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hash string
	var exp int64
	var used, revoked sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT secret_hash, expires_at, used_at, revoked_at FROM enrollment_tokens WHERE id=?`, id).Scan(&hash, &exp, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrTokenInvalid
	}
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(secretHash)) != 1 {
		return ErrTokenInvalid
	}
	if used.Valid {
		return ErrTokenUsed
	}
	now := Now(ctx)
	if revoked.Valid || now.After(fromMs(exp)) {
		return ErrTokenExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrollment_tokens SET used_at=?, used_by_node=? WHERE id=?`, ms(now), nodeID, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RevokeEnrollmentToken(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE enrollment_tokens SET revoked_at=? WHERE id=? AND revoked_at IS NULL AND used_at IS NULL`, ms(Now(ctx)), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListEnrollmentTokens(ctx context.Context) ([]*model.EnrollmentToken, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,description,created_by,created_at,expires_at,used_at,used_by_node,revoked_at FROM enrollment_tokens ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.EnrollmentToken{}
	for rows.Next() {
		var t model.EnrollmentToken
		var c, e int64
		var u, r sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Description, &t.CreatedBy, &c, &e, &u, &t.UsedByNode, &r); err != nil {
			return nil, err
		}
		t.CreatedAt, t.ExpiresAt = fromMs(c), fromMs(e)
		if u.Valid {
			v := fromMs(u.Int64)
			t.UsedAt = &v
		}
		if r.Valid {
			v := fromMs(r.Int64)
			t.RevokedAt = &v
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *Store) UpsertClusterNode(ctx context.Context, n *model.ClusterNode) error {
	now := Now(ctx)
	if n.JoinedAt.IsZero() {
		n.JoinedAt = now
	}
	n.UpdatedAt = now
	_, err := s.DB.ExecContext(ctx, `INSERT INTO cluster_nodes(id,name,cluster_addr,api_url,location,version,joined_at,updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, cluster_addr=excluded.cluster_addr, api_url=excluded.api_url, location=excluded.location,
		version=excluded.version, updated_at=excluded.updated_at`,
		n.ID, n.Name, n.ClusterAddr, n.APIURL, n.Location, n.Version, ms(n.JoinedAt), ms(n.UpdatedAt))
	return err
}

func (s *Store) DeleteClusterNode(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM cluster_nodes WHERE id=?`, id)
	return err
}

func (s *Store) ListClusterNodes(ctx context.Context) ([]*model.ClusterNode, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,name,cluster_addr,api_url,location,version,joined_at,updated_at FROM cluster_nodes ORDER BY joined_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.ClusterNode{}
	for rows.Next() {
		var n model.ClusterNode
		var j, u int64
		if err := rows.Scan(&n.ID, &n.Name, &n.ClusterAddr, &n.APIURL, &n.Location, &n.Version, &j, &u); err != nil {
			return nil, err
		}
		n.JoinedAt, n.UpdatedAt = fromMs(j), fromMs(u)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// EnqueueOutbox marks results as pending delivery to the leader (node-local).
func (s *Store) EnqueueOutbox(ctx context.Context, ids []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ms(time.Now())
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO result_outbox(result_id, queued_at) VALUES(?,?)`, id, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingOutbox returns up to limit buffered results, oldest first.
func (s *Store) PendingOutbox(ctx context.Context, limit int) ([]*model.CheckResult, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+prefixed("c.", resultCols)+` FROM result_outbox o JOIN check_results c ON c.id=o.result_id ORDER BY o.queued_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.CheckResult
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AckOutbox removes delivered results from the outbox.
func (s *Store) AckOutbox(ctx context.Context, ids []string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM result_outbox WHERE result_id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) OutboxSize(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM result_outbox`).Scan(&n)
	return n, err
}

func mapConflict(err error) error {
	if err != nil && isUnique(err) {
		return ErrConflict
	}
	return err
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ",")
}
