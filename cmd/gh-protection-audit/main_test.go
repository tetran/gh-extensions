package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tetran/gh-extensions/internal/exitcode"
)

func fixedTime() time.Time {
	loc := time.FixedZone("JST", 9*60*60)
	return time.Date(2026, 5, 4, 14, 23, 0, 0, loc)
}

// --- Heuristic table tests --------------------------------------------------

func TestDetectHashTruncation(t *testing.T) {
	cases := []struct {
		name     string
		required string
		runs     []string
		want     bool
	}{
		{
			name:     "trailing-space truncation matches run with #",
			required: "pytest (Ubuntu, python alias absent — Issue ",
			runs:     []string{"pytest (Ubuntu, python alias absent — Issue #33 regression guard)"},
			want:     true,
		},
		{
			name:     "no run contains #",
			required: "build",
			runs:     []string{"build (3.10)", "build (3.11)"},
			want:     false,
		},
		{
			name:     "run has # but is unrelated",
			required: "build",
			runs:     []string{"docs #README"},
			want:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectHashTruncation(tc.required, tc.runs) != ""
			if got != tc.want {
				t.Errorf("detectHashTruncation(%q, %v) = %v, want %v", tc.required, tc.runs, got, tc.want)
			}
		})
	}
}

func TestDetectMatrixDisplay(t *testing.T) {
	cases := []struct {
		name     string
		required string
		runs     []string
		want     string
	}{
		{
			name:     "two matrix variants -> both listed",
			required: "build",
			runs:     []string{"build (3.10)", "build (3.11)", "build (3.12)"},
			want:     "suspect: matrix job — actual was 'build (3.10)' / 'build (3.11)'",
		},
		{
			name:     "single variant -> 'actual was X' (no slash)",
			required: "test",
			runs:     []string{"test (linux)"},
			want:     "suspect: matrix job — actual was 'test (linux)'",
		},
		{
			name:     "no parens -> no match",
			required: "build",
			runs:     []string{"buildx", "build-fast"},
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectMatrixDisplay(tc.required, tc.runs)
			if got != tc.want {
				t.Errorf("detectMatrixDisplay(%q, %v) =\n  got=%q\n want=%q", tc.required, tc.runs, got, tc.want)
			}
		})
	}
}

func TestHeuristicForMissing_PreferenceOrder(t *testing.T) {
	// '#' truncation wins over matrix-display when both could match.
	required := "pytest "
	runs := []string{
		"pytest (linux)",
		"pytest #issue42",
	}
	got := heuristicForMissing(required, runs)
	if !strings.HasPrefix(got, "suspect: '#' truncation") {
		t.Errorf("expected '#' truncation to win, got %q", got)
	}
}

func TestHeuristicForMissing_PureRename_NoNote(t *testing.T) {
	// Required name has no truncation cues, no matrix variants — return
	// empty so caller draws the line with no `← suspect:` annotation.
	got := heuristicForMissing("lint-old", []string{"lint", "build"})
	if got != "" {
		t.Errorf("expected empty heuristic for pure rename, got %q", got)
	}
}

// --- Report tests -----------------------------------------------------------

func TestBuildReport_CleanPasses(t *testing.T) {
	required := loadProtection(t, "protection_clean.json")
	runs := loadCheckRuns(t, "check_runs_clean.json")
	rep := buildReport(required, runs)
	if len(rep.MissingRequired) != 0 {
		t.Errorf("expected no missing, got %v", rep.MissingRequired)
	}
	// `lint` is in runs but not required → informational entry expected.
	if len(rep.UnpinnedCheckRuns) != 1 || rep.UnpinnedCheckRuns[0].Name != "lint" {
		t.Errorf("expected single 'lint' info entry, got %v", rep.UnpinnedCheckRuns)
	}
}

func TestBuildReport_DriftedDetectsBoth(t *testing.T) {
	required := loadProtection(t, "protection_drifted.json")
	runs := loadCheckRuns(t, "check_runs_drifted.json")
	rep := buildReport(required, runs)

	if len(rep.MissingRequired) != 3 {
		t.Fatalf("expected 3 missing, got %d: %v", len(rep.MissingRequired), rep.MissingRequired)
	}
	notes := map[string]string{}
	for _, m := range rep.MissingRequired {
		notes[m.Name] = m.Note
	}
	if !strings.HasPrefix(notes["pytest (Ubuntu, python alias absent — Issue "], "suspect: '#' truncation") {
		t.Errorf("expected '#' truncation note, got %q", notes["pytest (Ubuntu, python alias absent — Issue "])
	}
	if !strings.HasPrefix(notes["build"], "suspect: matrix job") {
		t.Errorf("expected matrix-job note, got %q", notes["build"])
	}
	if notes["lint-old"] != "" {
		t.Errorf("expected pure-rename to have no note, got %q", notes["lint-old"])
	}
	// `lint` (in runs only) should appear in informational list.
	infoNames := []string{}
	for _, i := range rep.UnpinnedCheckRuns {
		infoNames = append(infoNames, i.Name)
	}
	wantInfo := []string{"build (3.10)", "build (3.11)", "build (3.12)", "lint", "pytest (Ubuntu, python alias absent — Issue #33 regression guard)"}
	if !equalStringSlices(infoNames, wantInfo) {
		t.Errorf("info entries mismatch:\n got=%v\nwant=%v", infoNames, wantInfo)
	}
}

// --- Output rendering -------------------------------------------------------

