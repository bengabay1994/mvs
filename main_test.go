package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bengabay1994/mvs/internal/session"
	"github.com/bengabay1994/mvs/internal/tui"
)

func TestCLIRequiresExplicitSessionOrProjectSelection(t *testing.T) {
	root := t.TempDir()
	old, target := filepath.Join(root, "old"), filepath.Join(root, "new")
	groups := []tui.Group{{Agent: "codex", CWD: old, Sessions: []session.Session{
		{Agent: "codex", ID: "one", CWD: old, Path: filepath.Join(root, "one.jsonl")},
		{Agent: "codex", ID: "two", CWD: old, Path: filepath.Join(root, "two.jsonl")},
	}}}
	for _, tc := range []struct {
		name    string
		ids     []string
		project bool
		want    int
		err     string
	}{
		{name: "no implicit project migration", err: "select --session"},
		{name: "one session", ids: []string{"one"}, want: 1},
		{name: "several sessions", ids: []string{"one", "two"}, want: 2},
		{name: "whole project", project: true, want: 2},
		{name: "unknown ID refuses entire run", ids: []string{"one", "missing"}, err: "not found"},
		{name: "conflicting selection flags", ids: []string{"one"}, project: true, err: "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans, err := planMigration(groups, old, target, "", tc.ids, tc.project, session.ModeMove)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(plans) != 1 || len(plans[0].SessionIDs) != tc.want {
				t.Fatalf("unexpected plans: %+v", plans)
			}
			wantScope := session.ScopeSession
			if tc.project {
				wantScope = session.ScopeProject
			}
			if plans[0].Scope != wantScope.String() {
				t.Fatalf("scope = %q", plans[0].Scope)
			}
		})
	}
}

func TestCLIRejectsAmbiguousIDsUnlessAgentIsSelected(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	old, target := filepath.Join(root, "old"), filepath.Join(root, "target")
	var groups []tui.Group
	for _, agent := range []string{"claude", "codex"} {
		groups = append(groups, tui.Group{Agent: agent, CWD: old, Sessions: []session.Session{{Agent: agent, ID: "same", CWD: old, Path: filepath.Join(root, agent, "same.jsonl")}}})
	}
	if _, err := planMigration(groups, old, target, "", []string{"same"}, false, session.ModeMove); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous selection error = %v", err)
	}
	plans, err := planMigration(groups, old, target, "codex", []string{"same"}, false, session.ModeMove)
	if err != nil || len(plans) != 1 || plans[0].Agent != "codex" {
		t.Fatalf("agent filter = %+v, %v", plans, err)
	}
}
