// Package backup creates per-run backup directories and undo manifests.
//
// Each migration writes its backup snapshots under:
//
//	~/.local/state/mvs/backups/<RUN_ID>/
//
// alongside an undo.json manifest. To roll back, an operator runs `mvs undo
// <RUN_ID>`, which walks the manifest and copies each backed-up file/dir back
// into place.
package backup

import (
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
)

// Manifest is the on-disk undo record for a single migration run.
type Manifest struct {
	ID        string           `json:"id"`
	Timestamp time.Time        `json:"timestamp"`
	Plans     []session.Plan   `json:"plans"`
	Reports   []session.Report `json:"reports,omitempty"`
}

// NewRunID returns a sortable run identifier suitable for a directory name.
func NewRunID() string {
	return time.Now().UTC().Format("20060102-150405")
}

// RunDir returns the absolute backup directory for the given run id.
func RunDir(id string) string {
	return filepath.Join(paths.MvsStateRoot(), "backups", id)
}

// Prepare creates the run dir and writes an initial manifest.
func Prepare(id string, plans []session.Plan) (string, error) {
	dir := RunDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	m := Manifest{ID: id, Timestamp: time.Now().UTC(), Plans: plans}
	if err := writeManifest(dir, m); err != nil {
		return "", err
	}
	return dir, nil
}

// Finalize appends reports to an existing run's manifest.
func Finalize(id string, reports []session.Report) error {
	dir := RunDir(id)
	m, err := loadManifest(dir)
	if err != nil {
		return err
	}
	m.Reports = reports
	return writeManifest(dir, m)
}

// ListRuns enumerates known run ids, newest first.
func ListRuns() ([]Manifest, error) {
	root := filepath.Join(paths.MvsStateRoot(), "backups")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := loadManifest(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	// Newest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Undo restores the backup for the given run id by overwriting current files
// with the snapshotted copies. Snapshot subtrees under <run>/<agent>/... map
// directly back to the agent's home root via a small per-agent restore policy.
func Undo(id string) error {
	dir := RunDir(id)
	m, err := loadManifest(dir)
	if err != nil {
		return err
	}
	// Best-effort restoration per agent.
	for _, plan := range m.Plans {
		switch plan.Agent {
		case "claude":
			if err := restoreClaude(dir, plan); err != nil {
				return fmt.Errorf("claude undo: %w", err)
			}
		case "codex":
			if err := restoreCodex(dir, plan); err != nil {
				return fmt.Errorf("codex undo: %w", err)
			}
		case "gemini":
			if err := restoreGemini(dir, plan); err != nil {
				return fmt.Errorf("gemini undo: %w", err)
			}
		case "opencode":
			if err := restoreOpenCode(dir, plan); err != nil {
				return fmt.Errorf("opencode undo: %w", err)
			}
		}
	}
	return nil
}

func writeManifest(dir string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "undo.json"), data, 0o644)
}

