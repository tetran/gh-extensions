package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetran/gh-extensions/internal/exitcode"
)

func fixedNow() time.Time {
	return time.Date(2026, 5, 4, 14, 23, 0, 0, time.UTC)
}

// --- Verdict logic ----------------------------------------------------------

func TestDecideVerdict(t *testing.T) {
	cases := []struct {
		name             string
		num, den         int
		fileFound, inPR  bool
		want             Verdict
		wantReasonHasSub string
	}{
		// Spec: main 失敗率 = 0/N → LIKELY REGRESSION
		{"main green N=8", 0, 8, true, true, VerdictLikelyRegression, ""},
		{"main green N=8 no test file", 0, 8, false, false, VerdictLikelyRegression, ""},

		// Spec: main 失敗率 ≥ 1/5 AND test file outside PR → PRE-EXISTING FLAKY
		{"main 2/8 + file outside PR", 2, 8, true, false, VerdictPreExistingFlaky, ""},
		{"main 1/5 boundary + no test file", 1, 5, false, false, VerdictPreExistingFlaky, ""},
		{"main 4/8 + file outside PR", 4, 8, true, false, VerdictPreExistingFlaky, ""},

		// Boundary: 1/4 (den < 5) → grey-zone with explicit reason
		{"main 1/4 below threshold den", 1, 4, true, false, VerdictGrey, "<5"},

		// Mixed signal: file IS in this PR → grey-zone (regression suspected but flaky)
		{"main flaky AND file in PR", 2, 8, true, true, VerdictGrey, "last-touched in this PR"},

		// No samples → grey
		{"no main samples", 0, 0, false, false, VerdictGrey, "no main runs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := decideVerdict(tc.num, tc.den, tc.fileFound, tc.inPR)
			if got != tc.want {
				t.Errorf("verdict = %v, want %v (reason=%q)", got, tc.want, reason)
			}
			if tc.wantReasonHasSub != "" && !strings.Contains(reason, tc.wantReasonHasSub) {
				t.Errorf("reason missing %q: %q", tc.wantReasonHasSub, reason)
			}
		})
	}
}

// --- Failure / grouping helpers --------------------------------------------

