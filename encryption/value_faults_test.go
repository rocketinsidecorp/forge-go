package encryption_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rocketinsidecorp/forge-go/encryption"
)

func TestValueReportsWhatItCannotSealOrDecode(t *testing.T) {
	t.Parallel()
	c := newCipher(t, testKey(t, "k1", 1))

	// A value without a cipher is a programming error, reported on use.
	orphan := encryption.NewValue[string](nil, "people", "ssn").ForRecord("row")
	orphan.Set("x")
	if _, err := orphan.Value(); err == nil || !strings.Contains(err.Error(), "cipher is required") {
		t.Errorf("a value without a cipher: %v", err)
	}
	scanned := encryption.NewValue[string](nil, "people", "ssn").ForRecord("row")
	if err := scanned.Scan("fg1.k1.AAAA"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scanned.Get(); err == nil || !strings.Contains(err.Error(), "cipher is required") {
		t.Errorf("Get without a cipher: %v", err)
	}

	// A value over the size limit is refused when it is written.
	big := encryption.NewValue[string](c, "people", "ssn").ForRecord("row")
	big.Set(strings.Repeat("x", encryption.MaxPlaintextBytes+1))
	if _, err := big.Value(); !errors.Is(err, encryption.ErrTooLarge) {
		t.Errorf("an oversized value: %v", err)
	}

	// A ciphertext that opens but holds something the Go type cannot decode.
	sealed, err := c.Encrypt([]byte("not a number"), encryption.AAD{Table: "people", Column: "age", RecordID: "row"})
	if err != nil {
		t.Fatal(err)
	}
	age := encryption.NewValue[int](c, "people", "age")
	if err := age.Scan(sealed); err != nil {
		t.Fatal(err)
	}
	_, _, decodeErr := age.ForRecord("row").Get()
	if decodeErr == nil || !strings.Contains(decodeErr.Error(), "decode people.age") {
		t.Fatalf("an undecodable value: %v", decodeErr)
	}
	if strings.Contains(decodeErr.Error(), "not a number") {
		t.Errorf("the plaintext leaked into the error: %v", decodeErr)
	}

	// Clear makes the value NULL, and writing it stores NULL.
	cleared := encryption.NewValue[string](c, "people", "ssn").ForRecord("row")
	cleared.Set("secret")
	cleared.Clear()
	if got, present, err := cleared.Get(); err != nil || present || got != "" {
		t.Errorf("after Clear: %q %v %v", got, present, err)
	}
	if value, err := cleared.Value(); err != nil || value != nil {
		t.Errorf("a cleared value stores %v, %v; want NULL", value, err)
	}
}
