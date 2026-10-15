package adapter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

func TestClaudeOneOfFiveSessionsAndUndo(t *testing.T) {
	for _, mode := range []session.Mode{session.ModeMove, session.ModeCopy} {
		t.Run(mode.String(), func(t *testing.T) {
			configureTestHome(t)
			root := t.TempDir()
			old, target := filepath.Join(root, "old"), filepath.Join(root, "target")
			source := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaude(old))
			var history strings.Builder
			for i := 1; i <= 5; i++ {
				id := fmt.Sprintf("session-%d", i)
				mustWriteJSONLine(t, filepath.Join(source, id+".jsonl"), map[string]any{"cwd": old, "sessionId": id, "message": "historical " + old})
				history.WriteString(jsonLine(t, map[string]any{"project": old, "sessionId": id, "display": id}))
			}
			history.WriteString(jsonLine(t, map[string]any{"project": old, "display": "unattributed"}))
			historyPath := filepath.Join(paths.ClaudeRoot(), "history.jsonl")
			mustWriteFile(t, historyPath, history.String())
			companion := filepath.Join(source, "session-1", "subagents", "agent.jsonl")
			mustWriteJSONLine(t, companion, map[string]any{"cwd": old, "message": "subagent history"})
			config := jsonObject(t, map[string]any{"projects": map[string]any{old: map[string]any{"trust": "source"}, target: map[string]any{"trust": "target"}}})
			mustWriteFile(t, paths.ClaudeJSON(), config)
			a := &Claude{}
			all, err := a.Discover()
			if err != nil {
				t.Fatal(err)
			}
			plan, err := a.Plan(all[:1], session.PlanOpts{NewCWD: target, Mode: mode})
			if err != nil {
				t.Fatal(err)
			}
			id, report := applyForTest(t, a, plan)
			if !report.OK {
				t.Fatal(report.Errors)
			}
			dest := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaude(target), "session-1.jsonl")
			assertFileContains(t, dest, `"cwd":`+jsonLiteral(t, target))
			assertFileContains(t, dest, jsonLiteral(t, "historical "+old))
			assertFileContains(t, filepath.Join(filepath.Dir(dest), "session-1", "subagents", "agent.jsonl"), `"cwd":`+jsonLiteral(t, target))
			assertFileContents(t, paths.ClaudeJSON(), config)
			data, err := os.ReadFile(historyPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(data), `"project":`+jsonLiteral(t, target)); got != 1 {
				t.Fatalf("moved %d prompt history entries, want 1", got)
			}
			for i := 2; i <= 5; i++ {
				assertFileContains(t, filepath.Join(source, fmt.Sprintf("session-%d.jsonl", i)), `"cwd":`+jsonLiteral(t, old))
			}
			changed := filepath.Join(source, "session-2.jsonl")
			mustWriteJSONLine(t, changed, map[string]any{"cwd": old, "new": "unselected conversation continued"})
			// Session transfers do not own shared trust configuration, even on undo.
			mustWriteFile(t, paths.ClaudeJSON(), config+"\n")
			if err := backup.Undo(id); err != nil {
				t.Fatal(err)
			}
			assertFileContains(t, filepath.Join(source, "session-1.jsonl"), `"cwd":`+jsonLiteral(t, old))
			assertFileContains(t, changed, "unselected conversation continued")
			assertFileContains(t, companion, `"cwd":`+jsonLiteral(t, old))
			assertFileContents(t, historyPath, history.String())
			assertFileContents(t, paths.ClaudeJSON(), config+"\n")
			assertAbsent(t, dest)
		})
	}
}