func TestWriteReport_VerbatimSections(t *testing.T) {
	required := loadProtection(t, "protection_drifted.json")
	runs := loadCheckRuns(t, "check_runs_drifted.json")
	rep := buildReport(required, runs)

	var buf bytes.Buffer
	writeReport(&buf, rep, fixedTime())
	out := buf.String()

	for _, want := range []string{
		"=== Required contexts NOT matched by recent check-runs ===",
		"=== Recent check-runs NOT in required contexts ===",
		"=== Last verified ===",
		"  2026-05-04T14:23+09:00",
		"← suspect: '#' truncation in source job name",
		"← suspect: matrix job — actual was 'build (3.10)' / 'build (3.11)'",
		"← informational only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestWriteReport_AllPass_NoneMarkers(t *testing.T) {
	required := loadProtection(t, "protection_clean.json")
	runs := []string{"build (macos-latest)", "build (ubuntu-latest)", "build (windows-latest)"}
	rep := buildReport(required, runs)

	var buf bytes.Buffer
	writeReport(&buf, rep, fixedTime())
	out := buf.String()

	if !strings.Contains(out, "=== Required contexts NOT matched by recent check-runs ===\n  (none)\n") {
		t.Errorf("expected '(none)' under missing-section, got:\n%s", out)
	}
	if !strings.Contains(out, "=== Recent check-runs NOT in required contexts ===\n  (none)\n") {
		t.Errorf("expected '(none)' under unpinned-section, got:\n%s", out)
	}
}

// --- run() / exit codes -----------------------------------------------------

func stubDeps(required, runs []string, repoErr error, refErr error) deps {
	return deps{
		getProtection: func(_ Repo, _ string) ([]string, error) { return required, nil },
		getCheckRuns:  func(_ Repo, _ string) ([]string, error) { return runs, nil },
		resolveRepo:   func() (Repo, error) { return Repo{Host: "github.com", Owner: "cli", Name: "cli"}, repoErr },
		resolveRef:    func() (string, error) { return "abc1234", refErr },
		now:           fixedTime,
	}
}

func TestRun_ExitCodes(t *testing.T) {
	cleanReq := loadProtection(t, "protection_clean.json")
	cleanRuns := loadCheckRuns(t, "check_runs_clean.json")

	driftedReq := loadProtection(t, "protection_drifted.json")
	driftedRuns := loadCheckRuns(t, "check_runs_drifted.json")

	cases := []struct {
		name      string
		args      []string
		deps      deps
		wantCode  int
		wantInOut string
		wantInErr string
	}{
		{"success no missing", nil, stubDeps(cleanReq, cleanRuns, nil, nil), exitcode.Success, "(none)", ""},
		{"verify fail missing", nil, stubDeps(driftedReq, driftedRuns, nil, nil), exitcode.VerifyFailed, "← suspect:", ""},
		{"usage error positional", []string{"unexpected"}, stubDeps(cleanReq, cleanRuns, nil, nil), exitcode.UsageError, "", "takes no positional"},
		{"usage error bad flag", []string{"--foo"}, stubDeps(cleanReq, cleanRuns, nil, nil), exitcode.UsageError, "", "flag provided but not defined"},
		{"upstream HEAD failure", []string{"--ref", "HEAD"}, stubDeps(cleanReq, cleanRuns, nil, errors.New("not a git repo")), exitcode.UpstreamErr, "", "resolve HEAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr, tc.deps)
			gotCode := exitcode.Success
			if err != nil {
				var ec *exitcode.Error
				if errors.As(err, &ec) {
					gotCode = ec.Code
				} else {
					gotCode = exitcode.UpstreamErr
				}
			}
			if gotCode != tc.wantCode {
				t.Errorf("got exit %d, want %d (err=%v)", gotCode, tc.wantCode, err)
			}
			if tc.wantInOut != "" && !strings.Contains(stdout.String(), tc.wantInOut) {
				t.Errorf("stdout missing %q:\n%s", tc.wantInOut, stdout.String())
			}
			if tc.wantInErr != "" && !strings.Contains(stderr.String(), tc.wantInErr) {
				t.Errorf("stderr missing %q:\n%s", tc.wantInErr, stderr.String())
			}
		})
	}
}

// --- Repo flag handling -----------------------------------------------------

func TestResolveRepoFromFlag(t *testing.T) {
	cur := func() (Repo, error) { return Repo{Host: "github.com", Owner: "auto", Name: "detect"}, nil }
	cases := []struct {
		flag    string
		want    Repo
		wantErr bool
	}{
		{"", Repo{Host: "github.com", Owner: "auto", Name: "detect"}, false},
		{"foo/bar", Repo{Host: "github.com", Owner: "foo", Name: "bar"}, false},
		{"foo", Repo{}, true},
		{"/bar", Repo{}, true},
		{"foo/", Repo{}, true},
	}
	for _, tc := range cases {
		got, err := resolveRepoFromFlag(tc.flag, cur)
		if tc.wantErr {
			if err == nil {
				t.Errorf("resolveRepoFromFlag(%q): expected error, got %+v", tc.flag, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveRepoFromFlag(%q): unexpected error %v", tc.flag, err)
			continue
		}
		if got != tc.want {
			t.Errorf("resolveRepoFromFlag(%q) = %+v, want %+v", tc.flag, got, tc.want)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func loadProtection(t *testing.T, name string) []string {
	t.Helper()
	b := readFixture(t, name)
	out, err := parseProtectionPayload(b)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return out
}

func loadCheckRuns(t *testing.T, name string) []string {
	t.Helper()
	b := readFixture(t, name)
	out, err := parseCheckRunsPayload(b)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return out
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "A1", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
