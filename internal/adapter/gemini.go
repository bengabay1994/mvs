package adapter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

// Gemini implements the Adapter interface for Google's gemini-cli.
//
// Layout (May 2026 main):
//
//	~/.gemini/tmp/<project-id>/        chats/, checkpoints/, .project_root, logs.json
//	~/.gemini/history/<project-id>/    persisted history
//	~/.gemini/projects.json            { projects: { "<abs path>": "<slug>" } }
//
// <project-id> is either sha256(cwd) (legacy) or a basename-derived slug
// claimed in projects.json. We honor whichever layout the host uses.
type Gemini struct{}

func (g *Gemini) Name() string { return "gemini" }

func (g *Gemini) Available() bool {
	root := paths.GeminiRoot()
	return paths.Exists(filepath.Join(root, "tmp")) || paths.Exists(filepath.Join(root, "history"))
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func geminiSlug(text string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(text) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		s = "project"
	}
	return s
}

func geminiNormalize(p string) string {
	resolved, err := filepath.Abs(p)
	if err != nil {
		resolved = p
	}
	if runtime.GOOS == "windows" {
		resolved = strings.ToLower(resolved)
	}
	return resolved
}

// readGeminiProjects returns the projects.json map.
func readGeminiProjects() (map[string]string, error) {
	path := filepath.Join(paths.GeminiRoot(), "projects.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var wrap struct {
		Projects map[string]string `json:"projects"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if wrap.Projects == nil {
		wrap.Projects = map[string]string{}
	}
	return wrap.Projects, nil
}

// resolveGeminiID returns the on-disk project-id for cwd given current registry.
// Falls back to sha256(cwd) for legacy hosts.
func resolveGeminiID(cwd string, registry map[string]string) string {
	if slug, ok := registry[geminiNormalize(cwd)]; ok {
		return slug
	}
	return sha256Hex(cwd)
}

func (g *Gemini) Discover() ([]session.Session, error) {
	root := paths.GeminiRoot()
	if !paths.Exists(root) {
		return nil, nil
	}
	registry, err := readGeminiProjects()
	if err != nil {
		return nil, err
	}
	// invert: id -> cwd
	idToCWD := map[string]string{}
	for cwd, slug := range registry {
		idToCWD[slug] = cwd
	}

	var out []session.Session
	for _, sub := range []string{"tmp", "history"} {
		base := filepath.Join(root, sub)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			id := e.Name()
			cwd := idToCWD[id]
			if cwd == "" {
				pr := filepath.Join(base, id, ".project_root")
				if data, err := os.ReadFile(pr); err == nil {
					cwd = strings.TrimSpace(string(data))
				}
			}
			if cwd == "" {
				continue
			}
			chatsDir := filepath.Join(base, id, "chats")
			files, err := os.ReadDir(chatsDir)
			if err != nil {
				// No chats — still surface one synthetic session so the project is visible.
				info, _ := e.Info()
				modTime := info.ModTime()
				size := info.Size()
				out = append(out, session.Session{
					Agent:       g.Name(),
					ID:          sub + "/" + id,
					CWD:         cwd,
					Title:       "(no chats recorded)",
					Modified:    modTime,
					Size:        size,
					Path:        filepath.Join(base, id),
					ProjectOnly: true,
				})
				continue
			}
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
					continue
				}
				info, ierr := f.Info()
				if ierr != nil {
					continue
				}
				out = append(out, session.Session{
					Agent:    g.Name(),
					ID:       sub + "/" + id + "/" + f.Name(),
					CWD:      cwd,
					Title:    firstLine(f.Name(), 80),
					Modified: info.ModTime(),
					Size:     info.Size(),
					Path:     filepath.Join(chatsDir, f.Name()),
				})
			}
		}
	}
	return out, nil
}

func (g *Gemini) Plan(sessions []session.Session, opts session.PlanOpts) (session.Plan, error) {
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
		Agent:      g.Name(),
		OldCWD:     oldCWD,
		NewCWD:     opts.NewCWD,
		Mode:       opts.Mode.String(),
		Scope:      opts.Scope.String(),
		SessionIDs: selectedIDs(sessions),
	}
	registry, err := readGeminiProjects()
	if err != nil {
		return session.Plan{}, err
	}
	oldID := resolveGeminiID(oldCWD, registry)
	if opts.Scope == session.ScopeSession {
		return planGeminiSessions(plan, sessions, oldID, registry, opts.Mode)
	}
	newID, err := geminiDestinationID(oldID, opts.NewCWD, registry, opts.Mode)
	if err != nil {
		return session.Plan{}, err
	}
	for _, sub := range []string{"tmp", "history"} {
		src := filepath.Join(paths.GeminiRoot(), sub, oldID)
		dst := filepath.Join(paths.GeminiRoot(), sub, newID)
		if !paths.Exists(src) {
			continue
		}
		if src != dst && paths.Exists(dst) {
			return session.Plan{}, fmt.Errorf("Gemini destination storage already exists: %s; migrate before opening Gemini in the target folder", dst)
		}
		kind := "rename_dir"
		if opts.Mode == session.ModeCopy {
			kind = "copy_dir"
		}
		plan.Actions = append(plan.Actions, session.Action{
			Kind:   kind,
			From:   src,
			Target: dst,
			Detail: sub + "/" + oldID + " → " + sub + "/" + newID,
		})
	}
	for _, sub := range []string{"tmp", "history"} {
		plan.Actions = append(plan.Actions,
			session.Action{Kind: "write_file", Target: filepath.Join(paths.GeminiRoot(), sub, newID, ".project_root"), Detail: "set canonical cwd"},
			session.Action{Kind: "write_file", Target: filepath.Join(paths.GeminiRoot(), sub, newID, "chats"), Detail: "rewrite projectHash in chat headers + path prefixes"},
		)
	}
	plan.Actions = append(plan.Actions, session.Action{Kind: "write_file", Target: filepath.Join(paths.GeminiRoot(), "projects.json"), Detail: "update registry"})
	return plan, nil
}

// isSlugID returns true if id looks like a slug (not a 64-char hex string).
func isSlugID(id string) bool {
	if len(id) != 64 {
		return true
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return true
		}
	}
	return false
}

func geminiDestinationID(oldID, newCWD string, registry map[string]string, mode session.Mode) (string, error) {
	newKey := geminiNormalize(newCWD)
	if existing, ok := registry[newKey]; ok {
		if mode == session.ModeCopy || existing != oldID {
			return "", fmt.Errorf("Gemini target path is already registered as project %q; refusing to merge project stores", existing)
		}
		return existing, nil
	}
	if !isSlugID(oldID) {
		return sha256Hex(newCWD), nil
	}
	if mode == session.ModeMove {
		// Slugs are registry values, not hashes. Keeping the existing value
		// avoids an unnecessary directory rename and basename collisions.
		return oldID, nil
	}

	used := make(map[string]struct{}, len(registry))
	for _, id := range registry {
		used[id] = struct{}{}
	}
	base := geminiSlug(filepath.Base(newCWD))
	candidate := base
	for n := 2; ; n++ {
		if _, exists := used[candidate]; !exists {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
}

func (g *Gemini) Apply(plan session.Plan, opts session.ApplyOpts) session.Report {
	if plan.Scope == session.ScopeSession.String() {
		return applyGeminiSessions(plan, opts)
	}
	r := session.Report{Agent: g.Name(), OK: true}
	addErr := func(format string, args ...any) {
		r.OK = false
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}

	registry, err := readGeminiProjects()
	if err != nil {
		addErr("read projects.json: %v", err)
		return r
	}
	oldID := resolveGeminiID(plan.OldCWD, registry)
	mode := session.ModeMove
	if plan.Mode == session.ModeCopy.String() {
		mode = session.ModeCopy
	}
	newID, err := geminiDestinationID(oldID, plan.NewCWD, registry, mode)
	if err != nil {
		addErr("resolve destination: %v", err)
		return r
	}

	bkRoot := filepath.Join(opts.BackupDir, "gemini")
	if !opts.DryRun {
		if err := os.MkdirAll(bkRoot, 0o700); err != nil {
			addErr("create backup root: %v", err)
			return r
		}
		for _, sub := range []string{"tmp", "history"} {
			src := filepath.Join(paths.GeminiRoot(), sub, oldID)
			if paths.Exists(src) {
				if err := copyTree(src, filepath.Join(bkRoot, sub+"-"+oldID)); err != nil {
					addErr("backup %s: %v", src, err)
					return r
				}
			}
		}
		pj := filepath.Join(paths.GeminiRoot(), "projects.json")
		if paths.Exists(pj) {
			if err := copyFile(pj, filepath.Join(bkRoot, "projects.json")); err != nil {
				addErr("backup projects.json: %v", err)
				return r
			}
		}
	}

	// 1. Move/copy per-project dirs.
	for _, sub := range []string{"tmp", "history"} {
		src := filepath.Join(paths.GeminiRoot(), sub, oldID)
		dst := filepath.Join(paths.GeminiRoot(), sub, newID)
		if !paths.Exists(src) {
			continue
		}
		if opts.DryRun {
			continue
		}
		created := src != dst
		if created && paths.Exists(dst) {
			addErr("destination storage already exists: %s", dst)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			addErr("mkdir %s: %v", filepath.Dir(dst), err)
			continue
		}
		if !created {
			r.Applied = append(r.Applied, session.Action{Kind: "write_dir", Target: dst})
			continue
		}
		if plan.Mode == "move" {
			if err := os.Rename(src, dst); err != nil {
				if err := copyTree(src, dst); err != nil {
					_ = os.RemoveAll(dst)
					addErr("move %s → %s: %v", src, dst, err)
					continue
				}
				r.Applied = append(r.Applied, session.Action{Kind: "move_dir", From: src, Target: dst, Created: true})
				if err := os.RemoveAll(src); err != nil {
					addErr("remove source %s: %v", src, err)
					continue
				}
			}
		} else {
			if err := copyTree(src, dst); err != nil {
				_ = os.RemoveAll(dst)
				addErr("copy %s → %s: %v", src, dst, err)
				continue
			}
		}
		alreadyRecorded := len(r.Applied) > 0 && r.Applied[len(r.Applied)-1].Target == dst
		if !alreadyRecorded {
			r.Applied = append(r.Applied, session.Action{Kind: plan.Mode + "_dir", From: src, Target: dst, Created: true})
		}
	}

	// 2. Update the ownership marker in every migrated store.
	if !opts.DryRun {
		for _, sub := range []string{"tmp", "history"} {
			pr := filepath.Join(paths.GeminiRoot(), sub, newID, ".project_root")
			if paths.Exists(filepath.Dir(pr)) {
				created := !paths.Exists(pr)
				if err := atomicWrite(pr, []byte(geminiNormalize(plan.NewCWD)+"\n")); err != nil {
					addErr("write %s .project_root: %v", sub, err)
				} else {
					r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: pr, Created: created})
				}
			}
		}
	}

	// 3. Rewrite chats/*.jsonl projectHash + best-effort cwd prefix replace.
	if !opts.DryRun {
		for _, sub := range []string{"tmp", "history"} {
			chatsDir := filepath.Join(paths.GeminiRoot(), sub, newID, "chats")
			entries, err := os.ReadDir(chatsDir)
			if err != nil {
				continue
			}
			oldHash := sha256Hex(plan.OldCWD)
			newHash := sha256Hex(plan.NewCWD)
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if !strings.HasSuffix(e.Name(), ".jsonl") {
					continue
				}
				p := filepath.Join(chatsDir, e.Name())
				if err := rewriteGeminiChatHeader(p, oldHash, newHash, plan.OldCWD, plan.NewCWD); err != nil {
					addErr("rewrite %s: %v", p, err)
				} else {
					r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: p})
				}
			}
		}
	}

	// 4. Update projects.json.
	if !opts.DryRun {
		projectsPath := filepath.Join(paths.GeminiRoot(), "projects.json")
		created := !paths.Exists(projectsPath)
		if err := updateGeminiProjectsJSON(plan.OldCWD, plan.NewCWD, newID, plan.Mode == "copy"); err != nil {
			addErr("update projects.json: %v", err)
		} else {
			r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: projectsPath, Created: created})
		}
	}

	finalizeReportDigests(&r)
	return r
}

func rewriteGeminiChatHeader(path, oldHash, newHash, oldCWD, newCWD string) error {
	first := true
	if err := streamRewriteJSONL(path, func(line []byte) []byte {
		if !first {
			return line
		}
		first = false
		var m map[string]json.RawMessage
		if err := json.Unmarshal(line, &m); err != nil {
			return line
		}
		// projectHash → surgical byte-replace so the rest of the header
		// metadata (key order, escape style) survives unchanged.
		if cur, ok := m["projectHash"]; ok {
			var s string
			if json.Unmarshal(cur, &s) == nil && s == oldHash {
				oldPat, newPat, perr := jsonFieldPattern("projectHash", oldHash, newHash)
				if perr == nil {
					line = bytes.Replace(line, oldPat, newPat, 1)
				}
			}
		}
		// directories[] needs structural editing (array element rewrite),
		// so re-parse, transform, and serialize without HTML escaping. The
		// first JSONL line is metadata only — reordering its keys is fine.
		if cur, ok := m["directories"]; ok {
			var dirs []string
			if json.Unmarshal(cur, &dirs) == nil {
				changed := false
				for i, d := range dirs {
					if d == oldCWD || strings.HasPrefix(d, oldCWD+string(os.PathSeparator)) {
						dirs[i] = newCWD + strings.TrimPrefix(d, oldCWD)
						changed = true
					}
				}
				if changed {
					// Re-parse the (possibly already-patched) line, then
					// substitute the directories field.
					var fresh map[string]json.RawMessage
					if err := json.Unmarshal(line, &fresh); err == nil {
						nb, err := marshalNoHTMLEscape(dirs, "")
						if err == nil {
							fresh["directories"] = nb
							out, err := marshalNoHTMLEscape(fresh, "")
							if err == nil {
								return out
							}
						}
					}
				}
			}
		}
		return line
	}); err != nil {
		return err
	}
	return validateGeminiChatHeader(path, oldHash, oldCWD)
}

func validateGeminiChatHeader(path, oldHash, oldCWD string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return err
		}
		return errors.New("Gemini chat file is empty")
	}
	var header struct {
		ProjectHash string   `json:"projectHash"`
		Directories []string `json:"directories"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
		return fmt.Errorf("parse rewritten chat header: %w", err)
	}
	if header.ProjectHash == oldHash {
		return errors.New("chat header still contains the old projectHash after rewrite")
	}
	for _, directory := range header.Directories {
		if directory == oldCWD || strings.HasPrefix(directory, oldCWD+string(os.PathSeparator)) {
			return fmt.Errorf("chat header still contains old directory %q after rewrite", directory)
		}
	}
	return nil
}

func updateGeminiProjectsJSON(oldCWD, newCWD, newSlug string, copyMode bool) error {
	path := filepath.Join(paths.GeminiRoot(), "projects.json")
	top := map[string]json.RawMessage{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &top); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	registry := map[string]string{}
	if raw, ok := top["projects"]; ok {
		if err := json.Unmarshal(raw, &registry); err != nil {
			return fmt.Errorf("parse projects registry: %w", err)
		}
	}
	oldKey := geminiNormalize(oldCWD)
	newKey := geminiNormalize(newCWD)
	registry[newKey] = newSlug
	if !copyMode {
		delete(registry, oldKey)
	}
	projectsJSON, err := marshalNoHTMLEscape(registry, "  ")
	if err != nil {
		return err
	}
	top["projects"] = projectsJSON
	out, err := marshalNoHTMLEscape(top, "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicWrite(path, out)
}
