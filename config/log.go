package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// LogFormat is how the application writes its logs (FORGE_LOG_FORMAT).
type LogFormat string

// The supported log formats. Auto picks pretty on a terminal in development,
// text elsewhere in development and test, and JSON in staging and
// production.
const (
	LogFormatAuto   LogFormat = "auto"
	LogFormatPretty LogFormat = "pretty"
	LogFormatText   LogFormat = "text"
	LogFormatJSON   LogFormat = "json"
)

// defaultSlowQuery is the duration above which a SQL line is marked slow.
const defaultSlowQuery = 100 * time.Millisecond

// Log configures the application log: its format, the slowest query that is
// not flagged and the attribute keys that are never written.
type Log struct {
	// Format is FORGE_LOG_FORMAT: auto (default), pretty, text or json.
	Format LogFormat
	// Level is FORGE_LOG_LEVEL, the lowest level written: debug, info
	// (default), warn or error.
	Level string
	// SlowQuery is FORGE_LOG_SLOW_QUERY (default 100ms): a SQL line of a
	// statement slower than this is marked slow, and yellow in the pretty
	// format.
	SlowQuery time.Duration
	// RedactKeys is FORGE_LOG_REDACT_KEYS, a comma-separated list of key
	// fragments added to the default ones (password, secret, token, email...):
	// the value of every log attribute whose key contains one of them,
	// whatever its case, is written as [REDACTED].
	RedactKeys []string
}

func defaultLog() Log {
	return Log{Format: LogFormatAuto, Level: "info", SlowQuery: defaultSlowQuery}
}

func loadLog(lookup func(string) (string, bool), cfg *Log) error {
	if value, ok := lookup("FORGE_LOG_FORMAT"); ok {
		cfg.Format = LogFormat(strings.ToLower(strings.TrimSpace(value)))
	}
	setString(lookup, "FORGE_LOG_LEVEL", &cfg.Level)
	if value, ok := lookup("FORGE_LOG_SLOW_QUERY"); ok {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return invalidValue("FORGE_LOG_SLOW_QUERY", "a duration such as 50ms")
		}
		cfg.SlowQuery = parsed
	}
	if value, ok := lookup("FORGE_LOG_REDACT_KEYS"); ok {
		cfg.RedactKeys = nil
		for _, key := range strings.Split(value, ",") {
			if key = strings.TrimSpace(key); key != "" {
				cfg.RedactKeys = append(cfg.RedactKeys, key)
			}
		}
	}
	return nil
}

func (l Log) validate() error {
	switch l.Format {
	case LogFormatAuto, LogFormatPretty, LogFormatText, LogFormatJSON:
	default:
		return fmt.Errorf("unsupported FORGE_LOG_FORMAT %q: expected auto, pretty, text or json", l.Format)
	}
	if _, ok := l.LevelValue(); !ok {
		return fmt.Errorf("unsupported FORGE_LOG_LEVEL %q: expected debug, info, warn or error", l.Level)
	}
	if l.SlowQuery <= 0 {
		return errors.New("FORGE_LOG_SLOW_QUERY must be positive")
	}
	return nil
}

// LevelValue returns the slog level Level names, and false when it names
// none.
func (l Log) LevelValue() (slog.Level, bool) {
	switch strings.ToLower(l.Level) {
	case "debug":
		return slog.LevelDebug, true
	case "info", "":
		return slog.LevelInfo, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// EventLevel returns the level of the request, SQL and job lines, which
// follow a request or a job step by step: Info in development, where they
// are the development log, and Debug elsewhere, where FORGE_LOG_LEVEL=debug
// turns them on.
func (c Config) EventLevel() slog.Level {
	if c.Environment == Development {
		return slog.LevelInfo
	}
	return slog.LevelDebug
}
