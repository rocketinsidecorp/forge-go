package auth

import (
	"context"
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
	"sync"
	"sync/atomic"

	"github.com/rocketinsidecorp/forge-go/fault"
)

// PasswordAlgorithm names the algorithm a PasswordHasher writes new hashes
// with. Both are verified whichever writes; Argon2id only once an
// implementation is registered (RegisterArgon2id).
type PasswordAlgorithm string

const (
	// PasswordAlgorithmArgon2id is Argon2id v19 (RFC 9106) with Forge-Rust's
	// parameters: m=19456 KiB, t=2, p=1, a 16-byte salt and a 32-byte key,
	// encoded as a PHC string. The core has no Argon2 of its own: it hashes
	// and verifies with the function given to RegisterArgon2id, which the
	// identity adapters (auth/postgres, auth/mysql, auth/sqlite) register.
	PasswordAlgorithmArgon2id PasswordAlgorithm = "argon2id"
	// PasswordAlgorithmPBKDF2SHA256 is PBKDF2-HMAC-SHA256 from the Go
	// standard library, DefaultPasswordIterations rounds: the choice of a
	// FIPS 140 deployment, where Argon2id is not an approved algorithm.
	PasswordAlgorithmPBKDF2SHA256 PasswordAlgorithm = "pbkdf2-sha256" // #nosec G101 -- an algorithm name, not a credential
)

// DefaultPasswordAlgorithm returns the algorithm DefaultPasswordHasher,
// HashPassword and every hasher built without WithPasswordAlgorithm write
// new hashes with at this moment: Argon2id once an implementation is
// registered (RegisterArgon2id; linking an identity adapter does it), and
// PBKDF2-HMAC-SHA256 before that or in a binary built with the forge_pbkdf2
// build tag (go build -tags forge_pbkdf2). Registration is permanent, so the
// answer changes at most once, from PBKDF2 to Argon2id, normally during
// package initialization.
func DefaultPasswordAlgorithm() PasswordAlgorithm {
	return argon2id.defaultAlgorithm()
}

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

	// Forge-Rust's Argon2idPasswordHasher policy (ARGON2_MEMORY_KIB,
	// ARGON2_ITERATIONS, ARGON2_PARALLELISM, ARGON2_OUTPUT_BYTES; the argon2
	// crate's 16-byte salt). The key length is the same as PBKDF2's.
	argon2MemoryKiB   = 19_456
	argon2Iterations  = 2
	argon2Parallelism = 1
	argon2Version     = 19

	// A stored hash names its own cost, so these bound what one verification
	// can demand of the process (a planted m=4194304 would take 4 GiB per
	// attempt): 64 MiB, RFC 9106's second recommended memory size, at most
	// 10 passes and 8 lanes. The lower bounds are the algorithm's own.
	maxArgon2MemoryKiB    = 65_536
	maxArgon2Iterations   = 10
	maxArgon2Parallelism  = 8
	minArgon2SaltBytes    = 8
	maxArgon2SaltBytes    = 64
	maxPasswordHashLength = 1024
)

var (
	// ErrPasswordRequired is returned for an empty password.
	ErrPasswordRequired = errors.New("password is required")
	// ErrPasswordTooLong is returned for a password longer than 4096 bytes.
	ErrPasswordTooLong = errors.New("password exceeds 4096 bytes")
	// ErrPasswordHashInvalid is returned for a stored hash that is malformed or uses parameters
	// this package does not accept.
	ErrPasswordHashInvalid = errors.New("invalid password hash")

	// ErrPasswordAlgorithmUnavailable is returned for a stored $argon2id$
	// hash, or an Argon2id hasher asked to hash, while no Argon2id
	// implementation is registered (RegisterArgon2id). It fails closed: it
	// is not a wrong password, the hash is never rewritten, and HTTP
	// adapters answer 503 through web.Error, which logs the Cause naming
	// the fix. It is a deployment error, not a transient one.
	ErrPasswordAlgorithmUnavailable = &fault.Error{
		Code:     "password_algorithm_unavailable",
		Message:  "service temporarily unavailable",
		Category: fault.CategoryUnavailable,
		Cause:    errors.New("a password hash is Argon2id but no Argon2id implementation is registered: import an identity adapter (auth/postgres, auth/mysql or auth/sqlite), or call auth.RegisterArgon2id(argon2.IDKey) with golang.org/x/crypto/argon2, in the application binary"),
	}
)

// argon2Params is the cost of one Argon2id hash.
type argon2Params struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
}