func TestCodexOneOfFiveSessionsAndUndo(t *testing.T) {
	configureTestHome(t)
	root := t.TempDir()
	old, target := filepath.Join(root, "old"), filepath.Join(root, "target")
	dbPath := filepath.Join(paths.CodexRoot(), "state_5.sqlite")
	db := openTestDB(t, dbPath)
	mustExec(t, db, `CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT, cwd TEXT)`)
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("session-%d", i)
		path := filepath.Join(paths.CodexRoot(), "sessions", "rollout-"+id+".jsonl")
		mustWriteFile(t, path, jsonLine(t, map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "cwd": old}})+jsonLine(t, map[string]any{"type": "turn_context", "payload": map[string]any{"cwd": old}}))
		mustExec(t, db, `INSERT INTO threads VALUES (?, ?, ?)`, id, path, old)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	a := &Codex{}
	all, err := a.Discover()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := a.Plan(all[:1], session.PlanOpts{NewCWD: target})
	if err != nil {
		t.Fatal(err)
	}
	id, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatal(report.Errors)
	}
	db = openTestDB(t, dbPath)
	var moved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE cwd = ?`, target).Scan(&moved); err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("moved %d threads, want 1", moved)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, s := range all[1:] {
		assertFileContains(t, s.Path, `"cwd":`+jsonLiteral(t, old))
	}
	if err := backup.Undo(id); err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		assertFileContains(t, s.Path, `"cwd":`+jsonLiteral(t, old))
	}
}

func TestGeminiOneOfFiveSessionsAndUndo(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, mode := range []session.Mode{session.ModeMove, session.ModeCopy} {
			for _, existingTarget := range []bool{false, true} {
				t.Run(fmt.Sprintf("legacy=%t/%s/existing=%t", legacy, mode, existingTarget), func(t *testing.T) {
					configureTestHome(t)
					root := t.TempDir()
					old, target := filepath.Join(root, "old"), filepath.Join(root, "target")
					old, target = geminiNormalize(old), geminiNormalize(target)
					oldID, newID := "source-slug", "target-slug"
					registry := map[string]string{}
					if legacy {
						oldID, newID = sha256Hex(old), sha256Hex(target)
					} else {
						registry[geminiNormalize(old)] = oldID
					}
					if existingTarget {
						registry[geminiNormalize(target)] = newID
					}
					registryPath := filepath.Join(paths.GeminiRoot(), "projects.json")
					mustWriteJSONFile(t, registryPath, map[string]any{"projects": registry, "other": "keep"})
					source := filepath.Join(paths.GeminiRoot(), "tmp", oldID)
					mustWriteFile(t, filepath.Join(source, ".project_root"), old+"\n")
					for i := 1; i <= 5; i++ {
						mustWriteFile(t, filepath.Join(source, "chats", fmt.Sprintf("chat-%d.jsonl", i)), jsonLine(t, map[string]any{"projectHash": sha256Hex(old), "directories": []string{old}})+jsonLine(t, map[string]any{"text": "historical " + old}))
					}
					if existingTarget {
						dst := filepath.Join(paths.GeminiRoot(), "tmp", newID)
						mustWriteFile(t, filepath.Join(dst, ".project_root"), target+"\n")
						mustWriteFile(t, filepath.Join(dst, "chats", "existing.jsonl"), "existing target conversation\n")
					}
					a := &Gemini{}
					all, err := a.Discover()
					if err != nil {
						t.Fatal(err)
					}
					var selected session.Session
					for _, s := range all {
						if s.CWD == old && filepath.Base(s.Path) == "chat-1.jsonl" {
							selected = s
						}
					}
					if selected.ID == "" {
						t.Fatal("source chat not discovered")
					}
					plan, err := a.Plan([]session.Session{selected}, session.PlanOpts{NewCWD: target, Mode: mode})
					if err != nil {
						t.Fatal(err)
					}
					id, report := applyForTest(t, a, plan)
					if !report.OK {
						t.Fatal(report.Errors)
					}
					dest := plan.Actions[0].Target
					assertFileContains(t, dest, sha256Hex(target))
					assertFileContains(t, dest, jsonLiteral(t, "historical "+old))
					assertFileContents(t, filepath.Join(source, ".project_root"), old+"\n")
					for i := 2; i <= 5; i++ {
						assertFileContains(t, filepath.Join(source, "chats", fmt.Sprintf("chat-%d.jsonl", i)), sha256Hex(old))
					}
					if mode == session.ModeMove {
						assertAbsent(t, selected.Path)
					} else {
						assertFileContains(t, selected.Path, sha256Hex(old))
					}
					current, err := readGeminiProjects()
					if err != nil {
						t.Fatal(err)
					}
					if !legacy && current[geminiNormalize(old)] != oldID {
						t.Fatal("source project lost its registry entry")
					}
					changed := filepath.Join(source, "chats", "chat-2.jsonl")
					mustWriteFile(t, changed, "unselected conversation continued\n")
					if err := backup.Undo(id); err != nil {
						t.Fatal(err)
					}
					assertFileContains(t, selected.Path, sha256Hex(old))
					assertFileContents(t, changed, "unselected conversation continued\n")
					assertAbsent(t, dest)
					if existingTarget {
						assertFileContents(t, filepath.Join(filepath.Dir(dest), "existing.jsonl"), "existing target conversation\n")
					}
				})
			}
		}
	}
}

func TestOpenCodeOneOfFiveSessionsAndUndo(t *testing.T) {
	for _, existingTarget := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existingTarget), func(t *testing.T) {
			configureTestHome(t)
			root := t.TempDir()
			old, target := filepath.Join(root, "old"), filepath.Join(root, "target")
			dbPath := filepath.Join(paths.OpenCodeRoot(), "opencode.db")
			db := openTestDB(t, dbPath)
			mustExec(t, db, `CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT, sandboxes TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL)`)
			mustExec(t, db, `CREATE TABLE workspace (id TEXT PRIMARY KEY, directory TEXT)`)
			mustExec(t, db, `CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, project_id TEXT REFERENCES project(id), workspace_id TEXT REFERENCES workspace(id), parent_id TEXT, path TEXT, title TEXT, time_updated INTEGER)`)
			mustExec(t, db, `INSERT INTO project VALUES ('source', ?, '[]', 1, 1)`, filepath.ToSlash(old))
			mustExec(t, db, `INSERT INTO workspace VALUES ('source-workspace', ?)`, filepath.ToSlash(old))
			if existingTarget {
				mustExec(t, db, `INSERT INTO project VALUES ('target', ?, '[]', 2, 2)`, filepath.ToSlash(target))
				mustExec(t, db, `INSERT INTO workspace VALUES ('target-workspace', ?)`, filepath.ToSlash(target))
			}
			for i := 1; i <= 5; i++ {
				mustExec(t, db, `INSERT INTO session VALUES (?, ?, 'source', 'source-workspace', 'session-2', '.', 'title', 1)`, fmt.Sprintf("session-%d", i), filepath.ToSlash(old))
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(paths.OpenCodeRoot(), "snapshot", "source", sha1Hex(old), "objects", "state")
			mustWriteFile(t, snapshot, "snapshot objects")
			a := &OpenCode{}
			all, err := a.Discover()
			if err != nil {
				t.Fatal(err)
			}
			plan, err := a.Plan(all[:1], session.PlanOpts{NewCWD: target})
			if err != nil {
				t.Fatal(err)
			}
			id, report := applyForTest(t, a, plan)
			if !report.OK {
				t.Fatal(report.Errors)
			}
			db = openTestDB(t, dbPath)
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM session WHERE directory = ?`, filepath.ToSlash(old)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 4 {
				t.Fatalf("%d sessions stayed, want 4", count)
			}
			var project, directory string
			var workspace, parent any
			if err := db.QueryRow(`SELECT project_id, directory, workspace_id, parent_id FROM session WHERE id = ?`, all[0].ID).Scan(&project, &directory, &workspace, &parent); err != nil {
				t.Fatal(err)
			}
			wantProject := "global"
			if existingTarget {
				wantProject = "target"
			}
			if project != wantProject || directory != filepath.ToSlash(target) || parent != nil {
				t.Fatalf("wrong session destination: %q %q %v %v", project, directory, workspace, parent)
			}
			if existingTarget && workspace != "target-workspace" {
				t.Fatalf("target workspace = %v", workspace)
			}
			if !existingTarget && workspace != nil {
				t.Fatalf("kept source workspace: %v", workspace)
			}
			var worktree string
			if err := db.QueryRow(`SELECT worktree FROM project WHERE id = 'source'`).Scan(&worktree); err != nil {
				t.Fatal(err)
			}
			if worktree != filepath.ToSlash(old) {
				t.Fatal("shared source project was relocated")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			assertFileContents(t, snapshot, "snapshot objects")
			destinationSnapshot := filepath.Join(paths.OpenCodeRoot(), "snapshot", wantProject, sha1Hex(target))
			assertFileContents(t, filepath.Join(destinationSnapshot, "objects", "state"), "snapshot objects")
			// A second independent session can reuse the copied snapshot store.
			repeatPlan, err := a.Plan(all[1:2], session.PlanOpts{NewCWD: target})
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range repeatPlan.Actions {
				if action.Kind == "copy_dir" {
					t.Fatal("identical target snapshots should be reused")
				}
			}
			mustWriteFile(t, filepath.Join(destinationSnapshot, "objects", "state"), "conflicting target objects")
			if _, err := a.Plan(all[1:2], session.PlanOpts{NewCWD: target}); err == nil {
				t.Fatal("conflicting snapshots were accepted")
			}
			mustWriteFile(t, filepath.Join(destinationSnapshot, "objects", "state"), "snapshot objects")
			if err := backup.Undo(id); err != nil {
				t.Fatal(err)
			}
			assertFileContents(t, snapshot, "snapshot objects")
			assertAbsent(t, destinationSnapshot)
			db = openTestDB(t, dbPath)
			defer db.Close()
			if err := db.QueryRow(`SELECT COUNT(*) FROM session WHERE directory = ? AND project_id = 'source' AND workspace_id = 'source-workspace'`, filepath.ToSlash(old)).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 5 {
				t.Fatalf("restored %d source sessions, want 5", count)
			}
		})
	}
}

