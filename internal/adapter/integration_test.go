package adapter

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

func TestClaudeMovePreservesSessionsInLossyDirectoryAndUndoes(t *testing.T) {
	configureTestHome(t)
	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "mvs-a-b")
	otherCWD := filepath.Join(workspace, "mvs-a", "b")
	newCWD := filepath.Join(workspace, "mvs-target")
	if encodeClaude(oldCWD) != encodeClaude(otherCWD) {
		t.Fatal("test paths must collide under Claude encoding")
	}

	srcDir := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaude(oldCWD))
	mustWriteJSONLine(t, filepath.Join(srcDir, "selected.jsonl"), map[string]any{"cwd": oldCWD, "type": "user"})
	mustWriteJSONLine(t, filepath.Join(srcDir, "unrelated.jsonl"), map[string]any{"cwd": otherCWD, "type": "user"})
	mustWriteJSONLine(t, filepath.Join(paths.ClaudeRoot(), "history.jsonl"), map[string]any{"project": oldCWD, "display": "hello"})
	mustWriteJSONFile(t, paths.ClaudeJSON(), map[string]any{"projects": map[string]any{
		oldCWD: map[string]string{"allowed": "old"},
		newCWD: map[string]string{"allowed": "target"},
	}})

	a := &Claude{}
	discovered, err := a.Discover()
	if err != nil {
		t.Fatal(err)
	}
	var selected []session.Session
	for _, item := range discovered {
		if item.CWD == oldCWD {
			selected = append(selected, item)
		}
	}
	plan, err := a.Plan(selected, session.PlanOpts{NewCWD: newCWD, Mode: session.ModeMove})
	if err != nil {
		t.Fatal(err)
	}
	runID, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatalf("apply failed: %v", report.Errors)
	}

	dstFile := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaude(newCWD), "selected.jsonl")
	assertFileContains(t, dstFile, `"cwd":`+jsonLiteral(t, newCWD))
	assertFileContains(t, filepath.Join(srcDir, "unrelated.jsonl"), `"cwd":`+jsonLiteral(t, otherCWD))
	if _, err := os.Stat(filepath.Join(srcDir, "selected.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("selected transcript still exists at source: %v", err)
	}

	var claudeConfig struct {
		Projects map[string]map[string]string `json:"projects"`
	}
	readJSONFile(t, paths.ClaudeJSON(), &claudeConfig)
	if claudeConfig.Projects[newCWD]["allowed"] != "target" {
		t.Fatal("pre-existing destination Claude config was overwritten")
	}
	if _, exists := claudeConfig.Projects[oldCWD]; exists {
		t.Fatal("old Claude config key still exists after move")
	}
	newDestinationSession := filepath.Join(filepath.Dir(dstFile), "created-after-migration.jsonl")
	mustWriteJSONLine(t, newDestinationSession, map[string]any{"cwd": newCWD, "type": "user"})
	unrelatedSource := filepath.Join(srcDir, "unrelated.jsonl")
	mustWriteJSONLine(t, unrelatedSource, map[string]any{"cwd": otherCWD, "type": "user", "new": true})
	mustWriteJSONLine(t, filepath.Join(srcDir, "selected.jsonl"), map[string]any{"cwd": oldCWD, "type": "new-session"})
	if err := backup.Undo(runID); err == nil || !strings.Contains(err.Error(), "old source") {
		t.Fatalf("undo did not detect a recreated Claude source: %v", err)
	}
	if err := os.Remove(filepath.Join(srcDir, "selected.jsonl")); err != nil {
		t.Fatal(err)
	}

	if err := backup.Undo(runID); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, filepath.Join(srcDir, "selected.jsonl"), `"cwd":`+jsonLiteral(t, oldCWD))
	assertFileContains(t, filepath.Join(srcDir, "unrelated.jsonl"), `"cwd":`+jsonLiteral(t, otherCWD))
	if _, err := os.Stat(dstFile); !os.IsNotExist(err) {
		t.Fatalf("destination transcript remains after undo: %v", err)
	}
	if _, err := os.Stat(newDestinationSession); err != nil {
		t.Fatalf("unrelated destination session was not preserved: %v", err)
	}
	assertFileContains(t, unrelatedSource, `"new":true`)
	if err := backup.Undo(runID); err == nil {
		t.Fatal("second undo unexpectedly succeeded")
	}
}

