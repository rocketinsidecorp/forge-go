//go:build unix

package devloop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/internal/waittest"
)

// alive reports whether pid is a running process; a zombie that nobody has
// reaped yet is not running.
func alive(pid int) bool {
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
		return len(fields) > 0 && fields[0] != "Z"
	}
	return syscall.Kill(pid, 0) == nil
}

func readPID(t *testing.T, name string) int {
	t.Helper()
	waitForFile(t, name)
	var pid int
	waittest.Poll(time.Second, func() bool {
		data, _ := os.ReadFile(name)
		value, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			pid = value
		}
		return err == nil
	})
	if pid == 0 {
		t.Fatal("no pid recorded")
	}
	return pid
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	if waittest.Poll(testWait, func() bool { return !alive(pid) }) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("process %d is still running: it was orphaned", pid)
}

func TestStopEndsTheApplicationsChildrenToo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile, ready := filepath.Join(dir, "child.pid"), filepath.Join(dir, "ready")
	script := writeScript(t, "sleep 300 &\necho $! > \"$1\"\ntrap 'exit 0' TERM\n: > \"$2\"\nwhile :; do sleep 0.05; done\n")
	process, err := StartProcess("/bin/sh", []string{script, pidFile, ready}, dir, os.Environ(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	child := readPID(t, pidFile)
	if !alive(child) {
		t.Fatal("the helper process did not start")
	}
	if err := process.Stop(5 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitDead(t, child)
}

func TestIsolatedCommandCancellationKillsItsChildren(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 300 &\necho $! > \"$1\"\nwait", "sh", pidFile) // #nosec G204 -- a fixed shell script; the pid file is a positional parameter
	IsolateCommand(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := readPID(t, pidFile)
	cancel()
	_ = cmd.Wait()
	waitDead(t, child)
}

func TestAnApplicationThatExitedByItselfLeavesNoChildrenBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// The application starts a helper and crashes; the helper would live on.
	script := writeScript(t, "sleep 300 &\necho $! > \"$1\"\nexit 3\n")
	process, err := StartProcess("/bin/sh", []string{script, pidFile}, dir, os.Environ(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	child := readPID(t, pidFile)
	select {
	case <-process.Done():
	case <-time.After(testWait):
		t.Fatal("the application did not exit")
	}
	waitDead(t, child)
}
