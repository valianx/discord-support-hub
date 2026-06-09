// Package oauth handles the Discord OAuth2 "Connect with Discord" flow (FR-22, §6).
//
// Responsibilities:
//   - StateManager: issue and validate HMAC-signed, single-use CSRF state tokens.
//   - TokenStore: encrypt and persist per-user OAuth2 access/refresh tokens.
//   - Exchange: swap a Discord code for tokens via the token endpoint.
//
// CSRF protection: the state parameter is an HMAC-SHA256 signed token binding the
// callback to a nonce stored in a short-lived Valkey key. The nonce is consumed on
// first use (single-use), making replay attacks impossible (AC-3, NFR-14).
//
// Token storage: tokens are AES-256-GCM encrypted via internal/secrets before being
// written to oauth_tokens (AC-3, NFR-6, §7). The plaintext is never written to disk.
package oauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/valianx/discord-support-hub/internal/secrets"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── StateManager ─────────────────────────────────────────────────────────────

// StateManager issues and validates single-use HMAC-signed CSRF state tokens.
// It persists nonces in a backing store interface (Valkey via the cache adapter,
// or an in-memory map for tests).
type StateManager struct {
	hmacSecret []byte
	nonces     nonceStore
}

// nonceStore is a minimal interface for single-use nonce storage.
// Backed by Valkey in production; by a sync.Map in tests.
type nonceStore interface {
	// SetNonce stores a nonce with a TTL. Returns an error on failure.
	SetNonce(ctx context.Context, nonce string, userID string, ttl time.Duration) error
	// ConsumeNonce retrieves and deletes the userID bound to a nonce.
	// Returns ("", false, nil) when the nonce does not exist or has expired.
	ConsumeNonce(ctx context.Context, nonce string) (userID string, ok bool, err error)
}

// NewStateManager creates a StateManager from a hex-encoded HMAC secret (from env).
// The secret must be at least 32 bytes of entropy (256-bit).
func NewStateManager(hexSecret string, nonces nonceStore) (*StateManager, error) {
	secret, err := hex.DecodeString(hexSecret)
	if err != nil {
		return nil, fmt.Errorf("oauth: decode state hmac secret: %w", err)
	}
	if len(secret) < 32 {
		return nil, fmt.Errorf("oauth: state hmac secret must be at least 32 bytes, got %d", len(secret))
	}
	return &StateManager{hmacSecret: secret, nonces: nonces}, nil
}

// stateTTL is how long a state token remains valid for redemption.
const stateTTL = 15 * time.Minute

// statePayload is the JSON body bound to a state token.
type statePayload struct {
	Nonce    string `json:"n"`
	UserID   string `json:"u"`
	IssuedAt int64  `json:"t"` // unix seconds
}

// Issue returns a signed state token for the given userID.
// Format: base64url(payload).base64url(hmac_sha256(payload))
// A random nonce is persisted in the nonceStore with stateTTL.
func (sm *StateManager) Issue(ctx context.Context, userID, _ string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("oauth: generate nonce: %w", err)
	}
	nonceHex := hex.EncodeToString(nonce)

	payload := statePayload{
		Nonce:    nonceHex,
		UserID:   userID,
		IssuedAt: time.Now().Unix(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("oauth: marshal state payload: %w", err)
	}

	mac := hmac.New(sha256.New, sm.hmacSecret)
	mac.Write(payloadJSON)
	sig := mac.Sum(nil)

	payloadB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	state := payloadB64 + "." + sigB64

	// Persist the nonce so Validate can consume it (single-use).
	if err := sm.nonces.SetNonce(ctx, nonceHex, userID, stateTTL); err != nil {
		return "", fmt.Errorf("oauth: persist nonce: %w", err)
	}
	return state, nil
}

// Validate verifies and consumes a state token. Returns the original userID on success.
// Returns an error when the state is missing, malformed, HMAC-invalid, or already used.
func (sm *StateManager) Validate(ctx context.Context, state string) (string, error) {
	if state == "" {
		return "", fmt.Errorf("oauth: state is required")
	}

	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("oauth: malformed state token")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("oauth: decode state payload: %w", err)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("oauth: decode state signature: %w", err)
	}

	// Verify HMAC.
	mac := hmac.New(sha256.New, sm.hmacSecret)
	mac.Write(payloadJSON)
	expected := mac.Sum(nil)
	if !hmac.Equal(sigBytes, expected) {
		return "", fmt.Errorf("oauth: state signature invalid")
	}

	var payload statePayload
	if err = json.Unmarshal(payloadJSON, &payload); err != nil {
		return "", fmt.Errorf("oauth: unmarshal state payload: %w", err)
	}

	// Consume the nonce (single-use — replay protection).
	storedUserID, ok, err := sm.nonces.ConsumeNonce(ctx, payload.Nonce)
	if err != nil {
		return "", fmt.Errorf("oauth: consume nonce: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("oauth: state token expired or already used")
	}
	// Guard: the nonce must bind to the same userID as the payload.
	if storedUserID != "" && storedUserID != payload.UserID {
		return "", fmt.Errorf("oauth: state user binding mismatch")
	}

	return payload.UserID, nil
}

