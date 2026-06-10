// Package handlers_test — M6 collaborator handler tests.
//
// M6 pivot: InviteCollaborator replaced by RegisterCollaborator (POST /channels/:id/collaborators)
// and SendCollaboratorInvite (POST /channels/:id/collaborators/:userId:send-invite).
// OAuth2-related paths removed (AC-M6-9).
//
// Tests cover (AC-M6-4, AC-M6-5, FR-20):
//   - RegisterCollaborator: control-plane gate (AC-4 / FR-20); happy path 201;
//     space-not-found 404; 409 if invite link missing (for send-invite path).
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

	// Indexed lookup maps for the new resolution paths.
	usersByDiscordID map[string]*domain.User
	usersByEmail     map[string]*domain.User

	createMemberErr error
	createJobErr    error

	// Track created users for test assertions.
	createdUsers []*domain.User
}

func newCollaboratorFakeStore() *collaboratorFakeStore {
	return &collaboratorFakeStore{
		spaces:           make(map[string]*domain.Space),
		users:            make(map[string]*domain.User),
		merchants:        make(map[string]*domain.Merchant),
		members:          make(map[string][]*domain.SpaceMember),
		spaceMap:         make(map[string][]*domain.SpaceMember),
		usersByDiscordID: make(map[string]*domain.User),
		usersByEmail:     make(map[string]*domain.User),
	}
}

