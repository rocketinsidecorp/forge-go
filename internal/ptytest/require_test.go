//go:build linux || darwin

package ptytest

import "testing"

// recorder is a testing.TB that records Fatalf and Skipf instead of ending
// the test.
type recorder struct {
	testing.TB
	failed, skipped bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Fatalf(string, ...any) { r.failed = true }
func (r *recorder) Skipf(string, ...any)  { r.skipped = true }

// A pseudo-terminal that cannot be set up skips the test, unless
// FORGE_TEST_REQUIRE_PTY=true (CI) makes it a failure.
func TestUnavailablePseudoTerminalSkipsUnlessRequired(t *testing.T) {
	for value, wantFail := range map[string]bool{"": false, "false": false, "true": true} {
		t.Setenv(RequireVariable, value)
		r := &recorder{TB: t}
		unavailable(r, "cannot open the pseudo-terminal: %v", "broken")
		if r.failed != wantFail || r.skipped == wantFail {
			t.Errorf("%s=%q: failed=%v skipped=%v", RequireVariable, value, r.failed, r.skipped)
		}
	}
}
