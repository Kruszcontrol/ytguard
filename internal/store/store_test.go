package store

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestMigrations(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	k := Kid{LinuxUser: "a", Name: "A"}
	if err := st.SaveKid(&k); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Pretend a newer build adds a migration.
	saved := migrations
	defer func() { migrations = saved }()
	migrations = append(append([]func(*sql.Tx) error{}, saved...), func(tx *sql.Tx) error {
		_, err := tx.Exec(`ALTER TABLE kids ADD COLUMN extra TEXT NOT NULL DEFAULT 'x'`)
		return err
	})
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var extra string
	if err := st.DB.QueryRow(`SELECT extra FROM kids WHERE linux_user='a'`).Scan(&extra); err != nil || extra != "x" {
		t.Fatalf("migration not applied: %q %v", extra, err)
	}
	st.Close()
	files, _ := os.ReadDir(dir)
	backups := 0
	for _, f := range files {
		if strings.HasPrefix(f.Name(), fmt.Sprintf("ytguard.db.backup-schema%d-", SchemaVersion()-1)) {
			backups++
		}
	}
	if backups != 1 || len(files) == 0 {
		t.Fatalf("want 1 backup, have %d", backups)
	}

	// Older build refuses the newer database.
	migrations = saved
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "newer ytguard") {
		t.Fatalf("downgrade not refused: %v", err)
	}
}