func loadManifest(dir string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(filepath.Join(dir, "undo.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// ---- per-agent restore policies ----

func encodeClaudeForUndo(p string) string {
	// duplicated from adapter to avoid an import cycle
	out := make([]byte, len(p))
	for i := 0; i < len(p); i++ {
		ch := p[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			out[i] = ch
		} else {
			out[i] = '-'
		}
	}
	return string(out)
}

func restoreClaude(runDir string, plan session.Plan) error {
	bk := filepath.Join(runDir, "claude")
	root := paths.ClaudeRoot()
	encOld := encodeClaudeForUndo(plan.OldCWD)
	encNew := encodeClaudeForUndo(plan.NewCWD)

	// Remove the post-migration destination dir before restoring (it was a
	// merge of src + original dst content).
	_ = os.RemoveAll(filepath.Join(root, "projects", encNew))

	// 1. Restore destination dir's pre-existing content if we captured it.
	dstSnap := filepath.Join(bk, "projects-dst-"+encNew)
	if pathExists(dstSnap) {
		dst := filepath.Join(root, "projects", encNew)
		if err := copyTreeOrFail(dstSnap, dst); err != nil {
			return err
		}
	}
	// 2. Restore source dir from the BEFORE snapshot. Try the current name
	// first, fall back to the legacy `projects-<enc>` name written by older
	// versions of mvs.
	srcSnap := filepath.Join(bk, "projects-src-"+encOld)
	if !pathExists(srcSnap) {
		legacy := filepath.Join(bk, "projects-"+encOld)
		if pathExists(legacy) {
			srcSnap = legacy
		} else {
			srcSnap = ""
		}
	}
	if srcSnap != "" {
		src := filepath.Join(root, "projects", encOld)
		_ = os.RemoveAll(src)
		if err := copyTreeOrFail(srcSnap, src); err != nil {
			return err
		}
	}
	// 3. Restore history.jsonl and .claude.json.
	if pathExists(filepath.Join(bk, "history.jsonl")) {
		if err := copyFileOrFail(filepath.Join(bk, "history.jsonl"), filepath.Join(root, "history.jsonl")); err != nil {
			return err
		}
	}
	if pathExists(filepath.Join(bk, "claude.json")) {
		if err := copyFileOrFail(filepath.Join(bk, "claude.json"), paths.ClaudeJSON()); err != nil {
			return err
		}
	}
	return nil
}

func restoreCodex(runDir string, plan session.Plan) error {
	bk := filepath.Join(runDir, "codex")
	root := paths.CodexRoot()
	return filepath.WalkDir(bk, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(bk, p)
		return copyFileOrFail(p, filepath.Join(root, rel))
	})
}

func restoreGemini(runDir string, plan session.Plan) error {
	bk := filepath.Join(runDir, "gemini")
	root := paths.GeminiRoot()
	// Restore both tmp-<oldID> and history-<oldID>, and projects.json.
	entries, _ := os.ReadDir(bk)
	for _, e := range entries {
		name := e.Name()
		if name == "projects.json" {
			if err := copyFileOrFail(filepath.Join(bk, name), filepath.Join(root, name)); err != nil {
				return err
			}
			continue
		}
		// tmp-<id> or history-<id>
		var sub, id string
		switch {
		case strings.HasPrefix(name, "tmp-"):
			sub, id = "tmp", name[len("tmp-"):]
		case strings.HasPrefix(name, "history-"):
			sub, id = "history", name[len("history-"):]
		default:
			continue
		}
		dst := filepath.Join(root, sub, id)
		_ = os.RemoveAll(dst)
		if err := copyTreeOrFail(filepath.Join(bk, name), dst); err != nil {
			return err
		}
	}
	return nil
}

func restoreOpenCode(runDir string, plan session.Plan) error {
	bk := filepath.Join(runDir, "opencode")
	root := paths.OpenCodeRoot()
	if pathExists(filepath.Join(bk, "opencode.db")) {
		if err := copyFileOrFail(filepath.Join(bk, "opencode.db"), filepath.Join(root, "opencode.db")); err != nil {
			return err
		}
	}
	for _, s := range []string{"opencode.db-wal", "opencode.db-shm"} {
		if pathExists(filepath.Join(bk, s)) {
			_ = copyFileOrFail(filepath.Join(bk, s), filepath.Join(root, s))
		}
	}
	if pathExists(filepath.Join(bk, "snapshot")) {
		return copyTreeOrFail(filepath.Join(bk, "snapshot"), filepath.Join(root, "snapshot"))
	}
	return nil
}

// ---- io helpers ----

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func copyFileOrFail(src, dst string) error {
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

func copyTreeOrFail(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, lerr := os.Readlink(p)
			if lerr != nil {
				return lerr
			}
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			return os.Symlink(link, target)
		}
		return copyFileOrFail(p, target)
	})
}
