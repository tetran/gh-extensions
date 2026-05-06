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

// fakeFetcher returns canned PR / Issue payloads for tests.
type fakeFetcher struct {
	pr     PRView
	issue  IssueView
	prErr  error
	issErr error
}

func (f fakeFetcher) PR(_ string, _ int) (PRView, error)       { return f.pr, f.prErr }
func (f fakeFetcher) Issue(_ string, _ int) (IssueView, error) { return f.issue, f.issErr }

func fixedNow() time.Time {
	return time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
}

func TestVerify_TableDriven(t *testing.T) {
	prMatched := PRView{
		Number:                  102,
		State:                   "MERGED",
		HeadRefName:             "feature/100-x",
		MergeCommit:             struct{ OID string `json:"oid"` }{OID: "4d7ccf0a8b1234567890"},
		ClosingIssuesReferences: []IssueRef{{Number: 100}},
	}
	issueMatched := IssueView{
		Number:                         100,
		State:                          "CLOSED",
		ClosedByPullRequestsReferences: []PRRef{{Number: 102}},
	}

	cases := []struct {
		name       string
		mutatePR   func(*PRView)
		mutateIss  func(*IssueView)
		expectSHA  string
		wantFailed bool
		wantSubstr string // substring expected somewhere in the failure list
	}{
		{name: "all pass / no SHA", wantFailed: false},
		{name: "all pass / matching SHA prefix", expectSHA: "4d7ccf0", wantFailed: false},
		{name: "all pass / matching full SHA", expectSHA: "4d7ccf0a8b1234567890", wantFailed: false},
		{
			name:       "PR refs Issue but Issue doesn't ref PR",
			mutateIss:  func(i *IssueView) { i.ClosedByPullRequestsReferences = nil },
			wantFailed: true, wantSubstr: "unidirectional",
		},
		{
			name:       "Issue refs PR but PR doesn't close Issue",
			mutatePR:   func(p *PRView) { p.ClosingIssuesReferences = nil },
			wantFailed: true, wantSubstr: "unidirectional",
		},
		{
			name:       "neither side refs the other",
			mutatePR:   func(p *PRView) { p.ClosingIssuesReferences = nil },
			mutateIss:  func(i *IssueView) { i.ClosedByPullRequestsReferences = nil },
			wantFailed: true, wantSubstr: "do not reference each other",
		},
		{
			name: "SHA mismatch",
			expectSHA: "deadbeef",
			wantFailed: true, wantSubstr: "Merge commit mismatch",
		},
		{
			name:       "PR not merged",
			mutatePR:   func(p *PRView) { p.State = "OPEN" },
			wantFailed: true, wantSubstr: "PR #102 state is OPEN",
		},
		{
			name:       "Issue still open",
			mutateIss:  func(i *IssueView) { i.State = "OPEN" },
			wantFailed: true, wantSubstr: "Issue #100 state is OPEN",
		},
		{
			name:       "PR points at a different issue",
			mutatePR:   func(p *PRView) { p.ClosingIssuesReferences = []IssueRef{{Number: 999}} },
			wantFailed: true, wantSubstr: "unidirectional",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := prMatched
			iss := issueMatched
			if tc.mutatePR != nil {
				tc.mutatePR(&pr)
			}
			if tc.mutateIss != nil {
				tc.mutateIss(&iss)
			}
			fails := verify(pr, iss, tc.expectSHA)
			if tc.wantFailed && len(fails) == 0 {
				t.Fatalf("expected failures, got none")
			}
			if !tc.wantFailed && len(fails) > 0 {
				t.Fatalf("expected no failures, got: %v", fails)
			}
			if tc.wantSubstr != "" {
				joined := strings.Join(fails, " || ")
				if !strings.Contains(joined, tc.wantSubstr) {
					t.Errorf("failure list missing %q: %v", tc.wantSubstr, fails)
				}
			}
		})
	}
}

func TestVerify_FixturesAllPass(t *testing.T) {
	pr := loadPR(t, "pr_ok.json")
	iss := loadIssue(t, "issue_ok.json")
	if fails := verify(pr, iss, ""); len(fails) > 0 {
		t.Errorf("expected all pass with fixtures, got: %v", fails)
	}
	if fails := verify(pr, iss, "6c470f6"); len(fails) > 0 {
		t.Errorf("matching short SHA should pass, got: %v", fails)
	}
}

func TestVerify_FixtureUnidirectional(t *testing.T) {
	pr := loadPR(t, "pr_unidirectional.json")
	iss := loadIssue(t, "issue_no_back_ref.json")
	fails := verify(pr, iss, "")
	if len(fails) == 0 {
		t.Fatalf("expected unidirectional failure, got none")
	}
	if !strings.Contains(strings.Join(fails, " "), "unidirectional") {
		t.Errorf("expected 'unidirectional' in failure: %v", fails)
	}
}

