// Package middleware_test — control-plane authority tests for the roster API.
//
// These tests verify the fixed behavior introduced in Iteration 1:
// a backoffice-scoped service key (api_keys.scope == "backoffice") confers
// control-plane authority and reaches the roster endpoints (§5.2).
//
// AC-coverage: AC-1 (Layer A), AC-2 (POST /agents control-plane gate),
// AC-8 (GET /agents control-plane gate), NFR-13 (authority from Postgres, not Discord).
package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// ─── Factories ───────────────────────────────────────────────────────────────

// makeBackofficeKey returns an active api_keys row with scope="backoffice".
// This is the canonical form of any key issued by cmd/keygen.
// The scope is a server-side DB value set at creation, never client-supplied.
func makeBackofficeKey() *domain.APIKey {
	return &domain.APIKey{
		ID:        "svc-key-001",
		Name:      "backoffice-service-key",
		Scope:     authz.ScopeBackoffice,
		CreatedAt: time.Now(),
		// RevokedAt is nil → key is active.
	}
}

// makeNarrowScopeKey returns an active api_keys row with a narrower scope
// (e.g. "readonly") that does NOT confer control-plane authority.
func makeNarrowScopeKey(scope string) *domain.APIKey {
	return &domain.APIKey{
		ID:        "narrow-svc-key-001",
		Name:      "narrow-scope-key",
		Scope:     scope,
		CreatedAt: time.Now(),
	}
}

// ─── Roster store ─────────────────────────────────────────────────────────────

// rosterFakeStore is the minimal store.Store implementation for roster tests
// that exercise the full Layer A → Layer B path.
type rosterFakeStore struct {
	noopStore
	lookupKey *domain.APIKey
	lookupErr error
	agents    []*domain.User
}

func newRosterFakeStore(key *domain.APIKey) *rosterFakeStore {
	return &rosterFakeStore{lookupKey: key}
}

func (s *rosterFakeStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}
	if s.lookupKey == nil {
		return nil, store.ErrNotFound
	}
	// Accept any non-empty hash — the raw key used in tests is testRawKey.
	return s.lookupKey, nil
}

func (s *rosterFakeStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error { return nil }

func (s *rosterFakeStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	return s.agents, nil
}

func (s *rosterFakeStore) CreateUser(_ context.Context, p store.CreateUserParams) (*domain.User, error) {
	u := &domain.User{
		ID:      "new-agent-id",
		Type:    p.Type,
		IsAdmin: p.IsAdmin,
	}
	return u, nil
}

func (s *rosterFakeStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	return nil, store.ErrNotFound
}

func (s *rosterFakeStore) DeactivateUser(_ context.Context, _ string) (*domain.User, error) {
	return nil, store.ErrNotFound
}

// buildFullAuthRouter creates a Gin engine that runs real Layer A (Auth middleware)
// followed by the given downstream handler. Used to verify the end-to-end path
// from a bearer token through Layer A into a handler that checks Layer B.
func buildFullAuthRouter(s store.Store, downstream gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.Recovery())
	r.GET("/probe", middleware.Auth(s), downstream)
	r.POST("/agents", middleware.Auth(s), downstream)
	r.GET("/agents", middleware.Auth(s), downstream)
	return r
}

