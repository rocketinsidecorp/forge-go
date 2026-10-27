//go:build linux

package forge

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/rocketinsidecorp/forge-go/config"
)

// openPTY returns a pseudo-terminal: the slave a logger writes to and the
// master the test reads the terminal's output from. The test is skipped where
// /dev/ptmx cannot be opened.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	master = os.NewFile(uintptr(fd), "/dev/ptmx")
	t.Cleanup(func() { _ = master.Close() })
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pseudo-terminal: %v", err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot name the pseudo-terminal: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot open the pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// On a terminal the development log is pretty and colored, unless NO_COLOR is
// set (to any value, an empty one included): then the layout stays and the
// ANSI codes go.
func TestNewLoggerOnATerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		format  config.LogFormat
		colored bool
	}{
		{"auto", nil, config.LogFormatAuto, true},
		{"pretty", nil, config.LogFormatPretty, true},
		{"NO_COLOR", map[string]string{"NO_COLOR": "1"}, config.LogFormatAuto, false},
		{"empty NO_COLOR", map[string]string{"NO_COLOR": ""}, config.LogFormatPretty, false},
	} {
		master, slave := openPTY(t)
		cfg := config.Default()
		cfg.Log.Format = tc.format
		logger := newLogger(cfg, slave, func(key string) (string, bool) { value, ok := tc.env[key]; return value, ok })
		logger.Info("hello", "password", "hunter2")
		buffer := make([]byte, 4096)
		n, err := master.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		got := string(buffer[:n])
		if strings.Contains(got, "hunter2") || !strings.Contains(got, "INFO") || !strings.Contains(got, "hello") || !strings.Contains(got, "password=") {
			t.Errorf("%s: %q", tc.name, got)
		}
		if colored := strings.Contains(got, "\x1b["); colored != tc.colored {
			t.Errorf("%s: colored=%t: %q", tc.name, colored, got)
		}
		if !isTerminal(slave) {
			t.Errorf("%s: the slave of a pseudo-terminal is a terminal", tc.name)
		}
	}
}
