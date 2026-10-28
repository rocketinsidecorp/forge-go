package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// argon2Hash encodes an Argon2id hash of password with any parameters, salt
// and key length, for the parser and rehash tests, through the registered
// (fake) implementation.
func argon2Hash(password string, params argon2Params, salt []byte, keyBytes uint32) string {
	key := fakeArgon2idKDF([]byte(password), salt, params.iterations, params.memoryKiB, params.parallelism, keyBytes)
	return encodeArgon2id(params, salt, key)
}

// The one test of the production parameters: m=19456 KiB, t=2, p=1, a 16-byte
// salt and a 32-byte key, the parameters of Forge-Rust's
// Argon2idPasswordHasher, with Argon2id registered (here the fake; the
// identity adapters' tests run the real one at this cost).
func TestPasswordHashDefaultsToArgon2idWithForgeRustParameters(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple 🔐")
	if err != nil {
		t.Fatal(err)
	}
	if DefaultPasswordAlgorithm() == PasswordAlgorithmPBKDF2SHA256 {
		// A binary built with -tags forge_pbkdf2.
		if !strings.HasPrefix(hash, "$pbkdf2-sha256$v=1$i=600000$") {
			t.Fatalf("forge_pbkdf2 hash %q", hash)
		}
		return
	}
	if DefaultPasswordAlgorithm() != PasswordAlgorithmArgon2id || DefaultPasswordHasher().Algorithm() != PasswordAlgorithmArgon2id {
		t.Fatalf("default algorithm %q", DefaultPasswordAlgorithm())
	}
	shape := regexp.MustCompile(`^\$argon2id\$v=19\$m=19456,t=2,p=1\$([A-Za-z0-9+/]{22})\$([A-Za-z0-9+/]{43})$`)
	parts := shape.FindStringSubmatch(hash)
	if parts == nil || strings.Contains(hash, "correct horse") {
		t.Fatalf("unexpected password hash %q", hash)
	}
	if salt, err := base64.RawStdEncoding.DecodeString(parts[1]); err != nil || len(salt) != 16 {
		t.Fatalf("salt %q: %v", parts[1], err)
	}
	match, rehash, err := VerifyPassword(hash, "correct horse battery staple 🔐")
	if err != nil || !match || rehash {
		t.Fatalf("VerifyPassword()=(%v, %v, %v)", match, rehash, err)
	}
	match, _, err = VerifyPassword(hash, "wrong password")
	if err != nil || match {
		t.Fatalf("wrong password match=%v err=%v", match, err)
	}
	other, err := HashPassword("correct horse battery staple 🔐")
	if err != nil || other == hash {
		t.Fatalf("two hashes of one password share a salt: %v", err)
	}
}

