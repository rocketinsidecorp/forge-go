// Package pagination implements keyset (cursor) pagination over UUIDv7
// identifiers. Because UUIDv7 values sort by creation time, "WHERE id < $cursor
// ORDER BY id DESC LIMIT $n" pages through records newest first with an index
// seek, at constant cost regardless of depth, and without OFFSET's skipped or
// duplicated rows under concurrent inserts.
package pagination

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"

	"github.com/rocketinsidecorp/forge-go/fault"
	"github.com/rocketinsidecorp/forge-go/uuid"
)

const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Request is a validated page request. After is the zero UUID for the first
// page.
type Request struct {
	After uuid.UUID
	Limit int
}

// HasCursor reports whether the request continues from a previous page.
func (r Request) HasCursor() bool { return r.After != uuid.UUID{} }

// Page is a page of items plus the cursor for the next one, empty on the last page.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// FromQuery reads the "cursor" and "limit" query parameters.
func FromQuery(query url.Values) (Request, error) {
	request := Request{Limit: DefaultLimit}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > MaxLimit {
			return Request{}, fault.New("invalid_limit", "limit must be an integer between 1 and "+strconv.Itoa(MaxLimit), fault.CategoryInvalid, http.StatusBadRequest)
		}
		request.Limit = limit
	}
	if raw := query.Get("cursor"); raw != "" {
		after, err := DecodeCursor(raw)
		if err != nil {
			return Request{}, err
		}
		request.After = after
	}
	return request, nil
}

// EncodeCursor returns an opaque, URL-safe cursor for id.
func EncodeCursor(id uuid.UUID) string { return base64.RawURLEncoding.EncodeToString(id[:]) }

// DecodeCursor parses a cursor produced by EncodeCursor.
func DecodeCursor(cursor string) (uuid.UUID, error) {
	invalid := fault.New("invalid_cursor", "cursor is invalid", fault.CategoryInvalid, http.StatusBadRequest)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil || len(raw) != len(uuid.UUID{}) {
		return uuid.UUID{}, invalid
	}
	var id uuid.UUID
	copy(id[:], raw)
	// Only the canonical encoding is accepted: the base64 decoder skips
	// \r and \n even in strict mode, which would let many strings name the
	// same position.
	if id.Version() != 7 || id.Variant() != 2 || EncodeCursor(id) != cursor {
		return uuid.UUID{}, invalid
	}
	return id, nil
}

// NewPage builds a page from up to limit+1 rows fetched by the caller. The
// extra row only signals that another page exists and is not returned.
func NewPage[T any](rows []T, limit int, id func(T) uuid.UUID) Page[T] {
	if rows == nil {
		rows = []T{}
	}
	if limit < 1 || len(rows) <= limit {
		return Page[T]{Items: rows}
	}
	items := rows[:limit]
	return Page[T]{Items: items, NextCursor: EncodeCursor(id(items[len(items)-1]))}
}
