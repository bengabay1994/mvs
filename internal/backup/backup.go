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
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bengabay1994/mvs/internal/fsutil"
	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

// Manifest is the on-disk undo record for a single migration run.
type Manifest struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	Timestamp time.Time        `json:"timestamp"`
	Plans     []session.Plan   `json:"plans"`
	Reports   []session.Report `json:"reports,omitempty"`
	UndoneAt  *time.Time       `json:"undone_at,omitempty"`
}

var runIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(?:-[0-9a-f]{8})?$`)

// NewRunID returns a sortable run identifier suitable for a directory name.
func NewRunID() string {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("%s-%08x", time.Now().UTC().Format("20060102-150405"), uint32(time.Now().UnixNano()))
	}
	return fmt.Sprintf("%s-%x", time.Now().UTC().Format("20060102-150405"), suffix)
}

// RunDir returns the absolute backup directory for the given run id.
func RunDir(id string) string {
	return filepath.Join(paths.MvsStateRoot(), "backups", id)
}

// Prepare creates the run dir and writes an initial manifest.
func Prepare(id string, plans []session.Plan) (string, error) {
	if err := validateRunID(id); err != nil {
		return "", err
	}
	root := filepath.Join(paths.MvsStateRoot(), "backups")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	dir := RunDir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	m := Manifest{Version: 2, ID: id, Timestamp: time.Now().UTC(), Plans: plans}
	if err := writeManifest(dir, m); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// Finalize appends reports to an existing run's manifest.
func Finalize(id string, reports []session.Report) error {
	if err := validateRunID(id); err != nil {
		return err
	}
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
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out, nil
}

// Undo restores the backup for the given run id by overwriting current files
// with the snapshotted copies. Snapshot subtrees under <run>/<agent>/... map
// directly back to the agent's home root via a small per-agent restore policy.
func Undo(id string) error {
	return undo(id, nil, true)
}

// Rollback restores a run using in-memory reports when recording the latest
// report failed. This keeps newly created targets discoverable during cleanup.
func Rollback(id string, reports []session.Report) error {
	return undo(id, reports, false)
}

func undo(id string, reports []session.Report, verifyDrift bool) error {
	if err := validateRunID(id); err != nil {
		return err
	}
	dir := RunDir(id)
	m, err := loadManifest(dir)
	if err != nil {
		return err
	}
	if m.UndoneAt != nil {
		return fmt.Errorf("backup %s was already restored at %s", id, m.UndoneAt.Local().Format(time.RFC3339))
	}
	if verifyDrift && m.Version < 2 {
		return errors.New("this backup predates safe drift detection; copy current agent data aside before restoring it manually")
	}
	if verifyDrift && len(m.Reports) != len(m.Plans) {
		return errors.New("this migration did not record a complete result; copy current agent data aside before restoring it manually")
	}
	if verifyDrift {
		for _, report := range m.Reports {
			if !report.OK {
				return errors.New("this migration did not finish successfully; copy current agent data aside before restoring it manually")
			}
		}
	}
	if reports != nil {
		m.Reports = reports
	}
	if verifyDrift {
		if err := verifyUnchangedTargets(m.Reports); err != nil {
			return err
		}
		if err := verifyMovedSourcesAbsent(m.Reports); err != nil {
			return err
		}
	}
	if err := removeCreatedTargets(m.Reports); err != nil {
		return err
	}
	restoreCount := len(m.Plans)
	if !verifyDrift && len(m.Reports) < restoreCount {
		restoreCount = len(m.Reports)
	}
	// Reverse order mirrors the order in which plans were applied.
	for i := restoreCount - 1; i >= 0; i-- {
		plan := m.Plans[i]
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
	now := time.Now().UTC()
	m.UndoneAt = &now
	return writeManifest(dir, m)
}

func verifyUnchangedTargets(reports []session.Report) error {
	checked := make(map[string]struct{})
	for _, report := range reports {
		for _, action := range report.Applied {
			if action.Target == "" {
				continue
			}
			if action.Digest == "" {
				return fmt.Errorf("refusing undo because %s has no post-migration fingerprint", action.Target)
			}
			if _, exists := checked[action.Target]; exists {
				continue
			}
			checked[action.Target] = struct{}{}
			var current string
			var err error
			if action.Kind == "sqlite_exec" {
				current, err = fsutil.SQLiteFingerprint(action.Target)
			} else {
				current, err = fsutil.Fingerprint(action.Target)
			}
			if err != nil {
				return fmt.Errorf("refusing undo because %s changed or disappeared after migration: %w", action.Target, err)
			}
			if current != action.Digest {
				return fmt.Errorf("refusing undo because %s changed after migration; back up current agent data before restoring", action.Target)
			}
		}
	}
	return nil
}

func verifyMovedSourcesAbsent(reports []session.Report) error {
	for _, report := range reports {
		for _, action := range report.Applied {
			if !action.Created || action.From == "" || action.From == action.Target {
				continue
			}
			if action.Kind != "move_file" && action.Kind != "move_dir" {
				continue
			}
			if pathExists(action.From) {
				return fmt.Errorf("refusing undo because the old source %s was recreated after migration", action.From)
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
	path := filepath.Join(dir, "undo.json")
	tmp, err := os.CreateTemp(dir, ".undo-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return fsutil.Replace(tmpName, path)
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

func validateRunID(id string) error {
	if !runIDPattern.MatchString(id) {
		return fmt.Errorf("invalid backup id %q", id)
	}
	return nil
}

func removeCreatedTargets(reports []session.Report) error {
	for i := len(reports) - 1; i >= 0; i-- {
		for j := len(reports[i].Applied) - 1; j >= 0; j-- {
			action := reports[i].Applied[j]
			if !action.Created || action.Target == "" {
				continue
			}
			switch action.Kind {
			case "copy_file", "copy_dir", "move_file", "move_dir", "rename_dir", "write_file":
				if err := os.RemoveAll(action.Target); err != nil {
					return fmt.Errorf("remove created target %s: %w", action.Target, err)
				}
			}
		}
	}
	return nil
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

	// Restore selected source transcripts from the BEFORE snapshot. Try the current name
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
	if srcSnap != "" && plan.Mode == session.ModeMove.String() {
		srcDir := filepath.Join(root, "projects", encOld)
		for _, action := range plan.Actions {
			if action.Kind != "move_file" {
				continue
			}
			rel, err := filepath.Rel(srcDir, action.From)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				return fmt.Errorf("invalid Claude source path in backup: %s", action.From)
			}
			snapshot := filepath.Join(srcSnap, rel)
			if pathExists(snapshot) {
				if err := copyFileOrFail(snapshot, action.From); err != nil {
					return err
				}
			}
		}
	}
	// Restore history.jsonl and .claude.json.
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
	if !pathExists(bk) {
		return nil
	}
	dbSnapshot := filepath.Join(bk, "state_5.sqlite")
	if pathExists(dbSnapshot) {
		if err := restoreSQLite(dbSnapshot, filepath.Join(root, "state_5.sqlite")); err != nil {
			return err
		}
	}
	return filepath.WalkDir(bk, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(bk, p)
		if rel == "state_5.sqlite" {
			return nil
		}
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
		if plan.Mode == session.ModeCopy.String() {
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
		if err := restoreSQLite(filepath.Join(bk, "opencode.db"), filepath.Join(root, "opencode.db")); err != nil {
			return err
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
	out, err := os.CreateTemp(filepath.Dir(dst), ".mvs-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := out.Name()
	cleanup := true
	defer func() {
		_ = out.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if st, err := os.Stat(src); err == nil {
		if err := out.Chmod(st.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := fsutil.Replace(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func restoreSQLite(snapshot, destination string) error {
	type displacedFile struct {
		original string
		temp     string
	}
	var displaced []displacedFile
	restoreDisplaced := func() error {
		_ = os.Remove(destination)
		for i := len(displaced) - 1; i >= 0; i-- {
			if err := os.Rename(displaced[i].temp, displaced[i].original); err != nil {
				return err
			}
		}
		return nil
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		current := destination + suffix
		if !pathExists(current) {
			continue
		}
		placeholder, err := os.CreateTemp(filepath.Dir(destination), ".mvs-sqlite-old-*")
		if err != nil {
			if restoreErr := restoreDisplaced(); restoreErr != nil {
				return fmt.Errorf("reserve SQLite restore path: %w; restore displaced files: %v", err, restoreErr)
			}
			return err
		}
		tempName := placeholder.Name()
		if err := placeholder.Close(); err != nil {
			_ = os.Remove(tempName)
			if restoreErr := restoreDisplaced(); restoreErr != nil {
				return fmt.Errorf("close SQLite restore placeholder: %w; restore displaced files: %v", err, restoreErr)
			}
			return err
		}
		if err := os.Remove(tempName); err != nil {
			if restoreErr := restoreDisplaced(); restoreErr != nil {
				return fmt.Errorf("remove SQLite restore placeholder: %w; restore displaced files: %v", err, restoreErr)
			}
			return err
		}
		if err := os.Rename(current, tempName); err != nil {
			if restoreErr := restoreDisplaced(); restoreErr != nil {
				return fmt.Errorf("displace %s: %w; restore displaced files: %v", current, err, restoreErr)
			}
			return err
		}
		displaced = append(displaced, displacedFile{original: current, temp: tempName})
	}

	if err := copyFileOrFail(snapshot, destination); err != nil {
		if restoreErr := restoreDisplaced(); restoreErr != nil {
			return fmt.Errorf("install SQLite snapshot: %w; restore displaced files: %v", err, restoreErr)
		}
		return err
	}
	for _, file := range displaced {
		_ = os.Remove(file.temp)
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
