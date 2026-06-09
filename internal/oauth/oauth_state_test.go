// Package oauth_test verifies the HMAC-signed single-use state token lifecycle (AC-3).
//
// Tests cover:
//   - Issue + Validate round-trip (happy path).
//   - Missing state → rejected.
//   - Tampered payload → HMAC invalid → rejected.
//   - Tampered signature → HMAC invalid → rejected.
//   - Replay (consumed nonce) → rejected.
//   - Token storage: encrypted ciphertext is stored, plaintext is never persisted.
//   - Encrypted token round-trip: Store then LoadAccessToken decrypts correctly.
package oauth_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/secrets"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Factories ────────────────────────────────────────────────────────────────

// makeStateManager returns a StateManager with a 64-hex-char (32-byte) secret
// backed by an in-memory nonce store. Safe for deterministic unit tests.
func makeStateManager(t *testing.T) *oauth.StateManager {
	t.Helper()
	// 32 bytes of fake entropy, hex-encoded (64 chars).
	secret := "aabbccdd" + "11223344" + "aabbccdd" + "11223344" +
		"aabbccdd" + "11223344" + "aabbccdd" + "11223344"
	sm, err := oauth.NewStateManager(secret, oauth.NewMemNonceStore())
	if err != nil {
		t.Fatalf("makeStateManager: %v", err)
	}
	return sm
}

// makeEncrypter returns a secrets.Encrypter with a fake 32-byte AES-256-GCM key.
func makeEncrypter(t *testing.T) *secrets.Encrypter {
	t.Helper()
	// 32 zero bytes, base64-encoded — all-zero key is fine for unit tests.
	keyB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	enc, err := secrets.NewEncrypter(keyB64, 1)
	if err != nil {
		t.Fatalf("makeEncrypter: %v", err)
	}
	return enc
}

// ─── StateManager tests ───────────────────────────────────────────────────────

// TestStateManager_Issue_Validate_RoundTrip verifies that a freshly issued state token
// can be validated and returns the correct userID (AC-3 happy path).
func TestStateManager_Issue_Validate_RoundTrip(t *testing.T) {
	sm := makeStateManager(t)
	ctx := context.Background()
	const userID = "user-oauth-001"

	state, err := sm.Issue(ctx, userID, "https://hub.example.com/callback")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if state == "" {
		t.Fatal("Issue returned empty state token")
	}

	got, err := sm.Validate(ctx, state)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got != userID {
		t.Errorf("Validate returned userID %q, want %q", got, userID)
	}
}

// TestStateManager_MissingState_Rejected verifies that an empty state string is rejected (AC-3).
func TestStateManager_MissingState_Rejected(t *testing.T) {
	sm := makeStateManager(t)
	ctx := context.Background()

	_, err := sm.Validate(ctx, "")
	if err == nil {
		t.Fatal("Validate with empty state must return an error")
	}
}

// TestStateManager_TamperedPayload_Rejected verifies that changing the payload portion of
// a valid state token causes the HMAC signature check to fail (AC-3).
func TestStateManager_TamperedPayload_Rejected(t *testing.T) {
	sm := makeStateManager(t)
	ctx := context.Background()

	state, err := sm.Issue(ctx, "user-t1", "https://hub.example.com/callback")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// State format: base64url(payload).base64url(sig)
	// Tamper the payload by appending a character to the raw base64 section.
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected state format (no dot): %s", state)
	}
	// Decode, modify, re-encode to produce an invalid payload that changes the JSON content.
	payloadJSON, decErr := base64.RawURLEncoding.DecodeString(parts[0])
	if decErr != nil {
		t.Fatalf("decode payload: %v", decErr)
	}
	// Append a space to the JSON to produce a different byte string (same JSON meaning,
	// different bytes → HMAC mismatch because HMAC is computed over raw bytes).
	tampered := base64.RawURLEncoding.EncodeToString(append(payloadJSON, ' '))
	tamperedState := tampered + "." + parts[1]

	_, err = sm.Validate(ctx, tamperedState)
	if err == nil {
		t.Fatal("Validate with tampered payload must return an error")
	}
}

// TestStateManager_TamperedSignature_Rejected verifies that an altered signature portion
// of a valid state token is rejected (AC-3).
func TestStateManager_TamperedSignature_Rejected(t *testing.T) {
	sm := makeStateManager(t)
	ctx := context.Background()

	state, err := sm.Issue(ctx, "user-t2", "https://hub.example.com/callback")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected state format (no dot): %s", state)
	}
	// Flip the first character of the base64-encoded signature.
	sigBytes := []byte(parts[1])
	if sigBytes[0] == 'A' {
		sigBytes[0] = 'B'
	} else {
		sigBytes[0] = 'A'
	}
	tamperedState := parts[0] + "." + string(sigBytes)

	_, err = sm.Validate(ctx, tamperedState)
	if err == nil {
		t.Fatal("Validate with tampered signature must return an error")
	}
}