var defaultArgon2Params = argon2Params{memoryKiB: argon2MemoryKiB, iterations: argon2Iterations, parallelism: argon2Parallelism}

// PasswordHasher writes and verifies password hashes. Every hash and
// verification runs through its PasswordWorkLimiter. Verification accepts
// Argon2id (PHC: $argon2id$v=19$m=…,t=…,p=…$salt$key, unpadded standard
// base64) and Forge's PBKDF2 format ($pbkdf2-sha256$v=1$i=…$salt$key)
// whichever algorithm writes, and reports a hash that is not in the writing
// algorithm with its current parameters as needing a rehash, so a successful
// sign-in upgrades it. Argon2id runs the function given to RegisterArgon2id,
// looked up at each hash and verification, so the order of package
// initialization does not matter; with none registered, an Argon2id hash
// fails with ErrPasswordAlgorithmUnavailable.
type PasswordHasher struct {
	// algorithm is what WithPasswordAlgorithm set; empty follows
	// DefaultPasswordAlgorithm at each hash.
	algorithm PasswordAlgorithm
	argon2    argon2Params
	// kdfs is where the Argon2id implementation is looked up: the
	// process-wide registry, or (tests only) another one.
	kdfs    *argon2Registry
	work    *PasswordWorkLimiter
	entropy io.Reader
	// pbkdf2Dummy and argon2Dummy are well-formed hashes with each
	// algorithm's current cost that no password matches: a sign-in for an
	// unknown account verifies against the one of the writing algorithm, so
	// it costs what a known account costs.
	pbkdf2Dummy, argon2Dummy string
	// floor pads failed checks while legacy PBKDF2 hashes may exist; nil
	// when off (WithLegacyHashTimingFloor). Unless the option set it, it
	// applies only while the hasher writes Argon2id (activeFloor).
	floor                  *legacyTimingFloor
	floorSet, floorEnabled bool
}

// argon2idKDF is the signature of golang.org/x/crypto/argon2.IDKey.
type argon2idKDF = func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte

// argon2Registry holds the one Argon2id implementation of the process.
type argon2Registry struct {
	kdf atomic.Pointer[argon2idKDF]
}

// argon2id is the process-wide registry RegisterArgon2id writes.
var argon2id argon2Registry

// register keeps kdf unless one is already registered or kdf is nil; it
// reports whether kdf was kept.
func (r *argon2Registry) register(kdf argon2idKDF) bool {
	if kdf == nil {
		return false
	}
	return r.kdf.CompareAndSwap(nil, &kdf)
}

// load returns the registered implementation, or nil.
func (r *argon2Registry) load() argon2idKDF {
	if kdf := r.kdf.Load(); kdf != nil {
		return *kdf
	}
	return nil
}

// defaultAlgorithm is DefaultPasswordAlgorithm for this registry.
func (r *argon2Registry) defaultAlgorithm() PasswordAlgorithm {
	if pbkdf2Build || r.load() == nil {
		return PasswordAlgorithmPBKDF2SHA256
	}
	return PasswordAlgorithmArgon2id
}

// RegisterArgon2id sets the Argon2id implementation of the process, a
// function with the signature of golang.org/x/crypto/argon2.IDKey, which is
// what the identity adapters (auth/postgres, auth/mysql, auth/sqlite)
// register in their init. The core imports no third-party module, so it has
// no Argon2 of its own: without a registration, new hashes are
// PBKDF2-HMAC-SHA256 and verifying an existing $argon2id$ hash fails with
// ErrPasswordAlgorithmUnavailable. An application whose credential store is
// not one of those adapters calls
//
//	auth.RegisterArgon2id(argon2.IDKey)
//
// in main or an init. The first registration wins; later ones, and a nil
// kdf, are ignored, so a second import of an adapter cannot swap the
// implementation. It is safe for concurrent use, and hashers look the
// implementation up at each hash and verification, so it may run before or
// after they are built. kdf must be a correct Argon2id (RFC 9106, version
// 19): Forge does not check it beyond verifying the hashes it writes.
func RegisterArgon2id(kdf func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte) {
	argon2id.register(kdf)
}

// PasswordHasherOption configures NewPasswordHasher.
type PasswordHasherOption func(*PasswordHasher) error

