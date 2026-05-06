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

// expectedPrefixes maps a file extension to the set of acceptable first
// non-whitespace tokens. An entry of nil/empty means "no sanity rule".
var expectedPrefixes = map[string][]string{
	".html": {"<"},
	".htm":  {"<"},
	".xml":  {"<"},
	".svg":  {"<"},
	".json": {"{", "["},
}

// sanityCheck returns a warning message when content's first non-whitespace
// run does not match the prefix table for filename's extension. Empty string
// means "all good" (or no rule for this extension).
func sanityCheck(filename, content string) string {
	ext := strings.ToLower(extOf(filename))
	want, ok := expectedPrefixes[ext]
	if !ok {
		return ""
	}
	trimmed := strings.TrimLeft(content, " \t\r\n")
	for _, w := range want {
		if strings.HasPrefix(trimmed, w) {
			return ""
		}
	}
	got := snippet(trimmed, 40)
	expected := strings.Join(want, " | ")
	return fmt.Sprintf("gist-content: warning: expected '%s' at start, got '%s' (gist description leak?)", expected, got)
}

func extOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return name[i:]
}

func snippet(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
