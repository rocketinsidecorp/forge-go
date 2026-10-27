package telemetry

import (
	"log/slog"
	"strings"
)

// Redacted is what a redacted attribute's value is replaced with.
const Redacted = "[REDACTED]"

// DefaultRedactKeys are the key fragments whose attributes are never logged
// with their value: a key that contains one of them, whatever its case, is
// redacted ("db_password", "Authorization", "user.email").
var DefaultRedactKeys = []string{
	"password", "passwd", "secret", "token", "authorization", "cookie",
	"api_key", "apikey", "session", "email", "phone", "cpf", "cnpj",
}

// Redactor returns a slog.HandlerOptions.ReplaceAttr that replaces the value
// of every attribute whose key, or the name of a group it is in, contains
// one of DefaultRedactKeys or extra, case-insensitively, with Redacted. The
// key stays, so the log still says that the attribute was present. It
// applies to every format; NewOutputHandler reports it to the log exporter,
// which therefore never carries what standard output hides.
//
//	opts := &slog.HandlerOptions{ReplaceAttr: telemetry.Redactor("iban")}
func Redactor(extra ...string) func(groups []string, a slog.Attr) slog.Attr {
	fragments := make([]string, 0, len(DefaultRedactKeys)+len(extra))
	fragments = append(fragments, DefaultRedactKeys...)
	for _, key := range extra {
		if key = strings.ToLower(strings.TrimSpace(key)); key != "" {
			fragments = append(fragments, key)
		}
	}
	matches := func(key string) bool {
		key = strings.ToLower(key)
		for _, fragment := range fragments {
			if strings.Contains(key, fragment) {
				return true
			}
		}
		return false
	}
	return func(groups []string, a slog.Attr) slog.Attr {
		// slog does not pass group attributes here, only their members: a
		// member of a redacted group is redacted whatever its own key.
		if a.Value.Kind() == slog.KindGroup {
			return a
		}
		redact := matches(a.Key)
		for _, group := range groups {
			redact = redact || matches(group)
		}
		if redact {
			return slog.String(a.Key, Redacted)
		}
		return a
	}
}