// WithPasswordAlgorithm sets the algorithm new hashes are written with; the
// default follows DefaultPasswordAlgorithm. A hasher set to Argon2id fails
// to hash with ErrPasswordAlgorithmUnavailable while no implementation is
// registered (RegisterArgon2id).
func WithPasswordAlgorithm(algorithm PasswordAlgorithm) PasswordHasherOption {
	return func(h *PasswordHasher) error {
		if algorithm != PasswordAlgorithmArgon2id && algorithm != PasswordAlgorithmPBKDF2SHA256 {
			return fmt.Errorf("unknown password algorithm %q", algorithm)
		}
		h.algorithm = algorithm
		return nil
	}
}

// WithPasswordWorkLimiter sets the limiter hashes run through; the default is
// the process-wide DefaultPasswordWorkLimiter.
func WithPasswordWorkLimiter(limiter *PasswordWorkLimiter) PasswordHasherOption {
	return func(h *PasswordHasher) error {
		if limiter == nil {
			return errors.New("password work limiter is required")
		}
		h.work = limiter
		return nil
	}
}

// NewPasswordHasher returns a hasher writing DefaultPasswordAlgorithm through
// DefaultPasswordWorkLimiter unless options say otherwise.
func NewPasswordHasher(options ...PasswordHasherOption) (*PasswordHasher, error) {
	h := &PasswordHasher{
		argon2:  defaultArgon2Params,
		kdfs:    &argon2id,
		entropy: rand.Reader,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil password hasher option")
		}
		if err := option(h); err != nil {
			return nil, err
		}
	}
	if h.work == nil {
		h.work = DefaultPasswordWorkLimiter()
	}
	// The floor's calibration is lazy, so a hasher that may come to write
	// Argon2id (the default one) carries one at no cost until it is used.
	if (h.floorSet && h.floorEnabled) || (!h.floorSet && h.algorithm != PasswordAlgorithmPBKDF2SHA256) {
		h.floor = newLegacyTimingFloor()
	}
	h.setDummy()
	return h, nil
}

func (h *PasswordHasher) setDummy() {
	zeroSalt := make([]byte, passwordSaltBytes)
	zeroKey := make([]byte, passwordKeyBytes)
	h.pbkdf2Dummy = encodePBKDF2(DefaultPasswordIterations, zeroSalt, zeroKey)
	h.argon2Dummy = encodeArgon2id(h.argon2, zeroSalt, zeroKey)
}

// writing returns the algorithm h writes new hashes with now, given kdf,
// the registered Argon2id implementation or nil.
func (h *PasswordHasher) writing(kdf argon2idKDF) PasswordAlgorithm {
	switch {
	case h.algorithm != "":
		return h.algorithm
	case pbkdf2Build || kdf == nil:
		return PasswordAlgorithmPBKDF2SHA256
	default:
		return PasswordAlgorithmArgon2id
	}
}

// dummy returns the hash an unknown account is verified against: one in
// the writing algorithm, which nothing matches.
func (h *PasswordHasher) dummy() string {
	if h.Algorithm() == PasswordAlgorithmPBKDF2SHA256 {
		return h.pbkdf2Dummy
	}
	return h.argon2Dummy
}

// activeFloor returns the legacy timing floor when it applies now: always
// when WithLegacyHashTimingFloor(true) set it, otherwise while h writes
// Argon2id (a hasher writing PBKDF2 already pays PBKDF2 on every check).
func (h *PasswordHasher) activeFloor() *legacyTimingFloor {
	if h.floor == nil || (!h.floorSet && h.Algorithm() != PasswordAlgorithmArgon2id) {
		return nil
	}
	return h.floor
}

var defaultPasswordHasher = sync.OnceValue(func() *PasswordHasher {
	h, err := NewPasswordHasher()
	if err != nil {
		panic(err) // unreachable: no options
	}
	return h
})

// DefaultPasswordHasher returns the process-wide hasher HashPassword,
// VerifyPassword and the authentication services use unless given another:
// DefaultPasswordAlgorithm, looked up at each hash, through
// DefaultPasswordWorkLimiter.
func DefaultPasswordHasher() *PasswordHasher {
	return defaultPasswordHasher()
}

// Algorithm returns the algorithm h writes new hashes with now: the one
// WithPasswordAlgorithm set, or else DefaultPasswordAlgorithm.
func (h *PasswordHasher) Algorithm() PasswordAlgorithm {
	return h.writing(h.kdfs.load())
}

// HashPassword hashes password with DefaultPasswordHasher. Password bytes are
// used exactly as supplied: Forge never trims or normalizes passwords,
// because doing so would silently reduce user-selected entropy.
func HashPassword(password string) (string, error) {
	return DefaultPasswordHasher().Hash(context.Background(), password)
}

