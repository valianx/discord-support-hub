// Package middleware_test verifies Layer A (API key authentication) hermetically.
//
// The tests use fake store implementations so no real database or network is needed.
// They exercise:
//   - valid active key → Principal injected, handler runs (AC-1)
//   - missing Authorization header → 401, handler never runs (AC-1)
//   - malformed header → 401 (AC-1)
//   - invalid/unknown key hash → 401 (AC-1)
//   - revoked key → 401 (AC-1)
//   - key hashing: only the hash reaches the store, raw key never persisted (AC-7)
package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// noopStore is a full store.Store implementation where every method panics.
// Test-specific fakes embed it to get the full interface with only the methods
// they care about overridden.
type noopStore struct{}

func (n *noopStore) Ping(_ context.Context) error { panic("Ping not stubbed") }
func (n *noopStore) CreateMerchant(_ context.Context, _ store.CreateMerchantParams) (*domain.Merchant, error) {
	panic("CreateMerchant not stubbed")
}
func (n *noopStore) GetMerchantByID(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByID not stubbed")
}
func (n *noopStore) GetMerchantByExternalRef(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByExternalRef not stubbed")
}
func (n *noopStore) ListMerchants(_ context.Context, _ store.ListMerchantsParams) ([]*domain.Merchant, error) {
	panic("ListMerchants not stubbed")
}
func (n *noopStore) CreateUser(_ context.Context, _ store.CreateUserParams) (*domain.User, error) {
	panic("CreateUser not stubbed")
}
func (n *noopStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByID not stubbed")
}
func (n *noopStore) GetUserByDiscordID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByDiscordID not stubbed")
}
func (n *noopStore) GetUserByEmail(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByEmail not stubbed")
}
func (n *noopStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	panic("ListAgents not stubbed")
}
func (n *noopStore) DeactivateUser(_ context.Context, _ string) (*domain.User, error) {
	panic("DeactivateUser not stubbed")
}
func (n *noopStore) SetUserProvisionedAt(_ context.Context, _ string) (*domain.User, error) {
	panic("SetUserProvisionedAt not stubbed")
}
func (n *noopStore) CreateAPIKey(_ context.Context, _ store.CreateAPIKeyParams) (*domain.APIKey, error) {
	panic("CreateAPIKey not stubbed")
}
func (n *noopStore) ListAPIKeys(_ context.Context, _ bool) ([]*domain.APIKey, error) {
	panic("ListAPIKeys not stubbed")
}
func (n *noopStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	panic("LookupActiveAPIKeyByHash not stubbed")
}
func (n *noopStore) RevokeAPIKey(_ context.Context, _ string) error {
	panic("RevokeAPIKey not stubbed")
}
func (n *noopStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	panic("TouchAPIKeyLastUsed not stubbed")
}
func (n *noopStore) CreateSpace(_ context.Context, _ store.CreateSpaceParams) (*domain.Space, error) {
	panic("CreateSpace not stubbed")
}
func (n *noopStore) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByID not stubbed")
}
func (n *noopStore) GetSpaceByMerchantID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByMerchantID not stubbed")
}
func (n *noopStore) UpdateSpaceDiscordChannel(_ context.Context, _ store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	panic("UpdateSpaceDiscordChannel not stubbed")
}
func (n *noopStore) UpdateSpaceACLState(_ context.Context, _ string, _ domain.ACLState) (*domain.Space, error) {
	panic("UpdateSpaceACLState not stubbed")
}
func (n *noopStore) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	panic("CreateJob not stubbed")
}
func (n *noopStore) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	panic("GetJobByID not stubbed")
}
func (n *noopStore) UpdateJobStatus(_ context.Context, _ store.UpdateJobStatusParams) (*domain.Job, error) {
	panic("UpdateJobStatus not stubbed")
}
func (n *noopStore) InsertIdempotencyKey(_ context.Context, _ store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	panic("InsertIdempotencyKey not stubbed")
}
func (n *noopStore) GetIdempotencyKey(_ context.Context, _ string) (*domain.IdempotencyKey, error) {
	panic("GetIdempotencyKey not stubbed")
}
func (n *noopStore) UpdateIdempotencyKeyResponse(_ context.Context, _ store.UpdateIdempotencyKeyResponseParams) error {
	panic("UpdateIdempotencyKeyResponse not stubbed")
}
func (n *noopStore) CreateSpaceWithOutbox(_ context.Context, _ store.CreateSpaceParams, _ store.CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error) {
	panic("CreateSpaceWithOutbox not stubbed")
}
func (n *noopStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	panic("ListPendingOutbox not stubbed")
}
func (n *noopStore) StampOutboxEnqueued(_ context.Context, _ []string) error {
	panic("StampOutboxEnqueued not stubbed")
}
func (n *noopStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	panic("UpdateOutboxPayload not stubbed")
}
func (n *noopStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	panic("InsertAuditEntry not stubbed")
}
func (n *noopStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	panic("ListSpaces not stubbed")
}

