package adapter

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

func sqliteDSN(path, mode string, pragmas ...string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slashPath := filepath.ToSlash(abs)
	u := url.URL{Scheme: "file"}
	if runtime.GOOS == "windows" && strings.HasPrefix(slashPath, "//") {
		unc := strings.TrimPrefix(slashPath, "//")
		host, rest, found := strings.Cut(unc, "/")
		if !found {
			return "", fmt.Errorf("invalid UNC SQLite path %q", path)
		}
		u.Host = host
		u.Path = "/" + rest
	} else {
		if runtime.GOOS == "windows" && !strings.HasPrefix(slashPath, "/") {
			slashPath = "/" + slashPath
		}
		u.Path = slashPath
	}
	query := u.Query()
	if mode != "" {
		query.Set("mode", mode)
	}
	for _, pragma := range pragmas {
		query.Add("_pragma", pragma)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

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

	dsn, err := sqliteDSN(src, "", "busy_timeout(5000)")
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return err
	}
	return os.Chmod(dst, 0o600)
}