func TestIsFailure(t *testing.T) {
	cases := []struct {
		c    FailedCheck
		want bool
	}{
		{FailedCheck{Bucket: "fail"}, true},
		{FailedCheck{State: "FAILURE"}, true},
		{FailedCheck{State: "TIMED_OUT"}, true},
		{FailedCheck{State: "CANCELLED"}, true},
		{FailedCheck{State: "ACTION_REQUIRED"}, true},
		{FailedCheck{State: "SUCCESS"}, false},
		{FailedCheck{State: "SKIPPED", Bucket: "skipping"}, false},
		{FailedCheck{State: "PENDING"}, false},
	}
	for _, tc := range cases {
		if got := isFailure(tc.c); got != tc.want {
			t.Errorf("isFailure(%+v) = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestExtractRunIDFromCheck(t *testing.T) {
	cases := []struct {
		link string
		want int64
	}{
		{"https://github.com/owner/repo/actions/runs/12345/job/9999", 12345},
		{"https://github.com/owner/repo/actions/runs/12345", 12345},
		{"https://github.com/owner/repo/actions/runs/12345?attempt=2", 12345},
		{"https://example.com/no/runs/here", 0},
		{"", 0},
	}
	for _, tc := range cases {
		got := extractRunIDFromCheck(FailedCheck{Link: tc.link})
		if got != tc.want {
			t.Errorf("extract(%q) = %d, want %d", tc.link, got, tc.want)
		}
	}
}

func TestGroupByWorkflow(t *testing.T) {
	checks := []FailedCheck{
		{Workflow: "wf1", Name: "a"},
		{Workflow: "wf2", Name: "b"},
		{Workflow: "wf1", Name: "c"},
	}
	g := groupByWorkflow(checks)
	if len(g) != 2 {
		t.Fatalf("got %d groups, want 2", len(g))
	}
	if g[0].Workflow != "wf1" || len(g[0].Jobs) != 2 {
		t.Errorf("group 0 wrong: %+v", g[0])
	}
	if g[1].Workflow != "wf2" || len(g[1].Jobs) != 1 {
		t.Errorf("group 1 wrong: %+v", g[1])
	}
}

// --- analyzeWorkflow with stubs ---------------------------------------------

func loadChecks(t *testing.T) []FailedCheck {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "A3", "pr_checks_mixed.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var raw []FailedCheck
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return raw
}

func loadMain(t *testing.T, name string) []MainRun {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "A3", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var resp []MainRun
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return resp
}

func loadLog(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "A3", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// stubDeps lets each test set up the four IO funcs in one place.
type stubArgs struct {
	resolvePR func(string) (int, error)
	mainRuns  func(repo, branch, wf string, n int) ([]MainRun, error)
	runLog    func(repo string, runID int64) ([]byte, error)
	prCommits func(repo string, pr int) (map[string]bool, error)
	gitLog    func(file string) (string, error)
	prChecks  func(repo string, pr int) ([]FailedCheck, error)
}

func makeDeps(s stubArgs) deps {
	d := deps{now: fixedNow}
	if s.resolvePR != nil {
		d.resolvePR = s.resolvePR
	} else {
		d.resolvePR = func(string) (int, error) { return 0, errors.New("not stubbed") }
	}
	if s.mainRuns != nil {
		d.mainRuns = s.mainRuns
	} else {
		d.mainRuns = func(string, string, string, int) ([]MainRun, error) { return nil, nil }
	}
	if s.runLog != nil {
		d.runLog = s.runLog
	} else {
		d.runLog = func(string, int64) ([]byte, error) { return nil, errors.New("no log") }
	}
	if s.prCommits != nil {
		d.prCommits = s.prCommits
	} else {
		d.prCommits = func(string, int) (map[string]bool, error) { return map[string]bool{}, nil }
	}
	if s.gitLog != nil {
		d.gitLog = s.gitLog
	} else {
		d.gitLog = func(string) (string, error) { return "", errors.New("no git") }
	}
	if s.prChecks != nil {
		d.prChecks = s.prChecks
	} else {
		d.prChecks = func(string, int) ([]FailedCheck, error) { return nil, errors.New("no checks") }
	}
	return d
}

func TestRun_FlakyAndRegressionVerbatim(t *testing.T) {
	checks := loadChecks(t)
	flaky := loadMain(t, "main_runs_flaky.json")
	green := loadMain(t, "main_runs_green.json")
	pytestLog := loadLog(t, "log_pytest_failure.txt")
	unmatchedLog := loadLog(t, "log_unmatched.txt")

	d := makeDeps(stubArgs{
		prChecks: func(_ string, pr int) ([]FailedCheck, error) { return checks, nil },
		mainRuns: func(_, _, wf string, n int) ([]MainRun, error) {
			switch wf {
			case "pytest.yml":
				return flaky, nil
			case "lint.yml":
				return green, nil
			}
			return nil, nil
		},
		runLog: func(_ string, runID int64) ([]byte, error) {
			if runID == 1001 {
				return pytestLog, nil
			}
			return unmatchedLog, nil
		},
		prCommits: func(string, int) (map[string]bool, error) {
			return map[string]bool{"a443a31": true}, nil
		},
		gitLog: func(file string) (string, error) {
			if file == "tests/test_archive.py" {
				// last-touched outside the PR
				return "deadbeef00000000000000000000000000000000", nil
			}
			return "", errors.New("no git")
		},
	})

	var stdout, stderr bytes.Buffer
	err := run([]string{"103"}, &stdout, &stderr, d)
	// pytest.yml → PRE-EXISTING FLAKY (non-zero main fail rate, file outside PR)
	// lint.yml → LIKELY REGRESSION (main green) → triggers exit 1
	var ec *exitcode.Error
	if !errors.As(err, &ec) || ec.Code != exitcode.VerifyFailed {
		t.Fatalf("expected VerifyFailed (1), got %v / stderr=%s", err, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"PR #103 CI failures:",
		"  workflow: pytest.yml",
		"    PR result:        FAIL (job: pytest (Ubuntu, py3.11))",
		"    main last 8 runs: 2 FAIL / 6 PASS  ← 25% flaky",
		"    test file:        tests/test_archive.py (last touched: deadbee, not in this PR)",
		"    verdict:          PRE-EXISTING FLAKY",
		"    evidence:         document in PR thread before retry",
		"  workflow: lint.yml",
		"    PR result:        FAIL (job: lint)",
		"    main last 8 runs: 0 FAIL / 8 PASS",
		"    verdict:          LIKELY REGRESSION (caused by this PR)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing line:\n  want: %q\n  full output:\n%s", want, out)
		}
	}
}

func TestRun_NoFailuresExitZero(t *testing.T) {
	d := makeDeps(stubArgs{
		prChecks: func(string, int) ([]FailedCheck, error) {
			return []FailedCheck{{Workflow: "build.yml", State: "SUCCESS", Bucket: "pass"}}, nil
		},
	})
	var stdout, stderr bytes.Buffer
	if err := run([]string{"42"}, &stdout, &stderr, d); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if !strings.Contains(stdout.String(), "(none)") {
		t.Errorf("expected '(none)' in output, got: %s", stdout.String())
	}
}

func TestRun_LogUnmatched_FallsBackButContinues(t *testing.T) {
	// Ensure that a log we cannot parse still produces a verdict (uses main
	// fail-rate alone). Spec: graceful degradation.
	checks := []FailedCheck{
		{Workflow: "lint.yml", Name: "lint", State: "FAILURE", Bucket: "fail",
			Link: "https://github.com/o/r/actions/runs/777/job/1"},
	}
	d := makeDeps(stubArgs{
		prChecks: func(string, int) ([]FailedCheck, error) { return checks, nil },
		mainRuns: func(string, string, string, int) ([]MainRun, error) {
			return loadMain(t, "main_runs_flaky.json"), nil
		},
		runLog: func(string, int64) ([]byte, error) { return loadLog(t, "log_unmatched.txt"), nil },
	})
	var stdout, stderr bytes.Buffer
	err := run([]string{"55"}, &stdout, &stderr, d)
	// pytest log adapter cannot extract → file unknown → flaky branch fires
	// (file-unknown counts as "not in PR" for the flaky rule).
	if err != nil {
		t.Fatalf("expected nil err with flaky+unknown-file → flaky verdict, got %v / stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "(unable to extract test file from log)") {
		t.Errorf("expected fallback line, got: %s", out)
	}
	if !strings.Contains(out, "PRE-EXISTING FLAKY") {
		t.Errorf("expected PRE-EXISTING FLAKY verdict (file-unknown should still allow flaky judgment): %s", out)
	}
}

func TestRun_UsageErrors(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"non-numeric PR", []string{"abc"}, exitcode.UsageError, "invalid PR"},
		{"too many positional", []string{"1", "2"}, exitcode.UsageError, "at most one positional"},
		{"bad samples", []string{"1", "--samples", "0"}, exitcode.UsageError, "samples must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := makeDeps(stubArgs{
				prChecks: func(string, int) ([]FailedCheck, error) { return nil, nil },
			})
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr, d)
			var ec *exitcode.Error
			if !errors.As(err, &ec) || ec.Code != tc.wantCode {
				t.Fatalf("expected exit %d, got %v", tc.wantCode, err)
			}
			if !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("stderr missing %q: %s", tc.wantErr, stderr.String())
			}
		})
	}
}

