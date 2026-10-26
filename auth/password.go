package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	// DefaultPasswordIterations is Forge's PBKDF2-HMAC-SHA256 work factor.
	// Hashes carry their work factor so future releases can raise this value
	// and transparently identify credentials that should be rehashed.
	DefaultPasswordIterations = 600_000

	minAcceptedPasswordIterations = 100_000
	maxAcceptedPasswordIterations = 5_000_000
	passwordSaltBytes             = 16
	passwordKeyBytes              = 32
	maxPasswordBytes              = 4096
)

var (
	// ErrPasswordRequired is returned for an empty password.
	ErrPasswordRequired = errors.New("password is required")
	// ErrPasswordTooLong is returned for a password longer than 4096 bytes.
	ErrPasswordTooLong = errors.New("password exceeds 4096 bytes")
	// ErrPasswordHashInvalid is returned for a stored hash that is malformed or uses parameters
	// this package does not accept.
	ErrPasswordHashInvalid = errors.New("invalid password hash")
)

// HashPassword derives a versioned, salted PBKDF2-HMAC-SHA256 password hash.
// Password bytes are used exactly as supplied: Forge never trims or normalizes
// passwords, because doing so would silently reduce user-selected entropy.
func HashPassword(password string) (string, error) {
	return hashPassword(password, DefaultPasswordIterations, rand.Reader)
}

// VerifyPassword checks password against encoded. needsRehash is true when the
// stored work factor is valid but no longer Forge's current default.
func VerifyPassword(encoded, password string) (match, needsRehash bool, err error) {
	if password == "" {
		return false, false, nil
	}
	if len(password) > maxPasswordBytes {
		return false, false, ErrPasswordTooLong
	}
	parsed, err := parsePasswordHash(encoded)
	if err != nil {
		return false, false, err
	}
	derived, err := pbkdf2.Key(sha256.New, password, parsed.salt, parsed.iterations, passwordKeyBytes)
	if err != nil {
		return false, false, fmt.Errorf("derive password key: %w", err)
	}
	match = subtle.ConstantTimeCompare(derived, parsed.key) == 1
	return match, parsed.iterations != DefaultPasswordIterations, nil
}

func hashPassword(password string, iterations int, entropy io.Reader) (string, error) {
	switch {
	case password == "":
		return "", ErrPasswordRequired
	case len(password) > maxPasswordBytes:
		return "", ErrPasswordTooLong
	case iterations < minAcceptedPasswordIterations || iterations > maxAcceptedPasswordIterations:
		return "", fmt.Errorf("password iterations must be between %d and %d", minAcceptedPasswordIterations, maxAcceptedPasswordIterations)
	case entropy == nil:
		return "", errors.New("password entropy source is required")
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := io.ReadFull(entropy, salt); err != nil {
		return "", fmt.Errorf("read password salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, passwordKeyBytes)
	if err != nil {
		return "", fmt.Errorf("derive password key: %w", err)
	}
	return fmt.Sprintf(
		"$pbkdf2-sha256$v=1$i=%d$%s$%s",
		iterations,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(key),
	), nil
}

type parsedPasswordHash struct {
	iterations int
	salt       []byte
	key        []byte
}

func parsePasswordHash(encoded string) (parsedPasswordHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "pbkdf2-sha256" || parts[2] != "v=1" || !strings.HasPrefix(parts[3], "i=") {
		return parsedPasswordHash{}, ErrPasswordHashInvalid
	}
	rawIterations := strings.TrimPrefix(parts[3], "i=")
	iterations, err := strconv.Atoi(rawIterations)
	if err != nil || strconv.Itoa(iterations) != rawIterations ||
		iterations < minAcceptedPasswordIterations || iterations > maxAcceptedPasswordIterations {
		return parsedPasswordHash{}, ErrPasswordHashInvalid
	}
	salt, err := decodePasswordPart(parts[4], passwordSaltBytes)
	if err != nil {
		return parsedPasswordHash{}, err
	}
	key, err := decodePasswordPart(parts[5], passwordKeyBytes)
	if err != nil {
		return parsedPasswordHash{}, err
	}
	return parsedPasswordHash{iterations: iterations, salt: salt, key: key}, nil
}

func decodePasswordPart(encoded string, size int) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != size || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, ErrPasswordHashInvalid
	}
	return decoded, nil
}
