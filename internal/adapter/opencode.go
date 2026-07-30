package adapter

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
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
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
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
			CWD:   dir,
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
	plan.Actions = append(plan.Actions, session.Action{
		Kind:   "sqlite_exec",
		Target: dbPath,
		Detail: "rewrite session/workspace/project + REPLACE in part.data, message.data",
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

	root := paths.OpenCodeRoot()
	dbPath := filepath.Join(root, "opencode.db")
	bkRoot := filepath.Join(opts.BackupDir, "opencode")

	if !opts.DryRun {
		if err := os.MkdirAll(bkRoot, 0o755); err != nil {
			addErr("create backup root: %v", err)
			return r
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			src := dbPath + suffix
			if paths.Exists(src) {
				if err := copyFile(src, filepath.Join(bkRoot, filepath.Base(src))); err != nil {
					addErr("backup %s: %v", src, err)
					return r
				}
			}
		}
		// Snapshot dirs.
		snapRoot := filepath.Join(root, "snapshot")
		oldHash := sha1Hex(plan.OldCWD)
		_ = filepath.Walk(snapRoot, func(p string, info os.FileInfo, _ error) error {
			if info == nil || !info.IsDir() {
				return nil
			}
			if filepath.Base(p) == oldHash {
				rel, _ := filepath.Rel(snapRoot, p)
				_ = copyTree(p, filepath.Join(bkRoot, "snapshot", rel))
			}
			return nil
		})
	}

	if !opts.DryRun {
		if err := opencodeUpdateDB(dbPath, plan); err != nil {
			addErr("update db: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "sqlite_exec", Target: dbPath})
		}
		if err := opencodeRenameSnapshots(root, plan); err != nil {
			addErr("rename snapshot dirs: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "rename_dir", Target: filepath.Join(root, "snapshot")})
		}
	}
	return r
}

// opencodeUpdateDB applies the per-cwd migration in a single transaction. When
// in copy mode, it duplicates session rows (and their messages/parts) with new
// IDs so both old and new cwd carry the history.
func opencodeUpdateDB(dbPath string, plan session.Plan) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(off)")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	oldCWD := plan.OldCWD
	newCWD := plan.NewCWD
	oldNoSlash := strings.TrimPrefix(oldCWD, "/")
	newNoSlash := strings.TrimPrefix(newCWD, "/")

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
			"path":      newNoSlash,
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
			// Also REPLACE the cwd inside the *new* message rows' data column.
			for oldID, newID := range msgMap {
				_, _ = tx.Exec(`UPDATE message SET data = REPLACE(data, ?, ?) WHERE id = ?`, oldCWD, newCWD, newID)
				_ = oldID
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
				for _, newID := range partMap {
					_, _ = tx.Exec(`UPDATE part SET data = REPLACE(data, ?, ?) WHERE id = ?`, oldCWD, newCWD, newID)
				}
			}
		}
	} else {
		// Move mode: in-place rewrites.
		if _, err := tx.Exec(`UPDATE session SET directory = ?, path = ? WHERE directory = ?`, newCWD, newNoSlash, oldCWD); err != nil {
			return err
		}
		if hasTable(tx, "workspace") {
			if _, err := tx.Exec(`UPDATE workspace SET directory = ? WHERE directory = ?`, newCWD, oldCWD); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE workspace SET directory = REPLACE(directory, ?, ?) WHERE directory LIKE ?`, oldCWD+"/", newCWD+"/", oldCWD+"/%"); err != nil {
				return err
			}
		}
		if hasTable(tx, "project") {
			if _, err := tx.Exec(`UPDATE project SET worktree = ? WHERE worktree = ?`, newCWD, oldCWD); err != nil {
				return err
			}
		}
		if hasTable(tx, "part") {
			if _, err := tx.Exec(`UPDATE part SET data = REPLACE(data, ?, ?) WHERE data LIKE ?`, oldCWD, newCWD, "%"+oldCWD+"%"); err != nil {
				return err
			}
		}
		if hasTable(tx, "message") {
			if _, err := tx.Exec(`UPDATE message SET data = REPLACE(data, ?, ?) WHERE data LIKE ?`, oldCWD, newCWD, "%"+oldCWD+"%"); err != nil {
				return err
			}
		}
		_ = oldNoSlash
	}
	return tx.Commit()
}

func opencodeRenameSnapshots(root string, plan session.Plan) error {
	snapRoot := filepath.Join(root, "snapshot")
	if !paths.Exists(snapRoot) {
		return nil
	}
	oldHash := sha1Hex(plan.OldCWD)
	newHash := sha1Hex(plan.NewCWD)
	projects, err := os.ReadDir(snapRoot)
	if err != nil {
		return nil
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		src := filepath.Join(snapRoot, p.Name(), oldHash)
		dst := filepath.Join(snapRoot, p.Name(), newHash)
		if !paths.Exists(src) {
			continue
		}
		if plan.Mode == "move" {
			if err := os.Rename(src, dst); err != nil {
				if err := copyTree(src, dst); err != nil {
					return err
				}
				_ = os.RemoveAll(src)
			}
		} else {
			if err := copyTree(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- small helpers used by the copy-mode SQL plan ----

func hasTable(tx *sql.Tx, name string) bool {
	var n int
	_ = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name = ?`, name).Scan(&n)
	return n > 0
}

func tableColumns(tx *sql.Tx, name string) ([]string, error) {
	rows, err := tx.Query(`PRAGMA table_info(` + name + `)`)
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
		return nil
	}
	colList := strings.Join(cols, ",")
	placeholders := strings.Repeat("?,", len(cols))
	placeholders = strings.TrimRight(placeholders, ",")
	insertStmt, err := tx.Prepare(fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, colList, placeholders))
	if err != nil {
		return err
	}
	defer insertStmt.Close()

	oldIDs := make([]string, 0, len(idMap))
	for k := range idMap {
		oldIDs = append(oldIDs, k)
	}
	rows, err := tx.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE %s IN (%s)`, colList, table, idCol, inClause(oldIDs)), toAny(oldIDs)...)
	if err != nil {
		return err
	}
	defer rows.Close()
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
	}
	return nil
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
