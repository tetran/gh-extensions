// Command gh-xref-verify verifies that a PR/Issue/commit triple is
// bidirectionally consistent before pasting a citation into permanent docs.
//
// Usage: gh xref-verify <PR> <Issue> [<expected-sha>] [--repo owner/name]
//
// It runs:
//
//	gh pr view <PR> --json closingIssuesReferences,mergeCommit,state,number,headRefName
//	gh issue view <Issue> --json closedByPullRequestsReferences,state,number
//
// and checks four points:
//
//  1. PR.closingIssuesReferences contains the Issue.
//  2. Issue.closedByPullRequestsReferences contains the PR.
//  3. PR.mergeCommit.oid == <expected-sha> (only if the SHA arg is given).
//  4. PR.state == MERGED && Issue.state == CLOSED.
//
// All four pass → exit 0 with a verbatim citation block on stdout.
// Any fails → exit 1 with the failed checks on stderr.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tetran/gh-extensions/internal/exitcode"
	"github.com/tetran/gh-extensions/internal/ghclient"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, defaultFetcher{}, time.Now); err != nil {
		var ec *exitcode.Error
		if errors.As(err, &ec) {
			os.Exit(ec.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitcode.UpstreamErr)
	}
}

// fail wraps an error with an exit code AND writes the message to stderr.
// run() owns all user-visible stderr output; main() only translates the exit
// code, so error messages remain visible to in-process callers (tests).
func fail(stderr io.Writer, code int, err error) error {
	if err != nil && code != exitcode.Success {
		fmt.Fprintln(stderr, err)
	}
	return exitcode.Wrap(code, err)
}

// fetcher abstracts the two gh-CLI lookups so tests can inject fixtures.
type fetcher interface {
	PR(repo string, prNumber int) (PRView, error)
	Issue(repo string, issueNumber int) (IssueView, error)
}

type defaultFetcher struct{}

func (defaultFetcher) PR(repo string, n int) (PRView, error) {
	args := []string{"pr", "view", strconv.Itoa(n),
		"--json", "closingIssuesReferences,mergeCommit,state,number,headRefName"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return PRView{}, err
	}
	var v PRView
	if err := json.Unmarshal(out, &v); err != nil {
		return PRView{}, fmt.Errorf("xref-verify: parse pr JSON: %w", err)
	}
	return v, nil
}

func (defaultFetcher) Issue(repo string, n int) (IssueView, error) {
	args := []string{"issue", "view", strconv.Itoa(n),
		"--json", "closedByPullRequestsReferences,state,number"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return IssueView{}, err
	}
	var v IssueView
	if err := json.Unmarshal(out, &v); err != nil {
		return IssueView{}, fmt.Errorf("xref-verify: parse issue JSON: %w", err)
	}
	return v, nil
}

// PRView is the subset of `gh pr view --json` we consume.
type PRView struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	HeadRefName string `json:"headRefName"`
	MergeCommit struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	ClosingIssuesReferences []IssueRef `json:"closingIssuesReferences"`
}

// IssueView is the subset of `gh issue view --json` we consume.
type IssueView struct {
	Number                       int     `json:"number"`
	State                        string  `json:"state"`
	ClosedByPullRequestsReferences []PRRef `json:"closedByPullRequestsReferences"`
}

type IssueRef struct {
	Number int `json:"number"`
}

type PRRef struct {
	Number int `json:"number"`
}

func run(args []string, stdout, stderr io.Writer, f fetcher, now func() time.Time) error {
	fs := flag.NewFlagSet("gh-xref-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	repoFlag := fs.String("repo", "", "GitHub repository in OWNER/NAME form (default: detect from git remote)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gh xref-verify <PR> <Issue> [<expected-sha>] [--repo OWNER/NAME]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return fail(stderr, exitcode.UsageError, err)
	}
	if fs.NArg() < 2 || fs.NArg() > 3 {
		fs.Usage()
		return fail(stderr, exitcode.UsageError,
			fmt.Errorf("xref-verify: expected 2 or 3 positional args (PR, Issue, [SHA]), got %d", fs.NArg()))
	}
	prNum, err := parseNum(fs.Arg(0), "PR")
	if err != nil {
		return fail(stderr, exitcode.UsageError, err)
	}
	issueNum, err := parseNum(fs.Arg(1), "Issue")
	if err != nil {
		return fail(stderr, exitcode.UsageError, err)
	}
	var expectedSHA string
	if fs.NArg() == 3 {
		expectedSHA = strings.TrimSpace(fs.Arg(2))
		if expectedSHA == "" {
			return fail(stderr, exitcode.UsageError, errors.New("xref-verify: expected-sha is empty"))
		}
	}

	pr, err := f.PR(*repoFlag, prNum)
	if err != nil {
		if ghclient.IsNotFound(err) {
			return fail(stderr, exitcode.UpstreamErr, fmt.Errorf("xref-verify: PR #%d not found: %w", prNum, err))
		}
		return fail(stderr, exitcode.UpstreamErr, fmt.Errorf("xref-verify: fetch PR: %w", err))
	}
	issue, err := f.Issue(*repoFlag, issueNum)
	if err != nil {
		if ghclient.IsNotFound(err) {
			return fail(stderr, exitcode.UpstreamErr, fmt.Errorf("xref-verify: Issue #%d not found: %w", issueNum, err))
		}
		return fail(stderr, exitcode.UpstreamErr, fmt.Errorf("xref-verify: fetch Issue: %w", err))
	}

	failures := verify(pr, issue, expectedSHA)
	if len(failures) > 0 {
		for _, msg := range failures {
			fmt.Fprintln(stderr, "✗ "+msg)
		}
		return exitcode.Wrap(exitcode.VerifyFailed, errors.New("xref-verify: one or more checks failed"))
	}

	writeSuccess(stdout, pr, issue, now())
	return nil
}

