package argon2cost_test

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"

	"github.com/rocketinsidecorp/forge-go/auth"
)

// clampEnv names the wrapper the child process registers.
const clampEnv = "FORGE_TEST_ARGON2_CLAMP"

// clamps are wrappers around the real Argon2id that pass the small known
// answers RegisterArgon2id checks (m of 48 KiB or less is untouched) and
// compute Forge's default cost (m=19456 KiB) with less memory.
var clamps = map[string]func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte{
	"memory capped at 4096 KiB": func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		return argon2.IDKey(password, salt, time, min(memory, 4096), threads, keyLen)
	},
	"memory divided by 8 above 1024 KiB": func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		if memory > 1024 {
			memory /= 8
		}
		return argon2.IDKey(password, salt, time, memory, threads, keyLen)
	},
}

func TestMain(m *testing.M) {
	if clamp, ok := clamps[os.Getenv(clampEnv)]; ok {
		if !auth.RegisterArgon2id(clamp) {
			panic("the clamped wrapper was not registered")
		}
	}
	os.Exit(m.Run())
}

// A wrapper that changes the default cost passes registration (the small
// answers cannot see it) and is refused at its first use: Argon2id hashes
// fail closed, new passwords are PBKDF2, and the log names the fix.
func TestAnArgon2idThatChangesTheDefaultCostIsRefused(t *testing.T) {
	t.Parallel()
	for name := range clamps {
		command := exec.Command(os.Args[0], "-test.run=^TestRegisteredClampIsIgnored$", "-test.count=1", "-test.v")
		command.Env = append(os.Environ(), clampEnv+"="+name)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestRegisteredClampIsIgnored") ||
			!strings.Contains(string(output), "does not compute Argon2id at Forge's cost") {
			t.Errorf("%s: %v\n%s", name, err, output)
		}
	}
}

// TestRegisteredClampIsIgnored runs in the child process.
func TestRegisteredClampIsIgnored(t *testing.T) {
	if _, ok := clamps[os.Getenv(clampEnv)]; !ok {
		t.Skip("runs in a child process of TestAnArgon2idThatChangesTheDefaultCostIsRefused")
	}
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("correct horse battery staple"), salt, 2, 19456, 1, 32)
	stored := "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key)
	if match, rehash, err := auth.VerifyPassword(stored, "correct horse battery staple"); match || rehash || !errors.Is(err, auth.ErrPasswordAlgorithmUnavailable) {
		t.Fatalf("VerifyPassword = (%v, %v, %v), want ErrPasswordAlgorithmUnavailable", match, rehash, err)
	}
	if got := auth.DefaultPasswordAlgorithm(); got != auth.PasswordAlgorithmPBKDF2SHA256 {
		t.Fatalf("default algorithm %q, want PBKDF2", got)
	}
	if hash, err := auth.HashPassword("correct horse battery staple"); err != nil || !strings.HasPrefix(hash, "$pbkdf2-sha256$") {
		t.Fatalf("HashPassword = %q, %v; want PBKDF2", hash, err)
	}
}
