package adapter

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// OpenCode implements the Adapter interface for sst/opencode.
//
// Layout:
//
//	<XDG_DATA>/opencode/opencode.db                 (primary)
//	<XDG_DATA>/opencode/snapshot/<projectID>/<sha1(cwd)>/  (git snapshots)
//
// project.worktree holds the canonical cwd; session.directory mirrors it. The
// project_id is git-derived and survives renames as long as .git/opencode stays
// intact, so migration boils down to: rewrite the directory/path columns,
// REPLACE() across part.data + message.data JSON blobs, and rename the
// per-cwd snapshot subdir.
type OpenCode struct{}

func (o *OpenCode) Name() string { return "opencode" }

func (o *OpenCode) Available() bool {
	return paths.Exists(filepath.Join(paths.OpenCodeRoot(), "opencode.db"))
}

func (o *OpenCode) Discover() ([]session.Session, error) {
	dbPath := filepath.Join(paths.OpenCodeRoot(), "opencode.db")
	if !paths.Exists(dbPath) {
		return nil, nil
	}
	dsn, err := sqliteDSN(dbPath, "ro", "busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT id, COALESCE(directory, ''), COALESCE(title, ''), COALESCE(time_updated, 0)
	                       FROM session
	                       WHERE directory IS NOT NULL AND directory != ''
	                       ORDER BY time_updated DESC`)
	if err != nil {
		// Fall back to a minimal column set if schema is older.
		rows, err = db.Query(`SELECT id, COALESCE(directory, ''), COALESCE(title, ''), 0 FROM session`)
		if err != nil {
			return nil, err
		}
	}
	defer rows.Close()
	var out []session.Session
	for rows.Next() {
		var id, dir, title string
		var ts int64
		if err := rows.Scan(&id, &dir, &title, &ts); err != nil {
			continue
		}
		if dir == "" {
			continue
		}
		out = append(out, session.Session{
			Agent: o.Name(),
			ID:    id,
			CWD:   filepath.FromSlash(dir),
			Title: title,
			Path:  dbPath + "#" + id,
		})
	}
	return out, nil
}

func (o *OpenCode) Plan(sessions []session.Session, opts session.PlanOpts) (session.Plan, error) {
	if len(sessions) == 0 {
		return session.Plan{}, errors.New("no sessions")
	}
	if opts.Mode == session.ModeCopy {
		return session.Plan{}, errors.New("OpenCode copy mode is disabled because current schemas contain additional session-owned tables that cannot be cloned safely")
	}
	oldCWD := sessions[0].CWD
	for _, s := range sessions {
		if s.CWD != oldCWD {
			return session.Plan{}, fmt.Errorf("mixed source cwds: %q vs %q", oldCWD, s.CWD)
		}
	}
	plan := session.Plan{
		Agent:  o.Name(),
		OldCWD: oldCWD,
		NewCWD: opts.NewCWD,
		Mode:   opts.Mode.String(),
	}
	dbPath := filepath.Join(paths.OpenCodeRoot(), "opencode.db")
	if err := validateOpenCodeSnapshotTargets(opts.NewCWD, oldCWD, opts.Mode); err != nil {
		return session.Plan{}, err
	}
	plan.Actions = append(plan.Actions, session.Action{
		Kind:   "sqlite_exec",
		Target: dbPath,
		Detail: "rewrite session.directory, workspace.directory, and project.worktree",
	})
	plan.Actions = append(plan.Actions, session.Action{
		Kind:   "rename_dir",
		Target: filepath.Join(paths.OpenCodeRoot(), "snapshot"),
		Detail: fmt.Sprintf("snapshot/<projectID>/%s → /%s", sha1Hex(oldCWD), sha1Hex(opts.NewCWD)),
	})
	return plan, nil
}

func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func (o *OpenCode) Apply(plan session.Plan, opts session.ApplyOpts) session.Report {
	r := session.Report{Agent: o.Name(), OK: true}
	addErr := func(format string, args ...any) {
		r.OK = false
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
	if plan.Mode == session.ModeCopy.String() {
		addErr("OpenCode copy mode is not supported safely")
		return r
	}

	root := paths.OpenCodeRoot()
	dbPath := filepath.Join(root, "opencode.db")
	bkRoot := filepath.Join(opts.BackupDir, "opencode")

	if !opts.DryRun {
		if err := os.MkdirAll(bkRoot, 0o700); err != nil {
			addErr("create backup root: %v", err)
			return r
		}
		if err := snapshotSQLite(dbPath, filepath.Join(bkRoot, filepath.Base(dbPath))); err != nil {
			addErr("backup %s: %v", dbPath, err)
			return r
		}
		// Snapshot dirs.
		snapRoot := filepath.Join(root, "snapshot")
		oldHash := sha1Hex(plan.OldCWD)
		if err := filepath.Walk(snapRoot, func(p string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info == nil || !info.IsDir() {
				return nil
			}
			if filepath.Base(p) == oldHash {
				rel, _ := filepath.Rel(snapRoot, p)
				if err := copyTree(p, filepath.Join(bkRoot, "snapshot", rel)); err != nil {
					return err
				}
				return filepath.SkipDir
			}
			return nil
		}); err != nil && !errors.Is(err, os.ErrNotExist) {
			addErr("backup snapshots: %v", err)
			return r
		}
	}

	if !opts.DryRun {
		if err := opencodeUpdateDB(dbPath, plan); err != nil {
			addErr("update db: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "sqlite_exec", Target: dbPath})
		}
		actions, err := opencodeRenameSnapshots(root, plan)
		r.Applied = append(r.Applied, actions...)
		if err != nil {
			addErr("rename snapshot dirs: %v", err)
		}
	}
	finalizeReportDigests(&r)
	return r
}

// opencodeUpdateDB applies the per-cwd migration in a single transaction. When
// in copy mode, it duplicates session rows (and their messages/parts) with new
// IDs so both old and new cwd carry the history.
func opencodeUpdateDB(dbPath string, plan session.Plan) error {
	dsn, err := sqliteDSN(dbPath, "", "busy_timeout(5000)", "foreign_keys(off)")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	oldCWD := filepath.ToSlash(plan.OldCWD)
	newCWD := filepath.ToSlash(plan.NewCWD)

	if plan.Mode == "copy" {
		// Clone the cwd: gather affected session ids, generate new ids, replicate rows.
		sessionRows, err := tx.Query(`SELECT id FROM session WHERE directory = ?`, oldCWD)
		if err != nil {
			return err
		}
		var oldIDs []string
		for sessionRows.Next() {
			var id string
			if err := sessionRows.Scan(&id); err != nil {
				sessionRows.Close()
				return err
			}
			oldIDs = append(oldIDs, id)
		}
		sessionRows.Close()

		sessionIDMap := map[string]string{}
		for _, id := range oldIDs {
			sessionIDMap[id] = "ses_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		if err := cloneRowsRemap(tx, "session", "id", sessionIDMap, map[string]any{
			"directory": newCWD,
		}, nil); err != nil {
			return err
		}
		// Workspaces (if any) — duplicate with new ids, remap session links if a column exists.
		_ = cloneRowsRemap(tx, "workspace", "id", autoIDMap(tx, "workspace", "directory", oldCWD, "ws_"),
			map[string]any{"directory": newCWD}, nil)

		// Messages and parts may not be present on every host, query first.
		if hasTable(tx, "message") {
			msgRows, _ := tx.Query(`SELECT id FROM message WHERE session_id IN (`+inClause(oldIDs)+`)`, toAny(oldIDs)...)
			var oldMsgs []string
			for msgRows != nil && msgRows.Next() {
				var id string
				_ = msgRows.Scan(&id)
				oldMsgs = append(oldMsgs, id)
			}
			if msgRows != nil {
				msgRows.Close()
			}
			msgMap := map[string]string{}
			for _, id := range oldMsgs {
				msgMap[id] = "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			}
			if err := cloneRowsRemap(tx, "message", "id", msgMap, nil, map[string]map[string]string{"session_id": sessionIDMap}); err != nil {
				return err
			}
			if hasTable(tx, "part") {
				partRows, _ := tx.Query(`SELECT id FROM part WHERE message_id IN (`+inClause(oldMsgs)+`)`, toAny(oldMsgs)...)
				var oldParts []string
				for partRows != nil && partRows.Next() {
					var id string
					_ = partRows.Scan(&id)
					oldParts = append(oldParts, id)
				}
				if partRows != nil {
					partRows.Close()
				}
				partMap := map[string]string{}
				for _, id := range oldParts {
					partMap[id] = "prt_" + strings.ReplaceAll(uuid.NewString(), "-", "")
				}
				if err := cloneRowsRemap(tx, "part", "id", partMap, nil, map[string]map[string]string{"message_id": msgMap, "session_id": sessionIDMap}); err != nil {
					return err
				}
			}
		}
	} else {
		// Move mode: in-place rewrites.
		result, err := tx.Exec(`UPDATE session SET directory = ? WHERE directory = ?`, newCWD, oldCWD)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated == 0 {
			return fmt.Errorf("no OpenCode session rows matched directory %q", oldCWD)
		}
		if hasTable(tx, "workspace") {
			if _, err := tx.Exec(`UPDATE workspace SET directory = ? WHERE directory = ?`, newCWD, oldCWD); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE workspace SET directory = REPLACE(directory, ?, ?) WHERE directory LIKE ?`, oldCWD+"/", newCWD+"/", oldCWD+"/%"); err != nil {
				return err
			}
		}
		if err := updateOpenCodeProjectPaths(tx, oldCWD, newCWD); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func updateOpenCodeProjectPaths(tx *sql.Tx, oldCWD, newCWD string) error {
	if !hasTable(tx, "project") {
		return nil
	}
	columns, err := tableColumns(tx, "project")
	if err != nil {
		return err
	}
	hasSandboxes := false
	for _, column := range columns {
		if column == "sandboxes" {
			hasSandboxes = true
			break
		}
	}
	if !hasSandboxes {
		_, err := tx.Exec(`UPDATE project SET worktree = ? WHERE worktree = ?`, newCWD, oldCWD)
		return err
	}

	type projectPaths struct {
		id        string
		worktree  string
		sandboxes string
	}
	rows, err := tx.Query(`SELECT id, worktree, sandboxes FROM project`)
	if err != nil {
		return err
	}
	var projects []projectPaths
	for rows.Next() {
		var project projectPaths
		if err := rows.Scan(&project.id, &project.worktree, &project.sandboxes); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, project := range projects {
		var sandboxes []string
		if err := json.Unmarshal([]byte(project.sandboxes), &sandboxes); err != nil {
			return fmt.Errorf("parse project %s sandboxes: %w", project.id, err)
		}
		changed := false
		if project.worktree == oldCWD {
			project.worktree = newCWD
			changed = true
		}
		seen := make(map[string]struct{}, len(sandboxes))
		rewritten := make([]string, 0, len(sandboxes))
		for _, sandbox := range sandboxes {
			updated := replacePathPrefix(sandbox, oldCWD, newCWD)
			if updated != sandbox {
				changed = true
			}
			if _, exists := seen[updated]; exists {
				changed = true
				continue
			}
			seen[updated] = struct{}{}
			rewritten = append(rewritten, updated)
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(rewritten)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE project SET worktree = ?, sandboxes = ? WHERE id = ?`, project.worktree, string(encoded), project.id); err != nil {
			return err
		}
	}
	return nil
}

func replacePathPrefix(path, oldCWD, newCWD string) string {
	if path == oldCWD {
		return newCWD
	}
	if strings.HasPrefix(path, oldCWD+"/") {
		return newCWD + strings.TrimPrefix(path, oldCWD)
	}
	return path
}

func validateOpenCodeSnapshotTargets(newCWD, oldCWD string, mode session.Mode) error {
	if mode == session.ModeCopy && sha1Hex(oldCWD) == sha1Hex(newCWD) {
		return errors.New("OpenCode source and target use the same snapshot key")
	}
	snapRoot := filepath.Join(paths.OpenCodeRoot(), "snapshot")
	projects, err := os.ReadDir(snapRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	oldHash := sha1Hex(oldCWD)
	newHash := sha1Hex(newCWD)
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		src := filepath.Join(snapRoot, project.Name(), oldHash)
		dst := filepath.Join(snapRoot, project.Name(), newHash)
		if src != dst && paths.Exists(src) && paths.Exists(dst) {
			return fmt.Errorf("OpenCode destination snapshot already exists: %s", dst)
		}
	}
	return nil
}

func opencodeRenameSnapshots(root string, plan session.Plan) ([]session.Action, error) {
	snapRoot := filepath.Join(root, "snapshot")
	if !paths.Exists(snapRoot) {
		return nil, nil
	}
	oldHash := sha1Hex(plan.OldCWD)
	newHash := sha1Hex(plan.NewCWD)
	projects, err := os.ReadDir(snapRoot)
	if err != nil {
		return nil, err
	}
	var actions []session.Action
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		src := filepath.Join(snapRoot, p.Name(), oldHash)
		dst := filepath.Join(snapRoot, p.Name(), newHash)
		if !paths.Exists(src) {
			continue
		}
		if src == dst {
			continue
		}
		if paths.Exists(dst) {
			return actions, fmt.Errorf("destination snapshot already exists: %s", dst)
		}
		if plan.Mode == "move" {
			if err := os.Rename(src, dst); err != nil {
				if err := copyTree(src, dst); err != nil {
					return actions, err
				}
				if err := os.RemoveAll(src); err != nil {
					_ = os.RemoveAll(dst)
					return actions, err
				}
			}
		} else {
			if err := copyTree(src, dst); err != nil {
				_ = os.RemoveAll(dst)
				return actions, err
			}
		}
		actions = append(actions, session.Action{
			Kind: plan.Mode + "_dir", From: src, Target: dst, Created: true,
		})
	}
	return actions, nil
}

// ---- small helpers used by the copy-mode SQL plan ----

func hasTable(tx *sql.Tx, name string) bool {
	var n int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&n)
	return n > 0
}

func tableColumns(tx *sql.Tx, name string) ([]string, error) {
	rows, err := tx.Query(`PRAGMA table_info(` + quoteSQLiteIdentifier(name) + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var (
			cid     int
			cname   string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, cname)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

// cloneRowsRemap copies rows where idCol matches keys of idMap into the same
// table with idCol set to the mapped new id. overrides set columns to fixed
// values; fkRemaps remap columns via a per-column id-map.
func cloneRowsRemap(tx *sql.Tx, table, idCol string, idMap map[string]string,
	overrides map[string]any, fkRemaps map[string]map[string]string) error {
	if len(idMap) == 0 {
		return nil
	}
	cols, err := tableColumns(tx, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("table %s has no writable columns", table)
	}
	quotedCols := make([]string, len(cols))
	for i, column := range cols {
		quotedCols[i] = quoteSQLiteIdentifier(column)
	}
	colList := strings.Join(quotedCols, ",")
	placeholders := strings.Repeat("?,", len(cols))
	placeholders = strings.TrimRight(placeholders, ",")
	insertStmt, err := tx.Prepare(fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, quoteSQLiteIdentifier(table), colList, placeholders))
	if err != nil {
		return err
	}
	defer insertStmt.Close()

	oldIDs := make([]string, 0, len(idMap))
	for k := range idMap {
		oldIDs = append(oldIDs, k)
	}
	rows, err := tx.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE %s IN (%s)`, colList, quoteSQLiteIdentifier(table), quoteSQLiteIdentifier(idCol), inClause(oldIDs)), toAny(oldIDs)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	inserted := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, c := range cols {
			if c == idCol {
				if old, ok := vals[i].(string); ok {
					if nv, ok2 := idMap[old]; ok2 {
						vals[i] = nv
					}
				}
				continue
			}
			if v, ok := overrides[c]; ok {
				vals[i] = v
				continue
			}
			if remap, ok := fkRemaps[c]; ok {
				if old, ok2 := vals[i].(string); ok2 {
					if nv, ok3 := remap[old]; ok3 {
						vals[i] = nv
					}
				}
			}
		}
		if _, err := insertStmt.Exec(vals...); err != nil {
			return err
		}
		inserted++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if inserted != len(idMap) {
		return fmt.Errorf("cloned %d %s row(s), want %d", inserted, table, len(idMap))
	}
	return nil
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func autoIDMap(tx *sql.Tx, table, col string, val string, prefix string) map[string]string {
	if !hasTable(tx, table) {
		return nil
	}
	rows, err := tx.Query(fmt.Sprintf(`SELECT id FROM %s WHERE %s = ?`, table, col), val)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		out[id] = prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	return out
}

func inClause(items []string) string {
	if len(items) == 0 {
		return "''"
	}
	return strings.TrimRight(strings.Repeat("?,", len(items)), ",")
}

func toAny(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}