// TestStateManager_Replay_Rejected verifies that a valid state token is single-use:
// a second Validate call with the same token is rejected (AC-3 — replay protection).
func TestStateManager_Replay_Rejected(t *testing.T) {
	sm := makeStateManager(t)
	ctx := context.Background()

	state, err := sm.Issue(ctx, "user-replay", "https://hub.example.com/callback")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// First use must succeed.
	if _, err = sm.Validate(ctx, state); err != nil {
		t.Fatalf("first Validate must succeed: %v", err)
	}

	// Second use (replay) must fail.
	_, err = sm.Validate(ctx, state)
	if err == nil {
		t.Fatal("AC-3 violated: replaying a consumed state token must return an error")
	}
}

// TestStateManager_MalformedToken_Rejected verifies that a token with no dot separator
// (i.e. not in the expected format) is rejected without panic (AC-3).
func TestStateManager_MalformedToken_Rejected(t *testing.T) {
	sm := makeStateManager(t)
	ctx := context.Background()

	for _, bad := range []string{
		"nodotanywhere",
		"not-base64!!.alsoinvalid",
		"validbase64.but-sig-not-base64!",
		".",
	} {
		_, err := sm.Validate(ctx, bad)
		if err == nil {
			t.Errorf("malformed token %q must be rejected", bad)
		}
	}
}

// ─── TokenStore tests ─────────────────────────────────────────────────────────

// tokenFakeStore is a minimal in-memory store.Store for TokenStore tests.
// Only UpsertOAuthToken and GetOAuthTokenByUserID are implemented; others panic.
type tokenFakeStore struct {
	tokens map[string]*domain.OAuthToken // userID → encrypted token
}

func newTokenFakeStore() *tokenFakeStore {
	return &tokenFakeStore{tokens: make(map[string]*domain.OAuthToken)}
}

// full store.Store interface — methods used by TokenStore are implemented; rest panic.

func (f *tokenFakeStore) UpsertOAuthToken(_ context.Context, p store.UpsertOAuthTokenParams) (*domain.OAuthToken, error) {
	tok := &domain.OAuthToken{
		UserID:               p.UserID,
		AccessTokenCipher:    p.AccessTokenCipher,
		AccessTokenNonce:     p.AccessTokenNonce,
		RefreshTokenCipher:   p.RefreshTokenCipher,
		RefreshTokenNonce:    p.RefreshTokenNonce,
		EncryptionKeyVersion: p.EncryptionKeyVersion,
		Scopes:               p.Scopes,
		ExpiresAt:            p.ExpiresAt,
	}
	f.tokens[p.UserID] = tok
	return tok, nil
}

