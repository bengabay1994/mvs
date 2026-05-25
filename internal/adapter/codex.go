package adapter

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Codex implements the Adapter interface for the OpenAI Codex CLI.
//
// Layout:
//
//	~/.codex/sessions/YYYY/MM/DD/rollout-...-<uuid>.jsonl
//	~/.codex/state_5.sqlite             (threads table holds cwd + rollout_path)
//
// Session_meta is the first JSONL line, turn_context lines repeat the cwd.
// state_5.sqlite is the authoritative resume index in modern Codex builds.
type Codex struct{}

func (c *Codex) Name() string { return "codex" }

func (c *Codex) Available() bool {
	return paths.Exists(filepath.Join(paths.CodexRoot(), "sessions"))
}

func (c *Codex) Discover() ([]session.Session, error) {
	root := filepath.Join(paths.CodexRoot(), "sessions")
	if !paths.Exists(root) {
		return nil, nil
	}
	var out []session.Session
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		cwd, id, title := readCodexMeta(p)
		if cwd == "" {
			return nil
		}
		out = append(out, session.Session{
			Agent:    c.Name(),
			ID:       id,
			CWD:      cwd,
			Title:    title,
			Modified: info.ModTime(),
			Size:     info.Size(),
			Path:     p,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readCodexMeta peeks the session_meta line (line 1) for cwd + session id.
func readCodexMeta(path string) (cwd, id, title string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	if !sc.Scan() {
		return
	}
	var line struct {
		Type    string `json:"type"`
		Payload struct {
			CWD string `json:"cwd"`
			ID  string `json:"id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
		return
	}
	if line.Type != "session_meta" {
		return
	}
	cwd = line.Payload.CWD
	id = line.Payload.ID
	// Best-effort title: first user prompt on subsequent lines.
	for sc.Scan() {
		var probe struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &probe) != nil {
			continue
		}
		if probe.Type == "response_item" || probe.Type == "event_msg" {
			s := string(probe.Payload)
			if i := strings.Index(s, `"text":"`); i >= 0 {
				rest := s[i+len(`"text":"`):]
				if j := strings.Index(rest, `"`); j > 0 {
					title = firstLine(rest[:j], 80)
					break
				}
			}
		}
	}
	return
}

func (c *Codex) Plan(sessions []session.Session, opts session.PlanOpts) (session.Plan, error) {
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
		Agent:  c.Name(),
		OldCWD: oldCWD,
		NewCWD: opts.NewCWD,
		Mode:   opts.Mode.String(),
	}
	for _, s := range sessions {
		if opts.Mode == session.ModeCopy {
			plan.Actions = append(plan.Actions, session.Action{
				Kind:   "copy_file",
				From:   s.Path,
				Target: s.Path, // resolved during Apply (new uuid)
				Detail: "copy rollout JSONL with new id; insert threads row",
			})
		} else {
			plan.Actions = append(plan.Actions, session.Action{
				Kind:   "write_file",
				Target: s.Path,
				Detail: "rewrite payload.cwd on session_meta/turn_context lines",
			})
		}
	}
	plan.Actions = append(plan.Actions, session.Action{
		Kind:   "sqlite_exec",
		Target: filepath.Join(paths.CodexRoot(), "state_5.sqlite"),
		Detail: "UPDATE threads SET cwd=? WHERE rollout_path=?",
	})
	return plan, nil
}

func (c *Codex) Apply(plan session.Plan, opts session.ApplyOpts) session.Report {
	r := session.Report{Agent: c.Name(), OK: true}
	addErr := func(format string, args ...any) {
		r.OK = false
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}

	bkRoot := filepath.Join(opts.BackupDir, "codex")
	if !opts.DryRun {
		if err := os.MkdirAll(bkRoot, 0o755); err != nil {
			addErr("create backup root: %v", err)
			return r
		}
	}

	// Collect rollout paths involved.
	var rolloutPaths []string
	for _, a := range plan.Actions {
		if a.Kind == "write_file" || a.Kind == "copy_file" {
			rolloutPaths = append(rolloutPaths, a.From)
			if a.Kind == "write_file" {
				rolloutPaths[len(rolloutPaths)-1] = a.Target
			}
		}
	}

	// Backup affected files + DB.
	dbPath := filepath.Join(paths.CodexRoot(), "state_5.sqlite")
	if !opts.DryRun {
		for _, p := range rolloutPaths {
			rel, _ := filepath.Rel(paths.CodexRoot(), p)
			if err := copyFile(p, filepath.Join(bkRoot, rel)); err != nil {
				addErr("backup %s: %v", p, err)
				return r
			}
		}
		if paths.Exists(dbPath) {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				src := dbPath + suffix
				if paths.Exists(src) {
					if err := copyFile(src, filepath.Join(bkRoot, filepath.Base(src))); err != nil {
						addErr("backup db: %v", err)
						return r
					}
				}
			}
		}
	}

	var newCopies []codexCopy

	for _, a := range plan.Actions {
		switch a.Kind {
		case "write_file":
			if opts.DryRun {
				continue
			}
			if err := rewriteCodexRolloutCWD(a.Target, plan.OldCWD, plan.NewCWD); err != nil {
				addErr("rewrite %s: %v", a.Target, err)
				continue
			}
			r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: a.Target})
		case "copy_file":
			if opts.DryRun {
				continue
			}
			newID := uuid.NewString()
			oldBase := filepath.Base(a.From)
			// Replace the trailing -<uuid>.jsonl with the new id.
			newBase := codexRolloutBaseWithNewID(oldBase, newID)
			newPath := filepath.Join(filepath.Dir(a.From), newBase)
			if err := copyFile(a.From, newPath); err != nil {
				addErr("copy rollout: %v", err)
				continue
			}
			if err := rewriteCodexRolloutCWD(newPath, plan.OldCWD, plan.NewCWD); err != nil {
				addErr("rewrite copied rollout: %v", err)
				continue
			}
			if err := rewriteCodexRolloutID(newPath, newID); err != nil {
				addErr("rewrite rollout id: %v", err)
				continue
			}
			r.Applied = append(r.Applied, session.Action{Kind: "copy_file", From: a.From, Target: newPath})
			newCopies = append(newCopies, codexCopy{oldPath: a.From, newPath: newPath, newID: newID})
		}
	}

	// SQLite updates.
	if paths.Exists(dbPath) && !opts.DryRun {
		if err := codexUpdateDB(dbPath, plan, rolloutPaths, newCopies); err != nil {
			addErr("update sqlite: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "sqlite_exec", Target: dbPath})
		}
	}

	return r
}

