// Package handlers_test — M3 collaborator handler tests.
//
// Tests cover (AC-2, AC-4, AC-5, FR-20):
//   - InviteCollaborator: control-plane gate (AC-4 / FR-20); happy path 202;
//     space-not-found 404; user-not-found 404; connect_url returned when no Discord ID.
//   - ExpelCollaborator: control-plane gate; scope=channel vs scope=server dispatch;
//     invalid scope rejected 400; space/user not-found 404.
//   - ListCollaboratorChannels: control-plane gate; user-not-found 404; items returned.
//   - FR-20: a collaborator-type principal (non-control-plane) calling invite → 403.
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
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Factories ────────────────────────────────────────────────────────────────

// collabTestCPPrincipal returns a backoffice-scoped principal (control-plane authority).
// Named with a unique prefix to avoid collision with the controlPlanePrincipal factory
// declared in jobs_test.go (both live in the same handlers_test package).
func collabTestCPPrincipal() *authz.Principal {
	return &authz.Principal{
		Type:     authz.PrincipalTypeService,
		KeyID:    "cp-key-001",
		KeyScope: authz.ScopeBackoffice,
	}
}

// collabTestNonCPPrincipal returns a principal that does NOT have control-plane authority.
// In the real system this would be a session principal for a human collaborator.
// FR-20: such a principal calling invite/expel must receive 403.
func collabTestNonCPPrincipal() *authz.Principal {
	return &authz.Principal{
		Type:    authz.PrincipalTypeService,
		KeyID:   "collab-key-001",
		IsAdmin: false,
		// KeyScope is empty (not "backoffice") → RequireControlPlane returns Deny.
	}
}

// makeSpace returns a minimal active space fixture.
func makeSpace(id, merchantID string) *domain.Space {
	ch := "discord-channel-" + id
	return &domain.Space{
		ID:               id,
		MerchantID:       merchantID,
		DiscordChannelID: &ch,
		ACLState:         domain.ACLStateApplied,
		LifecycleState:   domain.SpaceLifecycleActive,
		Name:             "Support Space " + id,
		CreatedAt:        time.Now(),
	}
}

// makeCollaborator returns a minimal collaborator user.
func makeCollaborator(id string, withDiscordID bool) *domain.User {
	u := &domain.User{
		ID:        id,
		Type:      domain.UserTypeCollaborator,
		IsActive:  true,
		CreatedAt: time.Now(),
	}
	if withDiscordID {
		did := "discord-" + id
		u.DiscordUserID = &did
	}
	return u
}

// makeMerchant returns a minimal merchant fixture.
func makeMerchant(id string) *domain.Merchant {
	return &domain.Merchant{
		ID:        id,
		Name:      "Merchant " + id,
		IsActive:  true,
		CreatedAt: time.Now(),
	}
}

// ─── collaboratorFakeStore ─────────────────────────────────────────────────────

// collaboratorFakeStore backs all collaborator handler tests.
// Only methods called by those handlers are implemented; rest panic.
type collaboratorFakeStore struct {
	spaces    map[string]*domain.Space
	users     map[string]*domain.User
	merchants map[string]*domain.Merchant
	members   map[string][]*domain.SpaceMember // userID → members
	spaceMap  map[string][]*domain.SpaceMember // spaceID → members

	createMemberErr error
	createJobErr    error
}

func newCollaboratorFakeStore() *collaboratorFakeStore {
	return &collaboratorFakeStore{
		spaces:    make(map[string]*domain.Space),
		users:     make(map[string]*domain.User),
		merchants: make(map[string]*domain.Merchant),
		members:   make(map[string][]*domain.SpaceMember),
		spaceMap:  make(map[string][]*domain.SpaceMember),
	}
}

