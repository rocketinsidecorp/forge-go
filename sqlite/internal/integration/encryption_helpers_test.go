package integration_test

import (
	"bytes"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

// testKey and newCipher are encryption's own test helpers
// (encryption/encryption_test.go), for its tests that moved here.
func testKey(t testing.TB, id string, fill byte) encryption.Key {
	t.Helper()
	return encryption.Key{ID: id, Material: bytes.Repeat([]byte{fill}, encryption.KeySize)}
}

func newCipher(t testing.TB, active encryption.Key, previous ...encryption.Key) *encryption.Cipher {
	t.Helper()
	ring, err := encryption.NewKeyRing(active, previous...)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := encryption.NewCipher(ring)
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}
