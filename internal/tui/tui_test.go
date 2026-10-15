package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestSessionSelectionAndProjectToggle(t *testing.T) {
	root := t.TempDir()
	old, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	var sessions []session.Session
	for _, id := range []string{"one", "two", "three", "four", "five"} {
		sessions = append(sessions, session.Session{Agent: "codex", ID: id, CWD: old, Title: "Conversation " + id, Path: filepath.Join(root, id+".jsonl"), Modified: time.Now()})
	}
	m := NewModel(session.ModeMove)
	updated, _ := m.Update(scanDoneMsg{sessions: sessions})
	m = updated.(Model)
	if m.scope != session.ScopeSession || len(m.groups) != 5 {
		t.Fatalf("default view = %+v", m.groups)
	}
	if !strings.Contains(m.viewList(), "tab: switch sessions/projects") {
		t.Fatal("mode switch is not discoverable")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	m = updated.(Model)
	plans, err := buildPlans(m, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].SessionIDs) != 1 || plans[0].Scope != "session" {
		t.Fatalf("selected more than one session: %+v", plans)
	}
	m.selected[1] = true
	plans, err = buildPlans(m, target)
	if err != nil || len(plans) != 1 || len(plans[0].SessionIDs) != 2 {
		t.Fatalf("same-source sessions were not batched: %+v, %v", plans, err)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.scope != session.ScopeProject || len(m.groups) != 1 || m.anySelected() {
		t.Fatalf("bad project switch: %+v", m)
	}
	m.selected[0] = true
	plans, err = buildPlans(m, target)
	if err != nil || len(plans[0].SessionIDs) != 5 || plans[0].Scope != "project" {
		t.Fatalf("project does not include all sessions: %+v, %v", plans, err)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.scope != session.ScopeSession || len(m.groups) != 5 || m.anySelected() {
		t.Fatal("returning to sessions retained project selection")
	}
	for _, query := range []string{"Conversation two", "two", old, "codex"} {
		m.filter.SetValue(query)
		m.refilter()
		if len(m.visible) == 0 {
			t.Fatalf("cannot filter by %q", query)
		}
	}
}

func TestHistoryOnlyStoresAppearOnlyInProjectMode(t *testing.T) {
	m := NewModel(session.ModeMove)
	updated, _ := m.Update(scanDoneMsg{sessions: []session.Session{{Agent: "gemini", ID: "history/project", CWD: t.TempDir(), ProjectOnly: true}}})
	m = updated.(Model)
	if len(m.groups) != 0 {
		t.Fatal("history-only store was offered as a conversation")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if len(m.groups) != 1 {
		t.Fatal("history-only project is not available in project mode")
	}
}

func TestSessionPickerMovesOnlyChosenConversationThroughConfirmation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, ".claude"))
	old, target := filepath.Join(root, "old"), filepath.Join(root, "target")
	// Claude's path encoding is lossy, but the metadata remains authoritative.
	encoded := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, old)
	source := filepath.Join(root, ".claude", "projects", encoded)
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	var sessions []session.Session
	for _, id := range []string{"chosen", "leave"} {
		path := filepath.Join(source, id+".jsonl")
		data, err := json.Marshal(map[string]any{"cwd": old, "type": "user"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session.Session{Agent: "claude", ID: id, CWD: old, Path: path})
	}
	m := NewModel(session.ModeMove)
	step := func(msg tea.Msg) tea.Cmd {
		t.Helper()
		model, cmd := m.Update(msg)
		m = model.(Model)
		return cmd
	}
	step(scanDoneMsg{sessions: sessions})
	step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if m.phase != phaseTarget {
		t.Fatal("did not advance to target input")
	}
	m.target.SetValue(target + string(os.PathSeparator))
	step(tea.KeyMsg{Type: tea.KeyEnter})
	if m.phase != phaseConfirm || !strings.Contains(m.viewConfirm(), "selected sessions: chosen") || strings.Contains(m.viewConfirm(), "leave") {
		t.Fatalf("unexpected confirmation: %s", m.viewConfirm())
	}
	cmd := step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("confirmation did not start migration")
	}
	step(cmd())
	if m.err != nil || m.phase != phaseDone {
		t.Fatalf("migration failed: %v", m.err)
	}
	if _, err := os.Stat(sessions[0].Path); !os.IsNotExist(err) {
		t.Fatal("selected session still at source")
	}
	if _, err := os.Stat(sessions[1].Path); err != nil {
		t.Fatal("unselected session was moved")
	}
	if err := backup.Undo(m.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessions[0].Path); err != nil {
		t.Fatal("undo did not restore selected session")
	}
}