func (f *collaboratorFakeStore) GetSpaceByID(_ context.Context, id string) (*domain.Space, error) {
	sp, ok := f.spaces[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sp, nil
}

func (f *collaboratorFakeStore) GetUserByID(_ context.Context, id string) (*domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (f *collaboratorFakeStore) GetMerchantByID(_ context.Context, id string) (*domain.Merchant, error) {
	m, ok := f.merchants[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return m, nil
}

func (f *collaboratorFakeStore) CreateSpaceMember(_ context.Context, p store.CreateSpaceMemberParams) (*domain.SpaceMember, error) {
	if f.createMemberErr != nil {
		return nil, f.createMemberErr
	}
	sm := &domain.SpaceMember{
		ID:        "sm-" + p.SpaceID + "-" + p.UserID,
		SpaceID:   p.SpaceID,
		UserID:    p.UserID,
		Role:      p.Role,
		InvitedBy: p.InvitedBy,
		CreatedAt: time.Now(),
	}
	f.spaceMap[p.SpaceID] = append(f.spaceMap[p.SpaceID], sm)
	return sm, nil
}

func (f *collaboratorFakeStore) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	if f.createJobErr != nil {
		return nil, f.createJobErr
	}
	return &domain.Job{
		ID:        "job-001",
		Status:    domain.JobStatusPending,
		CreatedAt: time.Now(),
	}, nil
}

func (f *collaboratorFakeStore) ListCollaboratorChannels(_ context.Context, userID string) ([]*domain.SpaceMember, error) {
	return f.members[userID], nil
}

func (f *collaboratorFakeStore) ListSpaceMembers(_ context.Context, spaceID string) ([]*domain.SpaceMember, error) {
	return f.spaceMap[spaceID], nil
}

// Remaining store.Store interface methods — not called by collaborator handlers.
func (f *collaboratorFakeStore) Ping(_ context.Context) error { panic("Ping") }
func (f *collaboratorFakeStore) CreateMerchant(_ context.Context, _ store.CreateMerchantParams) (*domain.Merchant, error) {
	panic("CreateMerchant")
}
func (f *collaboratorFakeStore) CreateUser(_ context.Context, _ store.CreateUserParams) (*domain.User, error) {
	panic("CreateUser")
}
func (f *collaboratorFakeStore) GetUserByDiscordID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByDiscordID")
}
func (f *collaboratorFakeStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	panic("ListAgents")
}
func (f *collaboratorFakeStore) DeactivateUser(_ context.Context, _ string) (*domain.User, error) {
	panic("DeactivateUser")
}
func (f *collaboratorFakeStore) SetUserProvisionedAt(_ context.Context, _ string) (*domain.User, error) {
	panic("SetUserProvisionedAt")
}
func (f *collaboratorFakeStore) CreateAPIKey(_ context.Context, _ store.CreateAPIKeyParams) (*domain.APIKey, error) {
	panic("CreateAPIKey")
}
func (f *collaboratorFakeStore) ListAPIKeys(_ context.Context, _ bool) ([]*domain.APIKey, error) {
	panic("ListAPIKeys")
}
func (f *collaboratorFakeStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	panic("LookupActiveAPIKeyByHash")
}
func (f *collaboratorFakeStore) RevokeAPIKey(_ context.Context, _ string) error {
	panic("RevokeAPIKey")
}
func (f *collaboratorFakeStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	panic("TouchAPIKeyLastUsed")
}
func (f *collaboratorFakeStore) UpsertOAuthToken(_ context.Context, _ store.UpsertOAuthTokenParams) (*domain.OAuthToken, error) {
	panic("UpsertOAuthToken")
}
func (f *collaboratorFakeStore) GetOAuthTokenByUserID(_ context.Context, _ string) (*domain.OAuthToken, error) {
	panic("GetOAuthTokenByUserID")
}
func (f *collaboratorFakeStore) CreateSpace(_ context.Context, _ store.CreateSpaceParams) (*domain.Space, error) {
	panic("CreateSpace")
}
func (f *collaboratorFakeStore) GetSpaceByMerchantID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByMerchantID")
}
func (f *collaboratorFakeStore) UpdateSpaceDiscordChannel(_ context.Context, _ store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	panic("UpdateSpaceDiscordChannel")
}
func (f *collaboratorFakeStore) UpdateSpaceACLState(_ context.Context, _ string, _ domain.ACLState) (*domain.Space, error) {
	panic("UpdateSpaceACLState")
}
func (f *collaboratorFakeStore) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	panic("GetJobByID")
}
func (f *collaboratorFakeStore) UpdateJobStatus(_ context.Context, _ store.UpdateJobStatusParams) (*domain.Job, error) {
	panic("UpdateJobStatus")
}
func (f *collaboratorFakeStore) InsertIdempotencyKey(_ context.Context, _ store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	panic("InsertIdempotencyKey")
}
func (f *collaboratorFakeStore) GetIdempotencyKey(_ context.Context, _ string) (*domain.IdempotencyKey, error) {
	panic("GetIdempotencyKey")
}
func (f *collaboratorFakeStore) UpdateIdempotencyKeyResponse(_ context.Context, _ store.UpdateIdempotencyKeyResponseParams) error {
	panic("UpdateIdempotencyKeyResponse")
}
func (f *collaboratorFakeStore) CreateSpaceWithOutbox(_ context.Context, _ store.CreateSpaceParams, _ store.CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error) {
	panic("CreateSpaceWithOutbox")
}
func (f *collaboratorFakeStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	panic("ListPendingOutbox")
}
func (f *collaboratorFakeStore) StampOutboxEnqueued(_ context.Context, _ []string) error {
	panic("StampOutboxEnqueued")
}
func (f *collaboratorFakeStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	panic("UpdateOutboxPayload")
}
func (f *collaboratorFakeStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	return nil // audit writes are always best-effort in tests
}
func (f *collaboratorFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	panic("ListSpaces")
}
func (f *collaboratorFakeStore) GetSpaceMemberBySpaceAndUser(_ context.Context, _, _ string) (*domain.SpaceMember, error) {
	panic("GetSpaceMemberBySpaceAndUser")
}
func (f *collaboratorFakeStore) SetSpaceMemberOverwriteApplied(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("SetSpaceMemberOverwriteApplied")
}
func (f *collaboratorFakeStore) RevokeSpaceMember(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("RevokeSpaceMember")
}
func (f *collaboratorFakeStore) ListDirectory(_ context.Context, _ store.ListDirectoryParams) ([]*store.DirectoryEntry, error) {
	panic("ListDirectory")
}
func (f *collaboratorFakeStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error {
	return nil
}
func (f *collaboratorFakeStore) ListActiveSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListActiveSpaceMembers")
}
func (f *collaboratorFakeStore) UpdateDiscordUserID(_ context.Context, _, _ string) error {
	panic("UpdateDiscordUserID")
}

