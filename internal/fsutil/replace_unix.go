//go:build !windows

package fsutil

import (
	"os"
	"path/filepath"
)

// Replace atomically replaces dst with src on the same filesystem.
func Replace(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return nil
	}
	// The replacement has committed once rename succeeds. Directory syncing is
	// best-effort because reporting a later sync error would make callers treat
	// an applied replacement as unapplied and skip rollback bookkeeping.
	_ = dir.Sync()
	_ = dir.Close()
	return nil
}