func TestRun_VerbatimSuccessOutput(t *testing.T) {
	pr := loadPR(t, "pr_ok.json")
	iss := loadIssue(t, "issue_ok.json")
	var stdout, stderr bytes.Buffer
	err := run([]string{"13281", "13280"}, &stdout, &stderr,
		fakeFetcher{pr: pr, issue: iss}, fixedNow)
	if err != nil {
		t.Fatalf("run: %v (stderr=%s)", err, stderr.String())
	}
	want := "" +
		"✓ PR #13281 closes Issue #13280 (bidirectional)\n" +
		"✓ Merge commit: 6c470f6... (matches)\n" +
		"✓ States: PR=MERGED, Issue=CLOSED\n" +
		"\n" +
		"Verified citation:\n" +
		"  Issue #13280 PR: 6c470f6 (PR #13281 — fix/projects-v2-ignorable-error)\n" +
		"  verified via gh xref-verify on 2026-05-04\n"
	if stdout.String() != want {
		t.Errorf("verbatim mismatch:\n got=%q\nwant=%q", stdout.String(), want)
	}
}

func TestRun_ExitCodes(t *testing.T) {
	pr := loadPR(t, "pr_ok.json")
	iss := loadIssue(t, "issue_ok.json")
	prUni := loadPR(t, "pr_unidirectional.json")
	issBad := loadIssue(t, "issue_no_back_ref.json")

	cases := []struct {
		name      string
		args      []string
		fetcher   fakeFetcher
		wantCode  int
		wantInErr string // substring of stderr (only checked if non-empty)
	}{
		{"success no SHA", []string{"13281", "13280"}, fakeFetcher{pr: pr, issue: iss}, exitcode.Success, ""},
		{"success with SHA", []string{"13281", "13280", "6c470f6"}, fakeFetcher{pr: pr, issue: iss}, exitcode.Success, ""},
		{"verify fail unidirectional", []string{"13281", "13280"}, fakeFetcher{pr: prUni, issue: issBad}, exitcode.VerifyFailed, "unidirectional"},
		{"verify fail bad SHA", []string{"13281", "13280", "deadbeef"}, fakeFetcher{pr: pr, issue: iss}, exitcode.VerifyFailed, "Merge commit mismatch"},
		{"usage too few", []string{"13281"}, fakeFetcher{pr: pr, issue: iss}, exitcode.UsageError, "expected 2 or 3"},
		{"usage too many", []string{"a", "b", "c", "d"}, fakeFetcher{pr: pr, issue: iss}, exitcode.UsageError, "expected 2 or 3"},
		{"usage non-numeric PR", []string{"abc", "13280"}, fakeFetcher{pr: pr, issue: iss}, exitcode.UsageError, "invalid PR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tc.args, &stdout, &stderr, tc.fetcher, fixedNow)
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
				t.Errorf("got exit code %d, want %d (err=%v)", gotCode, tc.wantCode, err)
			}
			if tc.wantInErr != "" && !strings.Contains(stderr.String(), tc.wantInErr) {
				t.Errorf("stderr missing %q: %s", tc.wantInErr, stderr.String())
			}
		})
	}
}

func TestReorderFlags(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"13281", "13280", "--repo", "cli/cli"}, []string{"--repo", "cli/cli", "13281", "13280"}},
		{[]string{"13281", "--repo=cli/cli", "13280"}, []string{"--repo=cli/cli", "13281", "13280"}},
		{[]string{"--repo", "cli/cli", "13281", "13280"}, []string{"--repo", "cli/cli", "13281", "13280"}},
		{[]string{"13281", "13280"}, []string{"13281", "13280"}},
		{[]string{"13281", "--", "13280", "--repo"}, []string{"13281", "13280", "--repo"}},
	}
	for _, tc := range cases {
		got := reorderFlags(tc.in)
		if !equalSlice(got, tc.want) {
			t.Errorf("reorderFlags(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSHAEquivalent(t *testing.T) {
	cases := []struct {
		actual, expected string
		want             bool
	}{
		{"6c470f60803784e1558b626022677c53dccb6016", "6c470f6", true},
		{"6c470f60803784e1558b626022677c53dccb6016", "6C470F6", true}, // case insensitive
		{"6c470f6", "6c470f60803784e1558b626022677c53dccb6016", true},
		{"6c470f60803784e1558b626022677c53dccb6016", "deadbeef", false},
		{"", "6c470f6", false},
		{"6c470f6", "", false},
	}
	for _, tc := range cases {
		if got := shaEquivalent(tc.actual, tc.expected); got != tc.want {
			t.Errorf("shaEquivalent(%q, %q) = %v, want %v", tc.actual, tc.expected, got, tc.want)
		}
	}
}

func loadPR(t *testing.T, name string) PRView {
	t.Helper()
	b := readFixture(t, name)
	var v PRView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return v
}

func loadIssue(t *testing.T, name string) IssueView {
	t.Helper()
	b := readFixture(t, name)
	var v IssueView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	return v
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "A2", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func equalSlice(a, b []string) bool {
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
