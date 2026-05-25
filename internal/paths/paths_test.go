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

	cases := []struct {
		in   string
		want string
	}{
		// The actual bug: a trailing slash typed by the user would otherwise
		// be encoded as a trailing `-` in the on-disk dirname and stored
		// verbatim in the cwd field, breaking the exact-string match that
		// claude-code's --resume picker performs.
		{"/Users/ben/Desktop/foo/", "/Users/ben/Desktop/foo"},
		{"/Users/ben/Desktop/foo", "/Users/ben/Desktop/foo"},
		{"/Users/ben/Desktop/foo//", "/Users/ben/Desktop/foo"},
		{"/", "/"},
		{"", ""},
		{"   ", ""},
		{"  /foo  ", "/foo"},
		{"//double//slashes//here/", "/double/slashes/here"},
		// Tilde expansion happens before Clean.
		{"~", home},
		{"~" + sep + "Desktop", filepath.Join(home, "Desktop")},
		{"~" + sep + "Desktop" + sep, filepath.Join(home, "Desktop")},
		// Tilde-prefixed but not "~/" — left alone (Go has no ~user expansion).
		{"~something", "~something"},
	}
	for _, c := range cases {
		got := NormalizeCWD(c.in)
		if got != c.want {
			t.Errorf("NormalizeCWD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
