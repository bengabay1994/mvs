// Package adapter is the per-agent migration interface and registry.
package adapter

import (
	"fmt"

	"github.com/bengabay1994/mvs/internal/session"
)

// Adapter is the contract every agent harness implements.
type Adapter interface {
	// Name returns the short id ("claude", "codex", "gemini", "opencode").
	Name() string
	// Available reports whether the adapter's data dir is present on this host.
	Available() bool
	// Discover returns every session the adapter can see on disk.
	Discover() ([]session.Session, error)
	// Plan computes the migration actions for the given sessions and target cwd.
	// All sessions passed in are guaranteed to belong to this adapter and share
	// the same OldCWD value.
	Plan(sessions []session.Session, opts session.PlanOpts) (session.Plan, error)
	// Apply executes the plan. Backups are written under opts.BackupDir.
	Apply(plan session.Plan, opts session.ApplyOpts) session.Report
}

// All returns the adapters compiled into this binary.
func All() []Adapter {
	return []Adapter{
		&Claude{},
		&Codex{},
		&Gemini{},
		&OpenCode{},
	}
}

// ByName looks up an adapter by short name.
func ByName(name string) (Adapter, error) {
	for _, a := range All() {
		if a.Name() == name {
			return a, nil
		}
	}
	return nil, fmt.Errorf("unknown adapter %q", name)
}