// verify returns the list of failure messages. Empty slice means all pass.
func verify(pr PRView, issue IssueView, expectedSHA string) []string {
	var fails []string

	prClosesIssue := containsIssueRef(pr.ClosingIssuesReferences, issue.Number)
	issueClosedByPR := containsPRRef(issue.ClosedByPullRequestsReferences, pr.Number)
	switch {
	case prClosesIssue && issueClosedByPR:
		// pass
	case prClosesIssue && !issueClosedByPR:
		fails = append(fails, fmt.Sprintf("PR #%d -> Issue #%d ref present, but Issue #%d does not list PR #%d back (unidirectional)", pr.Number, issue.Number, issue.Number, pr.Number))
	case !prClosesIssue && issueClosedByPR:
		fails = append(fails, fmt.Sprintf("Issue #%d -> PR #%d ref present, but PR #%d does not close Issue #%d (unidirectional)", issue.Number, pr.Number, pr.Number, issue.Number))
	default:
		fails = append(fails, fmt.Sprintf("PR #%d and Issue #%d do not reference each other", pr.Number, issue.Number))
	}

	if expectedSHA != "" {
		if !shaEquivalent(pr.MergeCommit.OID, expectedSHA) {
			fails = append(fails, fmt.Sprintf("Merge commit mismatch: PR #%d merged as %s, expected %s", pr.Number, shortSHA(pr.MergeCommit.OID), expectedSHA))
		}
	}

	if !strings.EqualFold(pr.State, "MERGED") {
		fails = append(fails, fmt.Sprintf("PR #%d state is %s, expected MERGED", pr.Number, pr.State))
	}
	if !strings.EqualFold(issue.State, "CLOSED") {
		fails = append(fails, fmt.Sprintf("Issue #%d state is %s, expected CLOSED", issue.Number, issue.State))
	}
	return fails
}

// writeSuccess emits the verbatim §A2 success block.
func writeSuccess(w io.Writer, pr PRView, issue IssueView, now time.Time) {
	short := shortSHA(pr.MergeCommit.OID)
	fmt.Fprintf(w, "✓ PR #%d closes Issue #%d (bidirectional)\n", pr.Number, issue.Number)
	fmt.Fprintf(w, "✓ Merge commit: %s... (matches)\n", short)
	fmt.Fprintf(w, "✓ States: PR=MERGED, Issue=CLOSED\n")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Verified citation:")
	fmt.Fprintf(w, "  Issue #%d PR: %s (PR #%d — %s)\n", issue.Number, short, pr.Number, pr.HeadRefName)
	fmt.Fprintf(w, "  verified via gh xref-verify on %s\n", now.Format("2006-01-02"))
}

func containsIssueRef(refs []IssueRef, n int) bool {
	for _, r := range refs {
		if r.Number == n {
			return true
		}
	}
	return false
}

func containsPRRef(refs []PRRef, n int) bool {
	for _, r := range refs {
		if r.Number == n {
			return true
		}
	}
	return false
}

// shaEquivalent compares two SHAs case-insensitively where one may be a
// prefix of the other. The user-supplied SHA can be a short form (7+ chars).
func shaEquivalent(actual, expected string) bool {
	a := strings.ToLower(strings.TrimSpace(actual))
	e := strings.ToLower(strings.TrimSpace(expected))
	if a == "" || e == "" {
		return false
	}
	if a == e {
		return true
	}
	if strings.HasPrefix(a, e) || strings.HasPrefix(e, a) {
		return true
	}
	return false
}

func shortSHA(s string) string {
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}

// reorderFlags shifts any "-flag" / "--flag" tokens (and their values) to the
// front of args, so the stdlib `flag` package — which stops at the first
// positional — accepts the natural `<positional...> --flag value` ordering.
//
// Scope: only handles the `--repo OWNER/NAME` flag this binary defines.
// Anything starting with `-` is treated as a flag; the next token is taken as
// its value when the flag is `-repo`/`--repo` and not in `--flag=value` form.
func reorderFlags(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			isRepo := a == "-repo" || a == "--repo"
			noValue := strings.Contains(a, "=")
			if isRepo && !noValue && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

func parseNum(s, label string) (int, error) {
	s = strings.TrimPrefix(s, "#")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid %s number %q: %w", label, s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s number must be positive, got %d", label, n)
	}
	return n, nil
}
