package encryption_test

import (
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

// BenchmarkAttributeEncryption is scenario "attribute_encryption" in
// docs/BENCHMARKS.md: sealing and opening one 64-byte attribute (AES-GCM
// bound to its table, column and record) with Cipher, and the same through
// the encrypted column type encryption.Value that models use.
//
//	seal          Cipher.Encrypt
//	open          Cipher.Decrypt
//	value_seal    Value.Value (what database/sql calls to write the column)
//	value_open    Value.Scan followed by Value.Get
func BenchmarkAttributeEncryption(b *testing.B) {
	cipher := newCipher(b, testKey(b, "k1", 1))
	plain := []byte("123-45-6789 is a sixty-four byte attribute, padded to that size.")
	if len(plain) != 64 {
		b.Fatalf("plaintext is %d bytes", len(plain))
	}
	sealed, err := cipher.Encrypt(plain, rowAAD)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("seal", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := cipher.Encrypt(plain, rowAAD); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("open", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := cipher.Decrypt(sealed, rowAAD); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("value_seal", func(b *testing.B) {
		value := encryption.NewValue[string](cipher, rowAAD.Table, rowAAD.Column).ForRecord(rowAAD.RecordID)
		value.Set(string(plain))
		b.ReportAllocs()
		for b.Loop() {
			if _, err := value.Value(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("value_open", func(b *testing.B) {
		value := encryption.NewValue[string](cipher, rowAAD.Table, rowAAD.Column).ForRecord(rowAAD.RecordID)
		b.ReportAllocs()
		for b.Loop() {
			if err := value.Scan(sealed); err != nil {
				b.Fatal(err)
			}
			if _, _, err := value.Get(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