func TestRun_PRNotFound(t *testing.T) {
	d := makeDeps(stubArgs{
		prChecks: func(string, int) ([]FailedCheck, error) {
			// emulate what IsNotFound recognizes: a GhExecError with a 404 stderr
			// We can't directly construct a real one, but we can wrap a message.
			return nil, errors.New("HTTP 404: Not Found")
		},
	})
	var stdout, stderr bytes.Buffer
	err := run([]string{"99"}, &stdout, &stderr, d)
	var ec *exitcode.Error
	if !errors.As(err, &ec) || ec.Code != exitcode.UpstreamErr {
		t.Fatalf("expected UpstreamErr (3), got %v", err)
	}
}

// --- Workflow filter --------------------------------------------------------

func TestRun_WorkflowFilter(t *testing.T) {
	checks := loadChecks(t)
	d := makeDeps(stubArgs{
		prChecks: func(string, int) ([]FailedCheck, error) { return checks, nil },
		mainRuns: func(string, string, string, int) ([]MainRun, error) { return nil, nil },
	})
	var stdout, stderr bytes.Buffer
	if err := run([]string{"103", "--workflow", "lint.yml"}, &stdout, &stderr, d); err != nil {
		// no main samples means grey, not regression -> exit 0
		t.Fatalf("unexpected err: %v", err)
	}
	out := stdout.String()
	if strings.Contains(out, "pytest.yml") {
		t.Errorf("--workflow filter should drop pytest.yml, got:\n%s", out)
	}
	if !strings.Contains(out, "lint.yml") {
		t.Errorf("expected lint.yml in output: %s", out)
	}
}

// --- Reorder flags ----------------------------------------------------------

func TestReorderFlags(t *testing.T) {
	value := []string{"workflow", "samples", "repo"}
	cases := []struct {
		in, want []string
	}{
		{
			in:   []string{"103", "--workflow", "lint.yml"},
			want: []string{"--workflow", "lint.yml", "103"},
		},
		{
			in:   []string{"103", "--samples=4", "--repo", "o/r"},
			want: []string{"--samples=4", "--repo", "o/r", "103"},
		},
		{
			in:   []string{"--workflow", "lint.yml", "103"},
			want: []string{"--workflow", "lint.yml", "103"},
		},
	}
	for _, tc := range cases {
		got := reorderFlags(tc.in, value)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("reorderFlags(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