func TestCompletePath(t *testing.T) {
	tmp := t.TempDir()
	mk := func(name string, isDir bool) {
		p := filepath.Join(tmp, name)
		if isDir {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("Repos", true)
	mk("Records", true)
	mk("RemoteThing", false)
	mk("Other", true)
	mk(".hidden", true)

	sep := string(os.PathSeparator)

	tests := []struct {
		name        string
		in          string
		wantOut     string
		wantSuggest []string
	}{
		{
			name:    "unique dir match completes with separator",
			in:      tmp + sep + "Other",
			wantOut: tmp + sep + "Other" + sep,
		},
		{
			name:    "common prefix collapses ambiguous matches",
			in:      tmp + sep + "Re",
			wantOut: tmp + sep + "Re", // already at LCP among Repos/Records/RemoteThing
		},
		{
			name:    "narrower fragment resolves to single dir",
			in:      tmp + sep + "Rep",
			wantOut: tmp + sep + "Repos" + sep,
		},
		{
			name:        "fragment matching multiple non-LCP-extendable items returns menu",
			in:          tmp + sep + "Re",
			wantSuggest: []string{"Records", "RemoteThing", "Repos"},
		},
		{
			name:    "empty input is left alone",
			in:      "",
			wantOut: "",
		},
		{
			name: "empty fragment hides dotfiles",
			in:   tmp + sep,
			// Suggestions should not contain ".hidden"
			wantSuggest: []string{"Other", "Records", "RemoteThing", "Repos"},
		},
		{
			name:    "dotfile fragment surfaces dotfiles",
			in:      tmp + sep + ".",
			wantOut: tmp + sep + ".hidden" + sep,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, sug := completePath(tc.in)
			if tc.wantOut != "" && out != tc.wantOut {
				t.Errorf("got out %q, want %q", out, tc.wantOut)
			}
			if tc.wantSuggest != nil {
				sort.Strings(sug)
				if !reflect.DeepEqual(sug, tc.wantSuggest) {
					t.Errorf("got suggestions %v, want %v", sug, tc.wantSuggest)
				}
			}
		})
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"abc", "abd", "abe"}, "ab"},
		{[]string{"abc"}, "abc"},
		{[]string{}, ""},
		{[]string{"x", "y"}, ""},
		{[]string{"foo/", "foo/bar"}, "foo/"},
	}
	for _, c := range cases {
		got := longestCommonPrefix(c.in)
		if got != c.want {
			t.Errorf("longestCommonPrefix(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	sep := string(os.PathSeparator)
	if got := expandTilde("~"); got != home {
		t.Errorf("expandTilde(~) = %q, want %q", got, home)
	}
	if got := expandTilde("~" + sep + "foo"); got != filepath.Join(home, "foo") {
		t.Errorf("expandTilde(~/foo) = %q, want %q", got, filepath.Join(home, "foo"))
	}
	if got := expandTilde("/abs"); got != "/abs" {
		t.Errorf("expandTilde(/abs) = %q, want unchanged", got)
	}
	if got := expandTilde("~something"); !strings.HasPrefix(got, "~") {
		t.Errorf("expandTilde(~something) should not be expanded, got %q", got)
	}
}