func (f *tokenFakeStore) GetOAuthTokenByUserID(_ context.Context, userID string) (*domain.OAuthToken, error) {
	tok, ok := f.tokens[userID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return tok, nil
}

// Remaining store.Store methods — panic because TokenStore never calls them.
func (f *tokenFakeStore) Ping(_ context.Context) error { panic("Ping") }
func (f *tokenFakeStore) CreateMerchant(_ context.Context, _ store.CreateMerchantParams) (*domain.Merchant, error) {
	panic("CreateMerchant")
}
func (f *tokenFakeStore) GetMerchantByID(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByID")
}
func (f *tokenFakeStore) GetMerchantByExternalRef(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByExternalRef")
}
func (f *tokenFakeStore) ListMerchants(_ context.Context, _ store.ListMerchantsParams) ([]*domain.Merchant, error) {
	panic("ListMerchants")
}
func (f *tokenFakeStore) CreateUser(_ context.Context, _ store.CreateUserParams) (*domain.User, error) {
	panic("CreateUser")
}
func (f *tokenFakeStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByID")
}
func (f *tokenFakeStore) GetUserByDiscordID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByDiscordID")
}
func (f *tokenFakeStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	panic("ListAgents")
}
func (f *tokenFakeStore) DeactivateUser(_ context.Context, _ string) (*domain.User, error) {
	panic("DeactivateUser")
}
func (f *tokenFakeStore) SetUserProvisionedAt(_ context.Context, _ string) (*domain.User, error) {
	panic("SetUserProvisionedAt")
}
func (f *tokenFakeStore) CreateAPIKey(_ context.Context, _ store.CreateAPIKeyParams) (*domain.APIKey, error) {
	panic("CreateAPIKey")
}
func (f *tokenFakeStore) ListAPIKeys(_ context.Context, _ bool) ([]*domain.APIKey, error) {
	panic("ListAPIKeys")
}
func (f *tokenFakeStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	panic("LookupActiveAPIKeyByHash")
}
func (f *tokenFakeStore) RevokeAPIKey(_ context.Context, _ string) error { panic("RevokeAPIKey") }
func (f *tokenFakeStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	panic("TouchAPIKeyLastUsed")
}
func (f *tokenFakeStore) CreateSpace(_ context.Context, _ store.CreateSpaceParams) (*domain.Space, error) {
	panic("CreateSpace")
}
func (f *tokenFakeStore) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByID")
}
func (f *tokenFakeStore) GetSpaceByMerchantID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByMerchantID")
}
func (f *tokenFakeStore) UpdateSpaceDiscordChannel(_ context.Context, _ store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	panic("UpdateSpaceDiscordChannel")
}
func (f *tokenFakeStore) UpdateSpaceACLState(_ context.Context, _ string, _ domain.ACLState) (*domain.Space, error) {
	panic("UpdateSpaceACLState")
}
func (f *tokenFakeStore) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	panic("CreateJob")
}
func (f *tokenFakeStore) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	panic("GetJobByID")
}
func (f *tokenFakeStore) UpdateJobStatus(_ context.Context, _ store.UpdateJobStatusParams) (*domain.Job, error) {
	panic("UpdateJobStatus")
}
func (f *tokenFakeStore) InsertIdempotencyKey(_ context.Context, _ store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	panic("InsertIdempotencyKey")
}
func (f *tokenFakeStore) GetIdempotencyKey(_ context.Context, _ string) (*domain.IdempotencyKey, error) {
	panic("GetIdempotencyKey")
}
func (f *tokenFakeStore) UpdateIdempotencyKeyResponse(_ context.Context, _ store.UpdateIdempotencyKeyResponseParams) error {
	panic("UpdateIdempotencyKeyResponse")
}
func (f *tokenFakeStore) CreateSpaceWithOutbox(_ context.Context, _ store.CreateSpaceParams, _ store.CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error) {
	panic("CreateSpaceWithOutbox")
}
func (f *tokenFakeStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	panic("ListPendingOutbox")
}
func (f *tokenFakeStore) StampOutboxEnqueued(_ context.Context, _ []string) error {
	panic("StampOutboxEnqueued")
}
func (f *tokenFakeStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	panic("UpdateOutboxPayload")
}
func (f *tokenFakeStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	panic("InsertAuditEntry")
}
func (f *tokenFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	panic("ListSpaces")
}
func (f *tokenFakeStore) CreateSpaceMember(_ context.Context, _ store.CreateSpaceMemberParams) (*domain.SpaceMember, error) {
	panic("CreateSpaceMember")
}
func (f *tokenFakeStore) GetSpaceMemberBySpaceAndUser(_ context.Context, _, _ string) (*domain.SpaceMember, error) {
	panic("GetSpaceMemberBySpaceAndUser")
}
func (f *tokenFakeStore) SetSpaceMemberOverwriteApplied(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("SetSpaceMemberOverwriteApplied")
}
func (f *tokenFakeStore) RevokeSpaceMember(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("RevokeSpaceMember")
}
func (f *tokenFakeStore) ListSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListSpaceMembers")
}
func (f *tokenFakeStore) ListCollaboratorChannels(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListCollaboratorChannels")
}
func (f *tokenFakeStore) ListDirectory(_ context.Context, _ store.ListDirectoryParams) ([]*store.DirectoryEntry, error) {
	panic("ListDirectory")
}
func (f *tokenFakeStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error {
	panic("UpdateSpaceReconciledAt")
}
func (f *tokenFakeStore) ListActiveSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListActiveSpaceMembers")
}
func (f *tokenFakeStore) UpdateDiscordUserID(_ context.Context, _, _ string) error {
	panic("UpdateDiscordUserID")
}

// M4 store methods — not exercised by oauth tests; all panic.
func (f *tokenFakeStore) UpdateSpaceLifecycle(_ context.Context, _ store.UpdateSpaceLifecycleParams) (*domain.Space, error) {
	panic("UpdateSpaceLifecycle")
}
func (f *tokenFakeStore) UpdateSpaceWelcomeMessageID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceWelcomeMessageID")
}
func (f *tokenFakeStore) ListAuditEntries(_ context.Context, _ store.ListAuditEntriesParams) ([]*domain.AuditEntry, error) {
	panic("ListAuditEntries")
}
func (f *tokenFakeStore) GetJobBySpaceIDAndKind(_ context.Context, _, _ string) (*domain.Job, error) {
	return nil, store.ErrNotFound
}

