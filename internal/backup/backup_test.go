package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bengabay1994/mvs/internal/fsutil"
	"github.com/bengabay1994/mvs/internal/session"
)

func TestRunIDsAreUniqueAndUndoRejectsTraversal(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for range 100 {
		id := NewRunID()
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate run id: %s", id)
		}
		seen[id] = struct{}{}
	}
	for _, id := range []string{"../outside", "..", "/absolute", "20260730-120000/../../outside"} {
		if err := Undo(id); err == nil {
			t.Fatalf("unsafe run id %q was accepted", id)
		}
	}
}

func TestUndoRefusesToOverwritePostMigrationChanges(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	id := NewRunID()
	if _, err := Prepare(id, []session.Plan{{
		Agent: "unknown", OldCWD: "/old", NewCWD: "/new", Mode: "move",
	}}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "agent.db")
	if err := os.WriteFile(target, []byte("post-migration"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := fsutil.Fingerprint(target)
	if err != nil {
		t.Fatal(err)
	}
	reports := []session.Report{{
		Agent: "unknown",
		OK:    true,
		Applied: []session.Action{{
			Kind: "write_file", Target: target, Digest: digest,
		}},
	}}
	if err := Finalize(id, reports); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new session data"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Undo(id)
	if err == nil || !strings.Contains(err.Error(), "refusing undo") {
		t.Fatalf("Undo() error = %v, want drift refusal", err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new session data" {
		t.Fatalf("post-migration data was changed: %q", data)
	}
}

func TestPrepareCreatesPrivateBackupAndManifest(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	id := NewRunID()
	dir, err := Prepare(id, []session.Plan{{
		Agent: "claude", OldCWD: "/old", NewCWD: "/new", Mode: "move",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "mvs", "backups", id); dir != want {
		t.Fatalf("backup dir = %q, want %q", dir, want)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("backup dir mode = %o, want 700", got)
	}
	manifestInfo, err := os.Stat(filepath.Join(dir, "undo.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := manifestInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("manifest mode = %o, want 600", got)
	}
	if _, err := Prepare(id, nil); err == nil {
		t.Fatal("duplicate backup id unexpectedly reused")
	}
}

func TestRestoreSQLitePreservesLiveFamilyWhenSnapshotInstallFails(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "agent.db")
	if err := os.WriteFile(destination, []byte("main-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination+"-wal", []byte("wal-before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreSQLite(filepath.Join(dir, "missing-snapshot"), destination); err == nil {
		t.Fatal("restoreSQLite unexpectedly succeeded")
	}
	mainData, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	walData, err := os.ReadFile(destination + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if string(mainData) != "main-before" || string(walData) != "wal-before" {
		t.Fatalf("SQLite family was not restored: main=%q wal=%q", mainData, walData)
	}
}
