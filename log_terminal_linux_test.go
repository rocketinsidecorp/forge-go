//go:build linux

package forge

import (
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
	"github.com/rocketinsidecorp/forge-go/internal/ptytest"
)

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
		master, slave := ptytest.Open(t)
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
