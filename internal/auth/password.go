package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters used when hashing new passwords. VerifyPassword reads the
// concrete parameters back from the encoded string, so changing these only
// affects freshly produced hashes (older hashes still verify).
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB → 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// errMalformedHash is returned (wrapped) when an encoded hash cannot be parsed.
var errMalformedHash = errors.New("auth: malformed argon2id hash")

// HashPassword derives an argon2id hash of pw and returns it as a standard PHC
// string of the form:
//
//	$argon2id$v=19$m=65536,t=1,p=4$<b64salt>$<b64hash>
//
// A fresh 16-byte cryptographically random salt is used for every call.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyPassword reports whether pw matches the argon2id PHC-encoded hash. The
// argon2 parameters (memory, time, parallelism), salt, and digest length are
// parsed from encoded so that hashes produced with different parameters still
// verify. The comparison is constant-time. A malformed encoded string yields a
// non-nil error (and ok == false); a simple mismatch yields (false, nil).
func VerifyPassword(encoded, pw string) (bool, error) {
	memory, time, threads, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, time, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// parsePHC decodes an argon2id PHC string into its parameters and raw bytes.
func parsePHC(encoded string) (memory, time uint32, threads uint8, salt, hash []byte, err error) {
	// Layout: ["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"]
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		err = fmt.Errorf("%w: bad structure", errMalformedHash)
		return
	}
	if parts[1] != "argon2id" {
		err = fmt.Errorf("%w: unsupported variant %q", errMalformedHash, parts[1])
		return
	}

	var version int
	if _, e := fmt.Sscanf(parts[2], "v=%d", &version); e != nil {
		err = fmt.Errorf("%w: bad version: %v", errMalformedHash, e)
		return
	}
	if version != argon2.Version {
		err = fmt.Errorf("%w: unsupported version %d", errMalformedHash, version)
		return
	}

	if _, e := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); e != nil {
		err = fmt.Errorf("%w: bad params: %v", errMalformedHash, e)
		return
	}

	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil {
		err = fmt.Errorf("%w: bad salt: %v", errMalformedHash, e)
		return
	}
	hash, e = base64.RawStdEncoding.DecodeString(parts[5])
	if e != nil {
		err = fmt.Errorf("%w: bad hash: %v", errMalformedHash, e)
		return
	}
	if len(salt) == 0 || len(hash) == 0 {
		err = fmt.Errorf("%w: empty salt or hash", errMalformedHash)
		return
	}
	return memory, time, threads, salt, hash, nil
}
