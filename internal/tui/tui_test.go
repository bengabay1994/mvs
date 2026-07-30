package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestCompletePath(t *testing.T) {
	tmp := t.TempDir()
	mk := func(name string, isDir bool) {
		p := filepath.Join(tmp, name)
		if isDir {
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("Repos", true)
	mk("Records", true)
	mk("RemoteThing", false)
	mk("Other", true)
	mk(".hidden", true)

	sep := string(os.PathSeparator)

	tests := []struct {
		name        string
		in          string
		wantOut     string
		wantSuggest []string
	}{
		{
			name:    "unique dir match completes with separator",
			in:      tmp + sep + "Other",
			wantOut: tmp + sep + "Other" + sep,
		},
		{
			name:    "common prefix collapses ambiguous matches",
			in:      tmp + sep + "Re",
			wantOut: tmp + sep + "Re", // already at LCP among Repos/Records/RemoteThing
		},
		{
			name:    "narrower fragment resolves to single dir",
			in:      tmp + sep + "Rep",
			wantOut: tmp + sep + "Repos" + sep,
		},
		{
			name:        "fragment matching multiple non-LCP-extendable items returns menu",
			in:          tmp + sep + "Re",
			wantSuggest: []string{"Records", "RemoteThing", "Repos"},
		},
		{
			name:    "empty input is left alone",
			in:      "",
			wantOut: "",
		},
		{
			name: "empty fragment hides dotfiles",
			in:   tmp + sep,
			// Suggestions should not contain ".hidden"
			wantSuggest: []string{"Other", "Records", "RemoteThing", "Repos"},
		},
		{
			name:    "dotfile fragment surfaces dotfiles",
			in:      tmp + sep + ".",
			wantOut: tmp + sep + ".hidden" + sep,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, sug := completePath(tc.in)
			if tc.wantOut != "" && out != tc.wantOut {
				t.Errorf("got out %q, want %q", out, tc.wantOut)
			}
			if tc.wantSuggest != nil {
				sort.Strings(sug)
				if !reflect.DeepEqual(sug, tc.wantSuggest) {
					t.Errorf("got suggestions %v, want %v", sug, tc.wantSuggest)
				}
			}
		})
	}
}

func TestLongestCommonPrefix(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"abc", "abd", "abe"}, "ab"},
		{[]string{"abc"}, "abc"},
		{[]string{}, ""},
		{[]string{"x", "y"}, ""},
		{[]string{"foo/", "foo/bar"}, "foo/"},
	}
	for _, c := range cases {
		got := longestCommonPrefix(c.in)
		if got != c.want {
			t.Errorf("longestCommonPrefix(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	sep := string(os.PathSeparator)
	if got := expandTilde("~"); got != home {
		t.Errorf("expandTilde(~) = %q, want %q", got, home)
	}
	if got := expandTilde("~" + sep + "foo"); got != filepath.Join(home, "foo") {
		t.Errorf("expandTilde(~/foo) = %q, want %q", got, filepath.Join(home, "foo"))
	}
	if got := expandTilde("/abs"); got != "/abs" {
		t.Errorf("expandTilde(/abs) = %q, want unchanged", got)
	}
	if got := expandTilde("~something"); !strings.HasPrefix(got, "~") {
		t.Errorf("expandTilde(~something) should not be expanded, got %q", got)
	}
}
