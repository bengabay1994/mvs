package adapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bengabay1994/mvs/internal/fsutil"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

func planOpenCodeSessions(plan session.Plan) (session.Plan, error) {
	dbPath := filepath.Join(paths.OpenCodeRoot(), "opencode.db")
	db, err := openOpenCodeDB(dbPath, "ro")
	if err != nil {
		return session.Plan{}, err
	}
	defer db.Close()
	projectID, _, err := openCodeTargetProject(db, plan.NewCWD)
	if err != nil {
		return session.Plan{}, err
	}
	plan.Actions = append(plan.Actions, session.Action{Kind: "sqlite_exec", Target: dbPath, Detail: "move only selected sessions to the target project/workspace"})
	actions, err := openCodeSessionSnapshots(db, plan, projectID)
	if err != nil {
		return session.Plan{}, err
	}
	plan.Actions = append(plan.Actions, actions...)
	return plan, nil
}

func openOpenCodeDB(path, mode string) (*sql.DB, error) {
	dsn, err := sqliteDSN(path, mode, "busy_timeout(5000)", "foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	return sql.Open("sqlite", dsn)
}

// Prefer the destination's existing registered identity.
// For a new store, use OpenCode's cached Git ID/root commit, or its global ID
// for non-Git directories. These are read-only Git commands.
func openCodeTargetProject(db *sql.DB, cwd string) (string, string, error) {
	rows, err := db.Query(`SELECT id, worktree FROM project`)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var id, worktree string
		if err := rows.Scan(&id, &worktree); err != nil {
			rows.Close()
			return "", "", err
		}
		if worktree == filepath.ToSlash(cwd) {
			rows.Close()
			return id, worktree, nil
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", "", fmt.Errorf("Git is required to resolve an unregistered OpenCode target: %w", err)
	}
	// A new workspace can be a not-yet-created subdirectory of a Git project.
	gitCWD := cwd
	for {
		if info, err := os.Stat(gitCWD); err == nil {
			if !info.IsDir() {
				return "", "", fmt.Errorf("OpenCode target is not a directory: %s", gitCWD)
			}
			break
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
		parent := filepath.Dir(gitCWD)
		if parent == gitCWD {
			return "", "", fmt.Errorf("cannot resolve OpenCode target: %s", cwd)
		}
		gitCWD = parent
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, gitPath, append([]string{"-C", gitCWD}, args...)...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	worktree, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if strings.Contains(worktree, "not a git repository") {
			return "global", "/", nil
		}
		return "", "", fmt.Errorf("resolve OpenCode target Git project: %w: %s", err, worktree)
	}
	commonDir, err := git("rev-parse", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitCWD, commonDir)
	}
	if data, err := os.ReadFile(filepath.Join(commonDir, "opencode")); err == nil && strings.TrimSpace(string(data)) != "" {
		return strings.TrimSpace(string(data)), filepath.ToSlash(worktree), nil
	}
	roots, err := git("rev-list", "--max-parents=0", "--all")
	if err != nil {
		return "", "", err
	}
	ids := strings.Fields(roots)
	if len(ids) == 0 {
		return "global", filepath.ToSlash(worktree), nil
	}
	sort.Strings(ids)
	return ids[0], filepath.ToSlash(worktree), nil
}

func openCodeSessionSnapshots(db *sql.DB, plan session.Plan, targetProject string) ([]session.Action, error) {
	if strings.ContainsAny(targetProject, `/\`) || targetProject == ".." || targetProject == "." || targetProject == "" {
		return nil, errors.New("invalid OpenCode target project ID")
	}
	query := `SELECT DISTINCT project_id FROM session WHERE directory = ? AND id IN (` + inClause(plan.SessionIDs) + `)`
	args := append([]any{filepath.ToSlash(plan.OldCWD)}, toAny(plan.SessionIDs)...)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var actions []session.Action
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return nil, err
		}
		if strings.ContainsAny(project, `/\`) || project == ".." || project == "." || project == "" {
			return nil, errors.New("invalid OpenCode source project ID")
		}
		src := filepath.Join(paths.OpenCodeRoot(), "snapshot", project, sha1Hex(plan.OldCWD))
		if !paths.Exists(src) {
			continue
		}
		dst := filepath.Join(paths.OpenCodeRoot(), "snapshot", targetProject, sha1Hex(plan.NewCWD))
		if paths.Exists(dst) {
			sourceDigest, err := fsutil.Fingerprint(src)
			if err != nil {
				return nil, err
			}
			targetDigest, err := fsutil.Fingerprint(dst)
			if err != nil {
				return nil, err
			}
			if sourceDigest == targetDigest {
				continue
			}
			return nil, fmt.Errorf("OpenCode destination snapshot cannot be merged safely: %s", dst)
		}
		if len(actions) > 0 {
			return nil, fmt.Errorf("OpenCode destination snapshot cannot be merged safely: %s", dst)
		}
		actions = append(actions, session.Action{Kind: "copy_dir", From: src, Target: dst, Detail: "copy snapshot objects; retain the source project's snapshots"})
	}
	return actions, rows.Err()
}

func applyOpenCodeSessions(plan session.Plan, opts session.ApplyOpts) session.Report {
	r := session.Report{Agent: "opencode", OK: true}
	fail := func(err error) session.Report { r.OK = false; r.Errors = append(r.Errors, err.Error()); return r }
	if plan.Mode != session.ModeMove.String() {
		return fail(errors.New("OpenCode copy mode is not supported safely"))
	}
	if opts.DryRun {
		return r
	}
	dbPath := filepath.Join(paths.OpenCodeRoot(), "opencode.db")
	if err := snapshotSQLite(dbPath, filepath.Join(opts.BackupDir, "opencode", "opencode.db")); err != nil {
		return fail(err)
	}
	db, err := openOpenCodeDB(dbPath, "rw")
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	projectID, worktree, err := openCodeTargetProject(db, plan.NewCWD)
	if err != nil {
		return fail(err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	if err := ensureOpenCodeProject(tx, projectID, worktree); err != nil {
		return fail(err)
	}
	columns, err := tableColumns(tx, "session")
	if err != nil {
		return fail(err)
	}
	assignments := []string{"directory = ?", "project_id = ?"}
	args := []any{filepath.ToSlash(plan.NewCWD), projectID}
	for _, column := range columns {
		if column == "workspace_id" {
			var workspaceID any
			if hasTable(tx, "workspace") {
				var id string
				err := tx.QueryRow(`SELECT id FROM workspace WHERE directory = ? LIMIT 1`, filepath.ToSlash(plan.NewCWD)).Scan(&id)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fail(err)
				}
				if err == nil {
					workspaceID = id
				}
			}
			assignments = append(assignments, "workspace_id = ?")
			args = append(args, workspaceID)
		}
		if column == "parent_id" {
			// A selected child becomes resumable as a root when its parent stays.
			assignments = append(assignments, "parent_id = CASE WHEN parent_id IN ("+inClause(plan.SessionIDs)+") THEN parent_id ELSE NULL END")
			args = append(args, toAny(plan.SessionIDs)...)
		}
	}
	args = append(args, filepath.ToSlash(plan.OldCWD))
	args = append(args, toAny(plan.SessionIDs)...)
	result, err := tx.Exec(`UPDATE session SET `+strings.Join(assignments, ", ")+` WHERE directory = ? AND id IN (`+inClause(plan.SessionIDs)+`)`, args...)
	if err != nil {
		return fail(err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fail(err)
	}
	if count != int64(len(plan.SessionIDs)) {
		return fail(fmt.Errorf("matched %d of %d selected OpenCode sessions", count, len(plan.SessionIDs)))
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	r.Applied = append(r.Applied, session.Action{Kind: "sqlite_exec", Target: dbPath})
	for _, a := range plan.Actions {
		if a.Kind != "copy_dir" {
			continue
		}
		if paths.Exists(a.Target) {
			return fail(fmt.Errorf("OpenCode destination snapshot already exists: %s", a.Target))
		}
		// Record partial copies as well, so a later failure cleans them up.
		r.Applied = append(r.Applied, session.Action{Kind: "copy_dir", From: a.From, Target: a.Target, Created: true})
		if err := copyTree(a.From, a.Target); err != nil {
			return fail(err)
		}
	}
	// Close the database before fingerprinting its WAL/SHM family.
	if err := db.Close(); err != nil {
		return fail(err)
	}
	finalizeReportDigests(&r)
	return r
}

func ensureOpenCodeProject(tx *sql.Tx, id, worktree string) error {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM project WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	columns, err := tableColumns(tx, "project")
	if err != nil {
		return err
	}
	known := map[string]any{"id": id, "worktree": worktree, "vcs": "git", "sandboxes": "[]", "time_created": time.Now().UnixMilli(), "time_updated": time.Now().UnixMilli()}
	if id == "global" {
		known["vcs"] = nil
	}
	var names, placeholders []string
	var args []any
	for _, column := range columns {
		if value, ok := known[column]; ok {
			names = append(names, quoteSQLiteIdentifier(column))
			placeholders = append(placeholders, "?")
			args = append(args, value)
		}
	}
	_, err = tx.Exec(`INSERT INTO project (`+strings.Join(names, ",")+`) VALUES (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}