// VerifyPassword checks password against encoded with DefaultPasswordHasher.
// needsRehash is true when the hash is valid but not in Forge's current
// algorithm and parameters.
func VerifyPassword(encoded, password string) (match, needsRehash bool, err error) {
	return DefaultPasswordHasher().Verify(context.Background(), encoded, password)
}

// Hash derives a salted hash of password in h's algorithm. It fails with
// ErrPasswordHashingUnavailable when the limiter stays saturated, and with
// ErrPasswordAlgorithmUnavailable when h is set to Argon2id and no
// implementation is registered.
func (h *PasswordHasher) Hash(ctx context.Context, password string) (string, error) {
	switch {
	case password == "":
		return "", ErrPasswordRequired
	case len(password) > maxPasswordBytes:
		return "", ErrPasswordTooLong
	}
	kdf := h.kdfs.load()
	algorithm := h.writing(kdf)
	if algorithm == PasswordAlgorithmPBKDF2SHA256 {
		var encoded string
		var hashErr error
		if err := h.work.Do(ctx, func() {
			encoded, hashErr = hashPassword(password, DefaultPasswordIterations, h.entropy)
		}); err != nil {
			return "", err
		}
		return encoded, hashErr
	}
	if kdf == nil {
		return "", ErrPasswordAlgorithmUnavailable
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := io.ReadFull(h.entropy, salt); err != nil {
		return "", fmt.Errorf("read password salt: %w", err)
	}
	var key []byte
	if err := h.work.Do(ctx, func() {
		key = kdf([]byte(password), salt, h.argon2.iterations, h.argon2.memoryKiB, h.argon2.parallelism, passwordKeyBytes)
	}); err != nil {
		return "", err
	}
	return encodeArgon2id(h.argon2, salt, key), nil
}

// Verify checks password against encoded, with the parameters encoded names
// (within bounds). needsRehash is true when encoded is valid but not in h's
// algorithm with its current parameters. An empty password never matches and
// costs nothing. A well-formed $argon2id$ hash while no Argon2id
// implementation is registered fails with ErrPasswordAlgorithmUnavailable,
// before any hashing: not a mismatch, and never a rehash.
func (h *PasswordHasher) Verify(ctx context.Context, encoded, password string) (match, needsRehash bool, err error) {
	if password == "" {
		return false, false, nil
	}
	if len(password) > maxPasswordBytes {
		return false, false, ErrPasswordTooLong
	}
	stored, err := parseStoredPasswordHash(encoded)
	if err != nil {
		return false, false, err
	}
	kdf := h.kdfs.load()
	if stored.algorithm == PasswordAlgorithmArgon2id && kdf == nil {
		return false, false, ErrPasswordAlgorithmUnavailable
	}
	floor := h.activeFloor()
	var derived []byte
	var deriveErr error
	if err := h.work.Do(ctx, func() {
		if floor == nil || stored.algorithm != PasswordAlgorithmPBKDF2SHA256 {
			derived, deriveErr = stored.derive(password, kdf)
			return
		}
		// A legacy verification may be slower than the calibrated floor
		// (more iterations, a slower moment): the floor follows it.
		start := floor.now()
		derived, deriveErr = stored.derive(password, kdf)
		floor.observe(floor.now().Sub(start))
	}); err != nil {
		return false, false, err
	}
	if deriveErr != nil {
		return false, false, deriveErr
	}
	match = subtle.ConstantTimeCompare(derived, stored.key) == 1
	return match, h.needsRehash(stored, h.writing(kdf)), nil
}

// verifyDummy costs what verifying a stored hash in h's algorithm costs, and
// never matches.
func (h *PasswordHasher) verifyDummy(ctx context.Context, password string) error {
	_, _, err := h.Verify(ctx, h.dummy(), password)
	return err
}

// needsRehash reports whether stored is not in algorithm, the writing one,
// with its current parameters.
func (h *PasswordHasher) needsRehash(stored storedPasswordHash, algorithm PasswordAlgorithm) bool {
	if stored.algorithm != algorithm {
		return true
	}
	if stored.algorithm == PasswordAlgorithmPBKDF2SHA256 {
		return stored.iterations != DefaultPasswordIterations
	}
	return stored.argon2 != h.argon2 || len(stored.salt) != passwordSaltBytes
}

// hashPassword writes Forge's PBKDF2 format with the given work factor.
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
	return encodePBKDF2(iterations, salt, key), nil
}

func encodePBKDF2(iterations int, salt, key []byte) string {
	return fmt.Sprintf(
		"$pbkdf2-sha256$v=1$i=%d$%s$%s",
		iterations,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(key),
	)
}

