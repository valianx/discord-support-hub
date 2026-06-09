// Package handlers_test verifies the M1 agent handler logic hermetically.
//
// Tests cover:
//   - AC-2: POST /agents from non-Admin → 403; from Admin → 201 + connect_url
//   - AC-3: authZ is a pure function of Postgres state (collaborator w/ Agent role ignored)
//   - AC-8: GET /agents from Admin → 200 + items; from non-Admin → 403
//   - DELETE /agents/{id} from Admin → 202; from non-Admin → 403
package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ─── Fake store ───────────────────────────────────────────────────────────────

// agentFakeStore implements store.Store for agent handler tests.
// Only the methods called by agent handlers are overridden; others panic.
type agentFakeStore struct {
	users         map[string]*domain.User // keyed by id
	createErr     error
	deactivateErr error
}

func newAgentFakeStore() *agentFakeStore {
	return &agentFakeStore{users: make(map[string]*domain.User)}
}

func (f *agentFakeStore) CreateUser(_ context.Context, p store.CreateUserParams) (*domain.User, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	u := &domain.User{
		ID:            "new-user-id",
		Type:          p.Type,
		IsAdmin:       p.IsAdmin,
		Email:         p.Email,
		DisplayName:   p.DisplayName,
		DiscordUserID: p.DiscordUserID,
		IsActive:      true,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	f.users[u.ID] = u
	return u, nil
}

func (f *agentFakeStore) GetUserByID(_ context.Context, id string) (*domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (f *agentFakeStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	var agents []*domain.User
	for _, u := range f.users {
		if u.Type == domain.UserTypeAgent {
			agents = append(agents, u)
		}
	}
	return agents, nil
}

func (f *agentFakeStore) DeactivateUser(_ context.Context, id string) (*domain.User, error) {
	if f.deactivateErr != nil {
		return nil, f.deactivateErr
	}
	u, ok := f.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	u.IsActive = false
	return u, nil
}

// Full store.Store interface — remaining methods panic (not exercised by agent tests).
func (f *agentFakeStore) Ping(_ context.Context) error { panic("Ping") }
func (f *agentFakeStore) CreateMerchant(_ context.Context, _ store.CreateMerchantParams) (*domain.Merchant, error) {
	panic("CreateMerchant")
}
func (f *agentFakeStore) GetMerchantByID(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByID")
}
func (f *agentFakeStore) GetUserByDiscordID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByDiscordID")
}
func (f *agentFakeStore) SetUserProvisionedAt(_ context.Context, _ string) (*domain.User, error) {
	panic("SetUserProvisionedAt")
}
func (f *agentFakeStore) CreateAPIKey(_ context.Context, _ store.CreateAPIKeyParams) (*domain.APIKey, error) {
	panic("CreateAPIKey")
}
func (f *agentFakeStore) ListAPIKeys(_ context.Context, _ bool) ([]*domain.APIKey, error) {
	panic("ListAPIKeys")
}
func (f *agentFakeStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	panic("LookupActiveAPIKeyByHash")
}
func (f *agentFakeStore) RevokeAPIKey(_ context.Context, _ string) error { panic("RevokeAPIKey") }
func (f *agentFakeStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	panic("TouchAPIKeyLastUsed")
}
func (f *agentFakeStore) UpsertOAuthToken(_ context.Context, _ store.UpsertOAuthTokenParams) (*domain.OAuthToken, error) {
	panic("UpsertOAuthToken")
}
func (f *agentFakeStore) GetOAuthTokenByUserID(_ context.Context, _ string) (*domain.OAuthToken, error) {
	panic("GetOAuthTokenByUserID")
}
func (f *agentFakeStore) CreateSpace(_ context.Context, _ store.CreateSpaceParams) (*domain.Space, error) {
	panic("CreateSpace")
}
func (f *agentFakeStore) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByID")
}
func (f *agentFakeStore) GetSpaceByMerchantID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByMerchantID")
}
func (f *agentFakeStore) UpdateSpaceDiscordChannel(_ context.Context, _ store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	panic("UpdateSpaceDiscordChannel")
}
func (f *agentFakeStore) UpdateSpaceACLState(_ context.Context, _ string, _ domain.ACLState) (*domain.Space, error) {
	panic("UpdateSpaceACLState")
}
func (f *agentFakeStore) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	panic("CreateJob")
}
func (f *agentFakeStore) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	panic("GetJobByID")
}
func (f *agentFakeStore) UpdateJobStatus(_ context.Context, _ store.UpdateJobStatusParams) (*domain.Job, error) {
	panic("UpdateJobStatus")
}
func (f *agentFakeStore) InsertIdempotencyKey(_ context.Context, _ store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	panic("InsertIdempotencyKey")
}
func (f *agentFakeStore) GetIdempotencyKey(_ context.Context, _ string) (*domain.IdempotencyKey, error) {
	panic("GetIdempotencyKey")
}
func (f *agentFakeStore) UpdateIdempotencyKeyResponse(_ context.Context, _ store.UpdateIdempotencyKeyResponseParams) error {
	panic("UpdateIdempotencyKeyResponse")
}
func (f *agentFakeStore) CreateSpaceWithOutbox(_ context.Context, _ store.CreateSpaceParams, _ store.CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error) {
	panic("CreateSpaceWithOutbox")
}
func (f *agentFakeStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	panic("ListPendingOutbox")
}
func (f *agentFakeStore) StampOutboxEnqueued(_ context.Context, _ []string) error {
	panic("StampOutboxEnqueued")
}