func TestPasswordHasherPBKDF2OptionForFIPSBuilds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fips, err := NewPasswordHasher(WithPasswordAlgorithm(PasswordAlgorithmPBKDF2SHA256))
	if err != nil {
		t.Fatal(err)
	}
	if fips.Algorithm() != PasswordAlgorithmPBKDF2SHA256 || !strings.HasPrefix(fips.dummy(), "$pbkdf2-sha256$v=1$i=600000$") {
		t.Fatalf("algorithm=%q dummy=%q", fips.Algorithm(), fips.dummy())
	}
	legacy, err := hashPassword("secret", DefaultPasswordIterations, bytes.NewReader(make([]byte, passwordSaltBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if match, rehash, err := fips.Verify(ctx, legacy, "secret"); err != nil || !match || rehash {
		t.Fatalf("PBKDF2 hasher on a current PBKDF2 hash: (%v, %v, %v)", match, rehash, err)
	}
	// An Argon2id hash still verifies, and is rewritten as PBKDF2.
	modern := argon2Hash("secret", defaultArgon2Params, make([]byte, passwordSaltBytes), passwordKeyBytes)
	if match, rehash, err := fips.Verify(ctx, modern, "secret"); err != nil || !match || !rehash {
		t.Fatalf("PBKDF2 hasher on an Argon2id hash: (%v, %v, %v)", match, rehash, err)
	}
	if _, err := fips.Hash(ctx, ""); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("empty password error=%v", err)
	}

	if _, err := NewPasswordHasher(WithPasswordAlgorithm("bcrypt")); err == nil {
		t.Error("unknown algorithm accepted")
	}
	if _, err := NewPasswordHasher(WithPasswordWorkLimiter(nil)); err == nil {
		t.Error("nil limiter accepted")
	}
	if _, err := NewPasswordHasher(nil); err == nil {
		t.Error("nil option accepted")
	}
}

func TestPasswordHashMarksOlderWorkFactorForUpgrade(t *testing.T) {
	hash, err := hashPassword("secret", minAcceptedPasswordIterations, bytes.NewReader(make([]byte, passwordSaltBytes)))
	if err != nil {
		t.Fatal(err)
	}
	match, rehash, err := VerifyPassword(hash, "secret")
	if err != nil || !match || !rehash {
		t.Fatalf("VerifyPassword()=(%v, %v, %v)", match, rehash, err)
	}
}

// Any Argon2id hash whose cost is not the writer's current one, weaker above
// all, is upgraded on the next sign-in; only the exact current parameters are
// left alone.
func TestArgon2idHashWithOtherParametersNeedsRehash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hasher := cheapPasswordHasher(t)
	salt := bytes.Repeat([]byte{7}, passwordSaltBytes)
	current := argon2Hash("secret", testArgon2Params, salt, passwordKeyBytes)
	if match, rehash, err := hasher.Verify(ctx, current, "secret"); err != nil || !match || rehash {
		t.Fatalf("current parameters: (%v, %v, %v)", match, rehash, err)
	}
	for name, encoded := range map[string]string{
		"more memory":  argon2Hash("secret", argon2Params{memoryKiB: 16, iterations: 1, parallelism: 1}, salt, passwordKeyBytes),
		"more passes":  argon2Hash("secret", argon2Params{memoryKiB: 8, iterations: 2, parallelism: 1}, salt, passwordKeyBytes),
		"more lanes":   argon2Hash("secret", argon2Params{memoryKiB: 16, iterations: 1, parallelism: 2}, salt, passwordKeyBytes),
		"shorter salt": argon2Hash("secret", testArgon2Params, salt[:minArgon2SaltBytes], passwordKeyBytes),
		"PBKDF2 legacy": func() string {
			h, _ := hashPassword("secret", minAcceptedPasswordIterations, bytes.NewReader(salt))
			return h
		}(),
	} {
		match, rehash, err := hasher.Verify(ctx, encoded, "secret")
		if err != nil || !match || !rehash {
			t.Errorf("%s: (%v, %v, %v)", name, match, rehash, err)
		}
	}
	// With production parameters, anything weaker is upgraded.
	weak := argon2Hash("secret", argon2Params{memoryKiB: 4096, iterations: 2, parallelism: 1}, salt, passwordKeyBytes)
	production, err := NewPasswordHasher(WithPasswordAlgorithm(PasswordAlgorithmArgon2id))
	if err != nil {
		t.Fatal(err)
	}
	if match, rehash, err := production.Verify(ctx, weak, "secret"); err != nil || !match || !rehash {
		t.Fatalf("weaker hash under production parameters: (%v, %v, %v)", match, rehash, err)
	}
}

