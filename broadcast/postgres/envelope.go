package postgres

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// PostgreSQL limits a NOTIFY payload to less than 8000 bytes, so a message is
// sent as one or more chunks, each its own notification, all queued in one
// transaction. Notifications of a transaction are delivered together and in
// order, which lets the receiver reassemble them without timers.
//
// A chunk is one line of ASCII fields separated by single spaces:
//
//	v1 ORIGIN MESSAGE-ID INDEX COUNT CHANNEL-KEY BASE64-DATA
//
// ORIGIN identifies the publishing broker ("-" for PublishTx), MESSAGE-ID is
// random hex, INDEX/COUNT number the chunks, CHANNEL-KEY is
// broadcast.Channel.Key (no spaces) and the data is base64 (standard,
// padded), which keeps arbitrary bytes safe inside a notification payload.
const (
	envelopeVersion = "v1"
	// chunkBytes of message data per notification: 4500 bytes become 6000
	// base64 characters, leaving ample room under the 8000 byte limit for the
	// header and the longest channel key (about 140 bytes).
	chunkBytes = 4500
	// maxNotifyBytes is the largest payload Publish will send, a safety
	// margin below PostgreSQL's limit.
	maxNotifyBytes = 7500
)

type chunk struct {
	origin string
	id     string
	index  int
	count  int
	key    string
	data   []byte
}

var errMalformed = errors.New("malformed notification")

func (c chunk) encode() string {
	return strings.Join([]string{
		envelopeVersion, c.origin, c.id, strconv.Itoa(c.index), strconv.Itoa(c.count), c.key,
		base64.StdEncoding.EncodeToString(c.data),
	}, " ")
}

func parseChunk(payload string, maxChunks int) (chunk, error) {
	fields := strings.Split(payload, " ")
	if len(fields) != 7 || fields[0] != envelopeVersion {
		return chunk{}, errMalformed
	}
	index, err := strconv.Atoi(fields[3])
	if err != nil {
		return chunk{}, errMalformed
	}
	count, err := strconv.Atoi(fields[4])
	if err != nil || count < 1 || count > maxChunks || index < 0 || index >= count {
		return chunk{}, errMalformed
	}
	data, err := base64.StdEncoding.Strict().DecodeString(fields[6])
	if err != nil || len(data) > chunkBytes {
		return chunk{}, errMalformed
	}
	if fields[1] == "" || fields[2] == "" || fields[5] == "" {
		return chunk{}, errMalformed
	}
	return chunk{origin: fields[1], id: fields[2], index: index, count: count, key: fields[5], data: data}, nil
}

// split cuts data into chunks of at most chunkBytes; empty data is one empty
// chunk, so every message produces at least one notification.
func split(origin, id, key string, data []byte) []chunk {
	count := max(1, (len(data)+chunkBytes-1)/chunkBytes)
	chunks := make([]chunk, 0, count)
	for index := range count {
		start := index * chunkBytes
		end := min(len(data), start+chunkBytes)
		chunks = append(chunks, chunk{origin: origin, id: id, index: index, count: count, key: key, data: data[start:end]})
	}
	return chunks
}

// assembler reassembles chunked messages with bounded memory. It is used by
// the single listener goroutine and needs no locking.
type assembler struct {
	maxPending int
	maxBytes   int
	lifetime   time.Duration
	pending    map[string]*partial
	order      []string // insertion order, for eviction
	now        func() time.Time
}

type partial struct {
	key     string
	origin  string
	parts   [][]byte
	bytes   int
	started time.Time
}

func newAssembler(maxPending, maxBytes int, lifetime time.Duration, now func() time.Time) *assembler {
	return &assembler{maxPending: maxPending, maxBytes: maxBytes, lifetime: lifetime, pending: map[string]*partial{}, now: now}
}

// add takes one chunk and returns the finished message, if this chunk
// completed it. Out-of-order, oversized or inconsistent chunks discard their
// message.
func (a *assembler) add(c chunk) (key string, data []byte, origin string, done bool, err error) {
	a.sweep()
	if c.count == 1 {
		return c.key, c.data, c.origin, true, nil
	}
	id := c.origin + "/" + c.id
	current := a.pending[id]
	if c.index == 0 {
		if current != nil {
			a.drop(id)
		}
		for len(a.pending) >= a.maxPending && len(a.order) > 0 {
			a.drop(a.order[0])
		}
		current = &partial{key: c.key, origin: c.origin, parts: make([][]byte, 0, c.count), started: a.now()}
		a.pending[id] = current
		a.order = append(a.order, id)
	}
	if current == nil || c.index != len(current.parts) || c.key != current.key || current.bytes+len(c.data) > a.maxBytes {
		a.drop(id)
		return "", nil, "", false, fmt.Errorf("%w: unexpected chunk %d of %d", errMalformed, c.index, c.count)
	}
	current.parts = append(current.parts, c.data)
	current.bytes += len(c.data)
	if c.index != c.count-1 {
		return "", nil, "", false, nil
	}
	joined := make([]byte, 0, current.bytes)
	for _, part := range current.parts {
		joined = append(joined, part...)
	}
	a.drop(id)
	return current.key, joined, current.origin, true, nil
}

func (a *assembler) drop(id string) {
	delete(a.pending, id)
	for i, queued := range a.order {
		if queued == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
}

func (a *assembler) sweep() {
	cutoff := a.now().Add(-a.lifetime)
	for len(a.order) > 0 {
		oldest := a.pending[a.order[0]]
		if oldest != nil && oldest.started.After(cutoff) {
			return
		}
		a.drop(a.order[0])
	}
}
