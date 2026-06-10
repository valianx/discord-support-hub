// oauth_callback_test.go — hermetic tests for OAuthDiscordCallback (SEC-M3-002).
//
// Tests cover:
//   - State HMAC: valid state → accepted; tampered payload / tampered signature /
//     forged (wrong secret) / missing state → rejected before code exchange.
//   - Single-use: a replayed state (nonce already consumed) → rejected.
//   - Code exchange → token stored encrypted (assert ciphertext != plaintext).
//   - User link: users.discord_user_id set to discordUser.ID from /users/@me.
//   - Conflict: second callback linking the same Discord id to a different hub user
//     → 409 (ErrConflict), no overwrite.
//   - enqueuePendingInvites: after a successful link, pending space_member rows with
//     overwrite_applied=false result in enqueued KindInviteCollaborator tasks.
//   - End-to-end: connect (state issued) → link → pending invite enqueued.
package handlers_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/secrets"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── oauthFakeStore ────────────────────────────────────────────────────────────

// oauthFakeStore backs OAuth callback tests. It records:
//   - UpdateDiscordUserID calls (link assertions)
//   - UpsertOAuthToken calls (encryption assertions)
//   - ListCollaboratorChannels results (pending invite source)
//
// Remaining store.Store methods panic (not exercised by these tests).
type oauthFakeStore struct {
	// UpdateDiscordUserID control
	linkedUserID    string // hub user id that was linked
	linkedDiscordID string // Discord id that was linked
	linkConflict    bool   // when true, return ErrConflict

	// UpsertOAuthToken control
	storedToken *store.UpsertOAuthTokenParams // last stored params

	// ListCollaboratorChannels control
	pendingMembers []*domain.SpaceMember // returned to enqueuePendingInvites
}

func newOAuthFakeStore() *oauthFakeStore { return &oauthFakeStore{} }

func (f *oauthFakeStore) UpdateDiscordUserID(_ context.Context, userID, discordUserID string) error {
	if f.linkConflict {
		return store.ErrConflict
	}
	f.linkedUserID = userID
	f.linkedDiscordID = discordUserID
	return nil
}

func (f *oauthFakeStore) UpsertOAuthToken(_ context.Context, p store.UpsertOAuthTokenParams) (*domain.OAuthToken, error) {
	f.storedToken = &p
	return &domain.OAuthToken{
		ID:                   "tok-001",
		UserID:               p.UserID,
		AccessTokenCipher:    p.AccessTokenCipher,
		AccessTokenNonce:     p.AccessTokenNonce,
		EncryptionKeyVersion: p.EncryptionKeyVersion,
		Scopes:               p.Scopes,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}, nil
}

func (f *oauthFakeStore) ListCollaboratorChannels(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	return f.pendingMembers, nil
}