func assertFileContents(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s contents = %q, want %q", path, data, want)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists: %v", path, err)
	}
}

func TestGeminiFailedSessionTransferRollsBackOnlySelectedFiles(t *testing.T) {
	configureTestHome(t)
	root := t.TempDir()
	old, target := geminiNormalize(filepath.Join(root, "old")), geminiNormalize(filepath.Join(root, "target"))
	source := filepath.Join(paths.GeminiRoot(), "tmp", sha256Hex(old))
	marker := filepath.Join(source, ".project_root")
	mustWriteFile(t, marker, old+"\n")
	selected := filepath.Join(source, "chats", "broken.jsonl")
	mustWriteFile(t, selected, "invalid header\n")
	unselected := filepath.Join(source, "chats", "other.jsonl")
	mustWriteFile(t, unselected, "other conversation\n")
	a := &Gemini{}
	plan, err := a.Plan([]session.Session{{Agent: "gemini", CWD: old, ID: "broken", Path: selected}}, session.PlanOpts{NewCWD: target})
	if err != nil {
		t.Fatal(err)
	}
	id, report := applyForTest(t, a, plan)
	if report.OK {
		t.Fatal("invalid header unexpectedly migrated")
	}
	if err := backup.Rollback(id, []session.Report{report}); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, selected, "invalid header\n")
	assertFileContents(t, unselected, "other conversation\n")
	assertFileContents(t, marker, old+"\n")
	assertAbsent(t, filepath.Join(paths.GeminiRoot(), "tmp", sha256Hex(target)))
	assertAbsent(t, filepath.Join(paths.GeminiRoot(), "projects.json"))
}

