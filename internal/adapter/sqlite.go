package adapter

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// snapshotSQLite creates a consistent, standalone database image. Copying the
// main file beside live WAL files can produce a backup that never represented
// a committed database state.
func snapshotSQLite(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("snapshot destination already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return err
	}

	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}
