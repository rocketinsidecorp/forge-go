package config

import (
	"crypto/subtle"
	"log/slog"
)

const redacted = "[REDACTED]"

// Secret holds a sensitive value such as a database URL. Every formatting,
// logging and serialization path prints [REDACTED]; only Reveal returns the
// value, which makes each use explicit and easy to audit.
//
// The value sits behind a pointer on purpose: fmt cannot call String or
// GoString on an unexported struct field and would print the fields it
// finds, so a Secret held in an unexported field (`%+v` of a struct) would
// leak its text. Behind a pointer it prints an address. Consequently two
// Secrets must be compared with Equal, not ==, which compares identity.
type Secret struct{ value *string }

func NewSecret(value string) Secret {
	if value == "" {
		return Secret{}
	}
	return Secret{value: &value}
}

// Reveal returns the underlying value. Pass it straight to the component that
// needs it; never log or return it to clients.
func (s Secret) Reveal() string {
	if s.value == nil {
		return ""
	}
	return *s.value
}

func (s Secret) IsZero() bool { return s.value == nil }

// Equal reports whether both secrets hold the same value, in constant time
// for values of equal length.
func (s Secret) Equal(other Secret) bool {
	return subtle.ConstantTimeCompare([]byte(s.Reveal()), []byte(other.Reveal())) == 1
}

func (Secret) String() string { return redacted }

func (Secret) GoString() string { return redacted }

func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