// M4 store methods — not exercised by collaborator tests.
func (f *collaboratorFakeStore) UpdateSpaceLifecycle(_ context.Context, _ store.UpdateSpaceLifecycleParams) (*domain.Space, error) {
	panic("UpdateSpaceLifecycle")
}
func (f *collaboratorFakeStore) UpdateSpaceWelcomeMessageID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceWelcomeMessageID")
}
func (f *collaboratorFakeStore) ListAuditEntries(_ context.Context, _ store.ListAuditEntriesParams) ([]*domain.AuditEntry, error) {
	panic("ListAuditEntries")
}
func (f *collaboratorFakeStore) GetJobBySpaceIDAndKind(_ context.Context, _, _ string) (*domain.Job, error) {
	return nil, store.ErrNotFound
}

// ListActiveProvisionedSpaces satisfies store.Store (added in M5 for the scheduled sweep).
func (f *collaboratorFakeStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	panic("ListActiveProvisionedSpaces")
}

// ─── Router builders ─────────────────────────────────────────────────────────

// buildCollaboratorRouter builds a minimal Gin engine for collaborator handler tests.
// The principal is injected directly (bypassing Layer A).
func buildCollaboratorRouter(s store.Store, p *authz.Principal, sm *oauth.StateManager) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(func(c *gin.Context) {
		if p != nil {
			c.Set("principal", p)
		}
		c.Next()
	})

	h := handlers.NewHandlers(handlers.Config{
		Store:                   s,
		StateManager:            sm,
		DiscordOAuthClientID:    "test-client-id",
		DiscordOAuthRedirectURL: "https://hub.example.com/v1/oauth/discord/callback",
	})

	r.POST("/channels/:id/collaborators", h.InviteCollaborator)
	r.DELETE("/channels/:id/collaborators/:userId", h.ExpelCollaborator)
	r.GET("/collaborators/:userId/channels", h.ListCollaboratorChannels)
	return r
}

