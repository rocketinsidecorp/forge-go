package uuid_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

// BenchmarkUUIDv7 is scenario "uuid_v7" in docs/BENCHMARKS.md: generation of
// an identifier and its canonical text form.
func BenchmarkUUIDv7(b *testing.B) {
	b.Run("new", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := uuid.New(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("string", func(b *testing.B) {
		id := uuid.MustNew()
		b.ReportAllocs()
		for b.Loop() {
			_ = id.String()
		}
	})
}
