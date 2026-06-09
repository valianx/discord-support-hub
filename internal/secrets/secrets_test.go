package secrets_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/valianx/discord-support-hub/internal/secrets"
)

// TestEncryptDecryptRoundTrip verifies that AES-256-GCM round-trips correctly.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	enc, err := secrets.NewEncrypterFromRaw(key, 1)
	if err != nil {
		t.Fatalf("NewEncrypterFromRaw: %v", err)
	}

	plaintext := []byte("oauth2-access-token-value-super-secret")

	ev, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Ciphertext must differ from plaintext.
	if bytes.Equal(ev.Ciphertext, plaintext) {
		t.Error("ciphertext equals plaintext — encryption did nothing")
	}

	// Nonce must be non-empty.
	if len(ev.Nonce) == 0 {
		t.Error("nonce is empty")
	}

	// KeyVersion must be preserved.
	if ev.KeyVersion != 1 {
		t.Errorf("KeyVersion: want 1, got %d", ev.KeyVersion)
	}

	// Decrypt must recover the original plaintext.
	recovered, err := enc.Decrypt(ev)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(recovered, plaintext) {
		t.Errorf("recovered %q, want %q", recovered, plaintext)
	}
}

// TestEncryptDifferentNonces verifies that two encryptions of the same plaintext
// produce different ciphertexts (nonce is randomised each call).
func TestEncryptDifferentNonces(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	enc, _ := secrets.NewEncrypterFromRaw(key, 1)
	plaintext := []byte("same-value")

	ev1, _ := enc.Encrypt(plaintext)
	ev2, _ := enc.Encrypt(plaintext)

	if bytes.Equal(ev1.Nonce, ev2.Nonce) {
		t.Error("two encryptions share the same nonce — this breaks GCM security")
	}
	if bytes.Equal(ev1.Ciphertext, ev2.Ciphertext) {
		t.Error("two encryptions of the same plaintext produce identical ciphertexts")
	}
}

// TestNewEncrypterFromRaw_InvalidKeyLength verifies that a non-32-byte key is rejected.
func TestNewEncrypterFromRaw_InvalidKeyLength(t *testing.T) {
	_, err := secrets.NewEncrypterFromRaw(make([]byte, 16), 1)
	if err == nil {
		t.Error("NewEncrypterFromRaw should reject a 16-byte key")
	}
}

// TestDecrypt_TamperedCiphertext verifies that GCM authentication rejects tampered data.
func TestDecrypt_TamperedCiphertext(t *testing.T) {
	key := make([]byte, 32)
	rand.Read(key)

	enc, _ := secrets.NewEncrypterFromRaw(key, 1)
	ev, _ := enc.Encrypt([]byte("secret"))

	// Flip a byte in the ciphertext to simulate tampering.
	ev.Ciphertext[0] ^= 0xFF

	_, err := enc.Decrypt(ev)
	if err == nil {
		t.Error("Decrypt should fail for tampered ciphertext")
	}
}

// --- Redaction tests (NFR-6 / AC-5 and AC-6 in M0/M1) ---

// TestRedactLogAttr_SecretKeys verifies that known secret keys are redacted.
func TestRedactLogAttr_SecretKeys(t *testing.T) {
	cases := []struct {
		key   string
		value any
	}{
		{"bot_token", "real-bot-token"},
		{"access_token", "real-access-token"},
		{"refresh_token", "real-refresh-token"},
		{"authorization", "Bearer real-key"},
		{"api_key", "real-api-key"},
		{"Authorization", "Bearer real-key"}, // case-insensitive
		{"BOT_TOKEN", "real-token"},          // uppercase variant
	}

	for _, tc := range cases {
		got := secrets.RedactLogAttr(tc.key, tc.value)
		if got == tc.value {
			t.Errorf("RedactLogAttr(%q): expected redaction but got original value %q", tc.key, tc.value)
		}
	}
}

// TestRedactLogAttr_NonSecretKeys verifies that normal keys pass through unchanged.
func TestRedactLogAttr_NonSecretKeys(t *testing.T) {
	cases := []struct {
		key   string
		value any
	}{
		{"user_id", "abc-123"},
		{"space_id", "def-456"},
		{"action", "provision"},
		{"status", "ok"},
	}

	for _, tc := range cases {
		got := secrets.RedactLogAttr(tc.key, tc.value)
		if got != tc.value {
			t.Errorf("RedactLogAttr(%q): expected pass-through %v but got %v", tc.key, tc.value, got)
		}
	}
}

// TestRedactMap_NoSecretValuesLeak verifies that a JSONB payload map has secret values scrubbed.
func TestRedactMap_NoSecretValuesLeak(t *testing.T) {
	secretValue := "super-secret-token-value-that-must-not-appear"

	input := map[string]any{
		"user_id":       "abc-123",
		"access_token":  secretValue,
		"refresh_token": secretValue,
		"action":        "invite",
	}

	output := secrets.RedactMap(input)

	for k, v := range output {
		if v == secretValue {
			t.Errorf("RedactMap: secret value leaked under key %q", k)
		}
	}

	// Non-secret values must be preserved.
	if output["user_id"] != "abc-123" {
		t.Errorf("RedactMap: non-secret user_id was altered")
	}
	if output["action"] != "invite" {
		t.Errorf("RedactMap: non-secret action was altered")
	}
}
