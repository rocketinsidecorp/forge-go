package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLogDefaults(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Log
	if got.Format != LogFormatAuto || got.Level != "info" || got.SlowQuery != 100*time.Millisecond || len(got.RedactKeys) != 0 {
		t.Fatalf("defaults %+v", got)
	}
}

func TestLogLoadsEnvironment(t *testing.T) {
	cfg, err := LoadWithLookup(lookupFrom(map[string]string{
		"FORGE_LOG_FORMAT":      " Pretty ",
		"FORGE_LOG_LEVEL":       "DEBUG",
		"FORGE_LOG_SLOW_QUERY":  "50ms",
		"FORGE_LOG_REDACT_KEYS": "iban, , Card ,",
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Log
	if got.Format != LogFormatPretty || got.Level != "DEBUG" || got.SlowQuery != 50*time.Millisecond ||
		strings.Join(got.RedactKeys, "|") != "iban|Card" {
		t.Fatalf("loaded %+v", got)
	}
	if level, ok := got.LevelValue(); !ok || level != slog.LevelDebug {
		t.Fatalf("level %v %v", level, ok)
	}
}

func TestLogRejectsInvalidValues(t *testing.T) {
	for key, value := range map[string]string{
		"FORGE_LOG_FORMAT":     "xml",
		"FORGE_LOG_LEVEL":      "loud",
		"FORGE_LOG_SLOW_QUERY": "soon",
	} {
		if _, err := LoadWithLookup(lookupFrom(map[string]string{key: value})); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s=%s: error %v", key, value, err)
		}
	}
	if _, err := LoadWithLookup(lookupFrom(map[string]string{"FORGE_LOG_SLOW_QUERY": "0s"})); err == nil {
		t.Fatal("a zero slow query threshold is accepted")
	}
}

func TestLogLevelValue(t *testing.T) {
	for name, want := range map[string]slog.Level{"": slog.LevelInfo, "info": slog.LevelInfo, "warn": slog.LevelWarn, "Warning": slog.LevelWarn, "error": slog.LevelError, "debug": slog.LevelDebug} {
		if got, ok := (Log{Level: name}).LevelValue(); !ok || got != want {
			t.Errorf("%q: %v %v", name, got, ok)
		}
	}
	if _, ok := (Log{Level: "x"}).LevelValue(); ok {
		t.Error("an unknown level is accepted")
	}
}

func TestEventLevelFollowsEnvironment(t *testing.T) {
	for environment, want := range map[Environment]slog.Level{
		Development: slog.LevelInfo, Test: slog.LevelDebug, Staging: slog.LevelDebug, Production: slog.LevelDebug,
	} {
		if got := (Config{Environment: environment}).EventLevel(); got != want {
			t.Errorf("%s: %v", environment, got)
		}
	}
}
