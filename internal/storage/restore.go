package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// BackupInfo describes a database file that is about to be restored.
type BackupInfo struct {
	Users       int
	Monitors    int
	ClusterMode bool
	Migrations  int
}

// InspectBackup opens a database file read-only and checks that it is an intact Intrabin database.
func InspectBackup(ctx context.Context, path string) (*BackupInfo, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if st.IsDir() || st.Size() == 0 {
		return nil, errors.New("not a database file")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return nil, fmt.Errorf("not a readable SQLite database: %w", err)
	}
	if res != "ok" {
		return nil, fmt.Errorf("integrity check failed: %s", res)
	}
	info := &BackupInfo{}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&info.Migrations); err != nil {
		return nil, errors.New("not an Intrabin database (no migration table)")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&info.Users); err != nil {
		return nil, errors.New("not an Intrabin database (no users table)")
	}
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitors`).Scan(&info.Monitors)
	var mode sql.NullString
	_ = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'local.cluster_mode'`).Scan(&mode)
	info.ClusterMode = mode.Valid && mode.String == "1"
	return info, nil
}

// ReplaceDatabase installs src as the database at dst (the server must be stopped). The current
// database, if any, is kept next to it as <dst>.pre-restore-<time>; its WAL files are moved with it.
// It returns the name of that copy ("" if there was no database).
func ReplaceDatabase(src, dst string) (string, error) {
	tmp := dst + ".restore-tmp"
	if err := copyFile(src, tmp, 0o600); err != nil {
		return "", err
	}
	kept := ""
	if _, err := os.Stat(dst); err == nil {
		kept = dst + ".pre-restore-" + time.Now().UTC().Format("20060102-150405")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, err := os.Stat(dst + suffix); err == nil {
				if err := os.Rename(dst+suffix, kept+suffix); err != nil {
					os.Remove(tmp)
					return "", err
				}
			}
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return kept, err
	}
	return kept, syncDir(filepath.Dir(dst))
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
