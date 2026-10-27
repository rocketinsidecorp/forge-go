//go:build unix

package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// OpenSSH binds its control socket at "<ControlPath>.<16 random characters>",
// and a Unix socket path must be shorter than sun_path: 104 bytes on macOS,
// whose default $TMPDIR (/var/folders/xx/.../T/) leaves no room for it. These
// tests run with a $TMPDIR longer than any platform's limit.

// longTempDir returns a directory whose path is over 110 bytes, such as macOS's
// default $TMPDIR plus a test's own subdirectories.
func longTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for len(dir) <= 120 {
		dir = filepath.Join(dir, "a-long-directory-name")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSSHRunnerKeepsTheControlSocketPathShortWithALongTMPDIR(t *testing.T) {
	// Not parallel: TMPDIR is set for the process.
	long := longTempDir(t)
	t.Setenv("TMPDIR", long)
	binary, argsFile, _ := fakeSSH(t, "", "", 0)
	runner := &SSHRunner{Binary: binary}
	if _, err := runner.Run(t.Context(), "h", Command{Script: "true"}); err != nil {
		t.Fatalf("a long TMPDIR broke the runner: %v", err)
	}
	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^ControlPath=(.+)$`).FindStringSubmatch(string(argv))
	if match == nil {
		t.Fatalf("ssh got no ControlPath:\n%s", argv)
	}
	// What ssh binds: %C is 40 hexadecimal characters; it appends "." and 16.
	bound := strings.ReplaceAll(match[1], "%C", strings.Repeat("0", 40)) + "." + strings.Repeat("0", 16)
	if len(bound) >= 104 {
		t.Errorf("the socket path is %d bytes, macOS allows 103: %s", len(bound), bound)
	}
	dir := runner.controlDir
	if strings.HasPrefix(dir, long) {
		t.Errorf("the control directory %s is under the long TMPDIR", dir)
	}
	// Private: only its owner can enter it, and nobody else owns it.
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 || !info.IsDir() {
		t.Errorf("control directory mode = %v", info.Mode())
	}
	if owner := info.Sys().(*syscall.Stat_t).Uid; int(owner) != os.Geteuid() {
		t.Errorf("control directory owned by %d, want %d", owner, os.Geteuid())
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the control directory survived Close: %v", err)
	}
}

// TestSSHRunnerControlSocketFitsWithTheDefaultTMPDIR runs with whatever
// $TMPDIR the machine has: on macOS (the macos job of ci.yml) that is
// /var/folders/xx/.../T/, the path that broke `forge deploy` there.
func TestSSHRunnerControlSocketFitsWithTheDefaultTMPDIR(t *testing.T) {
	t.Parallel()
	binary, argsFile, _ := fakeSSH(t, "", "", 0)
	runner := &SSHRunner{Binary: binary}
	defer func() { _ = runner.Close() }()
	if _, err := runner.Run(t.Context(), "h", Command{Script: "true"}); err != nil {
		t.Fatalf("the default TMPDIR (%s) broke the runner: %v", os.TempDir(), err)
	}
	argv, _ := os.ReadFile(argsFile)
	match := regexp.MustCompile(`(?m)^ControlPath=(.+)$`).FindStringSubmatch(string(argv))
	if match == nil {
		t.Fatalf("ssh got no ControlPath:\n%s", argv)
	}
	if bound := len(strings.ReplaceAll(match[1], "%C", strings.Repeat("0", 40))) + len(".") + 16; bound >= 104 {
		t.Errorf("with TMPDIR %s the socket path is %d bytes, macOS allows 103", os.TempDir(), bound)
	}
}

func TestControlDirectoryStaysUnderTMPDIRWhenTheSocketsFit(t *testing.T) {
	t.Parallel()
	base, err := os.MkdirTemp("/tmp", "fc") // short by construction
	if err != nil {
		t.Skipf("no short directory to test with: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	dir, err := makeControlDirectory(base, "/does-not-exist", true)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != base {
		t.Errorf("control directory %s is not under %s", dir, base)
	}
}

func TestControlDirectoryRefusesASocketPathThatCannotFit(t *testing.T) {
	t.Parallel()
	long := longTempDir(t)
	_, err := makeControlDirectory(long, long, true)
	if err == nil || !strings.Contains(err.Error(), "TMPDIR") || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("err = %v, want a refusal that names the length and TMPDIR", err)
	}
	if entries, _ := os.ReadDir(long); len(entries) != 0 {
		t.Errorf("the refused directory was left behind: %v", entries)
	}
}

func TestCommandLineAgainstSSHWithALongTMPDIR(t *testing.T) {
	// Not parallel: PATH and TMPDIR are set for the commands' own ssh.
	c := newCLICluster(t)
	t.Setenv("TMPDIR", longTempDir(t))
	stdout, stderr, err := c.deploy("details")
	if err != nil || !strings.Contains(stdout, "alpha") {
		t.Fatalf("forge deploy details with a long TMPDIR: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
}