// M3 store methods — not exercised by auth/middleware tests; all panic.
func (n *noopStore) CreateSpaceMember(_ context.Context, _ store.CreateSpaceMemberParams) (*domain.SpaceMember, error) {
	panic("CreateSpaceMember not stubbed")
}
func (n *noopStore) GetSpaceMemberBySpaceAndUser(_ context.Context, _, _ string) (*domain.SpaceMember, error) {
	panic("GetSpaceMemberBySpaceAndUser not stubbed")
}
func (n *noopStore) StampSpaceMemberInviteSent(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("StampSpaceMemberInviteSent not stubbed")
}
func (n *noopStore) RevokeSpaceMember(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("RevokeSpaceMember not stubbed")
}
func (n *noopStore) ListSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListSpaceMembers not stubbed")
}
func (n *noopStore) ListCollaboratorChannels(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListCollaboratorChannels not stubbed")
}
func (n *noopStore) ListDirectory(_ context.Context, _ store.ListDirectoryParams) ([]*store.DirectoryEntry, error) {
	panic("ListDirectory not stubbed")
}
func (n *noopStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error {
	panic("UpdateSpaceReconciledAt not stubbed")
}
func (n *noopStore) ListActiveSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListActiveSpaceMembers not stubbed")
}
func (n *noopStore) UpdateDiscordUserID(_ context.Context, _, _ string) error {
	panic("UpdateDiscordUserID not stubbed")
}

// M4 store methods — not exercised by auth/middleware tests; all panic.
func (n *noopStore) UpdateSpaceLifecycle(_ context.Context, _ store.UpdateSpaceLifecycleParams) (*domain.Space, error) {
	panic("UpdateSpaceLifecycle not stubbed")
}
func (n *noopStore) UpdateSpaceWelcomeMessageID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceWelcomeMessageID not stubbed")
}
func (n *noopStore) ListAuditEntries(_ context.Context, _ store.ListAuditEntriesParams) ([]*domain.AuditEntry, error) {
	panic("ListAuditEntries not stubbed")
}
func (n *noopStore) GetJobBySpaceIDAndKind(_ context.Context, _, _ string) (*domain.Job, error) {
	panic("GetJobBySpaceIDAndKind not stubbed")
}

// ListActiveProvisionedSpaces satisfies store.Store (added in M5 for the scheduled sweep).
func (n *noopStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	panic("ListActiveProvisionedSpaces not stubbed")
}

// M6 store methods — not exercised by middleware tests.
func (n *noopStore) SetMerchantInviteLink(_ context.Context, _ string, _ string) (*domain.Merchant, error) {
	panic("SetMerchantInviteLink not stubbed")
}
func (n *noopStore) UpdateSpaceMerchantRoleID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceMerchantRoleID not stubbed")
}

// authFakeStore overrides only the two methods Layer A needs.
type authFakeStore struct {
	noopStore
	key     *domain.APIKey // returned when hash matches the canonical test key
	hashErr error          // if set, returned on any lookup
	touched []string       // records ids passed to TouchAPIKeyLastUsed
}

const testRawKey = "valid-raw-key-abc123"

func (f *authFakeStore) LookupActiveAPIKeyByHash(_ context.Context, hash []byte) (*domain.APIKey, error) {
	if f.hashErr != nil {
		return nil, f.hashErr
	}
	if f.key == nil {
		return nil, store.ErrNotFound
	}
	expected := authz.HashAPIKey(testRawKey)
	if string(hash) != string(expected) {
		return nil, store.ErrNotFound
	}
	return f.key, nil
}

func (f *authFakeStore) TouchAPIKeyLastUsed(_ context.Context, id string) error {
	f.touched = append(f.touched, id)
	return nil
}

// goodKey is a convenience factory for a valid active key.
func goodKey() *domain.APIKey {
	return &domain.APIKey{
		ID:        "key-id-001",
		Name:      "test-key",
		Scope:     "backoffice",
		CreatedAt: time.Now(),
	}
}

