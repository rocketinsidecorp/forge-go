//go:build unix

package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/exectest"
)

// fakeSSH writes a shell script that records its arguments and stdin and
// exits with code, to stand in for the ssh binary.
func fakeSSH(t *testing.T, stdout, stderr string, code int, readStdin ...bool) (binary, argsFile, stdinFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile, stdinFile = filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	read := ""
	if len(readStdin) > 0 && readStdin[0] {
		read = "cat > " + stdinFile + "\n"
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" >> %s\n%sprintf '%%s' %s\nprintf '%%s' %s >&2\nexit %d\n",
		argsFile, read, Quote(stdout), Quote(stderr), code)
	binary = filepath.Join(dir, "ssh")
	if err := exectest.Write(binary, script); err != nil {
		t.Fatal(err)
	}
	return binary, argsFile, stdinFile
}

func TestSSHRunnerPassesItsOptionsToSsh(t *testing.T) {
	t.Parallel()
	binary, argsFile, stdinFile := fakeSSH(t, "out", "", 0, true)
	runner := &SSHRunner{User: "deploy", Port: 2222, ProxyJump: "ops@bastion", ConfigFile: "/etc/forge/ssh_config", Binary: binary}
	defer func() { _ = runner.Close() }()
	result, err := runner.Run(t.Context(), "app1.example.com", Command{Script: "echo hi", Stdin: []byte("secret-on-stdin")})
	if err != nil || result.Stdout != "out" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	argv, _ := os.ReadFile(argsFile)
	for _, want := range []string{"BatchMode=yes", "-T", "-F\n/etc/forge/ssh_config", "-l\ndeploy", "-p\n2222", "-J\nops@bastion", "--\napp1.example.com\nsh -c 'echo hi'"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("ssh arguments lack %q:\n%s", want, argv)
		}
	}
	if strings.Contains(string(argv), "secret-on-stdin") {
		t.Error("stdin reached the command line")
	}
	if stdin, _ := os.ReadFile(stdinFile); string(stdin) != "secret-on-stdin" {
		t.Errorf("stdin = %q", stdin)
	}
}

func TestSSHRunnerDistinguishesSSHFailuresFromRemoteExits(t *testing.T) {
	t.Parallel()
	binary, _, _ := fakeSSH(t, "", "Permission denied (publickey).\n", 255)
	runner := &SSHRunner{Binary: binary}
	defer func() { _ = runner.Close() }()
	_, err := runner.Run(t.Context(), "h", Command{Script: "true"})
	var remote *RemoteError
	if err == nil || errors.As(err, &remote) || !strings.Contains(err.Error(), "ssh failed: Permission denied") {
		t.Fatalf("exit 255 = %v", err)
	}

	binary, _, _ = fakeSSH(t, "", "boom\n", 3)
	runner2 := &SSHRunner{Binary: binary}
	defer func() { _ = runner2.Close() }()
	_, err = runner2.Run(t.Context(), "h", Command{Script: "false"})
	if !errors.As(err, &remote) || remote.ExitCode != 3 || remote.Host != "h" || !strings.Contains(remote.Stderr, "boom") {
		t.Fatalf("exit 3 = %v", err)
	}

	missing := &SSHRunner{Binary: filepath.Join(t.TempDir(), "no-such-ssh")}
	defer func() { _ = missing.Close() }()
	if _, err := missing.Run(t.Context(), "h", Command{Script: "true"}); err == nil || errors.As(err, &remote) {
		t.Fatalf("missing binary = %v", err)
	}
}

func TestSSHRunnerRefusesAnOptionLookingHost(t *testing.T) {
	t.Parallel()
	binary, argsFile, _ := fakeSSH(t, "", "", 0)
	runner := &SSHRunner{Binary: binary}
	defer func() { _ = runner.Close() }()
	for _, host := range []string{"-oProxyCommand=evil", "", "a b"} {
		if _, err := runner.Run(t.Context(), host, Command{Script: "true"}); err == nil || !strings.Contains(err.Error(), "invalid host") {
			t.Errorf("host %q: %v", host, err)
		}
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("ssh ran for an invalid host")
	}
}

