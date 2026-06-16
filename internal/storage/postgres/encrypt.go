package postgres

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// errNoSecretKey is returned by the encryptor when a bot token operation is
// attempted but New was constructed without a secret key.
var errNoSecretKey = errors.New("postgres: no secret key configured for bot tokens")

// encryptor seals and opens bot tokens with AES-256-GCM. The 12-byte random
// nonce is prepended to the ciphertext in token_enc. token_sha is the sha256
// hex digest of the plaintext token, used for idempotent seeding and lookups.
type encryptor struct {
	aead cipher.AEAD // nil when no key was supplied
}

// newEncryptor builds an encryptor from a 32-byte AES-256 key. A nil key is
// permitted (no bots configured); token operations then return errNoSecretKey.
func newEncryptor(key []byte) (*encryptor, error) {
	if len(key) == 0 {
		return &encryptor{}, nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("new gcm: %w", err)
	}
	return &encryptor{aead: aead}, nil
}

// tokenSHA returns the sha256 hex digest of token.
func tokenSHA(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// encrypt seals token, returning nonce||ciphertext.
func (e *encryptor) encrypt(token string) ([]byte, error) {
	if e.aead == nil {
		return nil, errNoSecretKey
	}
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("read nonce: %w", err)
	}
	ct := e.aead.Seal(nil, nonce, []byte(token), nil)
	return append(nonce, ct...), nil
}

// decrypt opens a nonce||ciphertext blob produced by encrypt.
func (e *encryptor) decrypt(enc []byte) (string, error) {
	if e.aead == nil {
		return "", errNoSecretKey
	}
	ns := e.aead.NonceSize()
	if len(enc) < ns {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := enc[:ns], enc[ns:]
	pt, err := e.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("gcm open: %w", err)
	}
	return string(pt), nil
}