// store.Store full interface — remaining methods panic.
func (f *oauthFakeStore) Ping(_ context.Context) error { panic("Ping") }
func (f *oauthFakeStore) CreateMerchant(_ context.Context, _ store.CreateMerchantParams) (*domain.Merchant, error) {
	panic("CreateMerchant")
}
func (f *oauthFakeStore) GetMerchantByID(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByID")
}
func (f *oauthFakeStore) GetMerchantByExternalRef(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByExternalRef")
}
func (f *oauthFakeStore) ListMerchants(_ context.Context, _ store.ListMerchantsParams) ([]*domain.Merchant, error) {
	panic("ListMerchants")
}
func (f *oauthFakeStore) CreateUser(_ context.Context, _ store.CreateUserParams) (*domain.User, error) {
	panic("CreateUser")
}
func (f *oauthFakeStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByID")
}
func (f *oauthFakeStore) GetUserByDiscordID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByDiscordID")
}
func (f *oauthFakeStore) GetUserByEmail(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByEmail")
}
func (f *oauthFakeStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	panic("ListAgents")
}
func (f *oauthFakeStore) DeactivateUser(_ context.Context, _ string) (*domain.User, error) {
	panic("DeactivateUser")
}
func (f *oauthFakeStore) SetUserProvisionedAt(_ context.Context, _ string) (*domain.User, error) {
	panic("SetUserProvisionedAt")
}
func (f *oauthFakeStore) CreateAPIKey(_ context.Context, _ store.CreateAPIKeyParams) (*domain.APIKey, error) {
	panic("CreateAPIKey")
}
func (f *oauthFakeStore) ListAPIKeys(_ context.Context, _ bool) ([]*domain.APIKey, error) {
	panic("ListAPIKeys")
}
func (f *oauthFakeStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	panic("LookupActiveAPIKeyByHash")
}
func (f *oauthFakeStore) RevokeAPIKey(_ context.Context, _ string) error {
	panic("RevokeAPIKey")
}
func (f *oauthFakeStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	panic("TouchAPIKeyLastUsed")
}
func (f *oauthFakeStore) GetOAuthTokenByUserID(_ context.Context, _ string) (*domain.OAuthToken, error) {
	panic("GetOAuthTokenByUserID")
}
func (f *oauthFakeStore) CreateSpace(_ context.Context, _ store.CreateSpaceParams) (*domain.Space, error) {
	panic("CreateSpace")
}
func (f *oauthFakeStore) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByID")
}
func (f *oauthFakeStore) GetSpaceByMerchantID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByMerchantID")
}
func (f *oauthFakeStore) UpdateSpaceDiscordChannel(_ context.Context, _ store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	panic("UpdateSpaceDiscordChannel")
}
func (f *oauthFakeStore) UpdateSpaceACLState(_ context.Context, _ string, _ domain.ACLState) (*domain.Space, error) {
	panic("UpdateSpaceACLState")
}
func (f *oauthFakeStore) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	panic("CreateJob")
}
func (f *oauthFakeStore) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	panic("GetJobByID")
}
func (f *oauthFakeStore) UpdateJobStatus(_ context.Context, _ store.UpdateJobStatusParams) (*domain.Job, error) {
	panic("UpdateJobStatus")
}
func (f *oauthFakeStore) InsertIdempotencyKey(_ context.Context, _ store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	panic("InsertIdempotencyKey")
}
func (f *oauthFakeStore) GetIdempotencyKey(_ context.Context, _ string) (*domain.IdempotencyKey, error) {
	panic("GetIdempotencyKey")
}
func (f *oauthFakeStore) UpdateIdempotencyKeyResponse(_ context.Context, _ store.UpdateIdempotencyKeyResponseParams) error {
	panic("UpdateIdempotencyKeyResponse")
}
func (f *oauthFakeStore) CreateSpaceWithOutbox(_ context.Context, _ store.CreateSpaceParams, _ store.CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error) {
	panic("CreateSpaceWithOutbox")
}
func (f *oauthFakeStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	panic("ListPendingOutbox")
}
func (f *oauthFakeStore) StampOutboxEnqueued(_ context.Context, _ []string) error {
	panic("StampOutboxEnqueued")
}
func (f *oauthFakeStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	panic("UpdateOutboxPayload")
}
func (f *oauthFakeStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	return nil // best-effort audit writes always succeed in tests
}
func (f *oauthFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	panic("ListSpaces")
}
func (f *oauthFakeStore) CreateSpaceMember(_ context.Context, _ store.CreateSpaceMemberParams) (*domain.SpaceMember, error) {
	panic("CreateSpaceMember")
}
func (f *oauthFakeStore) GetSpaceMemberBySpaceAndUser(_ context.Context, _, _ string) (*domain.SpaceMember, error) {
	panic("GetSpaceMemberBySpaceAndUser")
}
func (f *oauthFakeStore) SetSpaceMemberOverwriteApplied(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("SetSpaceMemberOverwriteApplied")
}
func (f *oauthFakeStore) RevokeSpaceMember(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("RevokeSpaceMember")
}
func (f *oauthFakeStore) ListSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListSpaceMembers")
}
func (f *oauthFakeStore) ListDirectory(_ context.Context, _ store.ListDirectoryParams) ([]*store.DirectoryEntry, error) {
	panic("ListDirectory")
}
func (f *oauthFakeStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error {
	panic("UpdateSpaceReconciledAt")
}
func (f *oauthFakeStore) ListActiveSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListActiveSpaceMembers")
}
func (f *oauthFakeStore) UpdateDiscordUserIDConflict(_ context.Context, _, _ string) error {
	return store.ErrConflict
}