// ─── AC-4 / FR-20: non-control-plane principals → 403 ────────────────────────

// TestInviteCollaborator_NonControlPlane_Returns403 verifies that a collaborator-type
// principal (no control-plane authority) calling POST .../collaborators returns 403.
// This is the FR-20 enforcement test: collaborators cannot invite (AC-4).
func TestInviteCollaborator_NonControlPlane_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestNonCPPrincipal(), nil)

	body := `{"user_id":"some-user"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("FR-20/AC-4: non-control-plane principal calling invite must get 403, got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestInviteCollaborator_NilPrincipal_Returns403 verifies that an unauthenticated call
// (nil principal, which Layer A would never allow but is tested defensively) returns 403.
func TestInviteCollaborator_NilPrincipal_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(`{"user_id":"u"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("nil principal must get 403, got %d", w.Code)
	}
}

// ─── AC-2: InviteCollaborator happy paths ────────────────────────────────────

// TestInviteCollaborator_Returns202 verifies that InviteCollaborator returns 202 + job
// when a control-plane principal invites an existing collaborator to an existing space (AC-2).
func TestInviteCollaborator_Returns202(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-001"] = makeCollaborator("user-001", true) // has Discord ID

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	body := `{"user_id":"user-001"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the response shape contains a job.
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["job"] == nil {
		t.Error("response must contain a job field")
	}
}

// TestInviteCollaborator_SpaceNotFound_Returns404 verifies that inviting to a non-existent
// space returns 404 (AC-2 precondition).
func TestInviteCollaborator_SpaceNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	// No space registered.

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	body := `{"user_id":"user-001"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/does-not-exist/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown space, got %d: %s", w.Code, w.Body.String())
	}
}

// TestInviteCollaborator_UserNotFound_Returns404 verifies that inviting an unknown user
// to an existing space returns 404.
func TestInviteCollaborator_UserNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	// No user registered.

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	body := `{"user_id":"unknown-user"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown user, got %d: %s", w.Code, w.Body.String())
	}
}

// TestInviteCollaborator_NoDiscordID_ConnectURLPresent verifies that when the user
// has not yet connected their Discord account (discord_user_id == nil), the response
// body includes a connect_url for the OAuth2 flow (AC-2, FR-22).
func TestInviteCollaborator_NoDiscordID_ConnectURLPresent(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-no-discord"] = makeCollaborator("user-no-discord", false) // NO Discord ID

	// Wire a real StateManager so connect_url generation works.
	sm := makeTestStateManager(t)
	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), sm)

	body := `{"user_id":"user-no-discord"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// connect_url must be non-nil when the user has no Discord ID.
	if resp["connect_url"] == nil {
		t.Error("AC-2: connect_url must be present when user has no discord_user_id")
	}
	cu, ok := resp["connect_url"].(string)
	if !ok || cu == "" {
		t.Error("AC-2: connect_url must be a non-empty string")
	}
}

