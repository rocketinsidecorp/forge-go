package telemetry

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxAttributeValueLength bounds string attribute values in bytes; longer
// values are truncated at a UTF-8 boundary.
const MaxAttributeValueLength = 1024

// ValueKind is the type held by a Value.
type ValueKind uint8

// The kinds of Value.
const (
	KindString ValueKind = iota + 1
	KindInt64
	KindFloat64
	KindBool
)

// Value is an attribute value: a string, int64, float64 or bool. Values are
// immutable and comparable.
type Value struct {
	kind ValueKind
	str  string
	num  uint64
}

// Kind returns the type held by v.
func (v Value) Kind() ValueKind { return v.kind }

// AsString returns the string value, or "" for another kind.
func (v Value) AsString() string { return v.str }

// AsInt64 returns the integer value, or 0 for another kind.
func (v Value) AsInt64() int64 {
	if v.kind != KindInt64 {
		return 0
	}
	return int64(v.num) // #nosec G115 -- round-trips the bits stored by Int64.
}

// AsFloat64 returns the float value, or 0 for another kind.
func (v Value) AsFloat64() float64 {
	if v.kind != KindFloat64 {
		return 0
	}
	return math.Float64frombits(v.num)
}

// AsBool returns the bool value, or false for another kind.
func (v Value) AsBool() bool { return v.kind == KindBool && v.num == 1 }

// Emit returns the value formatted for text output.
func (v Value) Emit() string {
	switch v.kind {
	case KindString:
		return v.str
	case KindInt64:
		return strconv.FormatInt(v.AsInt64(), 10)
	case KindFloat64:
		return strconv.FormatFloat(v.AsFloat64(), 'g', -1, 64)
	case KindBool:
		return strconv.FormatBool(v.AsBool())
	}
	return ""
}

// Attribute is a key-value pair attached to a span, an event or a resource.
// Keys follow OpenTelemetry semantic conventions where one exists.
type Attribute struct {
	Key   string
	Value Value
}

// String returns a string attribute. The value is truncated to
// MaxAttributeValueLength bytes.
func String(key, value string) Attribute {
	return Attribute{Key: key, Value: Value{kind: KindString, str: truncate(value, MaxAttributeValueLength)}}
}

// Int returns an integer attribute.
func Int(key string, value int) Attribute { return Int64(key, int64(value)) }

// Int64 returns an integer attribute.
func Int64(key string, value int64) Attribute {
	return Attribute{Key: key, Value: Value{kind: KindInt64, num: uint64(value)}} // #nosec G115 -- stores the bits; AsInt64 restores them.
}

// Float64 returns a floating-point attribute.
func Float64(key string, value float64) Attribute {
	return Attribute{Key: key, Value: Value{kind: KindFloat64, num: math.Float64bits(value)}}
}

// Bool returns a boolean attribute.
func Bool(key string, value bool) Attribute {
	var n uint64
	if value {
		n = 1
	}
	return Attribute{Key: key, Value: Value{kind: KindBool, num: n}}
}

// truncate shortens s to at most limit bytes without splitting a UTF-8
// sequence, and replaces invalid UTF-8, which OTLP JSON cannot carry. A
// shortened value is a copy, so a queued span or log record does not keep
// the caller's whole string alive.
func truncate(s string, limit int) string {
	cut := len(s) > limit
	if cut {
		end := limit
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		s = s[:end]
	}
	switch {
	case !utf8.ValidString(s):
		return strings.ToValidUTF8(s, "\uFFFD") // a new string
	case cut:
		return strings.Clone(s)
	}
	return s
}