// M4 store methods — not exercised by OAuth callback tests.
func (f *oauthFakeStore) UpdateSpaceLifecycle(_ context.Context, _ store.UpdateSpaceLifecycleParams) (*domain.Space, error) {
	panic("UpdateSpaceLifecycle")
}
func (f *oauthFakeStore) UpdateSpaceWelcomeMessageID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceWelcomeMessageID")
}
func (f *oauthFakeStore) ListAuditEntries(_ context.Context, _ store.ListAuditEntriesParams) ([]*domain.AuditEntry, error) {
	panic("ListAuditEntries")
}
func (f *oauthFakeStore) GetJobBySpaceIDAndKind(_ context.Context, _, _ string) (*domain.Job, error) {
	return nil, store.ErrNotFound
}

// ListActiveProvisionedSpaces satisfies store.Store (added in M5 for the scheduled sweep).
func (f *oauthFakeStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	panic("ListActiveProvisionedSpaces")
}

// ─── fakeDiscordTransport ─────────────────────────────────────────────────────

// fakeDiscordTransport records requests and returns canned responses.
// It stubs both the token endpoint and /users/@me so no real network is needed.
type fakeDiscordTransport struct {
	tokenResp   string // JSON body for /oauth2/token
	tokenStatus int
	userResp    string // JSON body for /users/@me
	userStatus  int
}

func (f *fakeDiscordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var (
		body   string
		status int
	)
	switch {
	case strings.Contains(req.URL.Path, "/oauth2/token"):
		body = f.tokenResp
		status = f.tokenStatus
	case strings.Contains(req.URL.Path, "/users/@me"):
		body = f.userResp
		status = f.userStatus
	default:
		return nil, fmt.Errorf("fakeDiscordTransport: unexpected path %s", req.URL.Path)
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}, nil
}

// happyTransport returns a transport that serves a valid token + user identity.
func happyTransport(discordUserID string) *fakeDiscordTransport {
	tokenBody := fmt.Sprintf(`{"access_token":"tok-abc","token_type":"Bearer","expires_in":604800,"refresh_token":"ref-xyz","scope":"identify guilds.join"}`)
	userBody := fmt.Sprintf(`{"id":"%s","username":"testuser"}`, discordUserID)
	return &fakeDiscordTransport{
		tokenResp:   tokenBody,
		tokenStatus: http.StatusOK,
		userResp:    userBody,
		userStatus:  http.StatusOK,
	}
}

// ─── Factories ────────────────────────────────────────────────────────────────

const (
	oauthHMACSecret = "aabbccdd11223344aabbccdd11223344aabbccdd11223344aabbccdd11223344"
	testDiscordUser = "discord-user-001"
	testHubUser     = "hub-user-001"
)

// makeOAuthStateManager returns a StateManager backed by MemNonceStore.
func makeOAuthStateManager(t *testing.T) *oauth.StateManager {
	t.Helper()
	sm, err := oauth.NewStateManager(oauthHMACSecret, oauth.NewMemNonceStore())
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	return sm
}

// makeOAuthEncrypter returns a deterministic AES-256-GCM encrypter for test use.
func makeOAuthEncrypter(t *testing.T) *secrets.Encrypter {
	t.Helper()
	keyB64 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	enc, err := secrets.NewEncrypter(keyB64, 1)
	if err != nil {
		t.Fatalf("NewEncrypter: %v", err)
	}
	return enc
}

// buildOAuthRouter builds a minimal Gin engine for the OAuth callback endpoint.
// queueClient may be nil (disables enqueuePendingInvites path).
func buildOAuthRouter(
	s store.Store,
	sm *oauth.StateManager,
	enc *secrets.Encrypter,
	transport http.RoundTripper,
	queueClient *queue.Client,
) *gin.Engine {
	r := gin.New()
	var ts *oauth.TokenStore
	if enc != nil {
		ts = oauth.NewTokenStore(s, enc)
	}
	var httpClient *http.Client
	if transport != nil {
		httpClient = &http.Client{Transport: transport}
	}
	h := handlers.NewHandlers(handlers.Config{
		Store:                    s,
		StateManager:             sm,
		TokenStore:               ts,
		OAuthHTTPClient:          httpClient,
		QueueClient:              queueClient,
		DiscordOAuthClientID:     "test-client-id",
		DiscordOAuthClientSecret: "test-client-secret",
		DiscordOAuthRedirectURL:  "https://hub.example.com/v1/oauth/discord/callback",
	})
	r.GET("/oauth/discord/callback", h.OAuthDiscordCallback)
	return r
}

