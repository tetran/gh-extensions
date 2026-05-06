// Package exitcode defines the shared exit-code regime for the gh-extensions monorepo.
//
// The four codes follow the convention agreed in the implementation plan §3.1:
//
//	0  Success       — verification / operation succeeded
//	1  VerifyFailed  — verification check failed (semantic mismatch, sanity break)
//	2  UsageError    — invalid args, missing required flag, etc.
//	3  UpstreamErr   — upstream API / network / git failure (non-2xx, command not found)
package exitcode

const (
	Success      = 0
	VerifyFailed = 1
	UsageError   = 2
	UpstreamErr  = 3
)

// Error wraps an error with an exit code so main() can type-switch on it.
type Error struct {
	Code int
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Wrap returns an *Error that carries the given exit code.
func Wrap(code int, err error) *Error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Err: err}
}
