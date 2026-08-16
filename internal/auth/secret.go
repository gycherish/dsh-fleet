// Package auth mints and verifies the two kinds of secret this control plane
// issues: node tokens and console passwords.
//
// Both are stored as argon2id hashes in the PostgreSQL-standard encoded form,
// so the parameters travel with the hash and can be raised later without a
// migration or a flag day.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// TokenPrefix marks a node token so an operator can recognise one on sight and
// secret scanners have something to match.
const TokenPrefix = "nt_"

const (
	// Tuned for an interactive login on a small VPS: ~64 MiB and one pass is
	// the argon2id RFC 9106 second-recommended profile.
	argonTime    uint32 = 1
	argonMemory  uint32 = 64 * 1024
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// ErrMalformedHash reports a stored hash this build cannot parse.
var ErrMalformedHash = errors.New("auth: malformed argon2id hash")

// ErrUnsupportedHash reports a hash whose algorithm or version is not argon2id v19.
var ErrUnsupportedHash = errors.New("auth: unsupported hash algorithm or version")

// NewNodeToken mints one node token.
//
// The plaintext is returned exactly once, to be printed by `dshf node add`;
// only its hash reaches the database.
func NewNodeToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: cannot read entropy: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Hash derives an encoded argon2id hash of secret.
func Hash(secret string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: cannot read entropy: %w", err)
	}
	threads := uint8(runtime.NumCPU())
	if threads > 4 {
		threads = 4
	}
	if threads == 0 {
		threads = 1
	}
	sum := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, threads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum),
	), nil
}

// Verify reports whether secret matches an encoded argon2id hash.
//
// Parameters are read from the hash rather than from this package's constants,
// so raising the cost later leaves existing credentials verifiable.
func Verify(secret, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" {
		return false, ErrMalformedHash
	}
	if parts[1] != "argon2id" {
		return false, ErrUnsupportedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrMalformedHash
	}
	if version != argon2.Version {
		return false, ErrUnsupportedHash
	}

	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, ErrMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrMalformedHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrMalformedHash
	}

	got := argon2.IDKey([]byte(secret), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