// issueState issues a valid state token for testHubUser and returns the raw string.
func issueState(t *testing.T, sm *oauth.StateManager) string {
	t.Helper()
	state, err := sm.Issue(context.Background(), testHubUser, "")
	if err != nil {
		t.Fatalf("Issue state: %v", err)
	}
	return state
}

// callbackURL builds the GET URL with state and code query params.
func callbackURL(state, code string) string {
	return fmt.Sprintf("/oauth/discord/callback?state=%s&code=%s", state, code)
}

// ─── State HMAC tests ─────────────────────────────────────────────────────────

// TestOAuthCallback_ValidState_Accepted verifies that a well-formed, HMAC-valid, unused
// state token passes validation and the callback proceeds to code exchange (AC-3).
func TestOAuthCallback_ValidState_Accepted(t *testing.T) {
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), nil)

	state := issueState(t, sm)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "valid-code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusBadRequest {
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if code, _ := body["code"].(string); code == "invalid_state" {
			t.Fatalf("valid state token was rejected: %s", w.Body)
		}
	}
	// Any non-400 invalid_state response is a pass for this test.
	if w.Code != http.StatusOK && w.Code != http.StatusFound {
		t.Errorf("unexpected status %d: %s", w.Code, w.Body)
	}
}

// TestOAuthCallback_MissingState_Rejected verifies that a missing state is rejected
// with 400 before the code exchange is attempted (AC-3 HMAC gate).
func TestOAuthCallback_MissingState_Rejected(t *testing.T) {
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	r := buildOAuthRouter(s, sm, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/oauth/discord/callback?code=some-code", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("missing state must return 400, got %d: %s", w.Code, w.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "invalid_state" {
		t.Errorf("want code=invalid_state, got: %v", body)
	}
}

// TestOAuthCallback_TamperedPayload_Rejected verifies that a state with a modified
// payload (but valid base64) fails HMAC verification and is rejected (AC-3, CWE-290).
func TestOAuthCallback_TamperedPayload_Rejected(t *testing.T) {
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	r := buildOAuthRouter(s, sm, nil, nil, nil)

	state := issueState(t, sm)
	// Replace the payload with a different base64 to break HMAC.
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected state format: %s", state)
	}
	fakePayload := base64.RawURLEncoding.EncodeToString([]byte(`{"n":"forged","u":"attacker","t":1}`))
	tamperedState := fakePayload + "." + parts[1] // original sig, forged payload

	req := httptest.NewRequest(http.MethodGet, callbackURL(tamperedState, "x"), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("tampered payload must be rejected (400), got %d: %s", w.Code, w.Body)
	}
}

// TestOAuthCallback_TamperedSignature_Rejected verifies that a state with a modified
// HMAC signature is rejected before code exchange (AC-3).
func TestOAuthCallback_TamperedSignature_Rejected(t *testing.T) {
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	r := buildOAuthRouter(s, sm, nil, nil, nil)

	state := issueState(t, sm)
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected state format: %s", state)
	}
	// Replace signature with all-zeros of the same length.
	sigBytes := make([]byte, 32)
	fakeSig := base64.RawURLEncoding.EncodeToString(sigBytes)
	tamperedState := parts[0] + "." + fakeSig

	req := httptest.NewRequest(http.MethodGet, callbackURL(tamperedState, "x"), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("tampered signature must be rejected (400), got %d: %s", w.Code, w.Body)
	}
}

