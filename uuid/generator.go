package uuid

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrSequenceExhausted is returned when more UUIDs are requested within one millisecond than the
// monotonic sequence can number.
var ErrSequenceExhausted = errors.New("UUIDv7 sequence exhausted for millisecond")

// Generator creates UUIDv7 values that increase monotonically, also within one millisecond. It is
// safe for concurrent use.
type Generator struct {
	mu     sync.Mutex
	now    func() time.Time
	random io.Reader
	lastMS uint64
	randA  uint16
	randB  uint64
}

// NewGenerator returns a Generator that reads the time from clock and random bytes from random;
// both are required.
func NewGenerator(clock func() time.Time, random io.Reader) (*Generator, error) {
	if clock == nil || random == nil {
		return nil, errors.New("UUIDv7 clock and entropy source are required")
	}
	return &Generator{now: clock, random: random}, nil
}

var defaultGenerator, _ = NewGenerator(time.Now, rand.Reader)

// New returns a new UUIDv7 from the default generator, which uses the system clock and crypto/rand.
func New() (UUID, error) { return defaultGenerator.New() }

// NewV7 is New.
func NewV7() (UUID, error) { return New() }

// MustNew is New, and panics when the system entropy source fails. Use it in tests and fixtures.
func MustNew() UUID {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}

// New returns the next UUIDv7: the generator's clock in milliseconds, with a sequence that
// increases within the same millisecond or when the clock does not move forward.
func (g *Generator) New() (UUID, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	nowMS := uint64(g.now().UnixMilli())
	if nowMS > g.lastMS {
		var seed [10]byte
		if _, err := io.ReadFull(g.random, seed[:]); err != nil {
			return UUID{}, err
		}
		g.lastMS = nowMS
		g.randA = binary.BigEndian.Uint16(seed[0:2]) & 0x0fff
		g.randB = binary.BigEndian.Uint64(seed[2:10]) & 0x3fffffffffffffff
	} else {
		nowMS = g.lastMS
		g.randB++
		if g.randB > 0x3fffffffffffffff {
			g.randB = 0
			g.randA++
			if g.randA > 0x0fff {
				return UUID{}, ErrSequenceExhausted
			}
		}
	}

	var id UUID
	// The 48-bit big-endian millisecond timestamp occupies bytes 0-5.
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], nowMS<<16)
	copy(id[0:6], timestamp[0:6])
	binary.BigEndian.PutUint16(id[6:8], 0x7000|g.randA)
	binary.BigEndian.PutUint64(id[8:16], 0x8000000000000000|g.randB)
	return id, nil
}
