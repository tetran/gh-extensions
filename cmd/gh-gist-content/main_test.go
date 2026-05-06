package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetran/gh-extensions/internal/exitcode"
)

func TestSanityCheck(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		content  string
		want     string // empty == no warning
	}{
		{"html clean DOCTYPE", "page.html", "<!DOCTYPE html>\n<html></html>", ""},
		{"html clean comment", "page.html", "<!-- Option D2 -->\n<html>", ""},
		{"html clean leading whitespace", "page.html", "  \n  <!DOCTYPE html>", ""},
		// Per §A4 spec, '<html>' is NOT an accepted start: HTML rule is '<!'.
		{"html bare element rejected", "page.html", "<html></html>", "warning"},
		{"html leaked description", "weird.html", "Sample landing page demo\n<!DOCTYPE html>", "warning"},
		{"htm same rule as html", "old.htm", "Notes here\n<!DOCTYPE html>", "warning"},
		{"json clean object", "data.json", "{\"ok\":true}", ""},
		{"json clean array", "list.json", "[1,2,3]", ""},
		{"json leaked", "data.json", "Description leaked here\n{\"ok\":true}", "warning"},
		{"xml clean prolog", "feed.xml", "<?xml version=\"1.0\"?>", ""},
		{"xml clean root", "feed.xml", "<rss></rss>", ""},
		{"xml leaked", "feed.xml", "RSS demo\n<?xml ?>", "warning"},
		{"svg clean", "icon.svg", "<svg xmlns=\"...\">", ""},
		{"py shebang", "script.py", "#!/usr/bin/env python\nprint('x')", ""},
		{"py import", "script.py", "import os\n", ""},
		{"py from", "script.py", "from os import path\n", ""},
		{"py def", "script.py", "def main():\n    pass\n", ""},
		{"py class", "script.py", "class Foo:\n    pass\n", ""},
		{"py leaked description", "script.py", "A small script demo\nimport os\n", "warning"},
		{"unknown ext skipped", "script.rb", "anything", ""},
		{"no extension skipped", "README", "anything", ""},
		{"uppercase ext", "PAGE.HTML", "<!DOCTYPE html>", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanityCheck(tc.filename, tc.content)
			if tc.want == "" {
				if got != "" {
					t.Errorf("sanityCheck(%q, %q) = %q, want empty", tc.filename, tc.content, got)
				}
				return
			}
			if !strings.HasPrefix(got, "gist-content: warning: expected ") {
				t.Errorf("warning has wrong prefix: %q", got)
			}
			if !strings.HasSuffix(got, "(gist description leak?)") {
				t.Errorf("warning missing leak suffix: %q", got)
			}
		})
	}
}

// TestSanityCheck_VerbatimMessage pins the warning to the exact wording used in
// the §A4 spec (`expected '[' or '{' at start, got 'g'`).
func TestSanityCheck_VerbatimMessage(t *testing.T) {
	got := sanityCheck("weird.json", "gist description leaked\n{\"ok\":true}")
	want := "gist-content: warning: expected '[' or '{' at start, got 'g' (gist description leak?)"
	if got != want {
		t.Errorf("verbatim mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestExtractFile_FixtureClean(t *testing.T) {
	resp := loadFixture(t, "gist_html_clean.json")

	html, err := extractFile(resp, "page.html")
	if err != nil {
		t.Fatalf("extractFile(page.html): %v", err)
	}
	if !strings.HasPrefix(html, "<!DOCTYPE html>") {
		t.Errorf("page.html content does not start with DOCTYPE: %q", html)
	}
	if msg := sanityCheck("page.html", html); msg != "" {
		t.Errorf("sanityCheck unexpected warning: %q", msg)
	}

	js, err := extractFile(resp, "data.json")
	if err != nil {
		t.Fatalf("extractFile(data.json): %v", err)
	}
	if !strings.HasPrefix(js, "{") {
		t.Errorf("data.json content does not start with '{': %q", js)
	}
}

func TestExtractFile_FixtureLeaked(t *testing.T) {
	resp := loadFixture(t, "gist_html_leaked.json")

	html, err := extractFile(resp, "weird.html")
	if err != nil {
		t.Fatalf("extractFile(weird.html): %v", err)
	}
	msg := sanityCheck("weird.html", html)
	if msg == "" {
		t.Fatalf("expected warning for leaked HTML, got none")
	}
	if !strings.Contains(msg, "expected '<!' at start") {
		t.Errorf("warning missing expected fragment: %q", msg)
	}
	if !strings.Contains(msg, "got 'S'") {
		t.Errorf("warning should echo the leaked first char: %q", msg)
	}
}

func TestExtractFile_MissingFile(t *testing.T) {
	resp := loadFixture(t, "gist_html_clean.json")
	_, err := extractFile(resp, "absent.txt")
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "absent.txt") {
		t.Errorf("error should name the missing file: %v", err)
	}
}

func TestRun_SuccessfulHTML(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fetch := func(_, _ string) (string, error) {
		return "<!DOCTYPE html>\n<body>ok</body>\n", nil
	}
	err := run([]string{"abc", "page.html"}, &stdout, &stderr, fetch)
	if err != nil {
		t.Fatalf("run returned error: %v (stderr=%q)", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "<!DOCTYPE html>") {
		t.Errorf("stdout missing content: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr, got: %q", stderr.String())
	}
}

func TestRun_SanityFailExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fetch := func(_, _ string) (string, error) {
		return "Sample landing page demo\n<!DOCTYPE html>\n", nil
	}
	err := run([]string{"abc", "weird.html"}, &stdout, &stderr, fetch)
	if err == nil {
		t.Fatalf("expected error from sanity failure, got nil")
	}
	var ec *exitcode.Error
	if !errors.As(err, &ec) {
		t.Fatalf("expected *exitcode.Error, got %T", err)
	}
	if ec.Code != exitcode.VerifyFailed {
		t.Errorf("expected VerifyFailed (1), got %d", ec.Code)
	}
	if !strings.Contains(stderr.String(), "gist-content: warning: expected '<!' at start") {
		t.Errorf("stderr missing warning: %q", stderr.String())
	}
	// Content is still emitted on stdout even when sanity warning fires.
	if !strings.Contains(stdout.String(), "<!DOCTYPE html>") {
		t.Errorf("expected stdout to still contain content: %q", stdout.String())
	}
}

func TestRun_NoSanitySkipsCheck(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fetch := func(_, _ string) (string, error) {
		return "leak\n<html>", nil
	}
	err := run([]string{"--no-sanity", "abc", "weird.html"}, &stdout, &stderr, fetch)
	if err != nil {
		t.Fatalf("expected nil error with --no-sanity, got %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr with --no-sanity, got: %q", stderr.String())
	}
}

func TestRun_MissingArgsExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fetch := func(_, _ string) (string, error) { return "", nil }
	err := run([]string{"only-one-arg"}, &stdout, &stderr, fetch)
	if err == nil {
		t.Fatalf("expected usage error, got nil")
	}
	var ec *exitcode.Error
	if !errors.As(err, &ec) || ec.Code != exitcode.UsageError {
		t.Errorf("expected UsageError (2), got %v", err)
	}
}

func loadFixture(t *testing.T, name string) gistResponse {
	t.Helper()
	// testdata is at the module root, so walk up from cmd/gh-gist-content/.
	path := filepath.Join("..", "..", "testdata", "A4", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	r, err := parseGist(b)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return r
}
