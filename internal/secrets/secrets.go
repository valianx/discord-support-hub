// Package secrets provides AES-256-GCM encryption/decryption for secrets at rest (NFR-6)
// and a structured-log redaction helper that scrubs sensitive field values.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrInvalidKey is returned when the decoded key length is not 32 bytes.
var ErrInvalidKey = errors.New("secrets: encryption key must be 32 bytes (AES-256)")

// ErrCiphertextTooShort is returned when ciphertext is shorter than the nonce.
var ErrCiphertextTooShort = errors.New("secrets: ciphertext too short")

// EncryptedValue holds the ciphertext, nonce, and key version for a secret stored at rest.
// The DB columns access_token_cipher/nonce and refresh_token_cipher/nonce correspond to
// EncryptedValue.Ciphertext and EncryptedValue.Nonce respectively.
type EncryptedValue struct {
	Ciphertext []byte
	Nonce      []byte
	KeyVersion int
}

// Encrypter performs AES-256-GCM authenticated encryption/decryption.
// It is safe for concurrent use.
type Encrypter struct {
	keyVersion int
	key        []byte // exactly 32 bytes
}

// NewEncrypter creates an Encrypter from a base64-encoded 32-byte key and a key version.
// The key version is stored alongside ciphertext to allow key rotation without downtime.
func NewEncrypter(base64Key string, keyVersion int) (*Encrypter, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("secrets: decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	return &Encrypter{key: key, keyVersion: keyVersion}, nil
}

// NewEncrypterFromRaw creates an Encrypter from a raw 32-byte key.
// Used in tests; production code uses NewEncrypter with a base64 env var.
func NewEncrypterFromRaw(key []byte, keyVersion int) (*Encrypter, error) {
	if len(key) != 32 {
		return nil, ErrInvalidKey
	}
	keyCopy := make([]byte, 32)
	copy(keyCopy, key)
	return &Encrypter{key: keyCopy, keyVersion: keyVersion}, nil
}

// Encrypt seals plaintext with AES-256-GCM and a randomly generated nonce.
// The nonce is stored separately (not prepended) to match the DB schema.
func (e *Encrypter) Encrypt(plaintext []byte) (*EncryptedValue, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	return &EncryptedValue{
		Ciphertext: ciphertext,
		Nonce:      nonce,
		KeyVersion: e.keyVersion,
	}, nil
}

// Decrypt opens ciphertext sealed with AES-256-GCM using the provided nonce.
// Returns the plaintext or an error if the ciphertext has been tampered with.
func (e *Encrypter) Decrypt(ev *EncryptedValue) ([]byte, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}

	if len(ev.Ciphertext) < gcm.NonceSize() {
		return nil, ErrCiphertextTooShort
	}

	plaintext, err := gcm.Open(nil, ev.Nonce, ev.Ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plaintext, nil
}

// secretFields is the set of log attribute keys whose values must be redacted (NFR-6).
// Checked case-insensitively.
var secretFields = []string{
	"token",
	"authorization",
	"api_key",
	"access_token",
	"refresh_token",
	"bot_token",
}

const redacted = "***REDACTED***"

// RedactLogAttr inspects a slog-style key-value pair and returns the redacted value
// when the key matches a known secret field. Otherwise returns the original value unchanged.
//
// Usage in a slog.Handler: wrap every attribute through this function before emitting.
func RedactLogAttr(key string, value any) any {
	lower := strings.ToLower(key)
	for _, f := range secretFields {
		if strings.Contains(lower, f) {
			return redacted
		}
	}
	return value
}

// IsSecretKey reports whether a log attribute key refers to a secret field.
func IsSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, f := range secretFields {
		if strings.Contains(lower, f) {
			return true
		}
	}
	return false
}

// RedactMap returns a copy of m with all secret keys replaced by redacted.
// Useful for redacting JSONB payloads before writing to the audit log.
func RedactMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if IsSecretKey(k) {
			out[k] = redacted
		} else {
			out[k] = v
		}
	}
	return out
}
