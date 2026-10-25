package uuid_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func TestScanReadsOnlyUUIDv7FromTheTypesDatabasesReturn(t *testing.T) {
	t.Parallel()
	want := uuid.MustNew()
	var v4 uuid.UUID
	copy(v4[:], []byte{1, 2, 3, 4, 5, 6, 0x40, 8, 0x80, 10, 11, 12, 13, 14, 15, 16}) // version 4

	accepted := map[string]any{
		"text":              want.String(),
		"text as bytes":     []byte(want.String()),
		"sixteen raw bytes": want[:],
	}
	for name, value := range accepted {
		var got uuid.UUID
		if err := got.Scan(value); err != nil || got != want {
			t.Errorf("%s: Scan = %v, %v; want %v", name, got, err, want)
		}
	}
	refused := map[string]any{
		"raw bytes of another version": v4[:],
		"text of another version":      "11111111-1111-4111-8111-111111111111",
		"text that is not a UUID":      "not-a-uuid",
		"an integer":                   int64(7),
		"nil":                          nil,
		"a time":                       time.Now(),
	}
	for name, value := range refused {
		sentinel := uuid.UUID{0xaa}
		got := sentinel
		if err := got.Scan(value); err == nil {
			t.Errorf("%s: Scan accepted %v", name, value)
		}
		if got != sentinel {
			t.Errorf("%s: a refused Scan changed the target to %v", name, got)
		}
	}
	if err := (&uuid.UUID{}).Scan(v4[:]); !errors.Is(err, uuid.ErrInvalid) {
		t.Errorf("raw bytes of version 4: %v, want ErrInvalid", err)
	}
	var nilTarget *uuid.UUID
	if err := nilTarget.Scan(want.String()); err == nil || !strings.Contains(err.Error(), "nil target") {
		t.Errorf("Scan into a nil pointer: %v", err)
	}
	if value, err := want.Value(); err != nil || value != want.String() {
		t.Errorf("Value = %v, %v", value, err)
	}
}

func TestGeneratorRefusesMissingDependenciesAndStopsWhenASequenceIsExhausted(t *testing.T) {
	t.Parallel()
	if _, err := uuid.NewGenerator(nil, bytes.NewReader(nil)); err == nil {
		t.Error("a nil clock was accepted")
	}
	if _, err := uuid.NewGenerator(time.Now, nil); err == nil {
		t.Error("a nil entropy source was accepted")
	}

	// Entropy of all ones starts the millisecond at the very end of its
	// sequence space: a second ID in the same millisecond has nowhere to go.
	frozen := time.UnixMilli(1_900_000_000_000)
	generator, err := uuid.NewGenerator(func() time.Time { return frozen }, bytes.NewReader(bytes.Repeat([]byte{0xff}, 10)))
	if err != nil {
		t.Fatal(err)
	}
	first, err := generator.New()
	if err != nil {
		t.Fatal(err)
	}
	if second, err := generator.New(); !errors.Is(err, uuid.ErrSequenceExhausted) {
		t.Fatalf("second ID in an exhausted millisecond = %v, %v; want ErrSequenceExhausted", second, err)
	}
	// A later millisecond starts a fresh sequence (with fresh entropy).
	later := time.UnixMilli(1_900_000_000_001)
	generator, err = uuid.NewGenerator(func() time.Time { return later }, bytes.NewReader(bytes.Repeat([]byte{0x01}, 10)))
	if err != nil {
		t.Fatal(err)
	}
	next, err := generator.New()
	if err != nil || next.String() <= first.String() {
		t.Errorf("next = %v, %v; want an ID after %v", next, err, first)
	}
	// Entropy that fails is an error, not a weak ID.
	broken, err := uuid.NewGenerator(time.Now, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if id, err := broken.New(); err == nil {
		t.Errorf("an ID was made without entropy: %v", id)
	}
}
