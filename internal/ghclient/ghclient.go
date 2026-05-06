// Package ghclient is a thin wrapper around go-gh for shared use across the
// gh-extensions binaries. Signatures pinned in SIGNATURES.md (Phase 0).
package ghclient

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	gh "github.com/cli/go-gh/v2"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

// NewREST returns a default RESTClient. It picks up auth and host from gh's
// own config, so no options are needed for normal use.
func NewREST() (*api.RESTClient, error) {
	c, err := api.NewRESTClient(api.ClientOptions{})
	if err != nil {
		return nil, fmt.Errorf("ghclient: new REST: %w", err)
	}
	return c, nil
}

// CurrentRepo returns the repository for the current working directory's git
// remote. Host is preserved so callers can keep GHES compatibility.
func CurrentRepo() (repository.Repository, error) {
	r, err := repository.Current()
	if err != nil {
		return repository.Repository{}, fmt.Errorf("ghclient: current repo: %w", err)
	}
	return r, nil
}

// RunGh executes the gh CLI with the given args and returns stdout. stderr is
// embedded in the returned error on failure. Used for paths that go-gh's own
// SDK does not cover (notably `gh pr view --json closingIssuesReferences`).
func RunGh(args ...string) ([]byte, error) {
	stdout, stderr, err := gh.Exec(args...)
	if err != nil {
		return nil, &GhExecError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return bytesCopy(stdout), nil
}

func bytesCopy(b bytes.Buffer) []byte {
	out := make([]byte, b.Len())
	copy(out, b.Bytes())
	return out
}

// GhExecError carries the failed gh invocation and its stderr.
type GhExecError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *GhExecError) Error() string {
	return fmt.Sprintf("gh %v failed: %v: %s", e.Args, e.Err, e.Stderr)
}

func (e *GhExecError) Unwrap() error { return e.Err }

// IsNotFound reports whether the underlying gh / API call indicates a 404.
// Used to map upstream not-found responses to exit code UpstreamErr (3).
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == 404
	}
	var ghErr *GhExecError
	if errors.As(err, &ghErr) {
		s := ghErr.Stderr
		return strings.Contains(s, "Could not resolve") ||
			strings.Contains(s, "Not Found") ||
			strings.Contains(s, "404")
	}
	return false
}
