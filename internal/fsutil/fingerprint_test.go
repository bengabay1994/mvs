package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintDetectsDirectoryChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("directory fingerprint did not change")
	}
}

func TestSQLiteFingerprintIncludesWALState(t *testing.T) {
	db := filepath.Join(t.TempDir(), "agent.db")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := SQLiteFingerprint(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db+"-wal", []byte("new committed data"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := SQLiteFingerprint(db)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("SQLite fingerprint ignored WAL creation")
	}
}