func TestPasswordHashRejectsInvalidInputs(t *testing.T) {
	if _, err := HashPassword(""); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("empty password error=%v", err)
	}
	if _, err := HashPassword(strings.Repeat("x", maxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long password error=%v", err)
	}
	if _, _, err := VerifyPassword(cheapDummy(t), strings.Repeat("x", maxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long password verify error=%v", err)
	}
	if _, err := hashPassword("secret", DefaultPasswordIterations, bytes.NewReader(nil)); err == nil {
		t.Fatal("expected entropy failure")
	}
	argonHasher := cheapPasswordHasher(t)
	argonHasher.entropy = bytes.NewReader(nil)
	if _, err := argonHasher.Hash(context.Background(), "secret"); err == nil {
		t.Fatal("expected Argon2id entropy failure")
	}
	for _, encoded := range []string{
		"",
		"$argon2id$v=1$i=600000$AA$BB",
		"$pbkdf2-sha256$v=2$i=600000$AA$BB",
		"$pbkdf2-sha256$v=1$i=+600000$AA$BB",
		"$pbkdf2-sha256$v=1$i=99999$AA$BB",
		"$pbkdf2-sha256$v=1$i=600000$not-base64$also-not-base64",
	} {
		if _, _, err := VerifyPassword(encoded, "secret"); !errors.Is(err, ErrPasswordHashInvalid) {
			t.Fatalf("VerifyPassword(%q) error=%v", encoded, err)
		}
	}
}

// Every Argon2id refusal is decided by the parser, before any hashing: the
// hasher here has no free slot, so a hash that reached the work would fail
// with ErrPasswordHashingUnavailable instead of ErrPasswordHashInvalid.
func TestArgon2idParserRefusesEverythingButTheStrictPHCForm(t *testing.T) {
	t.Parallel()
	salt := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16))
	key := base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	ok := "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key
	if _, err := parseStoredPasswordHash(ok); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	b64 := func(n int) string { return base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, n)) }
	refused := map[string]string{
		"argon2i":                 "$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"argon2d":                 "$argon2d$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"upper-case algorithm":    "$Argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"version 16":              "$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key,
		"version 20":              "$argon2id$v=20$m=19456,t=2,p=1$" + salt + "$" + key,
		"no version":              "$argon2id$m=19456,t=2,p=1$" + salt + "$" + key,
		"too few fields":          "$argon2id$v=19$m=19456,t=2,p=1$" + salt,
		"too many fields":         ok + "$",
		"text before":             "x" + ok,
		"memory above bound":      "$argon2id$v=19$m=65537,t=2,p=1$" + salt + "$" + key,
		"memory overflowing u32":  "$argon2id$v=19$m=4294967296,t=2,p=1$" + salt + "$" + key,
		"passes above bound":      "$argon2id$v=19$m=19456,t=11,p=1$" + salt + "$" + key,
		"lanes above bound":       "$argon2id$v=19$m=19456,t=2,p=9$" + salt + "$" + key,
		"zero memory":             "$argon2id$v=19$m=0,t=2,p=1$" + salt + "$" + key,
		"zero passes":             "$argon2id$v=19$m=19456,t=0,p=1$" + salt + "$" + key,
		"zero lanes":              "$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key,
		"memory below 8 per lane": "$argon2id$v=19$m=23,t=2,p=3$" + salt + "$" + key,
		"parameters reordered":    "$argon2id$v=19$t=2,m=19456,p=1$" + salt + "$" + key,
		"parameter missing":       "$argon2id$v=19$m=19456,t=2$" + salt + "$" + key,
		"keyid parameter":         "$argon2id$v=19$m=19456,t=2,p=1,keyid=AA$" + salt + "$" + key,
		"leading zero":            "$argon2id$v=19$m=019456,t=2,p=1$" + salt + "$" + key,
		"plus sign":               "$argon2id$v=19$m=+19456,t=2,p=1$" + salt + "$" + key,
		"space":                   "$argon2id$v=19$m=19456, t=2,p=1$" + salt + "$" + key,
		"salt not base64":         "$argon2id$v=19$m=19456,t=2,p=1$!!!!!!!!!!$" + key,
		"salt URL alphabet":       "$argon2id$v=19$m=19456,t=2,p=1$" + strings.NewReplacer("+", "-", "/", "_").Replace(base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 16))) + "$" + key,
		"salt padded":             "$argon2id$v=19$m=19456,t=2,p=1$" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 16)) + "$" + key,
		"salt non-canonical bits": "$argon2id$v=19$m=19456,t=2,p=1$" + salt[:len(salt)-1] + "B$" + key,
		"salt too short":          "$argon2id$v=19$m=19456,t=2,p=1$" + b64(7) + "$" + key,
		"salt too long":           "$argon2id$v=19$m=19456,t=2,p=1$" + b64(65) + "$" + key,
		"empty salt":              "$argon2id$v=19$m=19456,t=2,p=1$$" + key,
		"key 31 bytes":            "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64(31),
		"key 33 bytes":            "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64(33),
		"key 64 bytes":            "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + b64(64),
		"key padded":              "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
		"longer than 1024 bytes":  "$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + key + strings.Repeat("A", 1024),
	}
	limiter, err := NewPasswordWorkLimiter(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	hasher := cheapPasswordHasher(t, WithPasswordWorkLimiter(limiter))
	release := holdSlot(t, limiter)
	defer release()
	for name, encoded := range refused {
		if _, err := parseStoredPasswordHash(encoded); !errors.Is(err, ErrPasswordHashInvalid) {
			t.Errorf("%s: parse error=%v", name, err)
		}
		if match, rehash, err := hasher.Verify(context.Background(), encoded, "secret"); match || rehash || !errors.Is(err, ErrPasswordHashInvalid) {
			t.Errorf("%s: Verify=(%v, %v, %v)", name, match, rehash, err)
		}
	}
	// The baseline reaches the (saturated) work.
	if _, _, err := hasher.Verify(context.Background(), ok, "secret"); !errors.Is(err, ErrPasswordHashingUnavailable) {
		t.Fatalf("baseline Verify error=%v", err)
	}
}

func cheapDummy(t *testing.T) string {
	t.Helper()
	return cheapPasswordHasher(t).dummy()
}

func FuzzParsePasswordHashNeverPanics(f *testing.F) {
	for _, seed := range []string{
		"", "$", "$pbkdf2-sha256$", "$pbkdf2-sha256$v=1$i=600000$",
		"$argon2id$", "$argon2id$v=19$m=19456,t=2,p=1$", "$argon2id$v=19$m=8,t=1,p=1$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, encoded string) {
		stored, err := parseStoredPasswordHash(encoded)
		if err != nil {
			return
		}
		if stored.algorithm == PasswordAlgorithmArgon2id {
			p := stored.argon2
			if p.memoryKiB > maxArgon2MemoryKiB || p.iterations > maxArgon2Iterations || p.parallelism > maxArgon2Parallelism ||
				p.iterations < 1 || p.parallelism < 1 || p.memoryKiB < 8*uint32(p.parallelism) || len(stored.key) != passwordKeyBytes {
				t.Fatalf("out-of-bounds hash accepted: %q", encoded)
			}
		}
	})
}
