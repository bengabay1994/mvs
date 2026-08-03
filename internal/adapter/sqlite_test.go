package adapter

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSQLiteDSNAndSnapshotPreserveReservedFilenameCharacters(t *testing.T) {
	name := "agent database #%.sqlite"
	if runtime.GOOS != "windows" {
		name = "agent database ?#%.sqlite"
	}
	dbPath := filepath.Join(t.TempDir(), name)
	dsn, err := sqliteDSN(dbPath, "", "busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (value TEXT); INSERT INTO marker VALUES ('expected')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("encoded DSN did not create the exact database path: %v", err)
	}

	snapshotPath := filepath.Join(filepath.Dir(dbPath), "snapshot "+name)
	if err := snapshotSQLite(dbPath, snapshotPath); err != nil {
		t.Fatal(err)
	}
	snapshotDSN, err := sqliteDSN(snapshotPath, "ro")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := sql.Open("sqlite", snapshotDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var value string
	if err := snapshot.QueryRow(`SELECT value FROM marker`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "expected" {
		t.Fatalf("snapshot value = %q, want expected", value)
	}
}
