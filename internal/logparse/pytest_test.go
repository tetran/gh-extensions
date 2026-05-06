package logparse

import "testing"

func TestPytest_Extract(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want string
		ok   bool
	}{
		{
			name: "FAILED prefix with assertion error",
			log:  "some preamble\nFAILED tests/test_archive.py::TestX::test_y - AssertionError: x != y\n",
			want: "tests/test_archive.py",
			ok:   true,
		},
		{
			name: "FAILED prefix with parametrized id",
			log:  "FAILED tests/test_parser.py::test_round_trip[case-3]\n",
			want: "tests/test_parser.py",
			ok:   true,
		},
		{
			name: "Suffixed FAILED form",
			log:  "tests/foo/bar.py::test_thing FAILED\n",
			want: "tests/foo/bar.py",
			ok:   true,
		},
		{
			name: "Multiple failures: returns first",
			log:  "FAILED tests/a.py::test_one\nFAILED tests/b.py::test_two\n",
			want: "tests/a.py",
			ok:   true,
		},
		{
			name: "GitHub Actions timestamp prefix",
			log:  "2026-05-04T12:00:01.234Z FAILED tests/test_x.py::test_y - boom\n",
			want: "tests/test_x.py",
			ok:   true,
		},
		{
			name: "No failure marker",
			log:  "PASSED tests/test_x.py::test_y\nall good\n",
			want: "",
			ok:   false,
		},
		{
			name: "Empty input",
			log:  "",
			want: "",
			ok:   false,
		},
	}
	a := Pytest{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := a.Extract([]byte(tc.log))
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v (got=%q)", ok, tc.ok, got)
			}
			if got != tc.want {
				t.Errorf("got=%q, want=%q", got, tc.want)
			}
		})
	}
}

func TestFirstMatch_PytestRegistered(t *testing.T) {
	log := "FAILED tests/x.py::test_y - boom\n"
	path, name, ok := FirstMatch([]byte(log))
	if !ok || path != "tests/x.py" || name != "pytest" {
		t.Errorf("FirstMatch: ok=%v path=%q name=%q", ok, path, name)
	}
}

func TestMatchWith_NoAdapterMatches(t *testing.T) {
	_, _, ok := MatchWith([]Adapter{Pytest{}}, []byte("--- FAIL: TestSomething\n"))
	if ok {
		t.Errorf("expected no match for Go test format under Pytest-only set, got hit")
	}
}
