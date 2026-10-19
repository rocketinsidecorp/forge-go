package auth_test

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/rocketinsidecorp/forge-go/auth"
)

// TestAESGCMSecretCipherKeepsItsStoredFormat guards the stored MFA seed
// format (nonce||ciphertext||tag, no associated data) across the move of the
// primitives into package encryption: existing rows must stay readable.
func TestAESGCMSecretCipherKeepsItsStoredFormat(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := auth.NewAESGCMSecretCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := c.Seal([]byte("totp-seed"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil || string(plain) != "totp-seed" {
		t.Fatalf("independent AES-GCM open: %q, %v", plain, err)
	}
	legacy := gcm.Seal(append([]byte(nil), make([]byte, gcm.NonceSize())...), make([]byte, gcm.NonceSize()), []byte("legacy"), nil)
	if got, err := c.Open(legacy); err != nil || string(got) != "legacy" {
		t.Fatalf("legacy row: %q, %v", got, err)
	}
}
