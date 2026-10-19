package postgres

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestSplitAndReassemble(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 1, chunkBytes - 1, chunkBytes, chunkBytes + 1, 3*chunkBytes + 17} {
		data := bytes.Repeat([]byte("é<\n\r\x00x"), size/7+1)[:size]
		chunks := split("origin", "id1", "global/posts", data)
		if want := max(1, (size+chunkBytes-1)/chunkBytes); len(chunks) != want {
			t.Fatalf("size %d: %d chunks, want %d", size, len(chunks), want)
		}
		assembly := newAssembler(8, 1<<20, time.Minute, time.Now)
		var got []byte
		finished := 0
		for _, c := range chunks {
			payload := c.encode()
			if len(payload) > maxNotifyBytes {
				t.Fatalf("size %d: payload of %d bytes exceeds the limit", size, len(payload))
			}
			parsed, err := parseChunk(payload, 1000)
			if err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			key, data, origin, done, err := assembly.add(parsed)
			if err != nil {
				t.Fatalf("size %d: %v", size, err)
			}
			if done {
				finished++
				got = data
				if key != "global/posts" || origin != "origin" {
					t.Fatalf("key=%q origin=%q", key, origin)
				}
			}
		}
		if finished != 1 || !bytes.Equal(got, data) {
			t.Fatalf("size %d: finished=%d, data differs", size, finished)
		}
	}
}

// The longest channel key still fits one notification at full chunk size.
func TestLongestChunkFitsPostgresLimit(t *testing.T) {
	t.Parallel()
	key := "0190b7b8-aaaa-7bbb-8ccc-0123456789ab/" + strings.Repeat("a", 96)
	c := chunk{origin: "-", id: "0123456789abcdef", index: 999, count: 1000, key: key, data: bytes.Repeat([]byte{0xff}, chunkBytes)}
	if n := len(c.encode()); n > maxNotifyBytes || n >= 8000 {
		t.Fatalf("payload is %d bytes", n)
	}
}

func TestParseChunkRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()
	good := chunk{origin: "o", id: "i", index: 0, count: 2, key: "global/a", data: []byte("x")}.encode()
	if _, err := parseChunk(good, 10); err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{
		"empty":            "",
		"wrong version":    strings.Replace(good, "v1", "v2", 1),
		"missing field":    "v1 o i 0 2 global/a",
		"extra field":      good + " extra",
		"bad index":        "v1 o i x 2 global/a eA==",
		"negative index":   "v1 o i -1 2 global/a eA==",
		"index beyond":     "v1 o i 2 2 global/a eA==",
		"zero count":       "v1 o i 0 0 global/a eA==",
		"too many chunks":  "v1 o i 0 11 global/a eA==",
		"bad base64":       "v1 o i 0 2 global/a !!!!",
		"unpadded base64":  "v1 o i 0 2 global/a eA",
		"empty origin":     "v1  i 0 2 global/a eA==",
		"oversized chunk":  "v1 o i 0 2 global/a " + strings.Repeat("QUJD", chunkBytes),
		"extra whitespace": "v1 o i 0 2 global/a  eA==",
	} {
		if _, err := parseChunk(payload, 10); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAssemblerDiscardsInconsistentMessages(t *testing.T) {
	t.Parallel()
	assembly := newAssembler(8, 1<<20, time.Minute, time.Now)
	part := func(id string, index, count int, key string) chunk {
		return chunk{origin: "o", id: id, index: index, count: count, key: key, data: []byte("x")}
	}
	// A chunk that continues a message never started is dropped.
	if _, _, _, done, err := assembly.add(part("a", 1, 3, "global/a")); done || err == nil {
		t.Fatalf("orphan chunk: done=%v err=%v", done, err)
	}
	// Out of order: 0 then 2.
	_, _, _, _, _ = assembly.add(part("b", 0, 3, "global/a"))
	if _, _, _, done, err := assembly.add(part("b", 2, 3, "global/a")); done || err == nil {
		t.Fatalf("out of order: done=%v err=%v", done, err)
	}
	if len(assembly.pending) != 0 {
		t.Fatalf("pending = %d", len(assembly.pending))
	}
	// A different channel key in a later chunk is a forgery attempt.
	_, _, _, _, _ = assembly.add(part("c", 0, 2, "global/a"))
	if _, _, _, done, err := assembly.add(part("c", 1, 2, "global/b")); done || err == nil {
		t.Fatalf("key switch: done=%v err=%v", done, err)
	}
	// Restarting a message discards the earlier attempt.
	_, _, _, _, _ = assembly.add(part("d", 0, 2, "global/a"))
	_, _, _, _, _ = assembly.add(part("d", 0, 2, "global/a"))
	if len(assembly.pending) != 1 {
		t.Fatalf("pending = %d", len(assembly.pending))
	}
}

func TestAssemblerIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assembly := newAssembler(3, 10, time.Minute, func() time.Time { return now })
	start := func(id string) {
		_, _, _, _, _ = assembly.add(chunk{origin: "o", id: id, index: 0, count: 2, key: "global/a", data: []byte("x")})
	}
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		start(id)
	}
	if len(assembly.pending) != 3 {
		t.Fatalf("pending = %d, want the 3-message cap", len(assembly.pending))
	}
	if _, ok := assembly.pending["o/1"]; ok {
		t.Fatal("the oldest message was not evicted")
	}
	// Abandoned messages expire.
	now = now.Add(2 * time.Minute)
	start("6")
	if len(assembly.pending) != 1 {
		t.Fatalf("pending = %d after expiry", len(assembly.pending))
	}
	// A message over the byte budget is discarded.
	big := newAssembler(3, 4, time.Minute, time.Now)
	_, _, _, _, _ = big.add(chunk{origin: "o", id: "x", index: 0, count: 3, key: "global/a", data: []byte("abc")})
	if _, _, _, done, err := big.add(chunk{origin: "o", id: "x", index: 1, count: 3, key: "global/a", data: []byte("de")}); done || err == nil {
		t.Fatalf("over budget: done=%v err=%v", done, err)
	}
	if len(big.pending) != 0 {
		t.Fatal("over-budget message was kept")
	}
}