// ListActiveProvisionedSpaces satisfies store.Store (added in M5 for the scheduled sweep).
func (f *tokenFakeStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	panic("ListActiveProvisionedSpaces")
}

// TestTokenStore_Store_PersistsEncryptedOnly verifies that tokens are stored as
// ciphertext and that the plaintext does NOT appear in the stored row (AC-3, NFR-6).
func TestTokenStore_Store_PersistsEncryptedOnly(t *testing.T) {
	fs := newTokenFakeStore()
	enc := makeEncrypter(t)
	ts := oauth.NewTokenStore(fs, enc)
	ctx := context.Background()

	const userID = "user-token-001"
	const accessToken = "secret-access-token-abc123"
	const refreshToken = "secret-refresh-token-xyz789"

	if err := ts.Store(ctx, userID, accessToken, refreshToken, "identify guilds.join", nil); err != nil {
		t.Fatalf("Store: %v", err)
	}

	stored, ok := fs.tokens[userID]
	if !ok {
		t.Fatal("no token row stored for user")
	}

	// Plaintext must NOT appear in the ciphertext byte slices.
	if strings.Contains(string(stored.AccessTokenCipher), accessToken) {
		t.Error("AC-3/NFR-6 violated: access_token plaintext found in stored ciphertext")
	}
	if strings.Contains(string(stored.RefreshTokenCipher), refreshToken) {
		t.Error("AC-3/NFR-6 violated: refresh_token plaintext found in stored ciphertext")
	}
	// Ciphertext must be non-empty.
	if len(stored.AccessTokenCipher) == 0 {
		t.Error("AccessTokenCipher is empty — nothing was encrypted")
	}
	if len(stored.AccessTokenNonce) == 0 {
		t.Error("AccessTokenNonce is empty — AES-GCM nonce was not stored")
	}
	// key_version must be recorded alongside the ciphertext (non-zero for rotation support).
	if stored.EncryptionKeyVersion == 0 {
		t.Error("EncryptionKeyVersion is zero — token rotation cannot work without it")
	}
}

// TestTokenStore_LoadAccessToken_DecryptsCorrectly verifies the round-trip: Store then
// LoadAccessToken must return the original plaintext (AC-3, NFR-6).
func TestTokenStore_LoadAccessToken_DecryptsCorrectly(t *testing.T) {
	fs := newTokenFakeStore()
	enc := makeEncrypter(t)
	ts := oauth.NewTokenStore(fs, enc)
	ctx := context.Background()

	const userID = "user-token-002"
	const accessToken = "round-trip-access-token-12345"

	if err := ts.Store(ctx, userID, accessToken, "", "identify guilds.join", nil); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := ts.LoadAccessToken(ctx, userID)
	if err != nil {
		t.Fatalf("LoadAccessToken: %v", err)
	}
	if got != accessToken {
		t.Errorf("LoadAccessToken returned %q, want %q", got, accessToken)
	}
}

// TestTokenStore_LoadAccessToken_NotFound verifies that loading a token for an unknown
// user returns store.ErrNotFound, not a panic or silent empty string (defensive).
func TestTokenStore_LoadAccessToken_NotFound(t *testing.T) {
	fs := newTokenFakeStore()
	enc := makeEncrypter(t)
	ts := oauth.NewTokenStore(fs, enc)
	ctx := context.Background()

	_, err := ts.LoadAccessToken(ctx, "non-existent-user")
	if err == nil {
		t.Fatal("LoadAccessToken for unknown user must return an error")
	}
}

// TestTokenStore_ExpiresAt_StoredWhenProvided verifies that an expiresAt value is forwarded
// to the store when provided (needed for token expiry tracking).
func TestTokenStore_ExpiresAt_StoredWhenProvided(t *testing.T) {
	fs := newTokenFakeStore()
	enc := makeEncrypter(t)
	ts := oauth.NewTokenStore(fs, enc)
	ctx := context.Background()

	expiry := time.Now().Add(7 * 24 * time.Hour)
	if err := ts.Store(ctx, "user-expiry", "tok", "", "identify", &expiry); err != nil {
		t.Fatalf("Store: %v", err)
	}

	stored := fs.tokens["user-expiry"]
	if stored.ExpiresAt == nil {
		t.Fatal("ExpiresAt not stored — token expiry tracking broken")
	}
}
