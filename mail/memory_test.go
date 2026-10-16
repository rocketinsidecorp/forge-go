package mail_test

import (
	"runtime"
	"testing"

	forgemail "github.com/rocketinsidecorp/forge-go/mail"
)

// Encoding a large attachment must not hold many full copies of it at once.
// TotalAlloc counts every byte allocated during the call, so it bounds the
// number of full-size passes the encoder makes.
func TestEncodeLargeAttachmentAllocatesFewCopies(t *testing.T) {
	msg := baseMessage()
	const size = 4 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	msg.Attachments = []forgemail.Attachment{{Filename: "big.bin", Data: data}}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	encoded, err := forgemail.Encode(msg)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	// The encoded message is ~1.37x the input and must exist once as the
	// result; the multipart assembly needs at most one more such copy.
	if limit := uint64(size) * 3; allocated > limit {
		t.Fatalf("Encode allocated %.1fx the attachment size (%d bytes), want at most 3x", float64(allocated)/size, allocated)
	}
	if len(encoded.Data) < size {
		t.Fatalf("encoded size %d", len(encoded.Data))
	}
}

func TestDefaultLimitsAreModest(t *testing.T) {
	t.Parallel()
	limits := forgemail.DefaultLimits()
	if limits.MaxAttachmentBytes > 8<<20 || limits.MaxMessageBytes > 12<<20 || limits.MaxBodyBytes > 2<<20 {
		t.Fatalf("limits too large for in-memory encoding: %+v", limits)
	}
}
