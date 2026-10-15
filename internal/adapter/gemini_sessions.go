package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bengabay1994/mvs/internal/paths"
	"github.com/bengabay1994/mvs/internal/session"
)

func planGeminiSessions(plan session.Plan, sessions []session.Session, oldID string, registry map[string]string, mode session.Mode) (session.Plan, error) {
	// A session transfer always keeps the source project's identity. Existing
	// destination stores are allowed; only colliding conversation files fail.
	newID := registry[geminiNormalize(plan.NewCWD)]
	if newID == "" {
		var err error
		newID, err = geminiDestinationID(oldID, plan.NewCWD, registry, session.ModeCopy)
		if err != nil {
			return session.Plan{}, err
		}
	}
	if newID == oldID {
		return session.Plan{}, errors.New("Gemini source and target share a project store")
	}
	if newID == "" || newID == "." || newID == ".." || strings.ContainsAny(newID, `/\`) {
		return session.Plan{}, errors.New("invalid Gemini target project ID")
	}
	root := paths.GeminiRoot()
	for _, s := range sessions {
		if s.ProjectOnly {
			return session.Plan{}, errors.New("select a Gemini conversation, or switch to project mode for history-only stores")
		}
		rel, err := relativeStorePath(root, s.Path)
		if err != nil {
			return session.Plan{}, err
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		if len(parts) != 4 || (parts[0] != "tmp" && parts[0] != "history") || parts[1] != oldID || parts[2] != "chats" || !strings.HasSuffix(parts[3], ".jsonl") {
			return session.Plan{}, fmt.Errorf("unsupported Gemini conversation path: %s", s.Path)
		}
		target := filepath.Join(root, parts[0], newID, "chats", parts[3])
		if paths.Exists(target) {
			return session.Plan{}, fmt.Errorf("Gemini destination session already exists: %s", target)
		}
		marker := filepath.Join(root, parts[0], newID, ".project_root")
		if data, err := os.ReadFile(marker); err == nil {
			if strings.TrimSpace(string(data)) != geminiNormalize(plan.NewCWD) {
				return session.Plan{}, fmt.Errorf("Gemini destination store belongs to another project: %s", marker)
			}
		} else if !os.IsNotExist(err) {
			return session.Plan{}, err
		}
		kind := "move_file"
		if mode == session.ModeCopy {
			kind = "copy_file"
		}
		plan.Actions = append(plan.Actions, session.Action{Kind: kind, From: s.Path, Target: target, Detail: "transfer selected chat and rewrite its header"})
	}
	plan.Actions = append(plan.Actions, session.Action{Kind: "write_file", Target: filepath.Join(root, "projects.json"), Detail: "register target; retain source project"})
	return plan, nil
}

func applyGeminiSessions(plan session.Plan, opts session.ApplyOpts) session.Report {
	r := session.Report{Agent: "gemini", OK: true}
	fail := func(err error) session.Report { r.OK = false; r.Errors = append(r.Errors, err.Error()); return r }
	if opts.DryRun {
		return r
	}
	root := paths.GeminiRoot()
	bk := filepath.Join(opts.BackupDir, "gemini")
	if err := os.MkdirAll(bk, 0o700); err != nil {
		return fail(err)
	}
	registry := filepath.Join(root, "projects.json")
	if paths.Exists(registry) {
		if err := copyFile(registry, filepath.Join(bk, "projects.json")); err != nil {
			return fail(err)
		}
	}
	// Finish all source backups and collision checks before modifying anything.
	for _, a := range plan.Actions {
		if a.Kind != "move_file" && a.Kind != "copy_file" {
			continue
		}
		if paths.Exists(a.Target) {
			return fail(fmt.Errorf("Gemini destination session already exists: %s", a.Target))
		}
		if a.Kind == "move_file" {
			rel, err := relativeStorePath(root, a.From)
			if err != nil {
				return fail(err)
			}
			if err := copyFile(a.From, filepath.Join(bk, "files", rel)); err != nil {
				return fail(err)
			}
		}
	}
	newID := ""
	for _, a := range plan.Actions {
		if a.Kind != "move_file" && a.Kind != "copy_file" {
			continue
		}
		store := filepath.Dir(filepath.Dir(a.Target))
		newID = filepath.Base(store)
		if !paths.Exists(store) {
			if err := os.MkdirAll(store, 0o700); err != nil {
				return fail(err)
			}
			r.Applied = append(r.Applied, session.Action{Kind: "mkdir", Target: store, Created: true})
		}
		marker := filepath.Join(store, ".project_root")
		if data, err := os.ReadFile(marker); err == nil {
			if strings.TrimSpace(string(data)) != geminiNormalize(plan.NewCWD) {
				return fail(fmt.Errorf("Gemini destination store belongs to another project: %s", marker))
			}
		} else if os.IsNotExist(err) {
			if err := atomicWrite(marker, []byte(geminiNormalize(plan.NewCWD)+"\n")); err != nil {
				return fail(err)
			}
			r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: marker, Created: true})
		} else {
			return fail(err)
		}
		if err := os.MkdirAll(filepath.Dir(a.Target), 0o700); err != nil {
			return fail(err)
		}
		if err := copyFile(a.From, a.Target); err != nil {
			return fail(err)
		}
		r.Applied = append(r.Applied, session.Action{Kind: a.Kind, From: a.From, Target: a.Target, Created: true})
		if err := rewriteGeminiChatHeader(a.Target, sha256Hex(plan.OldCWD), sha256Hex(plan.NewCWD), plan.OldCWD, plan.NewCWD); err != nil {
			return fail(err)
		}
		if a.Kind == "move_file" {
			if err := os.Remove(a.From); err != nil {
				return fail(err)
			}
		}
	}
	if newID == "" {
		return fail(errors.New("no Gemini conversations selected"))
	}
	created := !paths.Exists(registry)
	if err := updateGeminiProjectsJSON(plan.OldCWD, plan.NewCWD, newID, true); err != nil {
		return fail(err)
	}
	r.Applied = append(r.Applied, session.Action{Kind: "write_file", Target: registry, Created: created})
	finalizeReportDigests(&r)
	return r
}

func relativeStorePath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path is outside agent storage: %s", path)
	}
	return rel, nil
}
