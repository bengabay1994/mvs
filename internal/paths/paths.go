// Package paths centralizes per-OS resolution of agent data directories.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Home returns the user's home directory or empty string.
func Home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// XDGData returns the XDG data home (~/.local/share on Unix, %LOCALAPPDATA% on Windows).
// This matches what xdg-basedir resolves to and is what opencode uses.
func XDGData() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
	}
	return filepath.Join(Home(), ".local", "share")
}

// XDGState returns the XDG state home (~/.local/state on Unix, %LOCALAPPDATA% on Windows).
func XDGState() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return v
		}
	}
	return filepath.Join(Home(), ".local", "state")
}

// ClaudeRoot returns ~/.claude (honoring $CLAUDE_CONFIG_DIR).
func ClaudeRoot() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(Home(), ".claude")
}

// ClaudeJSON is the path to ~/.claude.json.
func ClaudeJSON() string {
	return filepath.Join(Home(), ".claude.json")
}

// CodexRoot returns ~/.codex (honoring $CODEX_HOME).
func CodexRoot() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	return filepath.Join(Home(), ".codex")
}

// GeminiRoot returns ~/.gemini.
func GeminiRoot() string {
	return filepath.Join(Home(), ".gemini")
}

// OpenCodeRoot returns the opencode data dir (XDG_DATA_HOME/opencode).
func OpenCodeRoot() string {
	return filepath.Join(XDGData(), "opencode")
}

// MvsStateRoot is where mvs stores its own state (backups, undo manifests).
func MvsStateRoot() string {
	return filepath.Join(XDGState(), "mvs")
}

// Exists is a small helper.
func Exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// NormalizeCWD turns a user-supplied path into the canonical form mvs stores
// inside session files: `~` is expanded, redundant separators are collapsed,
// and any trailing separator is stripped.
//
// This matters because claude-code, codex, gemini, and opencode all filter
// sessions by an exact-string equality match between the in-file cwd and
// `process.cwd()` at resume time. The kernel's `process.cwd()` value never has
// a trailing slash, so a user typing `/foo/bar/` in mvs would otherwise create
// sessions that `--resume` from `/foo/bar` cannot see.
//
// Empty input returns empty (caller is expected to reject it). Root "/" is
// preserved exactly. Tilde (`~` or `~/...`) is expanded relative to $HOME.
func NormalizeCWD(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~"+string(os.PathSeparator)) {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				p = home
			} else {
				p = filepath.Join(home, p[2:])
			}
		}
	}
	return filepath.Clean(p)
}
