package adapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

// Claude implements the Adapter interface for Anthropic's Claude Code CLI.
//
// Layout (verified against claude-code v2.1.144):
//
//	~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl
//	~/.claude/history.jsonl           (lines have project=<cwd>)
//	~/.claude.json                    (root .projects object keyed by <cwd>)
//
// Encoding: every non-alphanumeric byte → '-'. Lossy; canonical cwd is read
// from the JSONL's first line.
type Claude struct{}

func (c *Claude) Name() string { return "claude" }

func (c *Claude) Available() bool {
	return paths.Exists(filepath.Join(paths.ClaudeRoot(), "projects"))
}

// encodeClaude applies the claude-code dir-name encoding: any byte that is not
// [A-Za-z0-9] becomes '-'. No dash-collapsing. Lossy.
func encodeClaude(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		ch := p[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			b.WriteByte(ch)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (c *Claude) Discover() ([]session.Session, error) {
	root := filepath.Join(paths.ClaudeRoot(), "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []session.Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(dir, f.Name())
			info, err := f.Info()
			if err != nil {
				continue
			}
			cwd, title := readClaudeMeta(p)
			if cwd == "" {
				continue // skip transcripts without a recoverable cwd
			}
			out = append(out, session.Session{
				Agent:    c.Name(),
				ID:       strings.TrimSuffix(f.Name(), ".jsonl"),
				CWD:      cwd,
				Title:    title,
				Modified: info.ModTime(),
				Size:     info.Size(),
				Path:     p,
			})
		}
	}
	return out, nil
}

// readClaudeMeta scans the first ~32 lines of a JSONL transcript to pluck the
// session's cwd and (best-effort) a human title.
func readClaudeMeta(p string) (cwd, title string) {
	f, err := os.Open(p)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	lines := 0
	for sc.Scan() && lines < 64 {
		lines++
		var m map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if cwd == "" {
			if raw, ok := m["cwd"]; ok {
				_ = json.Unmarshal(raw, &cwd)
			}
		}
		if title == "" {
			if raw, ok := m["aiTitle"]; ok {
				_ = json.Unmarshal(raw, &title)
			}
		}
		if title == "" {
			if raw, ok := m["lastPrompt"]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					title = firstLine(s, 80)
				}
			}
		}
		if cwd != "" && title != "" {
			break
		}
	}
	return cwd, title
}

