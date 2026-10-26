package deploy

import (
	"errors"
	"fmt"
	"strings"
)

// Exit codes of `forge deploy`, `forge logs` and `forge app`. They are part
// of the command contract (forge help --json).
const (
	ExitOK = 0
	// ExitFailed covers invalid configuration, SSH failures and failed
	// remote commands.
	ExitFailed = 1
	// ExitUsage means the command line was invalid: an unknown flag or
	// subcommand, or a missing or extra argument. Nothing ran.
	ExitUsage = 2
	// ExitLockHeld means another deploy holds the lock; nothing changed.
	ExitLockHeld = 3
	// ExitUnhealthy means a new container failed its health check; every
	// host serves the previous version again.
	ExitUnhealthy = 4
	// ExitMigrationFailed means the migration failed; no container was
	// started or switched.
	ExitMigrationFailed = 5
	// ExitHookFailed means a local hook exited non-zero; the step after it
	// did not run.
	ExitHookFailed = 6
	// ExitInconsistent means a failure could not be fully reverted: hosts
	// may run different versions. Run `forge deploy details`.
	ExitInconsistent = 7
)

// ExitError carries an exit code for the CLI.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit code.
func (e *ExitError) ExitCode() int { return e.Code }

// usageError is an invalid command line: it exits with [ExitUsage].
func usageError(format string, args ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, args...)}
}

func withCode(code int, err error) error {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Err: err}
}

// ExitCodeOf returns the exit code for err: 0 for nil, the code of an
// [ExitError], else [ExitFailed].
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return ExitFailed
}

// LockHeldError reports the holders of the deploy lock.
type LockHeldError struct {
	Holders map[string]LockInfo
}

func (e *LockHeldError) Error() string {
	var parts []string
	for _, host := range sortedKeys(e.Holders) {
		info := e.Holders[host]
		parts = append(parts, fmt.Sprintf("%s: held by %s since %s (%s)", host, info.Holder, info.AcquiredAt.Format("2006-01-02 15:04:05 MST"), info.Message))
	}
	return "deploy lock is held: " + strings.Join(parts, "; ") + "; wait, or release it with forge deploy lock release"
}