// ─── TokenStore ───────────────────────────────────────────────────────────────

// TokenStore encrypts and persists per-user OAuth2 tokens (NFR-6, §7).
type TokenStore struct {
	encrypter *secrets.Encrypter
	store     store.Store
}

// NewTokenStore creates a TokenStore backed by the given store and encrypter.
func NewTokenStore(s store.Store, enc *secrets.Encrypter) *TokenStore {
	return &TokenStore{store: s, encrypter: enc}
}

// Store encrypts the access/refresh tokens and saves them for the given user.
// Tokens are never written in plaintext — only ciphertext + nonce + key_version (NFR-6).
func (ts *TokenStore) Store(ctx context.Context, userID, accessToken, refreshToken, scopes string, expiresAt *time.Time) error {
	accessEnc, err := ts.encrypter.Encrypt([]byte(accessToken))
	if err != nil {
		return fmt.Errorf("oauth: encrypt access token: %w", err)
	}

	params := store.UpsertOAuthTokenParams{
		UserID:               userID,
		AccessTokenCipher:    accessEnc.Ciphertext,
		AccessTokenNonce:     accessEnc.Nonce,
		EncryptionKeyVersion: accessEnc.KeyVersion,
		Scopes:               scopes,
		ExpiresAt:            expiresAt,
	}

	if refreshToken != "" {
		refreshEnc, encErr := ts.encrypter.Encrypt([]byte(refreshToken))
		if encErr != nil {
			return fmt.Errorf("oauth: encrypt refresh token: %w", encErr)
		}
		params.RefreshTokenCipher = refreshEnc.Ciphertext
		params.RefreshTokenNonce = refreshEnc.Nonce
	}

	if _, err = ts.store.UpsertOAuthToken(ctx, params); err != nil {
		return fmt.Errorf("oauth: upsert token: %w", err)
	}
	return nil
}

// LoadAccessToken decrypts and returns the access token for the given user.
// Returns store.ErrNotFound when no token has been stored yet.
func (ts *TokenStore) LoadAccessToken(ctx context.Context, userID string) (string, error) {
	token, err := ts.store.GetOAuthTokenByUserID(ctx, userID)
	if err != nil {
		return "", err
	}
	plain, err := ts.encrypter.Decrypt(&secrets.EncryptedValue{
		Ciphertext: token.AccessTokenCipher,
		Nonce:      token.AccessTokenNonce,
		KeyVersion: token.EncryptionKeyVersion,
	})
	if err != nil {
		return "", fmt.Errorf("oauth: decrypt access token for user %s: %w", userID, err)
	}
	return string(plain), nil
}

// ─── Code exchange ────────────────────────────────────────────────────────────

// TokenResponse holds the fields returned by Discord's token endpoint.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"` // seconds
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// ExchangeConfig carries the OAuth2 application credentials.
type ExchangeConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// ExchangeCode swaps an authorization code for tokens at Discord's token endpoint.
// The clientSecret is passed in (from env/config, never hardcoded).
// The HTTP client is passed in so tests can inject a fake transport.
func ExchangeCode(ctx context.Context, cfg ExchangeConfig, code string, httpClient *http.Client) (*TokenResponse, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	data := url.Values{}
	data.Set("client_id", cfg.ClientID)
	data.Set("client_secret", cfg.ClientSecret)
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", cfg.RedirectURL)

	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		"https://discord.com/api/oauth2/token",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Warn("oauth: token exchange failed",
			"status", resp.StatusCode,
			// Never log the code or client_secret — they are consumed/secret.
		)
		return nil, fmt.Errorf("oauth: discord token endpoint returned %d: %s", resp.StatusCode, body)
	}

	var tr TokenResponse
	if err = json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, fmt.Errorf("oauth: decode token response: %w", err)
	}
	return &tr, nil
}

// DiscordUserResponse holds the fields from Discord's /users/@me endpoint.
type DiscordUserResponse struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// FetchDiscordUser calls /users/@me with the access token to retrieve the Discord user id.
// The access token is held only in memory (never logged, per NFR-6).
func FetchDiscordUser(ctx context.Context, accessToken string, httpClient *http.Client) (*DiscordUserResponse, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequestWithContext(ctx,
		http.MethodGet,
		"https://discord.com/api/users/@me",
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("oauth: build users/@me request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: users/@me request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: users/@me returned %d", resp.StatusCode)
	}

	var u DiscordUserResponse
	if err = json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("oauth: decode users/@me: %w", err)
	}
	return &u, nil
}

// BuildConnectURL constructs the Discord OAuth2 authorization URL for a user.
// The state token (HMAC-signed, single-use) is embedded so the callback can
// validate the response is bound to this request (CSRF protection, AC-3).
func BuildConnectURL(clientID, redirectURL, state string) string {
	v := url.Values{}
	v.Set("client_id", clientID)
	v.Set("redirect_uri", redirectURL)
	v.Set("response_type", "code")
	v.Set("scope", "identify guilds.join")
	v.Set("state", state)
	return "https://discord.com/api/oauth2/authorize?" + v.Encode()
}