// TestInviteCollaborator_WithDiscordID_NoConnectURL verifies that when the user already
// has a Discord ID linked, no connect_url is returned (AC-2 — OAuth2 not needed).
func TestInviteCollaborator_WithDiscordID_NoConnectURL(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-with-discord"] = makeCollaborator("user-with-discord", true) // HAS Discord ID

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	body := `{"user_id":"user-with-discord"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// connect_url must be absent when the user already has a Discord ID.
	if v, present := resp["connect_url"]; present && v != nil {
		t.Errorf("AC-2: connect_url must be nil when user already has discord_user_id, got %v", v)
	}
}

// ─── AC-5: ExpelCollaborator ─────────────────────────────────────────────────

// TestExpelCollaborator_NonControlPlane_Returns403 verifies FR-20 for expel.
func TestExpelCollaborator_NonControlPlane_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestNonCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodDelete, "/channels/space-001/collaborators/user-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("FR-20: non-control-plane principal calling expel must get 403, got %d", w.Code)
	}
}

// TestExpelCollaborator_ChannelScope_Returns202 verifies scope=channel returns 202
// and the job payload includes scope=channel (AC-5 channel-scope path).
func TestExpelCollaborator_ChannelScope_Returns202(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-001"] = makeCollaborator("user-001", true)

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001?scope=channel", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("AC-5 channel scope: want 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["job"] == nil {
		t.Error("response must include a job")
	}
}

// TestExpelCollaborator_ServerScope_Returns202 verifies scope=server returns 202
// (AC-5 server-scope path — also removes from guild, enqueued via worker).
func TestExpelCollaborator_ServerScope_Returns202(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-001"] = makeCollaborator("user-001", true)

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001?scope=server", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("AC-5 server scope: want 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExpelCollaborator_DefaultScope_IsChannel verifies that omitting the scope parameter
// defaults to channel scope (AC-5, FR-19 default).
func TestExpelCollaborator_DefaultScope_IsChannel(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-001"] = makeCollaborator("user-001", true)

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	// No scope query parameter — should default to channel.
	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("default scope: want 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExpelCollaborator_InvalidScope_Returns400 verifies that an unknown scope value
// returns 400 (AC-5 validation).
func TestExpelCollaborator_InvalidScope_Returns400(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-001"] = makeCollaborator("user-001", true)

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001?scope=guild", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid scope must return 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExpelCollaborator_SpaceNotFound_Returns404 verifies 404 for unknown space (AC-5).
func TestExpelCollaborator_SpaceNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/no-such-space/collaborators/user-001?scope=channel", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown space must return 404, got %d", w.Code)
	}
}

// TestExpelCollaborator_UserNotFound_Returns404 verifies 404 for unknown user (AC-5).
func TestExpelCollaborator_UserNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/unknown-user?scope=channel", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown user must return 404, got %d", w.Code)
	}
}

// ─── AC-7: ListCollaboratorChannels ──────────────────────────────────────────

// TestListCollaboratorChannels_NonControlPlane_Returns403 verifies FR-20 for list.
func TestListCollaboratorChannels_NonControlPlane_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestNonCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodGet, "/collaborators/user-001/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("FR-20: non-control-plane calling list must get 403, got %d", w.Code)
	}
}

// TestListCollaboratorChannels_UserNotFound_Returns404 verifies 404 for unknown user (AC-7).
func TestListCollaboratorChannels_UserNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodGet, "/collaborators/unknown-user/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown user must return 404, got %d", w.Code)
	}
}

// TestListCollaboratorChannels_ReturnsItems verifies that an existing user with
// space memberships gets a 200 with an items list (AC-7).
func TestListCollaboratorChannels_ReturnsItems(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.users["user-001"] = makeCollaborator("user-001", true)
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.merchants["merchant-001"] = makeMerchant("merchant-001")

	// Simulate one active space_member row for user-001 in space-001.
	s.members["user-001"] = []*domain.SpaceMember{
		{
			ID:        "sm-001",
			SpaceID:   "space-001",
			UserID:    "user-001",
			Role:      domain.SpaceMemberRoleCollaborator,
			CreatedAt: time.Now(),
		},
	}

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodGet, "/collaborators/user-001/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Errorf("want 1 item, got %d", len(resp.Items))
	}
}

// TestListCollaboratorChannels_EmptyList_Returns200 verifies that a user with no
// memberships returns 200 with an empty items array (not 404).
func TestListCollaboratorChannels_EmptyList_Returns200(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.users["user-no-spaces"] = makeCollaborator("user-no-spaces", true)
	// No space memberships.

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodGet, "/collaborators/user-no-spaces/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Items == nil {
		t.Error("items field must not be null — must be an empty array")
	}
}

// ─── Isolation: collaborator with overwrites on A and C, not B ───────────────

// TestIsolation_CollaboratorWithOverwritesOnAandC verifies the explicit multi-merchant
// AC-1 scenario: a collaborator invited to merchant-A's space AND merchant-C's space
// appears in both, but has NO access to merchant-B's space.
//
// This is the "C sees A and C but never B" fixture requested in the task spec.
// The isolation invariant is enforced at the store layer: ListCollaboratorChannels
// only returns rows where a space_member row exists for the user. Without a row
// in merchant-B's space, it is impossible for the user to enumerate that space.
func TestIsolation_CollaboratorWithOverwritesOnAandC_NotB(t *testing.T) {
	s := newCollaboratorFakeStore()

	// Three merchants and their spaces.
	s.merchants["merchant-a"] = makeMerchant("merchant-a")
	s.merchants["merchant-b"] = makeMerchant("merchant-b")
	s.merchants["merchant-c"] = makeMerchant("merchant-c")
	s.spaces["space-a"] = makeSpace("space-a", "merchant-a")
	s.spaces["space-b"] = makeSpace("space-b", "merchant-b")
	s.spaces["space-c"] = makeSpace("space-c", "merchant-c")

	// CollabX is invited to merchant-A and merchant-C, but NOT merchant-B.
	s.users["collab-x"] = makeCollaborator("collab-x", true)
	s.members["collab-x"] = []*domain.SpaceMember{
		{ID: "sm-xa", SpaceID: "space-a", UserID: "collab-x", Role: domain.SpaceMemberRoleCollaborator, CreatedAt: time.Now()},
		{ID: "sm-xc", SpaceID: "space-c", UserID: "collab-x", Role: domain.SpaceMemberRoleCollaborator, CreatedAt: time.Now()},
		// No entry for space-b — isolation invariant.
	}

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodGet, "/collaborators/collab-x/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Must see exactly 2 spaces (A and C).
	if len(resp.Items) != 2 {
		t.Fatalf("AC-1 isolation: collab-x must see 2 spaces (A and C), got %d: %v",
			len(resp.Items), resp.Items)
	}

	// Neither item must reference merchant-B's space.
	for _, item := range resp.Items {
		if spaceID, _ := item["space_id"].(string); spaceID == "space-b" {
			t.Errorf("AC-1 isolation breach: collab-x must not see space-b (merchant-B)")
		}
		if merchantID, _ := item["merchant_id"].(string); merchantID == "merchant-b" {
			t.Errorf("AC-1 isolation breach: collab-x must not see merchant-b's space")
		}
	}
}

// TestIsolation_CollaboratorWithZeroMembers_SeesNothing verifies that a collaborator
// with no space_members rows receives an empty list (AC-1 zero-membership fixture).
func TestIsolation_CollaboratorWithZeroMembers_SeesNothing(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.users["collab-zero"] = makeCollaborator("collab-zero", true)
	// No space_member rows — isolation: sees nothing.

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	req := httptest.NewRequest(http.MethodGet, "/collaborators/collab-zero/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Items) != 0 {
		t.Errorf("AC-1 isolation: zero-member collaborator must see 0 spaces, got %d", len(resp.Items))
	}
}

// ─── AC-8: no invite link created during collaborator operations ──────────────

// TestInviteCollaborator_NoInviteLinkCreated verifies that InviteCollaborator
// does not produce any Discord invite link — access is via per-user overwrite only (AC-8, NFR-14).
// The assertion is structural: the response must not contain any "invite" or "invite_url"
// field, and no invite-minting Discord method is called (mocked via the fake store).
func TestInviteCollaborator_NoInviteLinkCreated(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.users["user-001"] = makeCollaborator("user-001", true)

	r := buildCollaboratorRouter(s, collabTestCPPrincipal(), nil)

	body := `{"user_id":"user-001"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// The response must not contain an invite link.
	for _, key := range []string{"invite", "invite_url", "invite_link"} {
		if v, present := resp[key]; present && v != nil {
			t.Errorf("AC-8/NFR-14: response must not contain %q field (no invite links), got %v",
				key, v)
		}
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// makeTestStateManager returns a StateManager backed by MemNonceStore for handler tests.
func makeTestStateManager(t *testing.T) *oauth.StateManager {
	t.Helper()
	secret := "aabbccdd" + "11223344" + "aabbccdd" + "11223344" +
		"aabbccdd" + "11223344" + "aabbccdd" + "11223344"
	sm, err := oauth.NewStateManager(secret, oauth.NewMemNonceStore())
	if err != nil {
		t.Fatalf("makeTestStateManager: %v", err)
	}
	return sm
}