func codexRolloutBaseWithNewID(oldBase, newID string) string {
	// rollout-<ts>-<uuid>.jsonl
	const suffix = ".jsonl"
	if !strings.HasSuffix(oldBase, suffix) {
		return oldBase + "." + newID + suffix
	}
	stem := strings.TrimSuffix(oldBase, suffix)
	if i := strings.LastIndex(stem, "-"); i >= 0 {
		return stem[:i+1] + newID + suffix
	}
	return stem + "-" + newID + suffix
}

// rewriteCodexRolloutCWD streams the rollout JSONL and rewrites payload.cwd on
// session_meta + turn_context lines where the current cwd equals oldCWD.
func rewriteCodexRolloutCWD(path, oldCWD, newCWD string) error {
	return streamRewriteJSONL(path, func(line []byte) []byte {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return line
		}
		typeRaw, ok := m["type"]
		if !ok {
			return line
		}
		var t string
		if err := json.Unmarshal(typeRaw, &t); err != nil {
			return line
		}
		if t != "session_meta" && t != "turn_context" {
			return line
		}
		payloadRaw, ok := m["payload"]
		if !ok {
			return line
		}
		var pm map[string]json.RawMessage
		if err := json.Unmarshal(payloadRaw, &pm); err != nil {
			return line
		}
		cwdRaw, ok := pm["cwd"]
		if !ok {
			return line
		}
		var cur string
		if err := json.Unmarshal(cwdRaw, &cur); err != nil {
			return line
		}
		if cur != oldCWD {
			return line
		}
		nb, _ := json.Marshal(newCWD)
		pm["cwd"] = nb
		newPayload, err := json.Marshal(pm)
		if err != nil {
			return line
		}
		m["payload"] = newPayload
		out, err := json.Marshal(m)
		if err != nil {
			return line
		}
		return out
	})
}

func rewriteCodexRolloutID(path, newID string) error {
	return streamRewriteJSONL(path, func(line []byte) []byte {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return line
		}
		t := ""
		if raw, ok := m["type"]; ok {
			_ = json.Unmarshal(raw, &t)
		}
		if t != "session_meta" {
			return line
		}
		payloadRaw, ok := m["payload"]
		if !ok {
			return line
		}
		var pm map[string]json.RawMessage
		if err := json.Unmarshal(payloadRaw, &pm); err != nil {
			return line
		}
		nb, _ := json.Marshal(newID)
		pm["id"] = nb
		newPayload, _ := json.Marshal(pm)
		m["payload"] = newPayload
		out, _ := json.Marshal(m)
		return out
	})
}

// streamRewriteJSONL applies a per-line transformer to a JSONL file atomically.
func streamRewriteJSONL(path string, transform func(line []byte) []byte) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mvs-*.jsonl")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	br := bufio.NewReaderSize(in, 1<<20)
	w := bufio.NewWriter(tmp)
	for {
		line, err := br.ReadBytes('\n')
		hasNewline := len(line) > 0 && line[len(line)-1] == '\n'
		raw := line
		if hasNewline {
			raw = line[:len(line)-1]
		}
		if len(raw) > 0 {
			out := transform(raw)
			if _, werr := w.Write(out); werr != nil {
				return werr
			}
		}
		if hasNewline {
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmpName, st.Mode().Perm())
	}
	cleanup = false
	return os.Rename(tmpName, path)
}

type codexCopy struct {
	oldPath, newPath, newID string
}

// codexUpdateDB applies threads.cwd / rollout_path edits in a single tx.
func codexUpdateDB(dbPath string, plan session.Plan, oldPaths []string, copies []codexCopy) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if plan.Mode == "move" {
		stmt, err := tx.Prepare("UPDATE threads SET cwd = ? WHERE rollout_path = ?")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range oldPaths {
			if _, err := stmt.Exec(plan.NewCWD, p); err != nil {
				return err
			}
		}
	} else {
		// Copy mode: INSERT new threads rows mirroring the old, with new id, cwd, path.
		// We replicate the row exactly except for those three fields.
		for _, c := range copies {
			row := tx.QueryRow("SELECT id, rollout_path, created_at, updated_at, source, model_provider, cwd, title, sandbox_policy, approval_mode FROM threads WHERE rollout_path = ?", c.oldPath)
			var (
				oldID, oldRP, src, mp, cwd, title, sp, am string
				createdAt, updatedAt                      int64
			)
			if err := row.Scan(&oldID, &oldRP, &createdAt, &updatedAt, &src, &mp, &cwd, &title, &sp, &am); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			_ = oldID
			_, err := tx.Exec(`INSERT INTO threads (id, rollout_path, created_at, updated_at, source, model_provider, cwd, title, sandbox_policy, approval_mode)
                               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.newID, c.newPath, createdAt, time.Now().UnixMilli(), src, mp, plan.NewCWD, title, sp, am)
			if err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