// TestOAuthCallback_ForgedState_WrongSecret_Rejected verifies that a state signed
// with a different HMAC secret is rejected (AC-3, CWE-290).
func TestOAuthCallback_ForgedState_WrongSecret_Rejected(t *testing.T) {
	s := newOAuthFakeStore()
	smReal := makeOAuthStateManager(t)
	r := buildOAuthRouter(s, smReal, nil, nil, nil)

	// Create a second state manager with a different secret.
	differentSecret := "ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00ff00"
	smOther, err := oauth.NewStateManager(differentSecret, oauth.NewMemNonceStore())
	if err != nil {
		t.Fatalf("NewStateManager (other): %v", err)
	}
	forgedState, err := smOther.Issue(context.Background(), testHubUser, "")
	if err != nil {
		t.Fatalf("Issue (other): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, callbackURL(forgedState, "x"), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("state signed with wrong secret must be rejected (400), got %d: %s", w.Code, w.Body)
	}
}

// ─── Single-use nonce tests ───────────────────────────────────────────────────

// TestOAuthCallback_ReplayedState_Rejected verifies that a state whose nonce has
// already been consumed is rejected on second use (single-use, AC-3).
func TestOAuthCallback_ReplayedState_Rejected(t *testing.T) {
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), nil)

	state := issueState(t, sm)

	// First use — must succeed (nonce consumed).
	req1 := httptest.NewRequest(http.MethodGet, callbackURL(state, "code1"), nil)
	req1.Header.Set("Accept", "application/json")
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK && w1.Code != http.StatusFound {
		t.Fatalf("first use must succeed, got %d: %s", w1.Code, w1.Body)
	}

	// Second use — same state, nonce already consumed → rejected.
	req2 := httptest.NewRequest(http.MethodGet, callbackURL(state, "code2"), nil)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusBadRequest {
		t.Errorf("replayed state must be rejected (400), got %d: %s", w2.Code, w2.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(w2.Body.Bytes(), &body)
	if body["code"] != "invalid_state" {
		t.Errorf("want code=invalid_state on replay, got: %v", body)
	}
}

// ─── Token encryption tests ───────────────────────────────────────────────────

// TestOAuthCallback_TokenStoredEncrypted verifies that after a successful callback
// the stored token is ciphertext (not plaintext). The access_token plaintext must
// NOT appear in the persisted cipher bytes (AC-3, NFR-6).
func TestOAuthCallback_TokenStoredEncrypted(t *testing.T) {
	const accessToken = "tok-abc" // same value returned by happyTransport
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), nil)

	state := issueState(t, sm)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "any-code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	if s.storedToken == nil {
		t.Fatal("no token was stored after successful callback")
	}
	if strings.Contains(string(s.storedToken.AccessTokenCipher), accessToken) {
		t.Error("NFR-6 violated: plaintext access_token found in stored ciphertext")
	}
	if len(s.storedToken.AccessTokenCipher) == 0 {
		t.Error("AccessTokenCipher is empty — token was not encrypted")
	}
	if len(s.storedToken.AccessTokenNonce) == 0 {
		t.Error("AccessTokenNonce is empty — AES-GCM nonce was not stored")
	}
	if s.storedToken.EncryptionKeyVersion == 0 {
		t.Error("EncryptionKeyVersion is zero — key rotation is not possible")
	}
}

// ─── User-link tests ──────────────────────────────────────────────────────────

// TestOAuthCallback_UserLink_SetsDiscordUserID verifies that after a successful
// callback the hub user's discord_user_id is set to the Discord identity returned
// by /users/@me (SEC-M3-001 binding guarantee).
func TestOAuthCallback_UserLink_SetsDiscordUserID(t *testing.T) {
	s := newOAuthFakeStore()
	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), nil)

	state := issueState(t, sm)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	// The hub user named in the HMAC-signed state must have been linked.
	if s.linkedUserID != testHubUser {
		t.Errorf("want linkedUserID=%q, got %q", testHubUser, s.linkedUserID)
	}
	// The Discord id must come from /users/@me, not from a client-controlled field.
	if s.linkedDiscordID != testDiscordUser {
		t.Errorf("want linkedDiscordID=%q (from /users/@me), got %q", testDiscordUser, s.linkedDiscordID)
	}
}

// TestOAuthCallback_DiscordIDConflict_Returns409 verifies that when a second hub
// user tries to link a Discord id that is already bound to another hub user the
// callback returns 409 and does NOT overwrite the existing link (SEC-M3-001, CWE-290).
func TestOAuthCallback_DiscordIDConflict_Returns409(t *testing.T) {
	s := newOAuthFakeStore()
	s.linkConflict = true // simulate the UNIQUE constraint firing
	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), nil)

	state := issueState(t, sm)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("discord_user_id already linked must return 409, got %d: %s", w.Code, w.Body)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != "discord_id_conflict" {
		t.Errorf("want code=discord_id_conflict, got: %v", body)
	}
	// The existing link must NOT have been overwritten.
	if s.linkedDiscordID != "" {
		t.Error("conflict path must not modify linkedDiscordID")
	}
}

// ─── enqueuePendingInvites tests ──────────────────────────────────────────────

