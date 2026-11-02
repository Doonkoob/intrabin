package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/intrabin/intrabin/internal/model"
)

const userCols = `id, username, display_name, password_hash, role, disabled, language, created_at, updated_at, password_changed_at, last_login_at`

func scanUser(sc interface{ Scan(...any) error }) (*model.User, error) {
	var u model.User
	var role string
	var disabled int
	var created, updated, pwChanged int64
	var lastLogin sql.NullInt64
	if err := sc.Scan(&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &role, &disabled, &u.Language, &created, &updated, &pwChanged, &lastLogin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Role = model.Role(role)
	u.Disabled = disabled == 1
	u.CreatedAt, u.UpdatedAt, u.PasswordChangedAt = fromMs(created), fromMs(updated), fromMs(pwChanged)
	u.LastLoginAt = ptrTime(lastLogin)
	return &u, nil
}

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountActiveOwners returns the number of enabled owners.
func (s *Store) CountActiveOwners(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='owner' AND disabled=0`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO users(`+userCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, string(u.Role), boolInt(u.Disabled), u.Language,
		ms(u.CreatedAt), ms(u.UpdatedAt), ms(u.PasswordChangedAt), nullMs(u.LastLoginAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// CreateFirstUser inserts the user only if no users exist, atomically.
func (s *Store) CreateFirstUser(ctx context.Context, u *model.User) error {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO users(`+userCols+`) SELECT ?,?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		u.ID, u.Username, u.DisplayName, u.PasswordHash, string(u.Role), boolInt(u.Disabled), u.Language,
		ms(u.CreatedAt), ms(u.UpdatedAt), ms(u.PasswordChangedAt), nil)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username=?`, username))
}

func (s *Store) ListUsers(ctx context.Context) ([]*model.User, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser persists mutable user fields.
func (s *Store) UpdateUser(ctx context.Context, u *model.User) error {
	u.UpdatedAt = Now(ctx)
	res, err := s.DB.ExecContext(ctx, `UPDATE users SET display_name=?, role=?, disabled=?, language=?, updated_at=? WHERE id=?`,
		u.DisplayName, string(u.Role), boolInt(u.Disabled), u.Language, ms(u.UpdatedAt), u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetPassword updates the password hash and revokes all sessions of the user.
func (s *Store) SetPassword(ctx context.Context, userID, hash string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ms(Now(ctx))
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?, password_changed_at=?, updated_at=? WHERE id=?`, hash, now, now, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TouchLogin(ctx context.Context, userID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET last_login_at=? WHERE id=?`, ms(Now(ctx)), userID)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Sessions

func (s *Store) CreateSession(ctx context.Context, se *model.Session) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_token,created_at,last_seen_at,expires_at,ip,user_agent) VALUES(?,?,?,?,?,?,?,?)`,
		se.TokenHash, se.UserID, se.CSRFToken, ms(se.CreatedAt), ms(se.LastSeenAt), ms(se.ExpiresAt), se.IP, se.UserAgent)
	return err
}

func (s *Store) GetSession(ctx context.Context, tokenHash string) (*model.Session, error) {
	var se model.Session
	var c, l, e int64
	err := s.DB.QueryRowContext(ctx, `SELECT token_hash,user_id,csrf_token,created_at,last_seen_at,expires_at,ip,user_agent FROM sessions WHERE token_hash=?`, tokenHash).
		Scan(&se.TokenHash, &se.UserID, &se.CSRFToken, &c, &l, &e, &se.IP, &se.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	se.CreatedAt, se.LastSeenAt, se.ExpiresAt = fromMs(c), fromMs(l), fromMs(e)
	return &se, nil
}

func (s *Store) TouchSession(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE token_hash=?`, ms(at), tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ? OR last_seen_at < ?`, ms(now), ms(now.Add(-idle)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RenameUser changes a user's login name (offline CLI use). ErrConflict if the name is taken.
func (s *Store) RenameUser(ctx context.Context, id, username string) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE users SET username=?, updated_at=? WHERE id=?`, username, ms(Now(ctx)), id)
	if err != nil {
		return mapConflict(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
