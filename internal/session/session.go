// Package session holds the cross-agent session model and shared types.
package session

import (
	"time"
)

// Session is a single agent conversation transcript scoped to a working directory.
type Session struct {
	Agent    string    // "claude" | "codex" | "gemini" | "opencode"
	ID       string    // agent-native id (uuid, filename, etc.)
	CWD      string    // the canonical cwd this session was bound to
	Title    string    // best-effort human label
	Modified time.Time // last activity time
	Size     int64     // primary file size in bytes
	Path     string    // primary on-disk path (file or dir)
}

// Mode selects move-vs-copy semantics during migration.
type Mode int

const (
	ModeMove Mode = iota
	ModeCopy
)

func (m Mode) String() string {
	if m == ModeCopy {
		return "copy"
	}
	return "move"
}

// PlanOpts controls migration planning.
type PlanOpts struct {
	NewCWD string
	Mode   Mode
}

// ApplyOpts controls migration execution.
type ApplyOpts struct {
	DryRun    bool
	BackupDir string // run-specific backup root (created by caller)
}

// Action is a single concrete on-disk change. The string forms are stable so
// they can be displayed and recorded in undo manifests.
type Action struct {
	Kind    string `json:"kind"`           // "rename_dir" | "copy_dir" | "write_file" | "sqlite_exec"
	Target  string `json:"target"`         // primary path or db URI
	From    string `json:"from,omitempty"` // for renames/copies
	Detail  string `json:"detail,omitempty"`
	SQL     string `json:"sql,omitempty"`
	SQLArgs []any  `json:"sql_args,omitempty"`
}

// Plan is the ordered list of actions an adapter intends to run.
type Plan struct {
	Agent   string   `json:"agent"`
	OldCWD  string   `json:"old_cwd"`
	NewCWD  string   `json:"new_cwd"`
	Mode    string   `json:"mode"`
	Actions []Action `json:"actions"`
}

// Report is the result of executing a Plan.
type Report struct {
	Agent   string   `json:"agent"`
	OK      bool     `json:"ok"`
	Applied []Action `json:"applied"`
	Errors  []string `json:"errors,omitempty"`
}
