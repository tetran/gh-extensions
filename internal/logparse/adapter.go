// Package logparse contains pluggable extractors that pull a "first failed
// test file" out of a workflow log, used by gh-ci-triage to attribute a
// failure to a file. Adapters are added one per language/framework so the
// supported set can grow without touching the triage core.
package logparse

// Adapter is the contract every log extractor implements.
type Adapter interface {
	// Name identifies the adapter, e.g. "pytest". Used for diagnostics.
	Name() string

	// Extract returns the first failed test file path from the log, plus a
	// boolean OK flag. ok=false means "this adapter does not recognize a
	// failure pattern in this log" — the caller should try the next adapter
	// or fall back to the no-file branch of the triage logic.
	Extract(log []byte) (testFile string, ok bool)
}

// Default is the registered set of adapters in priority order. Currently
// pytest is the only entry; add more by appending here.
var Default = []Adapter{Pytest{}}

// FirstMatch tries each adapter in `Default` order and returns the first
// successful extraction. Returns ("", false) when no adapter matched.
func FirstMatch(log []byte) (testFile, adapterName string, ok bool) {
	return MatchWith(Default, log)
}

// MatchWith is FirstMatch but with a caller-provided adapter list. Tests use
// this to drive deterministic ordering.
func MatchWith(adapters []Adapter, log []byte) (testFile, adapterName string, ok bool) {
	for _, a := range adapters {
		if path, hit := a.Extract(log); hit {
			return path, a.Name(), true
		}
	}
	return "", "", false
}
