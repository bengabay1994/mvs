package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeCWD(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	sep := string(os.PathSeparator)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	absProject := filepath.Join(cwd, "project", "foo")
	absNested := filepath.Join(cwd, "double", "slashes", "here")
	root := filepath.VolumeName(cwd) + sep

	cases := []struct {
		in   string
		want string
	}{
		// The actual bug: a trailing slash typed by the user would otherwise
		// be encoded as a trailing `-` in the on-disk dirname and stored
		// verbatim in the cwd field, breaking the exact-string match that
		// claude-code's --resume picker performs.
		{absProject + sep, absProject},
		{absProject, absProject},
		{absProject + sep + sep, absProject},
		{root, root},
		{"", ""},
		{"   ", ""},
		{"  " + absProject + "  ", absProject},
		{cwd + sep + sep + "double" + sep + sep + "slashes" + sep + sep + "here" + sep, absNested},
		// Tilde expansion happens before Clean.
		{"~", home},
		{"~" + sep + "Desktop", filepath.Join(home, "Desktop")},
		{"~" + sep + "Desktop" + sep, filepath.Join(home, "Desktop")},
		// Tilde-prefixed but not "~/" — left alone (Go has no ~user expansion).
		{"~something", filepath.Join(cwd, "~something")},
		{".", cwd},
		{"relative/project", filepath.Join(cwd, "relative", "project")},
	}
	for _, c := range cases {
		got := NormalizeCWD(c.in)
		if got != c.want {
			t.Errorf("NormalizeCWD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
