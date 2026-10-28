package postgres_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
)

// BenchmarkPasswordVerify is scenario "password_verify" in docs/BENCHMARKS.md.
// It is deliberately slow: Argon2id with m=19456 KiB, t=2, p=1 (Forge-Rust's
// parameters, so the two frameworks' password_verify measure the same work)
// makes each guess of an attacker as expensive as one iteration of this
// benchmark.
// Do not optimize it; compare it only with itself. It lives here because
// this adapter registers the real Argon2id (the auth package's own tests use
// a fast stand-in).
func BenchmarkPasswordVerify(b *testing.B) {
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if match, _, err := auth.VerifyPassword(hash, "correct horse battery staple"); err != nil || !match {
			b.Fatalf("VerifyPassword = %v, %v", match, err)
		}
	}
}