// ─── Router helpers ───────────────────────────────────────────────────────────

// buildAgentRouter builds a minimal Gin engine for agent endpoint tests.
// The principal is injected directly (bypassing Layer A) so tests focus on Layer B.
func buildAgentRouter(s store.Store, principal *authz.Principal) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recovery())

	// Inject the principal directly (Layer A is not under test here).
	r.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("principal", principal)
		}
		c.Next()
	})

	h := handlers.NewHandlers(handlers.Config{
		Store:                   s,
		DiscordOAuthClientID:    "test-client-id",
		DiscordOAuthRedirectURL: "https://hub.example.com/v1/oauth/discord/callback",
	})

	r.GET("/agents", h.ListAgents)
	r.POST("/agents", h.AddAgent)
	r.DELETE("/agents/:userId", h.RemoveAgent)
	return r
}

func adminPrincipal() *authz.Principal {
	return &authz.Principal{Type: authz.PrincipalTypeService, KeyID: "k1", IsAdmin: true}
}

// backofficePrincipal returns a principal representing a backoffice-scoped service key.
// This is the canonical caller in production: the key's scope in api_keys.scope is
// "backoffice" (server-side DB value), granting control-plane authority without
// requiring is_admin=true (§5.2).
func backofficePrincipal() *authz.Principal {
	return &authz.Principal{
		Type:     authz.PrincipalTypeService,
		KeyID:    "k3",
		KeyScope: authz.ScopeBackoffice,
		IsAdmin:  false, // scope alone is sufficient for control-plane access
	}
}

func nonAdminPrincipal() *authz.Principal {
	return &authz.Principal{Type: authz.PrincipalTypeService, KeyID: "k4", IsAdmin: false}
}

// ─── AC-8: GET /agents ────────────────────────────────────────────────────────

// TestListAgents_BackofficeKey_Returns200 verifies that a backoffice-scoped service key
// (the canonical production caller) reaches GET /agents and receives 200 (§5.2).
// The authority comes from api_keys.scope == "backoffice" in Postgres, not from is_admin.
func TestListAgents_BackofficeKey_Returns200(t *testing.T) {
	s := newAgentFakeStore()
	s.users["agent-1"] = &domain.User{
		ID: "agent-1", Type: domain.UserTypeAgent, IsAdmin: false,
		IsActive: true, CreatedAt: time.Now(),
	}

	r := buildAgentRouter(s, backofficePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("backoffice-scoped key must reach GET /agents (want 200), got %d; body: %s",
			w.Code, w.Body.String())
	}
}

// TestListAgents_Admin_Returns200 verifies Admin gets the agent list (AC-8).
func TestListAgents_Admin_Returns200(t *testing.T) {
	s := newAgentFakeStore()
	s.users["agent-1"] = &domain.User{
		ID: "agent-1", Type: domain.UserTypeAgent, IsAdmin: false,
		IsActive: true, CreatedAt: time.Now(),
	}

	r := buildAgentRouter(s, adminPrincipal())
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("want 200, got %d; body: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Errorf("want 1 item, got %d", len(resp.Items))
	}
}

