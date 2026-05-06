package logparse

import "regexp"

// Pytest extracts the first failed-test file path from a pytest log.
//
// Recognized patterns (in priority order):
//
//	FAILED tests/test_archive.py::TestX::test_y - AssertionError: ...
//	FAILED path/to/file.py::test_y[param-1]
//	tests/test_archive.py::test_y FAILED
//
// All forms point to the same test-file path; we only need the first hit.
type Pytest struct{}

func (Pytest) Name() string { return "pytest" }

var (
	// Form 1: "FAILED <path>::..."
	pytestFailedPrefix = regexp.MustCompile(`(?m)^(?:[\w\-:.]+ )?FAILED ([^\s:]+\.py)(?:::|\s)`)

	// Form 2: "<path>::test_x FAILED"
	pytestSuffixed = regexp.MustCompile(`(?m)^(?:[\w\-:.]+ )?([^\s:]+\.py)::[^\s]+ FAILED\b`)
)

// Extract scans log lines for a pytest failure marker and returns the first
// `*.py` path found. The leading optional `[\w\-:.]+ ` group accommodates
// GitHub Actions' log line prefixes (timestamps, runner labels) without
// matching the path through them.
func (Pytest) Extract(log []byte) (string, bool) {
	if m := pytestFailedPrefix.FindSubmatch(log); m != nil {
		return string(m[1]), true
	}
	if m := pytestSuffixed.FindSubmatch(log); m != nil {
		return string(m[1]), true
	}
	return "", false
}
