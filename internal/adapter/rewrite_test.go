package adapter

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression test: rewriteJSONLField must preserve every byte of each line
// except the cwd value. json.Marshal-based round-trips HTML-escape <,>,& and
// sort keys alphabetically — both of which can hide the migrated session from
// claude-code's /resume picker.
func TestRewriteJSONLFieldPreservesBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Mirror the wire format claude-code actually writes: compact JSON with
	// literal < > & inside message content, non-alphabetical key order.
	lines := []string{
		`{"type":"summary","cwd":"/old/path","aiTitle":"Hi"}`,
		`{"parentUuid":null,"type":"user","message":{"role":"user","content":"<command-name>/effort</command-name>\n<args>foo & bar</args>"},"cwd":"/old/path","sessionId":"s1","gitBranch":"HEAD"}`,
		`{"type":"assistant","content":"unrelated","sessionId":"s1"}`, // no cwd
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rewriteJSONLField(path, "cwd", "/old/path", "/new/path"); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotLines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(gotLines) != 3 {
		t.Fatalf("got %d lines, want 3", len(gotLines))
	}

	// Property 1: cwd-bearing lines have ONLY the cwd value swapped.
	wantLine0 := `{"type":"summary","cwd":"/new/path","aiTitle":"Hi"}`
	if gotLines[0] != wantLine0 {
		t.Errorf("line 0:\n got %q\nwant %q", gotLines[0], wantLine0)
	}
	wantLine1 := strings.Replace(lines[1], "/old/path", "/new/path", 1)
	if gotLines[1] != wantLine1 {
		t.Errorf("line 1 not byte-identical-modulo-cwd:\n got %q\nwant %q", gotLines[1], wantLine1)
	}

	// Property 2: lines without a matching cwd field are passed through.
	if gotLines[2] != lines[2] {
		t.Errorf("line 2 should be passthrough:\n got %q\nwant %q", gotLines[2], lines[2])
	}

	// Property 3: NO HTML \uXXXX escaping snuck in (Go's default json.Marshal
	// re-encodes the literal characters < > & as backslash-u sequences). The
	// migrated file should still contain the literal characters.
	for _, esc := range []string{"\\u003c", "\\u003e", "\\u0026"} {
		if bytes.Contains(out, []byte(esc)) {
			t.Errorf("output contains HTML-escape sequence %q — bytes were re-encoded", esc)
		}
	}
	for _, lit := range []string{"<", ">", "&"} {
		if !bytes.Contains(out, []byte(lit)) {
			t.Errorf("output is missing literal %q present in input", lit)
		}
	}

	// Property 4: every literal < > & in the input is still literal.
	wantBangChars := strings.Count(lines[1], "<") + strings.Count(lines[1], ">") + strings.Count(lines[1], "&")
	gotBangChars := strings.Count(string(out), "<") + strings.Count(string(out), ">") + strings.Count(string(out), "&")
	if gotBangChars != wantBangChars {
		t.Errorf("special-char count changed: got %d, want %d", gotBangChars, wantBangChars)
	}
}

// rewriteJSONLField must leave a line alone when its cwd field doesn't match
// oldVal — even if the literal byte pattern appears elsewhere on the line.
func TestRewriteJSONLFieldSkipsNonMatchingLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.jsonl")
	in := `{"type":"user","cwd":"/different/path","message":{"text":"the string \"cwd\":\"/old/path\" appears in content"}}` + "\n"
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteJSONLField(path, "cwd", "/old/path", "/new/path"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	if string(out) != in {
		t.Errorf("line was modified despite top-level cwd != oldVal:\ngot  %q\nwant %q", out, in)
	}
}

// jsonFieldPattern produces the right byte patterns and they round-trip the
// JSON string-escape correctly.
func TestJSONFieldPattern(t *testing.T) {
	oldPat, newPat, err := jsonFieldPattern("cwd", "/a", "/b")
	if err != nil {
		t.Fatal(err)
	}
	if want := `"cwd":"/a"`; string(oldPat) != want {
		t.Errorf("oldPat = %q, want %q", oldPat, want)
	}
	if want := `"cwd":"/b"`; string(newPat) != want {
		t.Errorf("newPat = %q, want %q", newPat, want)
	}

	// Values with characters that JSON would escape.
	oldPat, newPat, err = jsonFieldPattern("k", "a\"b", "c\nd")
	if err != nil {
		t.Fatal(err)
	}
	if want := `"k":"a\"b"`; string(oldPat) != want {
		t.Errorf("escaped oldPat = %q, want %q", oldPat, want)
	}
	if want := `"k":"c\nd"`; string(newPat) != want {
		t.Errorf("escaped newPat = %q, want %q", newPat, want)
	}
}
