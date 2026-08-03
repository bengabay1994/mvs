package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Fingerprint returns a stable digest for a file, symlink, or directory tree.
// Modification times are intentionally excluded; content and relative paths
// are sufficient to detect post-migration writes before an undo.
func Fingerprint(path string) (string, error) {
	hash := sha256.New()
	rootInfo, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() {
		if err := hashEntry(hash, path, filepath.Base(path), rootInfo); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	err = filepath.WalkDir(path, func(entryPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, entryPath)
		if err != nil {
			return err
		}
		return hashEntry(hash, entryPath, filepath.ToSlash(rel), info)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// SQLiteFingerprint includes the main database and transient WAL/SHM files so
// an undo cannot discard committed data that has not yet checkpointed.
func SQLiteFingerprint(path string) (string, error) {
	hash := sha256.New()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		candidate := path + suffix
		digest, err := Fingerprint(candidate)
		if err != nil {
			if os.IsNotExist(err) && suffix != "" {
				if _, writeErr := fmt.Fprintf(hash, "%s\x00missing\x00", suffix); writeErr != nil {
					return "", writeErr
				}
				continue
			}
			return "", err
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%s\x00", suffix, digest); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashEntry(hash io.Writer, path, rel string, info fs.FileInfo) error {
	if _, err := fmt.Fprintf(hash, "%s\x00%s\x00", rel, info.Mode().Type().String()); err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		_, err = io.WriteString(hash, target)
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(hash, f)
	return err
}
