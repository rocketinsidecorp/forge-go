// Package storage is Forge's file storage, the equivalent of Rails' Active
// Storage (ADR 0028): a Storage port, an adapter for local disk
// (storage/disk), HMAC-signed time-limited links, an HTTP handler that serves
// them with safe headers, and a strict multipart upload helper.
//
// Stored files are untrusted input. The helpers therefore never trust a
// client-declared content type or file name: Receive checks the content
// against an allow-list, Handler serves anything that a browser could execute
// or render as a page as a download (Content-Disposition: attachment) with
// X-Content-Type-Options: nosniff, and links are only valid for the key they
// were signed for until they expire.
//
// There is no S3-compatible adapter yet. A future adapter implements Storage
// in its own package (the port deliberately needs nothing but streams and
// keys), and Signer and Handler work with it unchanged.
package storage

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// Storage keeps named blobs. Implementations are safe for concurrent use.
//
// Keys are validated by ValidateKey in every method. A Put replaces any
// existing blob of the key. Callers generate unguessable keys for user content
// (Receive uses UUIDv7) and keep the original file name in Info.Filename, not in
// the key.
type Storage interface {
	// Put stores body under key and returns what was stored. It reads at most
	// opts.MaxBytes bytes and fails with ErrTooLarge, storing nothing, when the
	// body is longer. A failed Put leaves no partial blob.
	Put(ctx context.Context, key string, body io.Reader, opts PutOptions) (Info, error)
	// Open returns the content of key. The caller closes it. When the returned
	// reader is also an io.Seeker, the Handler serves byte ranges from it.
	Open(ctx context.Context, key string) (io.ReadCloser, Info, error)
	// Stat describes key without opening it.
	Stat(ctx context.Context, key string) (Info, error)
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// PutOptions describes the blob being stored.
type PutOptions struct {
	// ContentType is a media type such as "image/png" (parameters are
	// dropped). Empty means application/octet-stream.
	ContentType string
	// Filename is the original file name, for downloads. It is sanitized.
	Filename string
	// MaxBytes is the size limit, required and positive.
	MaxBytes int64
}

// Info describes a stored blob.
type Info struct {
	Key         string
	Size        int64
	ContentType string
	Filename    string
	// SHA256 is the hex digest of the content, when the adapter knows it.
	SHA256  string
	ModTime time.Time
}

// OctetStream is the content type of anything unidentified.
const OctetStream = "application/octet-stream"

// Errors. They are faults, so web.Error maps them to the right status.
var (
	ErrNotFound        = fault.New("storage_not_found", "file not found", fault.CategoryNotFound, http.StatusNotFound)
	ErrTooLarge        = fault.New("storage_too_large", "file too large", fault.CategoryInvalid, http.StatusRequestEntityTooLarge)
	ErrInvalidKey      = fault.New("storage_invalid_key", "invalid storage key", fault.CategoryInvalid, http.StatusBadRequest)
	ErrUnsupportedType = fault.New("unsupported_media_type", "file type not allowed", fault.CategoryInvalid, http.StatusUnsupportedMediaType)
	ErrInvalidLink     = fault.New("link_invalid", "invalid or tampered link", fault.CategoryForbidden, http.StatusForbidden)
	ErrExpiredLink     = fault.New("link_expired", "link expired", fault.CategoryForbidden, http.StatusForbidden)
)

// Key limits. A key is slash-separated segments of ASCII letters, digits,
// ".", "_" and "-". Segments cannot be empty or start with a dot, so a valid
// key can never contain "..", a hidden-file name, a backslash, a drive letter
// or an absolute path. Adapters may rely on this and still defend in depth.
const (
	MaxKeyLength     = 512
	maxSegmentLength = 128
	maxKeyDepth      = 8
)

// ValidateKey reports whether key is a valid storage key. The error never
// contains the key.
func ValidateKey(key string) error {
	if key == "" || len(key) > MaxKeyLength {
		return ErrInvalidKey
	}
	segments := 1
	segmentLength := 0
	for i := range len(key) {
		c := key[i]
		switch {
		case c == '/':
			if segmentLength == 0 {
				return ErrInvalidKey // empty segment: leading, trailing or doubled slash
			}
			segments++
			segmentLength = 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			segmentLength++
		case c == '.':
			if segmentLength == 0 {
				return ErrInvalidKey // segment starting with a dot: ".", "..", ".hidden"
			}
			segmentLength++
		default:
			return ErrInvalidKey
		}
		if segmentLength > maxSegmentLength {
			return ErrInvalidKey
		}
	}
	if segmentLength == 0 || segments > maxKeyDepth {
		return ErrInvalidKey
	}
	return nil
}

// NormalizeContentType returns the lower-case "type/subtype" of a media type,
// without parameters, or an error when s is not a media type. Empty s is
// application/octet-stream.
func NormalizeContentType(s string) (string, error) {
	if strings.TrimSpace(s) == "" {
		return OctetStream, nil
	}
	mediaType, _, err := mime.ParseMediaType(s)
	if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
		return "", ErrUnsupportedType
	}
	if mediaType == "" || len(mediaType) > 127 || !strings.Contains(mediaType, "/") {
		return "", ErrUnsupportedType
	}
	return mediaType, nil
}

// SanitizeFilename reduces a client-supplied file name to something safe to
// show and to put in a Content-Disposition header: no directories, control or
// bidirectional-override characters, quotes or leading dots, at most 255
// bytes. It may return "".
func SanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), r == '"', isFormatting(r):
			continue
		}
		if b.Len()+utf8.RuneLen(r) > 255 {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimLeft(strings.TrimSpace(b.String()), ".")
}

// isFormatting reports whether r is a line or paragraph separator or a
// bidirectional control (U+2028, U+2029, U+202A-202E, U+2066-2069, U+200E,
// U+200F, U+061C): invisible characters that reorder or hide text, which a file
// name must not carry. They are compared by number so that this source has no
// such character in it.
func isFormatting(r rune) bool {
	switch {
	case r == 0x2028, r == 0x2029, r == 0x200e, r == 0x200f, r == 0x061c:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// inlineSafe are the content types a browser renders without running
// anything of the file's own: raster images. Everything else, notably HTML,
// SVG, XML, scripts and PDF (whose viewers run embedded code), is served as a
// download.
var inlineSafe = map[string]bool{
	"image/png":    true,
	"image/jpeg":   true,
	"image/gif":    true,
	"image/webp":   true,
	"image/avif":   true,
	"image/bmp":    true,
	"image/x-icon": true,
}

// InlineSafe reports whether files of the content type may be displayed inline
// by default.
func InlineSafe(contentType string) bool { return inlineSafe[contentType] }