func TestCodexCopyCreatesValidRolloutAndUndoRemovesIt(t *testing.T) {
	configureTestHome(t)
	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "codex-old")
	newCWD := filepath.Join(workspace, "codex-new")
	oldID := "550e8400-e29b-41d4-a716-446655440000"
	rollout := filepath.Join(paths.CodexRoot(), "sessions", "2026", "07", "30", "rollout-2026-07-30T12-34-56-"+oldID+".jsonl")
	mustWriteFile(t, rollout,
		jsonLine(t, map[string]any{"type": "session_meta", "payload": map[string]any{"id": oldID, "cwd": oldCWD}})+
			jsonLine(t, map[string]any{"type": "turn_context", "payload": map[string]any{"cwd": oldCWD}}))

	dbPath := filepath.Join(paths.CodexRoot(), "state_5.sqlite")
	db := openTestDB(t, dbPath)
	mustExec(t, db, `CREATE TABLE threads (
		id TEXT PRIMARY KEY, rollout_path TEXT, created_at INTEGER, updated_at INTEGER,
		source TEXT, model_provider TEXT, cwd TEXT, title TEXT, sandbox_policy TEXT, approval_mode TEXT
	)`)
	mustExec(t, db, `INSERT INTO threads VALUES (?, ?, 1, 1, 'cli', 'openai', ?, 'title', 'workspace-write', 'ask')`, oldID, rollout, oldCWD)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := &Codex{}
	plan, err := a.Plan([]session.Session{{Agent: "codex", ID: oldID, CWD: oldCWD, Path: rollout}}, session.PlanOpts{
		NewCWD: newCWD,
		Mode:   session.ModeCopy,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatalf("apply failed: %v", report.Errors)
	}

	rollouts, err := filepath.Glob(filepath.Join(filepath.Dir(rollout), "rollout-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rollouts) != 2 {
		t.Fatalf("got %d rollout files, want 2", len(rollouts))
	}
	var copied string
	for _, candidate := range rollouts {
		if candidate != rollout {
			copied = candidate
		}
	}
	assertFileContains(t, copied, `"cwd":`+jsonLiteral(t, newCWD))
	if strings.Contains(filepath.Base(copied), oldID[:24]) {
		t.Fatalf("copied rollout retained part of old UUID: %s", copied)
	}

	db = openTestDB(t, dbPath)
	var targetRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE cwd = ?`, newCWD).Scan(&targetRows); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if targetRows != 1 {
		t.Fatalf("got %d copied thread rows, want 1", targetRows)
	}
	sourceMarker := `{"type":"event_msg","payload":{"text":"source changed after copy"}}` + "\n"
	sourceData, err := os.ReadFile(rollout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, append(sourceData, sourceMarker...), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := backup.Undo(runID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copied); !os.IsNotExist(err) {
		t.Fatalf("copied rollout remains after undo: %v", err)
	}
	assertFileContains(t, rollout, "source changed after copy")
	db = openTestDB(t, dbPath)
	defer db.Close()
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&targetRows); err != nil {
		t.Fatal(err)
	}
	if targetRows != 1 {
		t.Fatalf("got %d thread rows after undo, want 1", targetRows)
	}
}

func TestGeminiMoveUpdatesRegistryWithoutMovingSlugStoreAndUndoes(t *testing.T) {
	configureTestHome(t)
	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "gemini-old")
	newCWD := filepath.Join(workspace, "gemini-new")
	id := "gemini-old"
	projectsPath := filepath.Join(paths.GeminiRoot(), "projects.json")
	mustWriteJSONFile(t, projectsPath, map[string]any{"version": 7, "projects": map[string]string{geminiNormalize(oldCWD): id}})
	chat := filepath.Join(paths.GeminiRoot(), "tmp", id, "chats", "chat.jsonl")
	mustWriteJSONLine(t, chat, map[string]any{"projectHash": sha256Hex(oldCWD), "directories": []string{oldCWD}})
	mustWriteFile(t, filepath.Join(paths.GeminiRoot(), "tmp", id, ".project_root"), geminiNormalize(oldCWD)+"\n")

	a := &Gemini{}
	plan, err := a.Plan([]session.Session{{Agent: "gemini", ID: id, CWD: oldCWD, Path: chat}}, session.PlanOpts{
		NewCWD: newCWD,
		Mode:   session.ModeMove,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatalf("apply failed: %v", report.Errors)
	}

	registry, err := readGeminiProjects()
	if err != nil {
		t.Fatal(err)
	}
	if registry[geminiNormalize(newCWD)] != id {
		t.Fatalf("target registry id = %q, want %q", registry[geminiNormalize(newCWD)], id)
	}
	if _, exists := registry[geminiNormalize(oldCWD)]; exists {
		t.Fatal("old Gemini registry key still exists")
	}
	var rawTop map[string]json.RawMessage
	readJSONFile(t, projectsPath, &rawTop)
	if _, exists := rawTop["version"]; !exists {
		t.Fatal("unknown projects.json fields were discarded")
	}
	assertFileContains(t, chat, sha256Hex(newCWD))
	assertFileContains(t, filepath.Join(paths.GeminiRoot(), "tmp", id, ".project_root"), geminiNormalize(newCWD))
	postMigrationChat := filepath.Join(paths.GeminiRoot(), "tmp", id, "chats", "new-chat.jsonl")
	mustWriteJSONLine(t, postMigrationChat, map[string]any{"projectHash": sha256Hex(newCWD)})
	if err := backup.Undo(runID); err == nil || !strings.Contains(err.Error(), "refusing undo") {
		t.Fatalf("undo did not detect a new Gemini chat: %v", err)
	}
	if _, err := os.Stat(postMigrationChat); err != nil {
		t.Fatalf("drift refusal removed the new Gemini chat: %v", err)
	}
	if err := os.Remove(postMigrationChat); err != nil {
		t.Fatal(err)
	}

	if err := backup.Undo(runID); err != nil {
		t.Fatal(err)
	}
	registry, err = readGeminiProjects()
	if err != nil {
		t.Fatal(err)
	}
	if registry[geminiNormalize(oldCWD)] != id {
		t.Fatal("Gemini registry was not restored")
	}
	assertFileContains(t, chat, sha256Hex(oldCWD))
}

func TestGeminiUndoRemovesMetadataCreatedByMove(t *testing.T) {
	configureTestHome(t)
	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "gemini-legacy-old")
	newCWD := filepath.Join(workspace, "gemini-legacy-new")
	oldID := sha256Hex(oldCWD)
	chat := filepath.Join(paths.GeminiRoot(), "tmp", oldID, "chats", "chat.jsonl")
	mustWriteJSONLine(t, chat, map[string]any{"projectHash": sha256Hex(oldCWD), "directories": []string{oldCWD}})

	a := &Gemini{}
	plan, err := a.Plan([]session.Session{{Agent: "gemini", ID: oldID, CWD: oldCWD, Path: chat}}, session.PlanOpts{
		NewCWD: newCWD,
		Mode:   session.ModeMove,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatalf("apply failed: %v", report.Errors)
	}
	projectsPath := filepath.Join(paths.GeminiRoot(), "projects.json")
	newRootMarker := filepath.Join(paths.GeminiRoot(), "tmp", sha256Hex(newCWD), ".project_root")
	if _, err := os.Stat(projectsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newRootMarker); err != nil {
		t.Fatal(err)
	}

	if err := backup.Undo(runID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(projectsPath); !os.IsNotExist(err) {
		t.Fatalf("projects.json created by migration remains after undo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.GeminiRoot(), "tmp", oldID, ".project_root")); !os.IsNotExist(err) {
		t.Fatalf(".project_root created by migration remains after undo: %v", err)
	}
	assertFileContains(t, chat, sha256Hex(oldCWD))
}

func TestGeminiCopyUndoDoesNotOverwriteActiveSource(t *testing.T) {
	configureTestHome(t)
	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "gemini-copy-old")
	newCWD := filepath.Join(workspace, "gemini-copy-new")
	id := "copy-old"
	mustWriteJSONFile(t, filepath.Join(paths.GeminiRoot(), "projects.json"), map[string]any{"projects": map[string]string{geminiNormalize(oldCWD): id}})
	chat := filepath.Join(paths.GeminiRoot(), "tmp", id, "chats", "chat.jsonl")
	mustWriteJSONLine(t, chat, map[string]any{"projectHash": sha256Hex(oldCWD), "directories": []string{oldCWD}})

	a := &Gemini{}
	plan, err := a.Plan([]session.Session{{Agent: "gemini", ID: id, CWD: oldCWD, Path: chat}}, session.PlanOpts{
		NewCWD: newCWD,
		Mode:   session.ModeCopy,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatalf("apply failed: %v", report.Errors)
	}
	mustWriteJSONLine(t, chat, map[string]any{"projectHash": sha256Hex(oldCWD), "sourceChanged": true})
	if err := backup.Undo(runID); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, chat, `"sourceChanged":true`)
}

func TestOpenCodeMovePreservesRelativePathAndHistoricalDataAndUndoes(t *testing.T) {
	configureTestHome(t)
	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "opencode-old")
	newCWD := filepath.Join(workspace, "opencode-new")
	storedOldCWD := filepath.ToSlash(oldCWD)
	storedNewCWD := filepath.ToSlash(newCWD)
	dbPath := filepath.Join(paths.OpenCodeRoot(), "opencode.db")
	db := openTestDB(t, dbPath)
	mustExec(t, db, `CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT, sandboxes TEXT)`)
	mustExec(t, db, `CREATE TABLE workspace (id TEXT PRIMARY KEY, directory TEXT)`)
	mustExec(t, db, `CREATE TABLE session (
		id TEXT PRIMARY KEY, project_id TEXT, workspace_id TEXT, parent_id TEXT,
		directory TEXT, path TEXT, title TEXT, time_updated INTEGER
	)`)
	mustExec(t, db, `CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT)`)
	mustExec(t, db, `CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, data TEXT)`)
	sandboxesJSON, err := json.Marshal([]string{storedOldCWD, filepath.ToSlash(filepath.Join(workspace, "other-worktree"))})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO project VALUES ('project-1', ?, ?)`, storedOldCWD, string(sandboxesJSON))
	mustExec(t, db, `INSERT INTO workspace VALUES ('workspace-1', ?)`, storedOldCWD)
	mustExec(t, db, `INSERT INTO session VALUES ('session-1', 'project-1', 'workspace-1', NULL, ?, '.', 'title', 1)`, storedOldCWD)
	messageData := jsonObject(t, map[string]any{"role": "user", "content": "historical path " + oldCWD})
	partData := jsonObject(t, map[string]any{"type": "text", "text": "do not rewrite " + oldCWD})
	mustExec(t, db, `INSERT INTO message VALUES ('message-1', 'session-1', ?)`, messageData)
	mustExec(t, db, `INSERT INTO part VALUES ('part-1', 'message-1', 'session-1', ?)`, partData)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	oldSnapshot := filepath.Join(paths.OpenCodeRoot(), "snapshot", "project-1", sha1Hex(oldCWD))
	newSnapshot := filepath.Join(paths.OpenCodeRoot(), "snapshot", "project-1", sha1Hex(newCWD))
	mustWriteFile(t, filepath.Join(oldSnapshot, "state"), "snapshot")

	a := &OpenCode{}
	plan, err := a.Plan([]session.Session{{Agent: "opencode", ID: "session-1", CWD: oldCWD}}, session.PlanOpts{
		NewCWD: newCWD,
		Mode:   session.ModeMove,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatalf("apply failed: %v", report.Errors)
	}

	db = openTestDB(t, dbPath)
	var directory, relativePath, gotMessage, gotPart, sandboxes string
	if err := db.QueryRow(`SELECT directory, path FROM session WHERE id = 'session-1'`).Scan(&directory, &relativePath); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT data FROM message WHERE id = 'message-1'`).Scan(&gotMessage); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT data FROM part WHERE id = 'part-1'`).Scan(&gotPart); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT sandboxes FROM project WHERE id = 'project-1'`).Scan(&sandboxes); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if directory != storedNewCWD || relativePath != "." {
		t.Fatalf("session paths = (%q, %q), want (%q, %q)", directory, relativePath, storedNewCWD, ".")
	}
	if gotMessage != messageData || gotPart != partData {
		t.Fatal("historical OpenCode JSON was rewritten")
	}
	if !strings.Contains(sandboxes, storedNewCWD) || strings.Contains(sandboxes, storedOldCWD) {
		t.Fatalf("project sandboxes were not migrated: %s", sandboxes)
	}
	if _, err := os.Stat(filepath.Join(newSnapshot, "state")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldSnapshot); !os.IsNotExist(err) {
		t.Fatalf("old snapshot remains after move: %v", err)
	}

	if err := backup.Undo(runID); err != nil {
		t.Fatal(err)
	}
	db = openTestDB(t, dbPath)
	defer db.Close()
	if err := db.QueryRow(`SELECT directory, path FROM session WHERE id = 'session-1'`).Scan(&directory, &relativePath); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT sandboxes FROM project WHERE id = 'project-1'`).Scan(&sandboxes); err != nil {
		t.Fatal(err)
	}
	if directory != storedOldCWD || relativePath != "." {
		t.Fatalf("session paths after undo = (%q, %q)", directory, relativePath)
	}
	if !strings.Contains(sandboxes, storedOldCWD) || strings.Contains(sandboxes, storedNewCWD) {
		t.Fatalf("project sandboxes were not restored: %s", sandboxes)
	}
	if _, err := os.Stat(filepath.Join(oldSnapshot, "state")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newSnapshot); !os.IsNotExist(err) {
		t.Fatalf("new snapshot remains after undo: %v", err)
	}

	if _, err := a.Plan([]session.Session{{Agent: "opencode", ID: "session-1", CWD: oldCWD}}, session.PlanOpts{
		NewCWD: newCWD,
		Mode:   session.ModeCopy,
	}); err == nil {
		t.Fatal("OpenCode copy mode unexpectedly planned successfully")
	}
}

func configureTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return home
}

func applyForTest(t *testing.T, a Adapter, plan session.Plan) (string, session.Report) {
	t.Helper()
	runID := backup.NewRunID()
	bkDir, err := backup.Prepare(runID, []session.Plan{plan})
	if err != nil {
		t.Fatal(err)
	}
	report := a.Apply(plan, session.ApplyOpts{BackupDir: bkDir})
	if err := backup.Finalize(runID, []session.Report{report}); err != nil {
		t.Fatal(err)
	}
	return runID, report
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustWriteJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	mustWriteFile(t, path, jsonObject(t, value))
}

func mustWriteJSONLine(t *testing.T, path string, value any) {
	t.Helper()
	mustWriteFile(t, path, jsonLine(t, value))
}

func jsonLine(t *testing.T, value any) string {
	t.Helper()
	return jsonObject(t, value) + "\n"
}

func jsonObject(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func jsonLiteral(t *testing.T, value string) string {
	t.Helper()
	return jsonObject(t, value)
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s does not contain %q; content: %s", path, want, data)
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
