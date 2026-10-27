package forge

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/config"
)

func TestResolveLogFormat(t *testing.T) {
	for _, tc := range []struct {
		environment config.Environment
		format      config.LogFormat
		terminal    bool
		want        config.LogFormat
	}{
		// auto: the development layout only where a developer is watching.
		{config.Development, config.LogFormatAuto, true, config.LogFormatPretty},
		{config.Development, config.LogFormatAuto, false, config.LogFormatText},
		{config.Test, config.LogFormatAuto, true, config.LogFormatText},
		{config.Test, config.LogFormatAuto, false, config.LogFormatText},
		{config.Staging, config.LogFormatAuto, true, config.LogFormatJSON},
		{config.Production, config.LogFormatAuto, true, config.LogFormatJSON},
		{config.Production, config.LogFormatAuto, false, config.LogFormatJSON},
		// An explicit format wins everywhere.
		{config.Production, config.LogFormatPretty, false, config.LogFormatPretty},
		{config.Development, config.LogFormatJSON, true, config.LogFormatJSON},
		{config.Development, config.LogFormatText, true, config.LogFormatText},
	} {
		if got := resolveLogFormat(tc.environment, tc.format, tc.terminal); got != tc.want {
			t.Errorf("%s %s terminal=%t: %s, want %s", tc.environment, tc.format, tc.terminal, got, tc.want)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) || isTerminal(io.Discard) {
		t.Fatal("a buffer is not a terminal")
	}
	file, err := os.Create(t.TempDir() + "/log")
	if err != nil {
		t.Fatal(err)
	}
	if isTerminal(file) {
		t.Fatal("a regular file is not a terminal")
	}
	_ = file.Close()
	if isTerminal(file) {
		t.Fatal("a closed file is not a terminal")
	}
	// The null device is a character device, but nobody is watching it.
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	if isTerminal(null) {
		t.Fatal("/dev/null is not a terminal")
	}
}

func noEnv(string) (string, bool) { return "", false }

func TestNewLoggerFormats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    func(*config.Config)
		expect []string
	}{
		{"json", func(c *config.Config) { c.Log.Format = config.LogFormatJSON }, []string{`"msg":"hello"`, `"password":"[REDACTED]"`}},
		{"text", func(c *config.Config) { c.Log.Format = config.LogFormatText }, []string{`msg=hello`, `password=[REDACTED]`}},
		{"pretty", func(c *config.Config) { c.Log.Format = config.LogFormatPretty }, []string{`  INFO  hello  `, `password=[REDACTED]`}},
		{"production JSON", func(c *config.Config) { c.Environment = config.Production }, []string{`"msg":"hello"`, `"password":"[REDACTED]"`}},
		{"extra keys", func(c *config.Config) { c.Log.RedactKeys = []string{"user"}; c.Log.Format = config.LogFormatJSON }, []string{`"user":"[REDACTED]"`}},
	} {
		var out bytes.Buffer
		cfg := config.Default()
		tc.cfg(&cfg)
		logger := newLogger(cfg, &out, noEnv)
		logger.Debug("hidden at Info")
		logger.Info("hello", "password", "hunter2", "user", "ana")
		got := out.String()
		if strings.Contains(got, "hunter2") || strings.Contains(got, "hidden") || strings.Contains(got, "\x1b") {
			t.Errorf("%s: %q", tc.name, got)
		}
		for _, want := range tc.expect {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q lacks %q", tc.name, got, want)
			}
		}
	}
}

func TestNewLoggerLevel(t *testing.T) {
	var out bytes.Buffer
	cfg := config.Default()
	cfg.Log.Format = config.LogFormatText
	cfg.Log.Level = "debug"
	newLogger(cfg, &out, noEnv).Debug("shown")
	cfg.Log.Level = "error"
	out.Reset()
	logger := newLogger(cfg, &out, noEnv)
	logger.Warn("hidden")
	logger.Error("shown too")
	if strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), "shown too") {
		t.Fatalf("%q", out.String())
	}
}