// TestListAgents_NonAdmin_Returns403 verifies non-Admin is rejected (AC-8, Layer B).
func TestListAgents_NonAdmin_Returns403(t *testing.T) {
	r := buildAgentRouter(newAgentFakeStore(), nonAdminPrincipal())
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestListAgents_NilPrincipal_Returns403 verifies unauthenticated is rejected (AC-8).
func TestListAgents_NilPrincipal_Returns403(t *testing.T) {
	r := buildAgentRouter(newAgentFakeStore(), nil)
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

// ─── AC-2: POST /agents ───────────────────────────────────────────────────────

// TestAddAgent_BackofficeKey_Returns201WithConnectURL verifies that a backoffice-scoped
// service key (the canonical production caller) can create an agent and receives 201
// with a connect_url (§5.2). Authority comes from api_keys.scope, not is_admin.
func TestAddAgent_BackofficeKey_Returns201WithConnectURL(t *testing.T) {
	s := newAgentFakeStore()
	r := buildAgentRouter(s, backofficePrincipal())

	body := `{"email":"agent@example.com","is_admin":false}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("backoffice-scoped key must reach POST /agents (want 201), got %d; body: %s",
			w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["connect_url"] == nil || resp["connect_url"] == "" {
		t.Error("response must include a non-empty 'connect_url'")
	}
}

// TestAddAgent_Admin_Returns201WithConnectURL verifies Admin creates agent and gets connect_url (AC-2).
func TestAddAgent_Admin_Returns201WithConnectURL(t *testing.T) {
	s := newAgentFakeStore()
	r := buildAgentRouter(s, adminPrincipal())

	body := `{"email":"agent@example.com","is_admin":false}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("want 201, got %d; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] == nil {
		t.Error("response must include 'id'")
	}
	if resp["connect_url"] == nil || resp["connect_url"] == "" {
		t.Error("response must include a non-empty 'connect_url'")
	}
	if resp["type"] != "agent" {
		t.Errorf("want type=agent, got %v", resp["type"])
	}
}

// TestAddAgent_NonAdmin_Returns403 verifies non-Admin is rejected (AC-2, Layer B).
func TestAddAgent_NonAdmin_Returns403(t *testing.T) {
	s := newAgentFakeStore()
	r := buildAgentRouter(s, nonAdminPrincipal())

	body := `{"email":"agent@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestAddAgent_InvalidBody_Returns400 verifies validation (email required).
func TestAddAgent_InvalidBody_Returns400(t *testing.T) {
	r := buildAgentRouter(newAgentFakeStore(), adminPrincipal())

	body := `{"is_admin":false}` // missing required email
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d; body: %s", w.Code, w.Body.String())
	}
}

// ─── AC-3: AuthZ is pure Postgres function, not Discord role ─────────────────

// TestAddAgent_AuthZPurePostgres_CollaboratorDenied verifies that a principal with
// IsAdmin=false is denied regardless of any hypothetical Discord role (AC-3, NFR-13).
// The test embodies the invariant: "even if Discord shows someone with the Agent role,
// if Postgres says non-admin they are NOT authorized."
func TestAddAgent_AuthZPurePostgres_CollaboratorDenied(t *testing.T) {
	s := newAgentFakeStore()
	// Principal has IsAdmin=false — this is the Postgres decision.
	// A collaborator with the Discord Agent role would still have IsAdmin=false from Postgres.
	collaboratorLikePrincipal := &authz.Principal{
		Type:    authz.PrincipalTypeService,
		KeyID:   "k3",
		IsAdmin: false, // Postgres says: not admin
	}
	r := buildAgentRouter(s, collaboratorLikePrincipal)

	body := `{"email":"agent@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Layer B denies based solely on Principal.IsAdmin (from Postgres), not Discord.
	if w.Code != http.StatusForbidden {
		t.Errorf("want 403 (Postgres-resolved non-admin), got %d; body: %s", w.Code, w.Body.String())
	}
}

// ─── DELETE /agents/{id} ─────────────────────────────────────────────────────

// TestRemoveAgent_Admin_Returns202 verifies Admin gets 202 (AC from OpenAPI).
func TestRemoveAgent_Admin_Returns202(t *testing.T) {
	s := newAgentFakeStore()
	s.users["agent-001"] = &domain.User{
		ID: "agent-001", Type: domain.UserTypeAgent, IsActive: true,
	}

	r := buildAgentRouter(s, adminPrincipal())
	req := httptest.NewRequest(http.MethodDelete, "/agents/agent-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestRemoveAgent_NonAdmin_Returns403 verifies non-Admin is rejected (Layer B).
func TestRemoveAgent_NonAdmin_Returns403(t *testing.T) {
	r := buildAgentRouter(newAgentFakeStore(), nonAdminPrincipal())
	req := httptest.NewRequest(http.MethodDelete, "/agents/agent-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

// TestRemoveAgent_NotFound_Returns404 verifies 404 when agent does not exist.
func TestRemoveAgent_NotFound_Returns404(t *testing.T) {
	r := buildAgentRouter(newAgentFakeStore(), adminPrincipal())
	req := httptest.NewRequest(http.MethodDelete, "/agents/nonexistent-id", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestRemoveAgent_NonAgentType_Returns404 verifies 404 when the user is not an agent.
// A collaborator cannot be removed via the /agents endpoint.
func TestRemoveAgent_NonAgentType_Returns404(t *testing.T) {
	s := newAgentFakeStore()
	s.users["collab-001"] = &domain.User{
		ID: "collab-001", Type: domain.UserTypeCollaborator, IsActive: true,
	}

	r := buildAgentRouter(s, adminPrincipal())
	req := httptest.NewRequest(http.MethodDelete, "/agents/collab-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for non-agent user, got %d; body: %s", w.Code, w.Body.String())
	}
}