func TestSSHRunnerStreamAndInteractive(t *testing.T) {
	t.Parallel()
	binary, _, _ := fakeSSH(t, "line one\nline two\n", "warn\n", 0)
	runner := &SSHRunner{Binary: binary, User: "u", Port: 22}
	defer func() { _ = runner.Close() }()
	var out syncBuffer
	if err := runner.Stream(t.Context(), "h", Command{Script: "docker logs x"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "line one\nline two\n") || !strings.Contains(out.String(), "warn") {
		t.Fatalf("stream = %q", out.String())
	}
	quiet, quietArgs, _ := fakeSSH(t, "", "", 0)
	terminal := &SSHRunner{Binary: quiet}
	defer func() { _ = terminal.Close() }()
	if err := terminal.Interactive(t.Context(), "h", "sh"); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(quietArgs)
	if !strings.Contains(string(argv), "\n-t\n") {
		t.Errorf("an interactive session must allocate a terminal:\n%s", argv)
	}
	// A failing stream is a RemoteError; a canceled one ends quietly.
	failing, _, _ := fakeSSH(t, "", "", 5)
	bad := &SSHRunner{Binary: failing}
	defer func() { _ = bad.Close() }()
	var remote *RemoteError
	if err := bad.Stream(t.Context(), "h", Command{Script: "x"}, &out); !errors.As(err, &remote) || remote.ExitCode != 5 {
		t.Errorf("failing stream = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := bad.Stream(canceled, "h", Command{Script: "x"}, &out); err == nil {
		t.Error("a stream that cannot start must report it")
	}
	if err := bad.Interactive(t.Context(), "h", "sh"); !errors.As(err, &remote) {
		t.Errorf("failing interactive session = %v", err)
	}
	if err := bad.Stream(t.Context(), "-bad", Command{Script: "x"}, &out); err == nil {
		t.Error("stream accepted an invalid host")
	}
	if err := bad.Interactive(t.Context(), "-bad", "sh"); err == nil {
		t.Error("interactive accepted an invalid host")
	}
}

func TestSSHRunnerCloseStopsMastersAndRemovesItsDirectory(t *testing.T) {
	t.Parallel()
	binary, argsFile, _ := fakeSSH(t, "", "", 0)
	runner := &SSHRunner{Binary: binary, User: "u", Port: 2200, ConfigFile: "/cfg"}
	if _, err := runner.Run(t.Context(), "h1", Command{Script: "true"}); err != nil {
		t.Fatal(err)
	}
	dir := runner.controlDir
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control directory survived Close: %v", err)
	}
	argv, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(argv), "-O\nexit\n--\nh1") || !strings.Contains(string(argv), "-F\n/cfg") {
		t.Errorf("Close did not ask the master to exit:\n%s", argv)
	}
	if err := (&SSHRunner{}).Close(); err != nil {
		t.Errorf("Close without use: %v", err)
	}
}

func TestSSHRunnerNeedsAControlDirectory(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	runner := &SSHRunner{Binary: "true"}
	_, err := runner.Run(t.Context(), "h", Command{Script: "true"})
	if err == nil || !strings.Contains(err.Error(), "control directory") {
		t.Fatalf("err = %v", err)
	}
	if err := runner.Stream(t.Context(), "h", Command{Script: "true"}, &bytes.Buffer{}); err == nil {
		t.Error("stream without a control directory")
	}
}

func TestExecLocalRunsProgramsAndReportsTheirStderr(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	local := ExecLocal{Stdout: &stdout, Stderr: &stderr}
	out, err := local.Output(t.Context(), t.TempDir(), []string{"sh", "-c", "echo hello"})
	if err != nil || out != "hello\n" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if _, err := local.Output(t.Context(), ".", []string{"sh", "-c", "echo oops >&2; exit 3"}); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("failing Output = %v", err)
	}
	if err := local.Run(t.Context(), ".", []string{"sh", "-c", `cat; echo "$FORGE_X"`}, []byte("piped\n"), []string{"FORGE_X=from-env"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "piped\nfrom-env\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
	if err := local.Run(t.Context(), ".", []string{"sh", "-c", "exit 2"}, nil, nil); err == nil || !strings.Contains(err.Error(), "sh:") {
		t.Errorf("failing Run = %v", err)
	}
}
