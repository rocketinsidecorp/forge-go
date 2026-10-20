//go:build unix

package deploy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shellRunner runs each host's commands with sh in its own directory, the
// way SSH starts them in the home directory, so the lock scripts run for
// real without sshd.
type shellRunner struct {
	dirs map[string]string
}

func (r shellRunner) Run(ctx context.Context, host string, cmd Command) (Result, error) {
	c := exec.CommandContext(ctx, "sh", "-c", cmd.Script) // #nosec G204 -- test scripts built by this package.
	c.Dir = r.dirs[host]
	c.Stdin = bytes.NewReader(cmd.Stdin)
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result, &RemoteError{Host: host, ExitCode: exitErr.ExitCode(), Stderr: result.Stderr}
	}
	return result, err
}

func (shellRunner) Stream(context.Context, string, Command, io.Writer) error { return nil }
func (shellRunner) Interactive(context.Context, string, string) error        { return nil }
func (shellRunner) Close() error                                             { return nil }

func lockFixture(t *testing.T, performer string) (*Deployer, *syncBuffer, string) {
	t.Helper()
	d, _, _, _ := planFixture(t, false)
	dirs := map[string]string{}
	for _, host := range d.Config.AllHosts() {
		dirs[host] = t.TempDir()
	}
	log := &syncBuffer{}
	d.Runner, d.Log, d.Performer = shellRunner{dirs: dirs}, log, performer
	if err := d.prepare(); err != nil {
		t.Fatal(err)
	}
	return d, log, dirs[d.Config.AllHosts()[0]]
}

func lockExists(home string) bool {
	_, err := os.Stat(filepath.Join(home, ".forge", "shop", "lock"))
	return err == nil
}

func TestReleaseRemovesOnlyItsOwnLock(t *testing.T) {
	d, log, home := lockFixture(t, "alice@laptop")
	ctx := t.Context()
	release, err := d.acquire(ctx, d.Config.AllHosts(), "deploy")
	if err != nil || !lockExists(home) {
		t.Fatalf("acquire: %v", err)
	}
	// Someone force-releases and takes the lock while the deploy runs.
	if err := d.LockRelease(ctx, true); err != nil {
		t.Fatal(err)
	}
	bob := []byte(`{"holder":"bob@ci","message":"maintenance","acquired_at":"2026-10-20T10:00:00Z"}` + "\n")
	if _, err := d.Runner.Run(ctx, d.Config.AllHosts()[0], d.cmds.acquireLock(bob)); err != nil {
		t.Fatal(err)
	}
	release()
	if !lockExists(home) {
		t.Fatal("the deferred release deleted a lock somebody else took")
	}
	if !strings.Contains(log.String(), "no longer ours") {
		t.Fatalf("no warning about the foreign lock:\n%s", log.String())
	}
	locks, err := d.LockStatus(ctx)
	if err != nil || locks[d.Config.AllHosts()[0]].Holder != "bob@ci" {
		t.Fatalf("lock status = %v, %v", locks, err)
	}
	if err := d.LockRelease(ctx, false); ExitCodeOf(err) != ExitLockHeld {
		t.Fatalf("released bob's lock without --force: %v", err)
	}
	if lockExists(home) == false {
		t.Fatal("bob's lock is gone")
	}
}

func TestReleaseRemovesOwnLock(t *testing.T) {
	d, _, home := lockFixture(t, "alice@laptop")
	ctx := t.Context()
	release, err := d.acquire(ctx, d.Config.AllHosts(), "deploy")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if lockExists(home) {
		t.Fatal("own lock not released")
	}
	if err := d.LockAcquire(ctx, "window"); err != nil {
		t.Fatal(err)
	}
	if err := d.LockRelease(ctx, false); err != nil || lockExists(home) {
		t.Fatalf("release of own lock: %v (exists %v)", err, lockExists(home))
	}
}

func TestLockWithoutRecordIsRecoverable(t *testing.T) {
	d, log, home := lockFixture(t, "alice@laptop")
	ctx := t.Context()
	hosts := d.Config.AllHosts()
	lock := filepath.Join(home, ".forge", "shop", "lock")
	if err := os.MkdirAll(lock, 0o750); err != nil {
		t.Fatal(err)
	}

	// Fresh: an acquire in progress may still write its record.
	_, err := d.acquire(ctx, hosts, "deploy")
	var held *LockHeldError
	if ExitCodeOf(err) != ExitLockHeld || !errors.As(err, &held) || !strings.Contains(err.Error(), "without a record") {
		t.Fatalf("fresh record-less lock: %v", err)
	}
	if !lockExists(home) {
		t.Fatal("a fresh record-less lock was removed")
	}
	if locks, err := d.LockStatus(ctx); err != nil || locks[hosts[0]].Holder != "unknown" || !strings.Contains(locks[hosts[0]].Message, "without a record") {
		t.Fatalf("status = %v, %v", locks, err)
	}
	if err := d.LockRelease(ctx, false); ExitCodeOf(err) != ExitLockHeld {
		t.Fatalf("release without --force of a record-less lock: %v", err)
	}

	// Older than the grace period: recovered without --force, and reported.
	old := time.Now().Add(-5 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	release, err := d.acquire(ctx, hosts, "deploy")
	if err != nil {
		t.Fatalf("stale record-less lock: %v", err)
	}
	if !strings.Contains(log.String(), staleLockNotice) {
		t.Fatalf("recovery not reported:\n%s", log.String())
	}
	if locks, _ := d.LockStatus(ctx); locks[hosts[0]].Holder != "alice@laptop" {
		t.Fatalf("lock not retaken: %v", locks)
	}
	release()
	if lockExists(home) {
		t.Fatal("lock not released")
	}
}

func TestStaleRecoveryNeverRemovesALockWithARecord(t *testing.T) {
	d, _, home := lockFixture(t, "alice@laptop")
	ctx := t.Context()
	if err := d.LockAcquire(ctx, "window"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-5 * time.Minute)
	lock := filepath.Join(home, ".forge", "shop", "lock")
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := d.acquire(ctx, d.Config.AllHosts(), "deploy"); ExitCodeOf(err) != ExitLockHeld {
		t.Fatalf("an old lock with a record must stay held: %v", err)
	}
}
