//go:build linux || darwin

package ptytest_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/internal/ptytest"
)

func TestOpenConnectsTheMasterToTheSlave(t *testing.T) {
	t.Parallel()
	master, slave := ptytest.Open(t)
	if _, err := slave.WriteString("ping\n"); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	n, err := master.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(buffer[:n]); got != "ping\r\n" {
		t.Fatalf("the master read %q, want the terminal's %q", got, "ping\r\n")
	}
}
