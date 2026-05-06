// Command gh-gist-content prints a single file's content from a gist by ID.
//
// Usage: gh gist-content <id> <filename> [--no-sanity]
//
// Sanity check: when the filename has a known extension (HTML, JSON, XML, SVG),
// the first non-whitespace character is compared against the expected prefix
// table. A mismatch typically means the gist's description leaked into the
// content body — emits a warning and exits 1. Pass --no-sanity to skip.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tetran/gh-extensions/internal/exitcode"
	"github.com/tetran/gh-extensions/internal/ghclient"
)

// fetcher resolves a gist file's content. Real impl hits the API; tests can
// inject a stub.
type fetcher func(gistID, filename string) (string, error)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, fetchGistFile); err != nil {
		var ec *exitcode.Error
		if errors.As(err, &ec) {
			if ec.Code != exitcode.Success {
				fmt.Fprintln(os.Stderr, ec.Err)
			}
			os.Exit(ec.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitcode.UpstreamErr)
	}
}

func run(args []string, stdout, stderr io.Writer, fetch fetcher) error {
	fs := flag.NewFlagSet("gh-gist-content", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noSanity := fs.Bool("no-sanity", false, "skip the extension/prefix sanity check")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gh gist-content <id> <filename> [--no-sanity]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitcode.Wrap(exitcode.UsageError, err)
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return exitcode.Wrap(exitcode.UsageError,
			fmt.Errorf("expected 2 positional args (id, filename), got %d", fs.NArg()))
	}
	gistID := fs.Arg(0)
	filename := fs.Arg(1)

	content, err := fetch(gistID, filename)
	if err != nil {
		if ghclient.IsNotFound(err) {
			return exitcode.Wrap(exitcode.UpstreamErr,
				fmt.Errorf("gh-gist-content: gist or file not found: %w", err))
		}
		return exitcode.Wrap(exitcode.UpstreamErr,
			fmt.Errorf("gh-gist-content: fetch: %w", err))
	}

	fmt.Fprint(stdout, content)

	if !*noSanity {
		if msg := sanityCheck(filename, content); msg != "" {
			fmt.Fprintln(stderr, msg)
			return exitcode.Wrap(exitcode.VerifyFailed, errors.New("sanity check failed"))
		}
	}
	return nil
}

// gistResponse is the subset of the gist payload we need.
type gistResponse struct {
	Files map[string]struct {
		Content string `json:"content"`
	} `json:"files"`
}

func fetchGistFile(id, filename string) (string, error) {
	c, err := ghclient.NewREST()
	if err != nil {
		return "", err
	}
	var resp gistResponse
	if err := c.Get("gists/"+id, &resp); err != nil {
		return "", err
	}
	return extractFile(resp, filename)
}

// extractFile picks one file's content out of a parsed gist payload.
// Split out from fetchGistFile so tests can drive it from a JSON fixture.
func extractFile(resp gistResponse, filename string) (string, error) {
	f, ok := resp.Files[filename]
	if !ok {
		names := make([]string, 0, len(resp.Files))
		for k := range resp.Files {
			names = append(names, k)
		}
		return "", fmt.Errorf("file %q not in gist (have: %s)", filename, strings.Join(names, ", "))
	}
	return f.Content, nil
}

// parseGist is a small helper used by tests to round-trip a fixture JSON
// into a gistResponse.
func parseGist(b []byte) (gistResponse, error) {
	var r gistResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	return r, nil
}

// prefixRule encodes one extension's acceptable starting tokens.
// `accept` lists the literal prefixes that pass the check; `display` is the
// human-readable rendering used in the warning ("'[' or '{'").
type prefixRule struct {
	accept  []string
	display string
}

// expectedPrefixes maps a file extension to its rule. Following the §A2 / §A4
// spec wording exactly:
//
//	.html / .htm → '<!'
//	.json        → '[' or '{'
//	.py          → shebang or 'import' / 'from' / 'def' / 'class'
//
// .xml and .svg are added because they are common gist contents and their
// first non-whitespace token is unambiguous; the rule mirrors the HTML one.
var expectedPrefixes = map[string]prefixRule{
	".html": {accept: []string{"<!"}, display: "'<!'"},
	".htm":  {accept: []string{"<!"}, display: "'<!'"},
	".xml":  {accept: []string{"<?", "<"}, display: "'<?' or '<'"},
	".svg":  {accept: []string{"<?", "<"}, display: "'<?' or '<'"},
	".json": {accept: []string{"[", "{"}, display: "'[' or '{'"},
	".py":   {accept: []string{"#!", "import ", "from ", "def ", "class "}, display: "shebang or 'import' / 'from' / 'def' / 'class'"},
}

// sanityCheck returns a warning message when content's first non-whitespace
// run does not match the prefix table for filename's extension. Empty string
// means "all good" (or no rule for this extension).
func sanityCheck(filename, content string) string {
	ext := strings.ToLower(extOf(filename))
	rule, ok := expectedPrefixes[ext]
	if !ok {
		return ""
	}
	trimmed := strings.TrimLeft(content, " \t\r\n")
	for _, w := range rule.accept {
		if strings.HasPrefix(trimmed, w) {
			return ""
		}
	}
	return fmt.Sprintf("gist-content: warning: expected %s at start, got '%s' (gist description leak?)", rule.display, firstChar(trimmed))
}

// firstChar returns the first rune of s as a string, or empty string if s is
// empty. Used for the "got '...'" portion of the warning, which the spec
// shows as a single character (e.g. "got 'g'").
func firstChar(s string) string {
	if s == "" {
		return ""
	}
	for _, r := range s {
		return string(r)
	}
	return ""
}

func extOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return name[i:]
}

