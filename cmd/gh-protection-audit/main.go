// Command gh-protection-audit detects silent BLOCKED states caused by drift
// between branch-protection required contexts and actual check-run names.
//
// Usage: gh protection-audit [--branch main] [--ref HEAD] [--repo OWNER/NAME]
//
// It calls:
//
//	GET repos/{owner}/{repo}/branches/<branch>/protection/required_status_checks
//	GET repos/{owner}/{repo}/commits/<ref>/check-runs
//
// then computes the bidirectional set diff. For each required context with no
// matching check-run, it applies two heuristics drawn from §A1 of the source
// spec:
//
//   - '#' truncation: a check-run name has '#' AND the required context is
//     a prefix of it. GitHub silently truncates '#' in job display names.
//   - matrix display: a required context 'foo' has check-run names like
//     'foo (3.10)' / 'foo (3.11)' (matrix-rendered variants).
//
// Exit 0 when nothing is missing on the required side; exit 1 otherwise.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/tetran/gh-extensions/internal/exitcode"
	"github.com/tetran/gh-extensions/internal/ghclient"
)

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

// deps bundles the IO-touching helpers so tests can inject fixtures.
type deps struct {
	getProtection func(repo Repo, branch string) ([]string, error)
	getCheckRuns  func(repo Repo, ref string) ([]string, error)
	resolveRepo   func() (Repo, error)
	resolveRef    func() (string, error)
	now           func() time.Time
}

// Repo identifies a GitHub repo without losing the host (GHES support).
type Repo struct {
	Host  string
	Owner string
	Name  string
}

func defaultDeps() deps {
	return deps{
		getProtection: fetchRequiredContexts,
		getCheckRuns:  fetchCheckRunNames,
		resolveRepo:   currentRepo,
		resolveRef:    headSHA,
		now:           time.Now,
	}
}

