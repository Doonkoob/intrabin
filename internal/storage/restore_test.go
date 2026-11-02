package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupInspectAndReplace(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	live := filepath.Join(dir, "intrabin.db")
	st, err := Open(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES('app.instance_name', 'before', 0)`); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup.db")
	if err := st.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE settings SET value='after' WHERE key='app.instance_name'`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	info, err := InspectBackup(ctx, backup)
	if err != nil || info.ClusterMode || info.Migrations == 0 {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	junk := filepath.Join(dir, "junk.db")
	_ = os.WriteFile(junk, []byte("not a database at all, just text"), 0o600)
	if _, err := InspectBackup(ctx, junk); err == nil {
		t.Fatal("junk accepted as a backup")
	}

	kept, err := ReplaceDatabase(backup, live)
	if err != nil || kept == "" {
		t.Fatalf("replace: %q %v", kept, err)
	}
	st, err = Open(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v, _ := st.GetSetting(ctx, "app.instance_name"); v != "before" {
		t.Fatalf("restored value = %q", v)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("previous database not kept: %v", err)
	}
}