func TestSessionMetadataTitles(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude.jsonl")
	mustWriteFile(t, claude, `{"cwd":"/source","type":"user","message":{"content":[{"type":"text","text":"Fix the parser\nDetails"}]}}`+"\n")
	if _, title := readClaudeMeta(claude); title != "Fix the parser" {
		t.Fatalf("Claude title = %q", title)
	}
	codex := filepath.Join(root, "codex.jsonl")
	mustWriteFile(t, codex, `{"type":"session_meta","payload":{"id":"one","cwd":"/source"}}`+"\n"+
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"text","text":"Ignore assistant"}]}}`+"\n"+
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"Fix the \"parser\""}]}}`+"\n")
	if _, _, title := readCodexMeta(codex); title != `Fix the "parser"` {
		t.Fatalf("Codex title = %q", title)
	}
}

func TestClaudeSessionWithinCollidingStoragePreservesCompanionSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires Windows privileges")
	}
	configureTestHome(t)
	root := t.TempDir()
	old, target := filepath.Join(root, "a-b"), filepath.Join(root, "a", "b")
	if encodeClaude(old) != encodeClaude(target) {
		t.Fatal("fixture paths do not share storage")
	}
	store := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaude(old))
	transcript := filepath.Join(store, "selected.jsonl")
	mustWriteJSONLine(t, transcript, map[string]any{"cwd": old})
	companion := filepath.Join(store, "selected", "subagents", "agent.jsonl")
	mustWriteJSONLine(t, companion, map[string]any{"cwd": old})
	link := filepath.Join(filepath.Dir(companion), "alias.jsonl")
	if err := os.Symlink("agent.jsonl", link); err != nil {
		t.Fatal(err)
	}
	a := &Claude{}
	plan, err := a.Plan([]session.Session{{ID: "selected", CWD: old, Path: transcript}}, session.PlanOpts{NewCWD: target})
	if err != nil {
		t.Fatal(err)
	}
	id, report := applyForTest(t, a, plan)
	if !report.OK {
		t.Fatal(report.Errors)
	}
	if got, err := os.Readlink(link); err != nil || got != "agent.jsonl" {
		t.Fatalf("companion symlink was replaced: %q, %v", got, err)
	}
	assertFileContains(t, companion, `"cwd":`+jsonLiteral(t, target))
	if err := backup.Undo(id); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, companion, `"cwd":`+jsonLiteral(t, old))
	if got, err := os.Readlink(link); err != nil || got != "agent.jsonl" {
		t.Fatalf("companion symlink was not restored: %q, %v", got, err)
	}
}

func TestOpenCodeResolvesNewGitTargetAndCachedIdentity(t *testing.T) {
	configureTestHome(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", target}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("-c", "user.name=MVS Test", "-c", "user.email=mvs@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=", "commit", "--allow-empty", "-qm", "root")
	wantID := git("rev-parse", "HEAD")
	db := openTestDB(t, filepath.Join(root, "agent.db"))
	defer db.Close()
	mustExec(t, db, `CREATE TABLE project (id TEXT PRIMARY KEY, worktree TEXT)`)
	id, worktree, err := openCodeTargetProject(db, filepath.Join(target, "new-workspace"))
	if err != nil {
		t.Fatal(err)
	}
	canonicalTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if id != wantID || worktree != filepath.ToSlash(canonicalTarget) {
		t.Fatalf("target identity = %q %q, want %q %q", id, worktree, wantID, filepath.ToSlash(canonicalTarget))
	}
	commonDir := git("rev-parse", "--git-common-dir")
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(target, commonDir)
	}
	mustWriteFile(t, filepath.Join(commonDir, "opencode"), "cached-project-id\n")
	id, _, err = openCodeTargetProject(db, target)
	if err != nil || id != "cached-project-id" {
		t.Fatalf("cached identity = %q, %v", id, err)
	}
}