func run(args []string, stdout, stderr io.Writer, d deps) error {
	fs := flag.NewFlagSet("gh-protection-audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	branch := fs.String("branch", "main", "branch whose protection rule to audit")
	ref := fs.String("ref", "HEAD", "commit ref (sha or HEAD) whose check-runs to compare")
	repoFlag := fs.String("repo", "", "GitHub repository in OWNER/NAME form (default: detect from git remote)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: gh protection-audit [--branch main] [--ref HEAD] [--repo OWNER/NAME]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return wrapErr(stderr, exitcode.UsageError, err)
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return wrapErr(stderr, exitcode.UsageError,
			fmt.Errorf("protection-audit: takes no positional args, got %d", fs.NArg()))
	}

	repo, err := resolveRepoFromFlag(*repoFlag, d.resolveRepo)
	if err != nil {
		return wrapErr(stderr, exitcode.UpstreamErr, fmt.Errorf("protection-audit: resolve repo: %w", err))
	}

	resolvedRef := *ref
	if resolvedRef == "HEAD" {
		sha, err := d.resolveRef()
		if err != nil {
			return wrapErr(stderr, exitcode.UpstreamErr, fmt.Errorf("protection-audit: resolve HEAD: %w", err))
		}
		resolvedRef = sha
	}

	required, err := d.getProtection(repo, *branch)
	if err != nil {
		if ghclient.IsNotFound(err) {
			return wrapErr(stderr, exitcode.UpstreamErr,
				fmt.Errorf("protection-audit: branch %q has no protection or does not exist: %w", *branch, err))
		}
		return wrapErr(stderr, exitcode.UpstreamErr, fmt.Errorf("protection-audit: fetch protection: %w", err))
	}
	runs, err := d.getCheckRuns(repo, resolvedRef)
	if err != nil {
		if ghclient.IsNotFound(err) {
			return wrapErr(stderr, exitcode.UpstreamErr,
				fmt.Errorf("protection-audit: ref %q not found: %w", resolvedRef, err))
		}
		return wrapErr(stderr, exitcode.UpstreamErr, fmt.Errorf("protection-audit: fetch check-runs: %w", err))
	}

	report := buildReport(required, runs)
	writeReport(stdout, report, d.now())

	if len(report.MissingRequired) > 0 {
		return exitcode.Wrap(exitcode.VerifyFailed,
			errors.New("protection-audit: required contexts have no matching check-run"))
	}
	return nil
}

// wrapErr writes the message to stderr and wraps it with the exit code.
func wrapErr(stderr io.Writer, code int, err error) error {
	if err != nil && code != exitcode.Success {
		fmt.Fprintln(stderr, err)
	}
	return exitcode.Wrap(code, err)
}

// MissingItem is one required context with no matching check-run.
type MissingItem struct {
	Name string
	Note string // suffix shown after the arrow, e.g. "suspect: ..." or empty
}

// InfoItem is one check-run that is not pinned by branch protection.
type InfoItem struct {
	Name string
	Note string
}

// Report holds the final diff for rendering.
type Report struct {
	MissingRequired   []MissingItem // required \ runs
	UnpinnedCheckRuns []InfoItem    // runs \ required
}

// buildReport computes the bidirectional diff and applies the two heuristics
// to every missing required context.
func buildReport(required, runs []string) Report {
	requiredSet := toSet(required)
	runsSet := toSet(runs)

	var rep Report

	// Missing on the required side (what protection wants, but no run produced).
	for _, name := range sortedKeys(requiredSet) {
		if runsSet[name] {
			continue
		}
		note := heuristicForMissing(name, runs)
		rep.MissingRequired = append(rep.MissingRequired, MissingItem{Name: name, Note: note})
	}

	// Informational: run-side names that are not pinned by protection.
	for _, name := range sortedKeys(runsSet) {
		if requiredSet[name] {
			continue
		}
		rep.UnpinnedCheckRuns = append(rep.UnpinnedCheckRuns, InfoItem{Name: name, Note: "informational only"})
	}
	return rep
}

// heuristicForMissing returns the suspect-note for a required context that
// has no exact match. Empty string means "no obvious explanation".
func heuristicForMissing(required string, runs []string) string {
	if note := detectHashTruncation(required, runs); note != "" {
		return note
	}
	if note := detectMatrixDisplay(required, runs); note != "" {
		return note
	}
	return ""
}

// detectHashTruncation fires when a check-run name contains '#' and the
// required context is a prefix of it (after trimming the optional trailing
// space that often appears in the truncated form).
func detectHashTruncation(required string, runs []string) string {
	trimmed := strings.TrimRight(required, " ")
	for _, r := range runs {
		if !strings.Contains(r, "#") {
			continue
		}
		if strings.HasPrefix(r, trimmed) {
			return "suspect: '#' truncation in source job name"
		}
	}
	return ""
}

// detectMatrixDisplay fires when there are check-run names of the form
// `<required> (<variant>)` — i.e., matrix-rendered job names. The note lists
// up to two of the variants seen, mirroring the §A1 spec example.
func detectMatrixDisplay(required string, runs []string) string {
	prefix := required + " ("
	var variants []string
	for _, r := range runs {
		if strings.HasPrefix(r, prefix) && strings.HasSuffix(r, ")") {
			variants = append(variants, r)
		}
	}
	if len(variants) == 0 {
		return ""
	}
	sort.Strings(variants)
	if len(variants) == 1 {
		return fmt.Sprintf("suspect: matrix job — actual was '%s'", variants[0])
	}
	return fmt.Sprintf("suspect: matrix job — actual was '%s' / '%s'", variants[0], variants[1])
}

// nameColumnWidth is the column at which the `←` arrow lands. Chosen so the
// §A1 example (`pytest (Ubuntu, python alias absent — Issue` ~44 chars +
// 2-space indent) sits at the column boundary with one space before the arrow.
const nameColumnWidth = 47

func writeReport(w io.Writer, rep Report, now time.Time) {
	fmt.Fprintln(w, "=== Required contexts NOT matched by recent check-runs ===")
	if len(rep.MissingRequired) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for _, m := range rep.MissingRequired {
			writeAnnotated(w, m.Name, m.Note)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "=== Recent check-runs NOT in required contexts ===")
	if len(rep.UnpinnedCheckRuns) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for _, i := range rep.UnpinnedCheckRuns {
			writeAnnotated(w, i.Name, i.Note)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "=== Last verified ===")
	fmt.Fprintln(w, "  "+now.Format("2006-01-02T15:04Z07:00"))
}

// writeAnnotated emits one bullet line. When note is empty no arrow is drawn.
// When note is non-empty the arrow is right-padded to nameColumnWidth.
func writeAnnotated(w io.Writer, name, note string) {
	if note == "" {
		fmt.Fprintln(w, "  "+name)
		return
	}
	body := "  " + name
	if len(body) < nameColumnWidth {
		body += strings.Repeat(" ", nameColumnWidth-len(body))
	} else {
		body += "  "
	}
	fmt.Fprintln(w, body+"← "+note)
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveRepoFromFlag turns "owner/name" (with optional host prefix) or the
// empty string (use current clone) into a Repo.
func resolveRepoFromFlag(flag string, currentRepo func() (Repo, error)) (Repo, error) {
	if flag == "" {
		return currentRepo()
	}
	parts := strings.SplitN(flag, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Repo{}, fmt.Errorf("invalid --repo %q, expected OWNER/NAME", flag)
	}
	return Repo{Host: "github.com", Owner: parts[0], Name: parts[1]}, nil
}

func currentRepo() (Repo, error) {
	r, err := ghclient.CurrentRepo()
	if err != nil {
		return Repo{}, err
	}
	return Repo{Host: r.Host, Owner: r.Owner, Name: r.Name}, nil
}

func headSHA() (string, error) {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD failed (run from inside a clone): %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// --- API calls ---------------------------------------------------------------

type protectionResp struct {
	Contexts []string `json:"contexts"`
	Checks   []struct {
		Context string `json:"context"`
	} `json:"checks"`
}

type checkRunsResp struct {
	CheckRuns []struct {
		Name string `json:"name"`
	} `json:"check_runs"`
}

func fetchRequiredContexts(repo Repo, branch string) ([]string, error) {
	c, err := ghclient.NewREST()
	if err != nil {
		return nil, err
	}
	var resp protectionResp
	path := fmt.Sprintf("repos/%s/%s/branches/%s/protection/required_status_checks", repo.Owner, repo.Name, branch)
	if err := c.Get(path, &resp); err != nil {
		return nil, err
	}
	// The API returns both `contexts` (string list) and `checks` (objects with
	// `context`). Modern repos populate `checks`; legacy ones use `contexts`.
	// Merge, dedup, and sort for deterministic output.
	seen := make(map[string]bool)
	for _, n := range resp.Contexts {
		seen[n] = true
	}
	for _, c := range resp.Checks {
		seen[c.Context] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func fetchCheckRunNames(repo Repo, ref string) ([]string, error) {
	c, err := ghclient.NewREST()
	if err != nil {
		return nil, err
	}
	var resp checkRunsResp
	path := fmt.Sprintf("repos/%s/%s/commits/%s/check-runs?per_page=100", repo.Owner, repo.Name, ref)
	if err := c.Get(path, &resp); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, r := range resp.CheckRuns {
		seen[r.Name] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// parsePayload helpers for tests --------------------------------------------

// parseProtectionPayload exposes the same JSON-shape parsing the REST path
// uses, so fixture tests can drive buildReport from a real API response shape.
func parseProtectionPayload(b []byte) ([]string, error) {
	var resp protectionResp
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, n := range resp.Contexts {
		seen[n] = true
	}
	for _, c := range resp.Checks {
		seen[c.Context] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func parseCheckRunsPayload(b []byte) ([]string, error) {
	var resp checkRunsResp
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, r := range resp.CheckRuns {
		seen[r.Name] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
