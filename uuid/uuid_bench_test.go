package uuid_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/uuid"
)

func BenchmarkNew(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := uuid.New(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkString(b *testing.B) {
	id := uuid.MustNew()
	b.ReportAllocs()
	for b.Loop() {
		_ = id.String()
	}
}
