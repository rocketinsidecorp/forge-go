package telemetry

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// W3C Trace Context header names (https://www.w3.org/TR/trace-context/).
const (
	TraceparentHeader = "traceparent"
	TracestateHeader  = "tracestate"
)

const (
	traceparentLength   = 55 // "00-" + 32 + "-" + 16 + "-" + 2
	maxTracestateLength = 512
	maxTracestateItems  = 32
)

// ErrInvalidTraceparent reports a traceparent value that does not follow the
// W3C format. The value itself is never included: it is untrusted input.
var ErrInvalidTraceparent = errors.New("telemetry: invalid traceparent")

// ParseTraceparent parses a W3C traceparent header value strictly:
// lowercase hex only, no all-zero identifiers, version ff rejected, version
// 00 exactly 55 characters. Values of a higher version are accepted when
// their first 55 characters parse as version 00 and are followed by "-", as
// the specification requires for forward compatibility.
func ParseTraceparent(value string) (SpanContext, error) {
	if len(value) < traceparentLength {
		return SpanContext{}, ErrInvalidTraceparent
	}
	version, ok := hexByte(value[0], value[1])
	if !ok || version == 0xff || value[2] != '-' || value[35] != '-' || value[52] != '-' {
		return SpanContext{}, ErrInvalidTraceparent
	}
	if version == 0 && len(value) != traceparentLength {
		return SpanContext{}, ErrInvalidTraceparent
	}
	if version > 0 && len(value) > traceparentLength && value[traceparentLength] != '-' {
		return SpanContext{}, ErrInvalidTraceparent
	}
	var sc SpanContext
	if !decodeHex(sc.TraceID[:], value[3:35]) || !decodeHex(sc.SpanID[:], value[36:52]) {
		return SpanContext{}, ErrInvalidTraceparent
	}
	flags, ok := hexByte(value[53], value[54])
	if !ok || !sc.IsValid() {
		return SpanContext{}, ErrInvalidTraceparent
	}
	// Only the sampled bit is defined; unknown bits are not propagated.
	sc.Flags = TraceFlags(flags) & FlagSampled
	return sc, nil
}

// FormatTraceparent returns the version 00 traceparent value of sc, or "" for
// an invalid span context.
func FormatTraceparent(sc SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	var buf [traceparentLength]byte
	buf[0], buf[1], buf[2] = '0', '0', '-'
	encodeHex(buf[3:35], sc.TraceID[:])
	buf[35] = '-'
	encodeHex(buf[36:52], sc.SpanID[:])
	buf[52] = '-'
	encodeHex(buf[53:55], []byte{byte(sc.Flags & FlagSampled)})
	return string(buf[:])
}

// NormalizeTracestate validates a W3C tracestate list and returns it with
// optional whitespace and empty members removed. It reports false for a list
// that is malformed, has duplicate keys, more than 32 members or more than
// 512 characters; the specification then requires discarding the whole
// tracestate while keeping traceparent.
func NormalizeTracestate(value string) (string, bool) {
	if value == "" {
		return "", true
	}
	if len(value) > 2*maxTracestateLength {
		return "", false
	}
	members := make([]string, 0, 4)
	for member := range strings.SplitSeq(value, ",") {
		member = strings.Trim(member, " \t")
		if member == "" {
			continue
		}
		if len(members) == maxTracestateItems {
			return "", false
		}
		key, val, found := strings.Cut(member, "=")
		if !found || !validTracestateKey(key) || !validTracestateValue(val) {
			return "", false
		}
		for _, existing := range members {
			if existingKey, _, _ := strings.Cut(existing, "="); existingKey == key {
				return "", false
			}
		}
		members = append(members, member)
	}
	normalized := strings.Join(members, ",")
	if len(normalized) > maxTracestateLength {
		return "", false
	}
	return normalized, true
}

// Extract returns ctx carrying the remote span context found in header. A
// missing, repeated or malformed traceparent leaves ctx unchanged; a
// malformed tracestate is dropped while traceparent is kept.
func Extract(ctx context.Context, header http.Header) context.Context {
	values := header.Values(TraceparentHeader)
	if len(values) != 1 {
		return ctx
	}
	sc, err := ParseTraceparent(values[0])
	if err != nil {
		return ctx
	}
	if states := header.Values(TracestateHeader); len(states) > 0 {
		if state, ok := NormalizeTracestate(strings.Join(states, ",")); ok {
			sc.TraceState = state
		}
	}
	return ContextWithRemoteSpanContext(ctx, sc)
}

// Inject writes the traceparent and tracestate of ctx's active span into
// header, replacing values already there. Without a valid span context it
// leaves header unchanged, so a traceparent the caller set deliberately is
// kept.
func Inject(ctx context.Context, header http.Header) {
	sc := SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	header.Set(TraceparentHeader, FormatTraceparent(sc))
	if sc.TraceState != "" {
		header.Set(TracestateHeader, sc.TraceState)
	} else {
		header.Del(TracestateHeader)
	}
}

// validTracestateKey implements the W3C key grammar:
//
//	simple-key = lcalpha 0*255( lcalpha / DIGIT / "_" / "-"/ "*" / "/" )
//	multi-tenant-key = tenant-id "@" system-id
//	tenant-id = ( lcalpha / DIGIT ) 0*240( lcalpha / DIGIT / "_" / "-"/ "*" / "/" )
//	system-id = lcalpha 0*13( lcalpha / DIGIT / "_" / "-"/ "*" / "/" )
func validTracestateKey(key string) bool {
	tenant, system, multi := strings.Cut(key, "@")
	if !multi {
		return len(key) >= 1 && len(key) <= 256 && isLowerAlpha(key[0]) && keyChars(key[1:])
	}
	if len(tenant) < 1 || len(tenant) > 241 || (!isLowerAlpha(tenant[0]) && !isDigit(tenant[0])) || !keyChars(tenant[1:]) {
		return false
	}
	return len(system) >= 1 && len(system) <= 14 && isLowerAlpha(system[0]) && keyChars(system[1:])
}

func keyChars(s string) bool {
	for i := range len(s) {
		c := s[i]
		if !isLowerAlpha(c) && !isDigit(c) && c != '_' && c != '-' && c != '*' && c != '/' {
			return false
		}
	}
	return true
}

// validTracestateValue implements value = 0*255(chr) nblk-chr, where chr is
// 0x20 or nblk-chr, and nblk-chr is 0x21-0x7E except "," and "=".
func validTracestateValue(value string) bool {
	if len(value) < 1 || len(value) > 256 || value[len(value)-1] == ' ' {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if c < 0x20 || c > 0x7e || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

func isLowerAlpha(c byte) bool { return c >= 'a' && c <= 'z' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// lowerHexValue decodes one lowercase hex digit. Uppercase is invalid in
// traceparent.
func lowerHexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

func hexByte(high, low byte) (byte, bool) {
	h, ok1 := lowerHexValue(high)
	l, ok2 := lowerHexValue(low)
	return h<<4 | l, ok1 && ok2
}

func decodeHex(dst []byte, src string) bool {
	for i := range dst {
		b, ok := hexByte(src[2*i], src[2*i+1])
		if !ok {
			return false
		}
		dst[i] = b
	}
	return true
}

const hexDigits = "0123456789abcdef"

func encodeHex(dst, src []byte) {
	for i, b := range src {
		dst[2*i] = hexDigits[b>>4]
		dst[2*i+1] = hexDigits[b&0x0f]
	}
}
