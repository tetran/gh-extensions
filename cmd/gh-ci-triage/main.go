// Command gh-ci-triage classifies a PR's CI failures as either
// PRE-EXISTING FLAKY (main is itself failing this workflow) or LIKELY
// REGRESSION (main is green; this PR caused it), with a grey-zone fallback.
//
// Usage: gh ci-triage [<PR>] [--workflow <name>] [--samples 8] [--repo OWNER/NAME]
//
// Pipeline:
//
//  1. Resolve PR number — explicit arg, or `gh pr view --json number` from
//     the current branch.
//  2. List failed checks via `gh pr checks <PR> --json ...`.
//  3. Group by workflow. For each failed workflow, sample the last N main
//     runs (`gh run list --workflow=... --branch=main --limit=N --json
//     conclusion`) and pull a log line via `gh run view <id> --log` to
//     extract a failed test file (pytest adapter only for now).
//  4. Apply the §A3 verdict rules:
//     - main fail rate ≥ 1/5 AND test-file last-touched outside this PR
//     → PRE-EXISTING FLAKY
//     - main fail rate = 0/N → LIKELY REGRESSION (caused by this PR)
//     - otherwise → grey-zone with reason
//  5. Emit the verbatim §A3 block on stdout. Any LIKELY REGRESSION line
//     escalates exit to 1; otherwise 0 (regime α from the implementation
//     plan). Upstream failures map to 3.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tetran/gh-extensions/internal/exitcode"
	"github.com/tetran/gh-extensions/internal/ghclient"
	"github.com/tetran/gh-extensions/internal/logparse"
)

const defaultSamples = 8

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, defaultDeps()); err != nil {
		var ec *exitcode.Error
		if errors.As(err, &ec) {
			os.Exit(ec.Code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitcode.UpstreamErr)
	}
}

// --- types ------------------------------------------------------------------

// FailedCheck is one entry from `gh pr checks --json`.
type FailedCheck struct {
	Name     string `json:"name"`
	Workflow string `json:"workflow"`
	State    string `json:"state"`
	Bucket   string `json:"bucket"` // gh's normalized state ("fail", "pass", "pending", ...)
	Link     string `json:"link"`   // run URL — used to extract Actions run id
}

// MainRun is one entry from `gh run list --json conclusion,databaseId`.
type MainRun struct {
	Conclusion string `json:"conclusion"`
	DatabaseID int64  `json:"databaseId"`
}

// PerWorkflow is the analysis for a single failed workflow.
type PerWorkflow struct {
	Workflow      string
	PRJobs        []FailedCheck
	MainSamples   []MainRun
	FailRateNum   int // numerator (M)
	FailRateDen   int // denominator (N) — may be < requested samples if main has fewer runs
	TestFile      string
	TestFileFound bool
	LastTouchSHA  string
	InThisPR      bool
	Verdict       Verdict
	GreyReason    string
}

// Verdict enumerates the three outcomes.
type Verdict int

const (
	VerdictUnknown Verdict = iota
	VerdictPreExistingFlaky
	VerdictLikelyRegression
	VerdictGrey
)

func (v Verdict) String() string {
	switch v {
	case VerdictPreExistingFlaky:
		return "PRE-EXISTING FLAKY"
	case VerdictLikelyRegression:
		return "LIKELY REGRESSION (caused by this PR)"
	case VerdictGrey:
		return "GREY ZONE"
	default:
		return "UNKNOWN"
	}
}

// deps bundles the IO-touching helpers; tests inject fixtures for each.
type deps struct {
	resolvePR func(repo string) (int, error)
	prChecks  func(repo string, pr int) ([]FailedCheck, error)
	mainRuns  func(repo, branch, workflowName string, samples int) ([]MainRun, error)
	runLog    func(repo string, runID int64) ([]byte, error)
	prCommits func(repo string, pr int) (map[string]bool, error)
	gitLog    func(file string) (string, error)
	now       func() time.Time
}

func defaultDeps() deps {
	return deps{
		resolvePR:  resolvePRFromBranch,
		prChecks:   fetchPRChecks,
		mainRuns:   fetchMainRuns,
		runLog:     fetchRunLog,
		prCommits:  fetchPRCommits,
		gitLog:     gitLogLastTouched,
		now:        time.Now,
	}
}

// --- run --------------------------------------------------------------------