func firstLine(s string, max int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func (c *Claude) Plan(sessions []session.Session, opts session.PlanOpts) (session.Plan, error) {
	if len(sessions) == 0 {
		return session.Plan{}, errors.New("no sessions")
	}
	oldCWD := sessions[0].CWD
	for _, s := range sessions {
		if s.CWD != oldCWD {
			return session.Plan{}, fmt.Errorf("mixed source cwds in one plan: %q vs %q", oldCWD, s.CWD)
		}
	}
	newCWD := opts.NewCWD
	encOld := encodeClaude(oldCWD)
	encNew := encodeClaude(newCWD)
	root := paths.ClaudeRoot()
	srcDir := filepath.Join(root, "projects", encOld)
	dstDir := filepath.Join(root, "projects", encNew)

	plan := session.Plan{
		Agent:  c.Name(),
		OldCWD: oldCWD,
		NewCWD: newCWD,
		Mode:   opts.Mode.String(),
	}
	if opts.Mode == session.ModeMove {
		plan.Actions = append(plan.Actions, session.Action{
			Kind:   "rename_dir",
			From:   srcDir,
			Target: dstDir,
			Detail: fmt.Sprintf("rename projects/%s → projects/%s", encOld, encNew),
		})
	} else {
		plan.Actions = append(plan.Actions, session.Action{
			Kind:   "copy_dir",
			From:   srcDir,
			Target: dstDir,
			Detail: fmt.Sprintf("copy projects/%s → projects/%s", encOld, encNew),
		})
	}
	for _, s := range sessions {
		plan.Actions = append(plan.Actions, session.Action{
			Kind:   "write_file",
			Target: filepath.Join(dstDir, s.ID+".jsonl"),
			Detail: "rewrite top-level cwd in every line",
		})
	}
	plan.Actions = append(plan.Actions,
		session.Action{
			Kind:   "write_file",
			Target: filepath.Join(root, "history.jsonl"),
			Detail: "rewrite project field on matching lines",
		},
		session.Action{
			Kind:   "write_file",
			Target: paths.ClaudeJSON(),
			Detail: "rename .projects[" + oldCWD + "] key",
		},
	)
	return plan, nil
}

func (c *Claude) Apply(plan session.Plan, opts session.ApplyOpts) session.Report {
	r := session.Report{Agent: c.Name(), OK: true}
	addErr := func(format string, args ...any) {
		r.OK = false
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}

	root := paths.ClaudeRoot()
	encOld := encodeClaude(plan.OldCWD)
	encNew := encodeClaude(plan.NewCWD)
	srcDir := filepath.Join(root, "projects", encOld)
	dstDir := filepath.Join(root, "projects", encNew)
	historyFile := filepath.Join(root, "history.jsonl")
	claudeJSON := paths.ClaudeJSON()

	bkRoot := filepath.Join(opts.BackupDir, "claude")
	if !opts.DryRun {
		if err := os.MkdirAll(bkRoot, 0o755); err != nil {
			addErr("create backup root: %v", err)
			return r
		}
		// Snapshot the source dir, history.jsonl, and .claude.json.
		if paths.Exists(srcDir) {
			if err := copyTree(srcDir, filepath.Join(bkRoot, "projects-"+encOld)); err != nil {
				addErr("backup project dir: %v", err)
				return r
			}
		}
		if paths.Exists(historyFile) {
			if err := copyFile(historyFile, filepath.Join(bkRoot, "history.jsonl")); err != nil {
				addErr("backup history.jsonl: %v", err)
				return r
			}
		}
		if paths.Exists(claudeJSON) {
			if err := copyFile(claudeJSON, filepath.Join(bkRoot, "claude.json")); err != nil {
				addErr("backup .claude.json: %v", err)
				return r
			}
		}
	}

	// 1. Place files at the destination.
	if !paths.Exists(srcDir) {
		addErr("source dir missing: %s", srcDir)
		return r
	}
	if !opts.DryRun {
		if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
			addErr("mkdir parent: %v", err)
			return r
		}
		if plan.Mode == "move" {
			if err := os.Rename(srcDir, dstDir); err != nil {
				if err := copyTree(srcDir, dstDir); err != nil {
					addErr("place files (copy fallback): %v", err)
					return r
				}
				if err := os.RemoveAll(srcDir); err != nil {
					addErr("remove src after copy fallback: %v", err)
				}
			}
		} else {
			if err := copyTree(srcDir, dstDir); err != nil {
				addErr("copy dir: %v", err)
				return r
			}
		}
	}
	r.Applied = append(r.Applied, session.Action{
		Kind:   plan.Mode + "_dir",
		From:   srcDir,
		Target: dstDir,
	})

	// 2. Rewrite top-level cwd in every JSONL under dstDir.
	if !opts.DryRun {
		entries, err := os.ReadDir(dstDir)
		if err != nil {
			addErr("read dst dir: %v", err)
		} else {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
					continue
				}
				p := filepath.Join(dstDir, e.Name())
				if err := rewriteJSONLField(p, "cwd", plan.OldCWD, plan.NewCWD); err != nil {
					addErr("rewrite %s: %v", p, err)
				} else {
					r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: p})
				}
			}
		}
	}

	// 3. history.jsonl — rewrite the `project` field on matching lines.
	if paths.Exists(historyFile) && !opts.DryRun {
		if err := rewriteHistoryJSONL(historyFile, plan.OldCWD, plan.NewCWD, plan.Mode == "copy"); err != nil {
			addErr("rewrite history.jsonl: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: historyFile})
		}
	}

	// 4. ~/.claude.json — rename projects[oldCWD] key.
	if paths.Exists(claudeJSON) && !opts.DryRun {
		if err := renameClaudeJSONProjectKey(claudeJSON, plan.OldCWD, plan.NewCWD, plan.Mode == "copy"); err != nil {
			addErr("rewrite .claude.json: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: claudeJSON})
		}
	}

	return r
}

// rewriteJSONLField rewrites a top-level string field across every line of a
// JSONL file when its current value matches oldVal. Atomic via temp-file + rename.
// Top-level keys may be re-ordered (we marshal a map), which is harmless for
// Claude Code's parser.
func rewriteJSONLField(path, field, oldVal, newVal string) error {
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
	defer func() {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	w := bufio.NewWriter(tmp)
	br := bufio.NewReaderSize(in, 1<<20)

	for {
		line, err := br.ReadBytes('\n')
		hasContent := len(line) > 0
		hasNewline := hasContent && line[len(line)-1] == '\n'
		raw := line
		if hasNewline {
			raw = line[:len(line)-1]
		}
		if len(raw) > 0 {
			out := raw
			var m map[string]json.RawMessage
			if json.Unmarshal(raw, &m) == nil {
				if cur, ok := m[field]; ok {
					var s string
					if json.Unmarshal(cur, &s) == nil && s == oldVal {
						nb, _ := json.Marshal(newVal)
						m[field] = nb
						if b, mErr := json.Marshal(m); mErr == nil {
							out = b
						}
					}
				}
			}
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
	tmp = nil
	// Preserve perms.
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmpName, st.Mode().Perm())
	}
	return os.Rename(tmpName, path)
}

// rewriteHistoryJSONL updates each line's `project` field. In move mode any
// matching line is rewritten; in copy mode matching lines are duplicated so the
// new cwd inherits prompt history without losing the old.
func rewriteHistoryJSONL(path, oldCWD, newCWD string, copyMode bool) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(path), ".mvs-history-*.jsonl")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmp != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	w := bufio.NewWriter(tmp)
	br := bufio.NewReaderSize(in, 1<<20)

	for {
		line, err := br.ReadBytes('\n')
		hasContent := len(line) > 0
		hasNewline := hasContent && line[len(line)-1] == '\n'
		raw := line
		if hasNewline {
			raw = line[:len(line)-1]
		}

		if len(raw) > 0 {
			var m map[string]json.RawMessage
			if uerr := json.Unmarshal(raw, &m); uerr == nil {
				if cur, ok := m["project"]; ok {
					var s string
					if json.Unmarshal(cur, &s) == nil && s == oldCWD {
						if copyMode {
							// keep original
							if _, werr := w.Write(raw); werr != nil {
								return werr
							}
							if err := w.WriteByte('\n'); err != nil {
								return err
							}
						}
						nb, _ := json.Marshal(newCWD)
						m["project"] = nb
						if b, mErr := json.Marshal(m); mErr == nil {
							raw = b
						}
					}
				}
			}
			if _, werr := w.Write(raw); werr != nil {
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
	tmp = nil
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmpName, st.Mode().Perm())
	}
	return os.Rename(tmpName, path)
}

// renameClaudeJSONProjectKey loads ~/.claude.json, renames .projects[oldCWD] to
// .projects[newCWD] (move) or duplicates it (copy), and atomically writes back.
func renameClaudeJSONProjectKey(path, oldCWD, newCWD string, copyMode bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// We must preserve all other top-level fields exactly. Use RawMessage map.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	projRaw, ok := top["projects"]
	if !ok {
		return nil // nothing to do
	}
	var projs map[string]json.RawMessage
	if err := json.Unmarshal(projRaw, &projs); err != nil {
		return err
	}
	old, ok := projs[oldCWD]
	if !ok {
		return nil // no entry to migrate; not an error
	}
	projs[newCWD] = old
	if !copyMode {
		delete(projs, oldCWD)
	}
	nb, err := json.MarshalIndent(projs, "", "  ")
	if err != nil {
		return err
	}
	top["projects"] = nb
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, out)
}

func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mvs-*.tmp")
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
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// copyFile copies regular files, creating dirs as needed.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if st, err := os.Stat(src); err == nil {
		_ = os.Chmod(dst, st.Mode().Perm())
	}
	return nil
}

// copyTree recursively copies a directory tree, preserving timestamps best-effort.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			return os.Symlink(link, target)
		}
		if err := copyFile(path, target); err != nil {
			return err
		}
		if info, err := os.Stat(path); err == nil {
			_ = os.Chtimes(target, info.ModTime(), info.ModTime())
		}
		return nil
	})
}
