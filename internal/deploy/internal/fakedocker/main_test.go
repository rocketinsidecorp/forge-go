//go:build unix

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain lets the test binary stand in for the docker command: the test
// below runs it as a child, the way `forge logs --follow` runs the real one.
func TestMain(m *testing.M) {
	if os.Getenv("FAKEDOCKER_AS_DOCKER") == "1" {
		os.Exit(run(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// A `logs --follow` records its pid before it prints: a test that has seen
// the output and then looks for the follower's pid file must always find it.
func TestLogsFollowRecordsItsPidBeforePrinting(t *testing.T) {
	dir := t.TempDir()
	save(dir, &state{Containers: []*container{{Name: "shop-web-1", State: "running"}}})

	follower := exec.Command(os.Args[0], "logs", "--follow", "shop-web-1") // #nosec G204 -- the test binary itself.
	follower.Env = append(os.Environ(), "FAKEDOCKER_AS_DOCKER=1", "FAKE_DOCKER_STATE="+dir)
	out, err := follower.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = follower.Process.Kill(); _ = follower.Wait() })

	// The first line is out: the pid file must already exist.
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatalf("the follower printed nothing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, fmt.Sprintf("logs-%d.pid", follower.Process.Pid))); err != nil {
		t.Fatalf("the pid file did not exist when the first line was printed: %v", err)
	}
}