// controlPlaneHandler mimics the Layer B guard used in the real agent handlers:
// requireControlPlane passes for backoffice-scoped keys or is_admin users.
func controlPlaneHandler(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		c.JSON(http.StatusForbidden, gin.H{
			"code":    "forbidden",
			"message": "control-plane authority required",
		})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

// ─── Fixed behavior: backoffice key reaches the roster API ───────────────────

// TestRosterAPI_BackofficeKey_PostAgents_Returns201 verifies that a service key with
// scope="backoffice" passes both Layer A and the control-plane Layer B check,
// reaching POST /agents and receiving 201 (the fixed behavior, §5.2).
//
// Authority is resolved from api_keys.scope in Postgres (server-side, set at creation),
// never from a client-supplied header or body value (CWE-639 guard).
func TestRosterAPI_BackofficeKey_PostAgents_Returns201(t *testing.T) {
	s := newRosterFakeStore(makeBackofficeKey())
	r := buildFullAuthRouter(s, controlPlaneHandler)

	body := `{"email":"agent@example.com","is_admin":false}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("backoffice-scoped key must reach POST /agents (want 201), got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestRosterAPI_BackofficeKey_GetAgents_Returns201 verifies that a backoffice-scoped
// key reaches GET /agents and receives a success response (the fixed behavior, §5.2).
func TestRosterAPI_BackofficeKey_GetAgents_Returns201(t *testing.T) {
	s := newRosterFakeStore(makeBackofficeKey())
	r := buildFullAuthRouter(s, controlPlaneHandler)

	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("backoffice-scoped key must reach GET /agents (want 201 from stub), got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestRosterAPI_Unauthenticated_Returns401 verifies that an unauthenticated request
// (no Authorization header) to a roster endpoint returns 401 before Layer B runs.
func TestRosterAPI_Unauthenticated_Returns401(t *testing.T) {
	s := newRosterFakeStore(makeBackofficeKey())
	r := buildFullAuthRouter(s, controlPlaneHandler)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(`{}`))
	// No Authorization header.

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request must return 401, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRosterAPI_NarrowScopedKey_Returns403 verifies that a service key with a narrower
// scope (not "backoffice") is authenticated by Layer A but denied by Layer B with 403.
// This confirms that only the explicit server-side "backoffice" scope grants control-plane
// authority — an arbitrary scope string is not enough (NFR-13).
func TestRosterAPI_NarrowScopedKey_Returns403(t *testing.T) {
	s := newRosterFakeStore(makeNarrowScopeKey("readonly"))
	r := buildFullAuthRouter(s, controlPlaneHandler)

	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer "+testRawKey)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Layer A succeeds (valid key), Layer B denies (scope != "backoffice").
	if w.Code != http.StatusForbidden {
		t.Errorf("narrow-scoped key must be denied (want 403), got %d: %s", w.Code, w.Body.String())
	}
	if w.Code == http.StatusUnauthorized {
		t.Error("a valid key must not produce 401 — the 403 must come from Layer B, not Layer A")
	}
}

// ─── Layer A fields are populated correctly ───────────────────────────────────

// TestLayerA_BackofficeKey_PrincipalFieldsCorrect verifies that Layer A correctly
// populates all Principal fields from the api_keys row for a backoffice-scoped key.
func TestLayerA_BackofficeKey_PrincipalFieldsCorrect(t *testing.T) {
	keyID := "svc-key-fields-test"
	key := &domain.APIKey{
		ID:        keyID,
		Name:      "fields-test-key",
		Scope:     authz.ScopeBackoffice,
		CreatedAt: time.Now(),
	}
	customStore := &exactHashStore{rawKey: testRawKey, key: key}

	var captured *authz.Principal
	r := gin.New()
	r.GET("/probe", middleware.Auth(customStore), func(c *gin.Context) {
		captured = middleware.GetPrincipal(c)
		c.JSON(http.StatusOK, gin.H{})
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Layer A failed: %d %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("Principal not injected")
	}
	if captured.KeyID != keyID {
		t.Errorf("KeyID: want %q, got %q", keyID, captured.KeyID)
	}
	if captured.KeyScope != authz.ScopeBackoffice {
		t.Errorf("KeyScope: want %q, got %q", authz.ScopeBackoffice, captured.KeyScope)
	}
	if captured.Type != authz.PrincipalTypeService {
		t.Errorf("Type: want PrincipalTypeService, got %q", captured.Type)
	}
}

// ─── NFR-13: authority from Postgres, not from arbitrary scope strings ────────

// TestAuth_NonBackofficeScope_DoesNotGrantControlPlane verifies that an arbitrary
// scope string (e.g. "admin", "superuser") does NOT confer control-plane authority.
// Only the explicit server-side ScopeBackoffice value ("backoffice") does.
// This guards against scope-as-claim privilege escalation (NFR-13, CWE-639).
func TestAuth_NonBackofficeScope_DoesNotGrantControlPlane(t *testing.T) {
	arbitraryScopes := []string{"admin", "superuser", "BACKOFFICE", "Backoffice", ""}
	for _, scope := range arbitraryScopes {
		scope := scope // capture
		t.Run("scope="+scope, func(t *testing.T) {
			key := &domain.APIKey{
				ID:        "scope-test-key",
				Name:      "scope-test",
				Scope:     scope,
				CreatedAt: time.Now(),
			}
			customStore := &exactHashStore{rawKey: testRawKey, key: key}

			var captured *authz.Principal
			r := gin.New()
			r.GET("/probe", middleware.Auth(customStore), func(c *gin.Context) {
				captured = middleware.GetPrincipal(c)
				c.JSON(http.StatusOK, gin.H{})
			})

			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Authorization", "Bearer "+testRawKey)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("Layer A should pass for a valid key (scope=%q), got %d", scope, w.Code)
			}
			if captured == nil {
				t.Fatal("Principal not injected")
			}
			// Only ScopeBackoffice grants control-plane authority.
			if authz.RequireControlPlane(captured) {
				t.Errorf("SECURITY: scope=%q must not grant control-plane authority — only %q does (NFR-13)",
					scope, authz.ScopeBackoffice)
			}
		})
	}
}

// ─── End-to-end: POST /agents with real Layer A middleware ────────────────────

// TestRosterAPI_EndToEnd_PostAgents_BackofficeKey_Returns201 is the end-to-end test
// that proves POST /agents succeeds (201 + connect_url body stub) when a real
// backoffice-scoped key passes through the real Layer A middleware and the
// RequireControlPlane Layer B check.
//
// This test uses the exactHashStore so Layer A performs a real hash comparison,
// ensuring the full auth path (bearer extraction → SHA-256 hash → store lookup →
// Principal injection → control-plane check) is exercised.
func TestRosterAPI_EndToEnd_PostAgents_BackofficeKey_Returns201(t *testing.T) {
	key := &domain.APIKey{
		ID:        "e2e-backoffice-key",
		Name:      "e2e-test-key",
		Scope:     authz.ScopeBackoffice,
		CreatedAt: time.Now(),
	}
	// exactHashStore enforces that only testRawKey is accepted — real hash comparison.
	s := &exactHashStore{rawKey: testRawKey, key: key}

	// The downstream simulates the agent handler's control-plane gate and 201 response.
	agentCreateHandler := func(c *gin.Context) {
		p := middleware.GetPrincipal(c)
		if !authz.RequireControlPlane(p) {
			c.JSON(http.StatusForbidden, gin.H{"code": "forbidden"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"id":          "new-agent-id",
			"connect_url": "https://discord.com/api/oauth2/authorize?...",
		})
	}

	r := gin.New()
	r.Use(middleware.Recovery())
	r.POST("/agents", middleware.Auth(s), agentCreateHandler)

	body := `{"email":"agent@example.com","is_admin":false}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+testRawKey)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("end-to-end POST /agents with backoffice key must return 201, got %d: %s",
			w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["connect_url"] == nil {
		t.Error("response must include connect_url")
	}
}

// ─── Helper: exactHashStore ───────────────────────────────────────────────────

// exactHashStore accepts ONLY the specified rawKey (by computing its hash).
// This lets tests control which specific raw key will be considered valid,
// exercising the real SHA-256 comparison in Layer A.
type exactHashStore struct {
	noopStore
	rawKey string
	key    *domain.APIKey
}

func (s *exactHashStore) LookupActiveAPIKeyByHash(_ context.Context, hash []byte) (*domain.APIKey, error) {
	expected := authz.HashAPIKey(s.rawKey)
	if string(hash) != string(expected) {
		return nil, store.ErrNotFound
	}
	return s.key, nil
}

func (s *exactHashStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error { return nil }

// ─── Keygen contract: raw key is never persisted ─────────────────────────────

// TestKeygenContract_RawKeyNeverPersisted verifies the keygen contract (AC-7):
// the hash stored in api_keys.key_hash is distinct from the raw key.
func TestKeygenContract_RawKeyNeverPersisted(t *testing.T) {
	raw, err := authz.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}

	hash := authz.HashAPIKey(raw)

	if string(hash) == raw {
		t.Error("hash equals raw key — raw key would be persisted as-is")
	}
	if len(hash) == len(raw) {
		t.Error("hash and raw key have the same length — likely not hashing")
	}

	// Deterministic: hashing the same raw key twice produces the same hash.
	hash2 := authz.HashAPIKey(raw)
	if string(hash) != string(hash2) {
		t.Error("HashAPIKey is not deterministic — store lookups would always miss")
	}
}
