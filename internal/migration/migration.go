// Package migration validates and executes cross-agent migration plans.
package migration

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bengabay1994/mvs/internal/adapter"
	"github.com/bengabay1994/mvs/internal/backup"
	"github.com/bengabay1994/mvs/internal/session"
)

// Execute applies plans as one recoverable run. A failed adapter causes every
// plan in the run to be restored from its pre-migration snapshot.
func Execute(plans []session.Plan) (string, []session.Report, error) {
	if err := Validate(plans); err != nil {
		return "", nil, err
	}

	runID := backup.NewRunID()
	bkDir, err := backup.Prepare(runID, plans)
	if err != nil {
		return runID, nil, fmt.Errorf("prepare backup: %w", err)
	}

	reports := make([]session.Report, 0, len(plans))
	for _, plan := range plans {
		a, err := adapter.ByName(plan.Agent)
		if err != nil {
			reports = append(reports, session.Report{
				Agent:  plan.Agent,
				OK:     false,
				Errors: []string{err.Error()},
			})
		} else {
			reports = append(reports, a.Apply(plan, session.ApplyOpts{BackupDir: bkDir}))
		}

		if err := backup.Finalize(runID, reports); err != nil {
			return runID, reports, rollback(runID, reports, fmt.Errorf("record migration result: %w", err))
		}
		if !reports[len(reports)-1].OK {
			return runID, reports, rollback(runID, reports, errors.New("migration failed"))
		}
	}

	return runID, reports, nil
}

// Validate rejects plans whose failure semantics cannot be made unambiguous.
func Validate(plans []session.Plan) error {
	if len(plans) == 0 {
		return errors.New("no migration plans")
	}

	seenAgents := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		if plan.OldCWD == "" || plan.NewCWD == "" {
			return fmt.Errorf("%s plan has an empty source or target cwd", plan.Agent)
		}
		if !filepath.IsAbs(plan.OldCWD) || !filepath.IsAbs(plan.NewCWD) {
			return fmt.Errorf("%s plan requires absolute source and target paths", plan.Agent)
		}
		oldPath := filepath.Clean(plan.OldCWD)
		newPath := filepath.Clean(plan.NewCWD)
		samePath := oldPath == newPath
		if runtime.GOOS == "windows" {
			samePath = strings.EqualFold(oldPath, newPath)
		}
		if samePath {
			return fmt.Errorf("%s source and target resolve to the same path", plan.Agent)
		}
		if plan.Mode != session.ModeMove.String() && plan.Mode != session.ModeCopy.String() {
			return fmt.Errorf("%s plan has invalid mode %q", plan.Agent, plan.Mode)
		}
		if plan.Scope != "" && plan.Scope != session.ScopeSession.String() && plan.Scope != session.ScopeProject.String() {
			return fmt.Errorf("%s plan has invalid selection scope %q", plan.Agent, plan.Scope)
		}
		if plan.Scope == session.ScopeSession.String() && len(plan.SessionIDs) == 0 {
			return fmt.Errorf("%s session plan has no selected session IDs", plan.Agent)
		}
		if plan.Scope == session.ScopeSession.String() {
			ids := make(map[string]bool, len(plan.SessionIDs))
			for _, id := range plan.SessionIDs {
				if id == "" || ids[id] {
					return fmt.Errorf("%s session plan has an empty or duplicate session ID", plan.Agent)
				}
				ids[id] = true
			}
		}
		if _, exists := seenAgents[plan.Agent]; exists {
			return fmt.Errorf("select only one source path per agent in each run; %s appears more than once", plan.Agent)
		}
		seenAgents[plan.Agent] = struct{}{}
	}
	return nil
}

func rollback(runID string, reports []session.Report, cause error) error {
	if err := backup.Rollback(runID, reports); err != nil {
		return fmt.Errorf("%w; automatic rollback also failed: %v", cause, err)
	}
	return fmt.Errorf("%w; all changes were rolled back", cause)
}