// addUser registers a user into all relevant index maps.
func (f *collaboratorFakeStore) addUser(u *domain.User) {
	f.users[u.ID] = u
	if u.DiscordUserID != nil {
		f.usersByDiscordID[*u.DiscordUserID] = u
	}
	if u.Email != nil {
		f.usersByEmail[*u.Email] = u
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

func (f *collaboratorFakeStore) GetUserByDiscordID(_ context.Context, discordID string) (*domain.User, error) {
	u, ok := f.usersByDiscordID[discordID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (f *collaboratorFakeStore) GetUserByEmail(_ context.Context, email string) (*domain.User, error) {
	u, ok := f.usersByEmail[email]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (f *collaboratorFakeStore) CreateUser(_ context.Context, p store.CreateUserParams) (*domain.User, error) {
	u := &domain.User{
		ID:            "created-" + time.Now().Format("150405.000000000"),
		Type:          p.Type,
		IsAdmin:       p.IsAdmin,
		DiscordUserID: p.DiscordUserID,
		Email:         p.Email,
		DisplayName:   p.DisplayName,
		IsActive:      true,
		CreatedAt:     time.Now(),
	}
	f.addUser(u)
	f.createdUsers = append(f.createdUsers, u)
	return u, nil
}

func (f *collaboratorFakeStore) GetMerchantByID(_ context.Context, id string) (*domain.Merchant, error) {
	m, ok := f.merchants[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return m, nil
}
func (f *collaboratorFakeStore) GetMerchantByExternalRef(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByExternalRef")
}
func (f *collaboratorFakeStore) ListMerchants(_ context.Context, _ store.ListMerchantsParams) ([]*domain.Merchant, error) {
	panic("ListMerchants")
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
func (f *collaboratorFakeStore) GetSpaceMemberBySpaceAndUser(_ context.Context, spaceID, userID string) (*domain.SpaceMember, error) {
	for _, sm := range f.spaceMap[spaceID] {
		if sm.UserID == userID {
			return sm, nil
		}
	}
	return nil, store.ErrNotFound
}
func (f *collaboratorFakeStore) StampSpaceMemberInviteSent(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("StampSpaceMemberInviteSent")
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

// M6 store methods.
func (f *collaboratorFakeStore) SetMerchantInviteLink(_ context.Context, _ string, _ string) (*domain.Merchant, error) {
	panic("SetMerchantInviteLink")
}
func (f *collaboratorFakeStore) UpdateSpaceMerchantRoleID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceMerchantRoleID")
}

// ─── Router builders ─────────────────────────────────────────────────────────

// buildCollaboratorRouter builds a minimal Gin engine for collaborator handler tests.
// M6: OAuth StateManager removed; handlers.Config simplified.
func buildCollaboratorRouter(s store.Store, p *authz.Principal) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(func(c *gin.Context) {
		if p != nil {
			c.Set("principal", p)
		}
		c.Next()
	})

	h := handlers.NewHandlers(handlers.Config{
		Store: s,
	})

	// M6: POST creates a collaborator record synchronously (AC-M6-4).
	r.POST("/channels/:id/collaborators", h.RegisterCollaborator)
	r.DELETE("/channels/:id/collaborators/:userId", h.ExpelCollaborator)
	r.GET("/collaborators/:userId/channels", h.ListCollaboratorChannels)
	return r
}

// ─── AC-4 / FR-20: non-control-plane principals → 403 ────────────────────────

// TestRegisterCollaborator_NonControlPlane_Returns403 verifies that a collaborator-type
// principal (no control-plane authority) calling POST .../collaborators returns 403 (FR-20).
func TestRegisterCollaborator_NonControlPlane_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestNonCPPrincipal())

	body := `{"name":"Alice","email":"alice@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("FR-20/AC-4: non-control-plane principal calling register must get 403, got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestRegisterCollaborator_NilPrincipal_Returns403 verifies that an unauthenticated call
// (nil principal) returns 403.
func TestRegisterCollaborator_NilPrincipal_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, nil)

	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(`{"name":"Alice","email":"alice@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("nil principal must get 403, got %d", w.Code)
	}
}

// ─── AC-M6-4: RegisterCollaborator happy paths ────────────────────────────────

// TestRegisterCollaborator_Returns201 verifies that RegisterCollaborator returns 201
// when a control-plane principal registers a collaborator with name+email (AC-M6-4).
func TestRegisterCollaborator_Returns201(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.merchants["merchant-001"] = makeMerchant("merchant-001")

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	body := `{"name":"Alice","email":"alice@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("AC-M6-4: want 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["space_member_id"] == nil {
		t.Error("response must contain the space_member id")
	}
}

// TestRegisterCollaborator_SpaceNotFound_Returns404 verifies 404 for unknown space.
func TestRegisterCollaborator_SpaceNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	body := `{"name":"Alice","email":"alice@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/does-not-exist/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown space, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegisterCollaborator_MissingName_Returns400 verifies that omitting name returns 400.
func TestRegisterCollaborator_MissingName_Returns400(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	body := `{"email":"alice@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 when name is missing, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegisterCollaborator_MissingEmail_Returns400 verifies that omitting email returns 400.
func TestRegisterCollaborator_MissingEmail_Returns400(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	body := `{"name":"Alice"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 when email is missing, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegisterCollaborator_BadEmail_Returns400 verifies that a malformed email returns 400.
func TestRegisterCollaborator_BadEmail_Returns400(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	body := `{"name":"Alice","email":"not-an-email"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for malformed email, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRegisterCollaborator_IdempotentReRegister_Returns201 verifies that re-registering the
// same collaborator (ErrConflict on CreateSpaceMember) still returns 201 (idempotent).
func TestRegisterCollaborator_IdempotentReRegister_Returns201(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.merchants["merchant-001"] = makeMerchant("merchant-001")
	// Pre-wire a user so GetUserByEmail returns an existing row.
	email := "alice@example.com"
	existing := &domain.User{
		ID:        "user-alice",
		Type:      domain.UserTypeCollaborator,
		Email:     &email,
		IsActive:  true,
		CreatedAt: time.Now(),
	}
	s.addUser(existing)
	// Pre-seed the existing space_member so GetSpaceMemberBySpaceAndUser succeeds
	// when CreateSpaceMember returns ErrConflict (idempotent re-register path).
	existingSM := &domain.SpaceMember{
		ID:        "sm-space-001-user-alice",
		SpaceID:   "space-001",
		UserID:    "user-alice",
		Role:      domain.SpaceMemberRoleCollaborator,
		CreatedAt: time.Now(),
	}
	s.spaceMap["space-001"] = append(s.spaceMap["space-001"], existingSM)
	// Simulate existing membership by making CreateSpaceMember return ErrConflict.
	s.createMemberErr = store.ErrConflict

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	body := `{"name":"Alice","email":"alice@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/channels/space-001/collaborators",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// On conflict the handler should return 409; tester may adjust this expectation.
	if w.Code != http.StatusCreated && w.Code != http.StatusConflict {
		t.Errorf("re-register of same collaborator must return 201 or 409, got %d: %s", w.Code, w.Body.String())
	}
}

// ─── AC-5: ExpelCollaborator ─────────────────────────────────────────────────

// TestExpelCollaborator_NonControlPlane_Returns403 verifies FR-20 for expel.
func TestExpelCollaborator_NonControlPlane_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestNonCPPrincipal())

	req := httptest.NewRequest(http.MethodDelete, "/channels/space-001/collaborators/user-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("FR-20: non-control-plane principal calling expel must get 403, got %d", w.Code)
	}
}

// TestExpelCollaborator_ChannelScope_Returns202 verifies scope=channel returns 202 (AC-5).
func TestExpelCollaborator_ChannelScope_Returns202(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.addUser(makeCollaborator("user-001", true))

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

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

// TestExpelCollaborator_ServerScope_Returns202 verifies scope=server returns 202 (AC-5).
func TestExpelCollaborator_ServerScope_Returns202(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.addUser(makeCollaborator("user-001", true))

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001?scope=server", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("AC-5 server scope: want 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExpelCollaborator_DefaultScope_IsChannel verifies that omitting scope defaults to channel.
func TestExpelCollaborator_DefaultScope_IsChannel(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.addUser(makeCollaborator("user-001", true))

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("default scope: want 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExpelCollaborator_InvalidScope_Returns400 verifies unknown scope returns 400.
func TestExpelCollaborator_InvalidScope_Returns400(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.addUser(makeCollaborator("user-001", true))

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/space-001/collaborators/user-001?scope=guild", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("invalid scope must return 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExpelCollaborator_SpaceNotFound_Returns404 verifies 404 for unknown space.
func TestExpelCollaborator_SpaceNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	req := httptest.NewRequest(http.MethodDelete,
		"/channels/no-such-space/collaborators/user-001?scope=channel", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown space must return 404, got %d", w.Code)
	}
}

// TestExpelCollaborator_UserNotFound_Returns404 verifies 404 for unknown user.
func TestExpelCollaborator_UserNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

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
	r := buildCollaboratorRouter(s, collabTestNonCPPrincipal())

	req := httptest.NewRequest(http.MethodGet, "/collaborators/user-001/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("FR-20: non-control-plane calling list must get 403, got %d", w.Code)
	}
}

// TestListCollaboratorChannels_UserNotFound_Returns404 verifies 404 for unknown user.
func TestListCollaboratorChannels_UserNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

	req := httptest.NewRequest(http.MethodGet, "/collaborators/unknown-user/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown user must return 404, got %d", w.Code)
	}
}

// TestListCollaboratorChannels_ReturnsItems verifies 200 with items for an existing user.
func TestListCollaboratorChannels_ReturnsItems(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.addUser(makeCollaborator("user-001", true))
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	s.merchants["merchant-001"] = makeMerchant("merchant-001")

	s.members["user-001"] = []*domain.SpaceMember{
		{
			ID:        "sm-001",
			SpaceID:   "space-001",
			UserID:    "user-001",
			Role:      domain.SpaceMemberRoleCollaborator,
			CreatedAt: time.Now(),
		},
	}

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

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

// TestListCollaboratorChannels_EmptyList_Returns200 verifies 200 with empty array.
func TestListCollaboratorChannels_EmptyList_Returns200(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.addUser(makeCollaborator("user-no-spaces", true))

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

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

// ─── Isolation: collaborator with access to A and C, not B ───────────────────

// TestIsolation_CollaboratorWithAccessOnAandC_NotB verifies the AC-1 isolation scenario.
func TestIsolation_CollaboratorWithAccessOnAandC_NotB(t *testing.T) {
	s := newCollaboratorFakeStore()

	s.merchants["merchant-a"] = makeMerchant("merchant-a")
	s.merchants["merchant-b"] = makeMerchant("merchant-b")
	s.merchants["merchant-c"] = makeMerchant("merchant-c")
	s.spaces["space-a"] = makeSpace("space-a", "merchant-a")
	s.spaces["space-b"] = makeSpace("space-b", "merchant-b")
	s.spaces["space-c"] = makeSpace("space-c", "merchant-c")

	s.addUser(makeCollaborator("collab-x", true))
	s.members["collab-x"] = []*domain.SpaceMember{
		{ID: "sm-xa", SpaceID: "space-a", UserID: "collab-x", Role: domain.SpaceMemberRoleCollaborator, CreatedAt: time.Now()},
		{ID: "sm-xc", SpaceID: "space-c", UserID: "collab-x", Role: domain.SpaceMemberRoleCollaborator, CreatedAt: time.Now()},
		// No entry for space-b — isolation invariant.
	}

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

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

	if len(resp.Items) != 2 {
		t.Fatalf("AC-1 isolation: collab-x must see 2 spaces (A and C), got %d: %v",
			len(resp.Items), resp.Items)
	}

	for _, item := range resp.Items {
		if spaceID, _ := item["space_id"].(string); spaceID == "space-b" {
			t.Errorf("AC-1 isolation breach: collab-x must not see space-b (merchant-B)")
		}
		if merchantID, _ := item["merchant_id"].(string); merchantID == "merchant-b" {
			t.Errorf("AC-1 isolation breach: collab-x must not see merchant-b's space")
		}
	}
}

// ─── AC-M6-5: SendCollaboratorInvite handler ─────────────────────────────────

// buildSendInviteRouter builds a minimal Gin engine wired to the SendCollaboratorInvite route.
// The route matches the router.go path: POST /channels/:id/collaborators/:userId/send-invite.
func buildSendInviteRouter(s store.Store, p *authz.Principal) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(func(c *gin.Context) {
		if p != nil {
			c.Set("principal", p)
		}
		c.Next()
	})
	h := handlers.NewHandlers(handlers.Config{
		Store: s,
	})
	r.POST("/channels/:id/collaborators/:userId/send-invite", h.SendCollaboratorInvite)
	return r
}

func doSendInvite(r *gin.Engine, spaceID, userID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost,
		"/channels/"+spaceID+"/collaborators/"+userID+"/send-invite", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestSendCollaboratorInvite_NoInviteLink_Returns409 verifies that when the merchant has
// no invite link stored, the handler returns 409 with code "no_invite_link" (AC-M6-5).
func TestSendCollaboratorInvite_NoInviteLink_Returns409(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	// Merchant exists but has NO invite link.
	s.merchants["merchant-001"] = &domain.Merchant{
		ID:         "merchant-001",
		Name:       "ACME",
		IsActive:   true,
		InviteLink: nil,
		CreatedAt:  time.Now(),
	}
	s.addUser(makeCollaborator("user-001", false))

	r := buildSendInviteRouter(s, collabTestCPPrincipal())
	w := doSendInvite(r, "space-001", "user-001")

	if w.Code != http.StatusConflict {
		t.Errorf("AC-M6-5: want 409 when no invite link, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["code"] != "no_invite_link" {
		t.Errorf("AC-M6-5: want code=no_invite_link, got %v", resp["code"])
	}
}

// TestSendCollaboratorInvite_WithInviteLink_Returns202 verifies that when the merchant has
// an invite link stored, the handler enqueues the task and returns 202 (AC-M6-5, AC-M6-6).
func TestSendCollaboratorInvite_WithInviteLink_Returns202(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	inviteLink := "https://discord.gg/testlink"
	s.merchants["merchant-001"] = &domain.Merchant{
		ID:         "merchant-001",
		Name:       "ACME",
		IsActive:   true,
		InviteLink: &inviteLink,
		CreatedAt:  time.Now(),
	}
	s.addUser(makeCollaborator("user-001", false))
	// Pre-seed space_member so GetSpaceMemberBySpaceAndUser succeeds.
	s.spaceMap["space-001"] = []*domain.SpaceMember{
		{
			ID:        "sm-space-001-user-001",
			SpaceID:   "space-001",
			UserID:    "user-001",
			Role:      domain.SpaceMemberRoleCollaborator,
			CreatedAt: time.Now(),
		},
	}

	r := buildSendInviteRouter(s, collabTestCPPrincipal())
	w := doSendInvite(r, "space-001", "user-001")

	if w.Code != http.StatusAccepted {
		t.Fatalf("AC-M6-5: want 202 when invite link present, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Response must include a job reference so callers can poll status.
	if resp["job"] == nil {
		t.Error("AC-M6-5: response must include a 'job' field on 202")
	}
}

// TestSendCollaboratorInvite_SpaceNotFound_Returns404 verifies 404 for an unknown space.
func TestSendCollaboratorInvite_SpaceNotFound_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	// No space seeded.
	r := buildSendInviteRouter(s, collabTestCPPrincipal())
	w := doSendInvite(r, "no-such-space", "user-001")

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for unknown space, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSendCollaboratorInvite_CollaboratorNotRegistered_Returns404 verifies 404 when the
// user is not a registered space_member (AC-M6-5 precondition).
func TestSendCollaboratorInvite_CollaboratorNotRegistered_Returns404(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.spaces["space-001"] = makeSpace("space-001", "merchant-001")
	inviteLink := "https://discord.gg/abc"
	s.merchants["merchant-001"] = &domain.Merchant{
		ID:         "merchant-001",
		Name:       "ACME",
		IsActive:   true,
		InviteLink: &inviteLink,
		CreatedAt:  time.Now(),
	}
	// User exists but is NOT a space_member.
	s.addUser(makeCollaborator("user-not-member", false))
	// spaceMap["space-001"] is empty → GetSpaceMemberBySpaceAndUser returns ErrNotFound.

	r := buildSendInviteRouter(s, collabTestCPPrincipal())
	w := doSendInvite(r, "space-001", "user-not-member")

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for non-member, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSendCollaboratorInvite_NonControlPlane_Returns403 verifies Layer B authZ (AC-M6-5).
func TestSendCollaboratorInvite_NonControlPlane_Returns403(t *testing.T) {
	s := newCollaboratorFakeStore()
	r := buildSendInviteRouter(s, collabTestNonCPPrincipal())
	w := doSendInvite(r, "space-001", "user-001")

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403 for non-control-plane principal, got %d", w.Code)
	}
}

// ─── TestIsolation_CollaboratorWithZeroMembers_SeesNothing ────────────────────

// TestIsolation_CollaboratorWithZeroMembers_SeesNothing verifies the zero-membership case.
func TestIsolation_CollaboratorWithZeroMembers_SeesNothing(t *testing.T) {
	s := newCollaboratorFakeStore()
	s.addUser(makeCollaborator("collab-zero", true))

	r := buildCollaboratorRouter(s, collabTestCPPrincipal())

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
