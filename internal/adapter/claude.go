package adapter

import (
	"bufio"
	"bytes"
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
		// Snapshot the source dir.
		if paths.Exists(srcDir) {
			if err := copyTree(srcDir, filepath.Join(bkRoot, "projects-src-"+encOld)); err != nil {
				addErr("backup project dir: %v", err)
				return r
			}
		}
		// Snapshot the destination dir if it already exists. Without this,
		// undo deletes the dst dir entirely and any pre-existing sessions
		// at the new cwd are lost.
		if paths.Exists(dstDir) {
			if err := copyTree(dstDir, filepath.Join(bkRoot, "projects-dst-"+encNew)); err != nil {
				addErr("backup destination dir: %v", err)
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
// JSONL file when its current value matches oldVal. Surgical: it locates the
// exact byte span of `"field":"<json-escaped-oldVal>"` and replaces it,
// leaving every other byte of the line (key order, whitespace, escape style)
// untouched. This matters because claude-code is sensitive to the original
// byte layout — a json.Marshal round-trip HTML-escapes <,>,& and sorts keys,
// which can hide the session from /resume.
func rewriteJSONLField(path, field, oldVal, newVal string) error {
	return streamRewriteLines(path, func(line []byte) []byte {
		return replaceTopLevelStringField(line, field, oldVal, newVal)
	})
}

// replaceTopLevelStringField swaps the value of a top-level string field of a
// JSON object on a single line, without round-tripping through map[string]any.
// Returns the line unchanged if parsing fails, the field is missing, or the
// current value doesn't match oldVal.
func replaceTopLevelStringField(line []byte, field, oldVal, newVal string) []byte {
	if len(line) == 0 {
		return line
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(line, &probe); err != nil {
		return line
	}
	raw, ok := probe[field]
	if !ok {
		return line
	}
	var cur string
	if err := json.Unmarshal(raw, &cur); err != nil {
		return line
	}
	if cur != oldVal {
		return line
	}
	oldEnc, err := json.Marshal(oldVal)
	if err != nil {
		return line
	}
	newEnc, err := json.Marshal(newVal)
	if err != nil {
		return line
	}
	// Construct `"field":"value"` byte patterns. claude-code writes compact
	// JSON with no whitespace, so the literal bytes will match.
	oldPat := append([]byte{'"'}, []byte(field)...)
	oldPat = append(oldPat, '"', ':')
	oldPat = append(oldPat, oldEnc...)
	newPat := append([]byte{'"'}, []byte(field)...)
	newPat = append(newPat, '"', ':')
	newPat = append(newPat, newEnc...)
	out := bytes.Replace(line, oldPat, newPat, 1)
	if !bytes.Equal(out, line) || cur == newVal {
		return out
	}
	return line
}

// streamRewriteLines applies a per-line transform to a JSONL/text file
// atomically via temp-file + rename. The transform receives the line without
// its trailing newline; the trailing newline is preserved if present.
func streamRewriteLines(path string, transform func(line []byte) []byte) error {
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
			if _, werr := w.Write(transform(raw)); werr != nil {
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

// rewriteHistoryJSONL updates each line's `project` field. In move mode any
// matching line is rewritten in place via surgical byte replacement. In copy
// mode, every matching line is also duplicated so the new cwd inherits prompt
// history without losing the old.
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
	cleanup := true
	defer func() {
		if cleanup {
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
			matched := lineHasTopLevelStringField(raw, "project", oldCWD)
			if matched && copyMode {
				if _, werr := w.Write(raw); werr != nil {
					return werr
				}
				if err := w.WriteByte('\n'); err != nil {
					return err
				}
			}
			out := replaceTopLevelStringField(raw, "project", oldCWD, newCWD)
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

// lineHasTopLevelStringField returns true if the JSON-line has a top-level
// string field == want.
func lineHasTopLevelStringField(line []byte, field, want string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(line, &probe); err != nil {
		return false
	}
	raw, ok := probe[field]
	if !ok {
		return false
	}
	var cur string
	if err := json.Unmarshal(raw, &cur); err != nil {
		return false
	}
	return cur == want
}

// renameClaudeJSONProjectKey loads ~/.claude.json, renames .projects[oldCWD] to
// .projects[newCWD] (move) or duplicates it (copy), and atomically writes back.
// HTML escaping is disabled so values containing <,>,& survive the round-trip
// unchanged. Top-level key order isn't preserved (Go maps are unordered) but
// .claude.json is a config file, not a content-addressed transcript.
func renameClaudeJSONProjectKey(path, oldCWD, newCWD string, copyMode bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	projRaw, ok := top["projects"]
	if !ok {
		return nil
	}
	var projs map[string]json.RawMessage
	if err := json.Unmarshal(projRaw, &projs); err != nil {
		return err
	}
	old, ok := projs[oldCWD]
	if !ok {
		return nil
	}
	projs[newCWD] = old
	if !copyMode {
		delete(projs, oldCWD)
	}
	nb, err := marshalNoHTMLEscape(projs, "  ")
	if err != nil {
		return err
	}
	top["projects"] = nb
	out, err := marshalNoHTMLEscape(top, "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, out)
}

// marshalNoHTMLEscape encodes v as compact-or-indented JSON without
// HTML-escaping <,>,&. Pass "" for compact output, "  " (or similar) for
// indented.
func marshalNoHTMLEscape(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if indent != "" {
		enc.SetIndent("", indent)
	}
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	// json.Encoder always appends a trailing newline; drop it.
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
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