// testEngine builds a Gin engine with the Auth middleware and a downstream sentinel handler.
func testEngine(s store.Store) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recovery())
	r.GET("/test", middleware.Auth(s), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// ─── AC-1: valid key → Principal injected, handler runs ──────────────────────

func TestAuth_ValidKey_PrincipalInjectedAndHandlerRuns(t *testing.T) {
	s := &authFakeStore{key: goodKey()}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestAuth_ValidKey_PrincipalHasCorrectFields verifies the injected Principal fields.
func TestAuth_ValidKey_PrincipalHasCorrectFields(t *testing.T) {
	s := &authFakeStore{key: goodKey()}

	var captured *authz.Principal
	r := gin.New()
	r.GET("/test", middleware.Auth(s), func(c *gin.Context) {
		captured = middleware.GetPrincipal(c)
		c.JSON(http.StatusOK, gin.H{})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if captured == nil {
		t.Fatal("expected Principal to be injected but got nil")
	}
	if captured.KeyID != "key-id-001" {
		t.Errorf("want KeyID 'key-id-001', got %q", captured.KeyID)
	}
	if captured.KeyScope != "backoffice" {
		t.Errorf("want KeyScope 'backoffice', got %q", captured.KeyScope)
	}
	if captured.Type != authz.PrincipalTypeService {
		t.Errorf("want PrincipalTypeService, got %q", captured.Type)
	}
}

// ─── AC-1: missing / malformed header → 401 before handler ───────────────────

func TestAuth_MissingAuthorizationHeader_Returns401(t *testing.T) {
	s := &authFakeStore{key: goodKey()}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestAuth_MalformedHeader_NoBearer_Returns401(t *testing.T) {
	s := &authFakeStore{key: goodKey()}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d; body: %s", w.Code, w.Body.String())
	}
}

func TestAuth_EmptyBearerToken_Returns401(t *testing.T) {
	s := &authFakeStore{key: goodKey()}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d; body: %s", w.Code, w.Body.String())
	}
}

// ─── AC-1: invalid/unknown key → 401 ─────────────────────────────────────────

func TestAuth_InvalidKey_Returns401(t *testing.T) {
	s := &authFakeStore{key: goodKey()}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-key-that-does-not-match")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d; body: %s", w.Code, w.Body.String())
	}
}

// ─── AC-1: revoked key → 401 ─────────────────────────────────────────────────

func TestAuth_RevokedKey_Returns401(t *testing.T) {
	// Simulate a revoked key: active lookup returns ErrNotFound.
	s := &authFakeStore{key: nil}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d; body: %s", w.Code, w.Body.String())
	}
}

// ─── Store error → 500 (fail-closed) ─────────────────────────────────────────

func TestAuth_StoreError_Returns500(t *testing.T) {
	s := &authFakeStore{hashErr: errors.New("db connection refused")}
	r := testEngine(s)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500 on store error, got %d; body: %s", w.Code, w.Body.String())
	}
}

// ─── Handler does NOT run on auth failure ────────────────────────────────────

func TestAuth_HandlerNotCalledOnFailure(t *testing.T) {
	s := &authFakeStore{key: nil}
	handlerCalled := false
	r := gin.New()
	r.GET("/test", middleware.Auth(s), func(c *gin.Context) {
		handlerCalled = true
		c.JSON(http.StatusOK, gin.H{})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if handlerCalled {
		t.Error("handler must not be called when auth fails")
	}
}

// ─── AC-7: raw key never reaches the store ───────────────────────────────────

func TestAuth_OnlyHashReachesStore(t *testing.T) {
	rawKey := testRawKey
	expectedHash := authz.HashAPIKey(rawKey)
	var receivedHash []byte

	spy := &hashSpyStore{
		onLookup: func(hash []byte) (*domain.APIKey, error) {
			receivedHash = hash
			return nil, store.ErrNotFound // 401; we only care about what was sent
		},
	}

	r := gin.New()
	r.GET("/test", middleware.Auth(spy), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if string(receivedHash) != string(expectedHash) {
		t.Error("store must receive the SHA-256 hash, not the raw key")
	}
	if string(receivedHash) == rawKey {
		t.Error("raw key must never reach the store")
	}
}

// hashSpyStore captures the hash passed to LookupActiveAPIKeyByHash.
type hashSpyStore struct {
	noopStore
	onLookup func(hash []byte) (*domain.APIKey, error)
}

func (s *hashSpyStore) LookupActiveAPIKeyByHash(_ context.Context, hash []byte) (*domain.APIKey, error) {
	return s.onLookup(hash)
}

func (s *hashSpyStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	return nil
}

// ─── GetPrincipal nil safety ──────────────────────────────────────────────────

func TestGetPrincipal_NilWhenNotSet(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	p := middleware.GetPrincipal(c)
	if p != nil {
		t.Error("GetPrincipal must return nil when no principal has been set")
	}
}