func run(args []string, stdout, stderr io.Writer, d deps) error {
	fs := flag.NewFlagSet("gh-ci-triage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workflowFilter := fs.String("workflow", "", "only triage this workflow (matches the 'workflow' field from gh pr checks)")
	samples := fs.Int("samples", defaultSamples, "number of recent main runs to sample per workflow")
	repoFlag := fs.String("repo", "", "GitHub repository in OWNER/NAME form (default: detect from git remote)")
	mainBranch := fs.String("main-branch", "main", "branch name to sample baseline runs from (set to 'trunk', etc.)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gh ci-triage [<PR>] [--workflow <name>] [--samples 8] [--main-branch main] [--repo OWNER/NAME]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorderFlags(args, []string{"workflow", "samples", "repo", "main-branch"})); err != nil {
		return wrapErr(stderr, exitcode.UsageError, err)
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return wrapErr(stderr, exitcode.UsageError,
			fmt.Errorf("ci-triage: at most one positional arg (PR number), got %d", fs.NArg()))
	}
	if *samples < 1 {
		return wrapErr(stderr, exitcode.UsageError,
			fmt.Errorf("ci-triage: --samples must be >= 1, got %d", *samples))
	}

	prNum := 0
	if fs.NArg() == 1 {
		n, err := parseNum(fs.Arg(0))
		if err != nil {
			return wrapErr(stderr, exitcode.UsageError, fmt.Errorf("ci-triage: %w", err))
		}
		prNum = n
	} else {
		n, err := d.resolvePR(*repoFlag)
		if err != nil {
			return wrapErr(stderr, exitcode.UpstreamErr,
				fmt.Errorf("ci-triage: cannot resolve PR from current branch: %w", err))
		}
		prNum = n
	}

	checks, err := d.prChecks(*repoFlag, prNum)
	if err != nil {
		if ghclient.IsNotFound(err) {
			return wrapErr(stderr, exitcode.UpstreamErr,
				fmt.Errorf("ci-triage: PR #%d not found: %w", prNum, err))
		}
		return wrapErr(stderr, exitcode.UpstreamErr, fmt.Errorf("ci-triage: fetch checks: %w", err))
	}

	failed := filterFailed(checks)
	if *workflowFilter != "" {
		failed = filterByWorkflow(failed, *workflowFilter)
	}

	if len(failed) == 0 {
		fmt.Fprintf(stdout, "PR #%d CI failures:\n\n  (none)\n", prNum)
		return nil
	}

	prCommits, err := d.prCommits(*repoFlag, prNum)
	if err != nil {
		// PR commit set is only used to attribute test-file ownership; failure
		// to fetch is non-fatal — fall through with empty set so attribution
		// becomes "unknown" rather than killing the whole triage.
		fmt.Fprintf(stderr, "ci-triage: warning: could not list PR commits: %v\n", err)
		prCommits = map[string]bool{}
	}

	groups := groupByWorkflow(failed)
	var perWFs []PerWorkflow
	anyRegression := false
	for _, g := range groups {
		pw := analyzeWorkflow(*repoFlag, *mainBranch, g, *samples, prCommits, d)
		perWFs = append(perWFs, pw)
		if pw.Verdict == VerdictLikelyRegression {
			anyRegression = true
		}
	}

	writeReport(stdout, prNum, perWFs)

	if anyRegression {
		return exitcode.Wrap(exitcode.VerifyFailed, errors.New("ci-triage: at least one LIKELY REGRESSION verdict"))
	}
	return nil
}

// --- analysis ---------------------------------------------------------------

// analyzeWorkflow turns one failing workflow's PR-side checks into a fully
// populated PerWorkflow, doing the main-rate sampling, log extraction, and
// verdict logic.
func analyzeWorkflow(repo, mainBranch string, g groupedChecks, samples int, prCommits map[string]bool, d deps) PerWorkflow {
	pw := PerWorkflow{
		Workflow: g.Workflow,
		PRJobs:   g.Jobs,
	}

	mains, err := d.mainRuns(repo, mainBranch, g.Workflow, samples)
	if err == nil {
		pw.MainSamples = mains
		pw.FailRateNum, pw.FailRateDen = countFails(mains)
	}

	// Try to extract a failing test file from the first failing PR run's log.
	// Pick the first PRJob whose Bucket=="fail" and walk back to its run id —
	// `gh pr checks` does not expose run id directly, so we re-fetch by
	// workflow name and look for the most recent failure on the PR head sha.
	// The simplest path is: try mainRuns' first FAIL run id, OR rely on the
	// gh adapter passing through run-id when available. The current `gh pr
	// checks --json` schema does include `link` (URL with run id).
	for _, j := range g.Jobs {
		runID := extractRunIDFromCheck(j)
		if runID == 0 {
			continue
		}
		log, err := d.runLog(repo, runID)
		if err != nil {
			continue
		}
		if testFile, _, ok := logparse.FirstMatch(log); ok {
			pw.TestFile = testFile
			pw.TestFileFound = true
			break
		}
	}

	if pw.TestFileFound {
		sha, err := d.gitLog(pw.TestFile)
		if err == nil && sha != "" {
			pw.LastTouchSHA = sha
			pw.InThisPR = prCommits[sha]
		}
	}

	pw.Verdict, pw.GreyReason = decideVerdict(pw.FailRateNum, pw.FailRateDen, pw.TestFileFound, pw.InThisPR)
	return pw
}

// decideVerdict implements the §A3 rule table.
//
//	main rate = 0/N           → LIKELY REGRESSION
//	main rate ≥ 1/5 AND test file outside this PR (or unknown)
//	                          → PRE-EXISTING FLAKY
//	otherwise                 → GREY ZONE
//
// "≥ 1/5" is interpreted as `num >= 1 && den >= 5` per the spec wording.
func decideVerdict(num, den int, fileFound, inThisPR bool) (Verdict, string) {
	if den == 0 {
		return VerdictGrey, "no main runs sampled"
	}
	if num == 0 {
		return VerdictLikelyRegression, ""
	}
	flakyMainRate := num >= 1 && den >= 5
	if flakyMainRate && (!fileFound || !inThisPR) {
		return VerdictPreExistingFlaky, ""
	}
	switch {
	case num >= 1 && den < 5:
		return VerdictGrey, fmt.Sprintf("main sampled %d runs (<5); cannot confirm flaky-rate threshold", den)
	case fileFound && inThisPR:
		return VerdictGrey, "main is mixed AND test file last-touched in this PR"
	default:
		return VerdictGrey, "indeterminate signal"
	}
}

// --- grouping ---------------------------------------------------------------

type groupedChecks struct {
	Workflow string
	Jobs     []FailedCheck
}

func filterFailed(checks []FailedCheck) []FailedCheck {
	var out []FailedCheck
	for _, c := range checks {
		if isFailure(c) {
			out = append(out, c)
		}
	}
	return out
}

func isFailure(c FailedCheck) bool {
	if strings.EqualFold(c.Bucket, "fail") {
		return true
	}
	// gh `pr checks --json state` carries values like "FAILURE", "SUCCESS",
	// "TIMED_OUT", "CANCELLED" — same vocabulary as GitHub Actions.
	switch strings.ToLower(c.State) {
	case "failure", "timed_out", "cancelled", "action_required":
		return true
	}
	return false
}

func filterByWorkflow(checks []FailedCheck, wf string) []FailedCheck {
	var out []FailedCheck
	for _, c := range checks {
		if c.Workflow == wf {
			out = append(out, c)
		}
	}
	return out
}

func groupByWorkflow(checks []FailedCheck) []groupedChecks {
	idx := map[string]int{}
	var out []groupedChecks
	for _, c := range checks {
		wf := c.Workflow
		if wf == "" {
			wf = "(unknown)"
		}
		if i, ok := idx[wf]; ok {
			out[i].Jobs = append(out[i].Jobs, c)
			continue
		}
		idx[wf] = len(out)
		out = append(out, groupedChecks{Workflow: wf, Jobs: []FailedCheck{c}})
	}
	return out
}

func countFails(mains []MainRun) (num, den int) {
	for _, r := range mains {
		den++
		if isMainFail(r.Conclusion) {
			num++
		}
	}
	return num, den
}

func isMainFail(s string) bool {
	switch strings.ToLower(s) {
	case "failure", "timed_out", "cancelled", "action_required":
		return true
	}
	return false
}

// --- output -----------------------------------------------------------------

func writeReport(w io.Writer, pr int, perWFs []PerWorkflow) {
	fmt.Fprintf(w, "PR #%d CI failures:\n\n", pr)
	for _, pw := range perWFs {
		fmt.Fprintf(w, "  workflow: %s\n", pw.Workflow)

		// PR result line — show first job's display name in parens for context.
		jobDetail := ""
		if len(pw.PRJobs) >= 1 && pw.PRJobs[0].Name != "" {
			jobDetail = fmt.Sprintf(" (job: %s)", pw.PRJobs[0].Name)
		}
		fmt.Fprintf(w, "    PR result:        FAIL%s\n", jobDetail)

		// main last N runs
		if pw.FailRateDen > 0 {
			pct := pw.FailRateNum * 100 / pw.FailRateDen
			line := fmt.Sprintf("    main last %d runs: %d FAIL / %d PASS",
				pw.FailRateDen, pw.FailRateNum, pw.FailRateDen-pw.FailRateNum)
			if pw.FailRateNum > 0 {
				line += fmt.Sprintf("  ← %d%% flaky", pct)
			}
			fmt.Fprintln(w, line)
		} else {
			fmt.Fprintln(w, "    main last 0 runs: (no main samples available)")
		}

		// test file (only when extracted)
		if pw.TestFileFound {
			loc := "not in this PR"
			if pw.InThisPR {
				loc = "in this PR"
			}
			fmt.Fprintf(w, "    test file:        %s (last touched: %s, %s)\n",
				pw.TestFile, shortSHA(pw.LastTouchSHA), loc)
		} else {
			fmt.Fprintln(w, "    test file:        (unable to extract test file from log)")
		}

		// verdict
		fmt.Fprintf(w, "    verdict:          %s\n", pw.Verdict.String())

		// evidence (specific to FLAKY)
		switch pw.Verdict {
		case VerdictPreExistingFlaky:
			fmt.Fprintln(w, "    evidence:         document in PR thread before retry")
		case VerdictGrey:
			if pw.GreyReason != "" {
				fmt.Fprintf(w, "    evidence:         %s\n", pw.GreyReason)
			}
		}

		fmt.Fprintln(w)
	}
}

// --- API helpers ------------------------------------------------------------

func resolvePRFromBranch(repo string) (int, error) {
	args := []string{"pr", "view", "--json", "number"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, fmt.Errorf("parse pr view: %w", err)
	}
	if resp.Number == 0 {
		return 0, errors.New("PR number not in response")
	}
	return resp.Number, nil
}

func fetchPRChecks(repo string, pr int) ([]FailedCheck, error) {
	args := []string{"pr", "checks", strconv.Itoa(pr),
		"--json", "name,workflow,state,bucket,link"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return nil, err
	}
	// `gh pr checks` returns a JSON array directly.
	var raw []FailedCheck
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse pr checks: %w", err)
	}
	return raw, nil
}