func encodeArgon2id(params argon2Params, salt, key []byte) string {
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version,
		params.memoryKiB, params.iterations, params.parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// storedPasswordHash is a parsed, bounded stored hash.
type storedPasswordHash struct {
	algorithm  PasswordAlgorithm
	iterations int          // PBKDF2
	argon2     argon2Params // Argon2id
	salt       []byte
	key        []byte
}

// derive computes the key of password with s's algorithm and parameters;
// kdf is the Argon2id implementation, not nil for an Argon2id hash.
func (s storedPasswordHash) derive(password string, kdf argon2idKDF) ([]byte, error) {
	if s.algorithm == PasswordAlgorithmPBKDF2SHA256 {
		key, err := pbkdf2.Key(sha256.New, password, s.salt, s.iterations, passwordKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("derive password key: %w", err)
		}
		return key, nil
	}
	return kdf([]byte(password), s.salt, s.argon2.iterations, s.argon2.memoryKiB, s.argon2.parallelism, passwordKeyBytes), nil
}

func parseStoredPasswordHash(encoded string) (storedPasswordHash, error) {
	if len(encoded) > maxPasswordHashLength {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	if strings.HasPrefix(encoded, "$argon2id$") {
		return parseArgon2idHash(encoded)
	}
	return parsePasswordHash(encoded)
}

// parseArgon2idHash accepts exactly $argon2id$v=19$m=M,t=T,p=P$SALT$KEY: the
// parameters in that order as canonical decimals within bounds, SALT 8 to 64
// bytes and KEY 32 bytes in canonical unpadded standard base64. No other
// version, parameter (keyid, data) or encoding is accepted.
func parseArgon2idHash(encoded string) (storedPasswordHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	memory, okM := argon2Parameter(params[0], "m=", maxArgon2MemoryKiB)
	iterations, okT := argon2Parameter(params[1], "t=", maxArgon2Iterations)
	parallelism, okP := argon2Parameter(params[2], "p=", maxArgon2Parallelism)
	// The lane bound is repeated here so the uint8 conversion below is
	// visibly in range.
	if !okM || !okT || !okP || parallelism > maxArgon2Parallelism || memory < 8*parallelism {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	salt, err := decodeArgon2Part(parts[4])
	if err != nil || len(salt) < minArgon2SaltBytes || len(salt) > maxArgon2SaltBytes {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	key, err := decodeArgon2Part(parts[5])
	if err != nil || len(key) != passwordKeyBytes {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	return storedPasswordHash{
		algorithm: PasswordAlgorithmArgon2id,
		argon2:    argon2Params{memoryKiB: memory, iterations: iterations, parallelism: uint8(parallelism)},
		salt:      salt,
		key:       key,
	}, nil
}

// argon2Parameter parses "name=N" with N a canonical decimal in 1..limit.
func argon2Parameter(field, name string, limit uint32) (uint32, bool) {
	raw, found := strings.CutPrefix(field, name)
	if !found {
		return 0, false
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || strconv.FormatUint(value, 10) != raw || value < 1 || value > uint64(limit) {
		return 0, false
	}
	return uint32(value), true
}

func decodeArgon2Part(encoded string) ([]byte, error) {
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawStdEncoding.EncodeToString(decoded) != encoded {
		return nil, ErrPasswordHashInvalid
	}
	return decoded, nil
}

// parsePasswordHash parses Forge's PBKDF2 format.
func parsePasswordHash(encoded string) (storedPasswordHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "pbkdf2-sha256" || parts[2] != "v=1" || !strings.HasPrefix(parts[3], "i=") {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	rawIterations := strings.TrimPrefix(parts[3], "i=")
	iterations, err := strconv.Atoi(rawIterations)
	if err != nil || strconv.Itoa(iterations) != rawIterations ||
		iterations < minAcceptedPasswordIterations || iterations > maxAcceptedPasswordIterations {
		return storedPasswordHash{}, ErrPasswordHashInvalid
	}
	salt, err := decodePasswordPart(parts[4], passwordSaltBytes)
	if err != nil {
		return storedPasswordHash{}, err
	}
	key, err := decodePasswordPart(parts[5], passwordKeyBytes)
	if err != nil {
		return storedPasswordHash{}, err
	}
	return storedPasswordHash{algorithm: PasswordAlgorithmPBKDF2SHA256, iterations: iterations, salt: salt, key: key}, nil
}

func decodePasswordPart(encoded string, size int) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != size || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, ErrPasswordHashInvalid
	}
	return decoded, nil
}