// TestOAuthCallback_EnqueuesPendingInvites verifies that after a successful link
// the handler enqueues KindInviteCollaborator tasks for pending space_members
// (overwrite_applied=false).
func TestOAuthCallback_EnqueuesPendingInvites(t *testing.T) {
	// Use miniredis to back the real queue.Client.
	mr := miniredis.RunT(t)

	qc := queue.NewClient(mr.Addr(), "", 0)
	t.Cleanup(func() { _ = qc.Close() })

	s := newOAuthFakeStore()
	// Two pending memberships (overwrite not yet applied).
	s.pendingMembers = []*domain.SpaceMember{
		{ID: "sm-1", SpaceID: "space-001", UserID: testHubUser, OverwriteApplied: false},
		{ID: "sm-2", SpaceID: "space-002", UserID: testHubUser, OverwriteApplied: false},
	}

	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), qc)

	state := issueState(t, sm)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	// Assert tasks were enqueued in miniredis.
	// Asynq stores pending tasks in sorted sets/lists keyed by queue name.
	// The presence of any "membership" key confirms the enqueue succeeded.
	keys := mr.Keys()
	membershipKeys := 0
	for _, k := range keys {
		if strings.Contains(k, "membership") {
			membershipKeys++
		}
	}
	if membershipKeys == 0 {
		t.Errorf("want membership queue entries in miniredis after pending invites enqueued; keys=%v", keys)
	}
}

// TestOAuthCallback_AlreadyAppliedMembers_NotEnqueued verifies that space_member
// rows with overwrite_applied=true are NOT re-enqueued (idempotency).
func TestOAuthCallback_AlreadyAppliedMembers_NotEnqueued(t *testing.T) {
	mr := miniredis.RunT(t)

	qc := queue.NewClient(mr.Addr(), "", 0)
	t.Cleanup(func() { _ = qc.Close() })

	s := newOAuthFakeStore()
	// Only applied members — nothing pending.
	s.pendingMembers = []*domain.SpaceMember{
		{ID: "sm-1", SpaceID: "space-001", UserID: testHubUser, OverwriteApplied: true},
	}

	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)
	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), qc)

	state := issueState(t, sm)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	// No membership keys should exist in miniredis (nothing to enqueue).
	for _, k := range mr.Keys() {
		if strings.Contains(k, "membership") {
			t.Errorf("already-applied members must not produce queue entries; found key: %s", k)
		}
	}
}

// ─── End-to-end: connect → link → pending invite enqueued ────────────────────

// TestOAuthCallback_EndToEnd_ConnectLinkEnqueue verifies the full happy path:
// state issued → callback validates + links discord id → stores encrypted token
// → enqueues pending invite for an un-projected space membership.
func TestOAuthCallback_EndToEnd_ConnectLinkEnqueue(t *testing.T) {
	mr := miniredis.RunT(t)

	qc := queue.NewClient(mr.Addr(), "", 0)
	t.Cleanup(func() { _ = qc.Close() })

	s := newOAuthFakeStore()
	s.pendingMembers = []*domain.SpaceMember{
		{ID: "sm-e2e", SpaceID: "space-e2e", UserID: testHubUser, OverwriteApplied: false},
	}

	sm := makeOAuthStateManager(t)
	enc := makeOAuthEncrypter(t)

	// Issue state (simulates GET /agents/:userId/connect_url).
	state := issueState(t, sm)

	r := buildOAuthRouter(s, sm, enc, happyTransport(testDiscordUser), qc)
	req := httptest.NewRequest(http.MethodGet, callbackURL(state, "auth-code"), nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// 1. Callback completes successfully.
	if w.Code != http.StatusOK {
		t.Fatalf("e2e: want 200, got %d: %s", w.Code, w.Body)
	}

	// 2. Discord user id was linked to the hub user named in the state.
	if s.linkedUserID != testHubUser {
		t.Errorf("e2e: want linkedUserID=%q, got %q", testHubUser, s.linkedUserID)
	}
	if s.linkedDiscordID != testDiscordUser {
		t.Errorf("e2e: want linkedDiscordID=%q, got %q", testDiscordUser, s.linkedDiscordID)
	}

	// 3. Token was stored (encrypted).
	if s.storedToken == nil {
		t.Fatal("e2e: token was not stored after callback")
	}
	if len(s.storedToken.AccessTokenCipher) == 0 {
		t.Error("e2e: token stored without ciphertext")
	}

	// 4. Pending invite was enqueued.
	membershipSeen := false
	for _, k := range mr.Keys() {
		if strings.Contains(k, "membership") {
			membershipSeen = true
			break
		}
	}
	if !membershipSeen {
		t.Error("e2e: pending invite was not enqueued after successful link")
	}
}