// extractRunIDFromCheck pulls the workflow run id out of the check's link URL,
// e.g. https://github.com/owner/repo/actions/runs/12345 → 12345. Returns 0
// when no run id can be parsed (e.g. for non-Actions checks).
func extractRunIDFromCheck(c FailedCheck) int64 {
	link := c.Link
	idx := strings.LastIndex(link, "/runs/")
	if idx < 0 {
		return 0
	}
	rest := link[idx+len("/runs/"):]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func fetchMainRuns(repo, branch, workflow string, samples int) ([]MainRun, error) {
	if workflow == "" {
		return nil, errors.New("workflow name is empty")
	}
	// `gh run list --workflow=` accepts the workflow display name OR the file
	// basename — passing through verbatim works for both shapes that
	// `gh pr checks --json workflow` emits.
	args := []string{"run", "list",
		"--workflow=" + workflow,
		"--branch=" + branch,
		"--limit=" + strconv.Itoa(samples),
		"--json", "conclusion,databaseId"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return nil, err
	}
	var resp []MainRun
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse run list: %w", err)
	}
	return resp, nil
}

func fetchRunLog(repo string, runID int64) ([]byte, error) {
	args := []string{"run", "view", strconv.FormatInt(runID, 10), "--log"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func fetchPRCommits(repo string, pr int) (map[string]bool, error) {
	args := []string{"pr", "view", strconv.Itoa(pr), "--json", "commits"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := ghclient.RunGh(args...)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Commits []struct {
			OID string `json:"oid"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse pr view commits: %w", err)
	}
	set := make(map[string]bool, len(resp.Commits))
	for _, c := range resp.Commits {
		set[c.OID] = true
		if len(c.OID) >= 7 {
			set[c.OID[:7]] = true
		}
	}
	return set, nil
}

func gitLogLastTouched(file string) (string, error) {
	out, err := exec.Command("git", "log", "-1", "--format=%H", "--", file).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// --- helpers ----------------------------------------------------------------

func wrapErr(stderr io.Writer, code int, err error) error {
	if err != nil && code != exitcode.Success {
		fmt.Fprintln(stderr, err)
	}
	return exitcode.Wrap(code, err)
}

func parseNum(s string) (int, error) {
	s = strings.TrimPrefix(s, "#")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid PR number %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("PR number must be positive, got %d", n)
	}
	return n, nil
}

func shortSHA(s string) string {
	if len(s) >= 7 {
		return s[:7]
	}
	return s
}

// reorderFlags pulls flag tokens (and their values for the named value-taking
// flags) ahead of any positional arg, so the stdlib `flag` package — which
// stops parsing at the first positional — accepts the natural
// `<positional> --flag value` ordering.
func reorderFlags(args []string, valueFlags []string) []string {
	takesValue := map[string]bool{}
	for _, name := range valueFlags {
		takesValue["-"+name] = true
		takesValue["--"+name] = true
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			noValue := strings.Contains(a, "=")
			if takesValue[a] && !noValue && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}
