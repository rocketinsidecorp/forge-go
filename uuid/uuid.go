// Package uuid provides Forge's default UUIDv7 identifier.
package uuid

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrInvalid is returned for text or bytes that are not a UUIDv7.
var ErrInvalid = errors.New("invalid UUIDv7")

// UUID is a 128-bit RFC 9562 UUIDv7 value.
type UUID [16]byte

// Parse parses the canonical 36-character text of a UUIDv7; any other UUID version or variant is
// ErrInvalid.
func Parse(value string) (UUID, error) {
	var id UUID
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return id, ErrInvalid
	}
	compact := value[0:8] + value[9:13] + value[14:18] + value[19:23] + value[24:36]
	if _, err := hex.Decode(id[:], []byte(compact)); err != nil {
		return UUID{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if id.Version() != 7 || id.Variant() != 2 {
		return UUID{}, ErrInvalid
	}
	return id, nil
}

func (id UUID) String() string {
	var text [36]byte
	hex.Encode(text[0:8], id[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], id[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], id[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], id[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], id[10:16])
	return string(text[:])
}

// Version returns the UUID version; it is 7 for every value this package creates.
func (id UUID) Version() byte { return id[6] >> 4 }

// Variant returns 2 for the RFC 4122/RFC 9562 variant.
func (id UUID) Variant() byte {
	if id[8]&0xc0 == 0x80 {
		return 2
	}
	return 0
}

// Time returns the creation time in the UUID's millisecond timestamp, in UTC.
func (id UUID) Time() time.Time {
	milliseconds := int64(id[0])<<40 | int64(id[1])<<32 | int64(id[2])<<24 |
		int64(id[3])<<16 | int64(id[4])<<8 | int64(id[5])
	return time.UnixMilli(milliseconds).UTC()
}

// Compare compares the UUIDs bytewise: because the timestamp comes first, it orders UUIDs by
// creation time. It returns -1, 0 or 1.
func (id UUID) Compare(other UUID) int { return bytes.Compare(id[:], other[:]) }

// MarshalText implements encoding.TextMarshaler with the canonical text.
func (id UUID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler; it accepts only a UUIDv7.
func (id *UUID) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// MarshalJSON implements json.Marshaler with the canonical text as a JSON string.
func (id UUID) MarshalJSON() ([]byte, error) {
	text := id.String()
	result := make([]byte, len(text)+2)
	result[0], result[len(result)-1] = '"', '"'
	copy(result[1:], text)
	return result, nil
}

// UnmarshalJSON implements json.Unmarshaler; it accepts only a JSON string holding a UUIDv7.
func (id *UUID) UnmarshalJSON(data []byte) error {
	if len(data) != 38 || data[0] != '"' || data[len(data)-1] != '"' {
		return ErrInvalid
	}
	return id.UnmarshalText(data[1 : len(data)-1])
}
