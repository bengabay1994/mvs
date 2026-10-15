package migration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bengabay1994/mvs/internal/adapter"
	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

func TestExecuteRollsBackEarlierAdaptersWhenLaterAdapterFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	workspace := t.TempDir()
	oldCWD := filepath.Join(workspace, "migration-old")
	newCWD := filepath.Join(workspace, "migration-new")
	oldDir := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaudeForTest(oldCWD))
	oldTranscript := filepath.Join(oldDir, "session-1.jsonl")
	transcript, err := json.Marshal(map[string]string{"cwd": oldCWD, "type": "user"})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteForTest(t, oldTranscript, string(transcript)+"\n")

	claude := &adapter.Claude{}
	discovered, err := claude.Discover()
	if err != nil {
		t.Fatal(err)
	}
	claudePlan, err := claude.Plan(discovered, session.PlanOpts{Scope: session.ScopeProject, NewCWD: newCWD, Mode: session.ModeMove})
	if err != nil {
		t.Fatal(err)
	}

	// Gemini must fail before it writes because malformed registries are not
	// safe to replace. This happens after the Claude plan has been applied.
	mustWriteForTest(t, filepath.Join(paths.GeminiRoot(), "projects.json"), "{not-json")
	geminiPlan := session.Plan{
		Agent:  "gemini",
		OldCWD: filepath.Join(workspace, "gemini-old"),
		NewCWD: filepath.Join(workspace, "gemini-new"),
		Mode:   session.ModeMove.String(),
	}
	unstartedCodexPlan := session.Plan{
		Agent:  "codex",
		OldCWD: filepath.Join(workspace, "codex-old"),
		NewCWD: filepath.Join(workspace, "codex-new"),
		Mode:   session.ModeMove.String(),
	}

	runID, reports, err := Execute([]session.Plan{claudePlan, geminiPlan, unstartedCodexPlan})
	if err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error does not confirm rollback: %v", err)
	}
	if len(reports) != 2 || !reports[0].OK || reports[1].OK {
		t.Fatalf("unexpected reports: %+v", reports)
	}
	data, err := os.ReadFile(oldTranscript)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.CWD != oldCWD {
		t.Fatalf("source transcript cwd = %q, want %q", restored.CWD, oldCWD)
	}
	dstTranscript := filepath.Join(paths.ClaudeRoot(), "projects", encodeClaudeForTest(newCWD), "session-1.jsonl")
	if _, err := os.Stat(dstTranscript); !os.IsNotExist(err) {
		t.Fatalf("destination transcript remains after rollback: %v", err)
	}
	if err := backup.Undo(runID); err == nil {
		t.Fatal("automatically restored run can be undone a second time")
	}
}

func TestValidateRejectsUnsafePlanSets(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	valid := session.Plan{Agent: "claude", OldCWD: oldPath, NewCWD: newPath, Mode: "move"}
	tests := []struct {
		name  string
		plans []session.Plan
	}{
		{name: "none"},
		{name: "same path", plans: []session.Plan{{Agent: "claude", OldCWD: oldPath, NewCWD: filepath.Join(oldPath, "."), Mode: "move"}}},
		{name: "relative", plans: []session.Plan{{Agent: "claude", OldCWD: "old", NewCWD: newPath, Mode: "move"}}},
		{name: "invalid mode", plans: []session.Plan{{Agent: "claude", OldCWD: oldPath, NewCWD: newPath, Mode: "merge"}}},
		{name: "duplicate adapter", plans: []session.Plan{valid, {Agent: "claude", OldCWD: filepath.Join(root, "other"), NewCWD: newPath, Mode: "move"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.plans); err == nil {
				t.Fatal("unsafe plans unexpectedly validated")
			}
		})
	}
}

func encodeClaudeForTest(path string) string {
	var out strings.Builder
	for i := 0; i < len(path); i++ {
		ch := path[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			out.WriteByte(ch)
		} else {
			out.WriteByte('-')
		}
	}
	return out.String()
}

func mustWriteForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
