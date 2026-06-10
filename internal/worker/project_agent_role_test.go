// Package worker_test verifies the project_agent_role handler (M1, AC-4).
//
// All tests are hermetic: a mock discord.Client and a fake store.Store are used;
// no real Discord API calls or database connections are made.
//
// Tests cover:
//   - AC-4: agent with discord_user_id → GuildMemberRoleAdd called (assign)
//   - AC-4: agent with discord_user_id → GuildMemberRoleRemove called (remove)
//   - AC-4: reconcile re-assertion — AssignAgentRole called on second run after manual removal
//   - agent without discord_user_id → role deferred, retryable error returned
//   - non-agent user → role NOT projected (Postgres always wins, NFR-13)
//   - user not found → skip silently
package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── Fakes ────────────────────────────────────────────────────────────────────

// fakeDiscordClient records calls to AssignAgentRole and RemoveAgentRole.
type fakeDiscordClient struct {
	assignCalls []string // discord user ids that had the role assigned
	removeCalls []string // discord user ids that had the role removed
	assignErr   error    // if set, AssignAgentRole returns this error
	removeErr   error    // if set, RemoveAgentRole returns this error
}

func (f *fakeDiscordClient) Ping(_ context.Context) error { return nil }

func (f *fakeDiscordClient) AssignAgentRole(_ context.Context, _, discordUserID, _ string) error {
	f.assignCalls = append(f.assignCalls, discordUserID)
	return f.assignErr
}

func (f *fakeDiscordClient) RemoveAgentRole(_ context.Context, _, discordUserID, _ string) error {
	f.removeCalls = append(f.removeCalls, discordUserID)
	return f.removeErr
}

// M2b discord.Client methods — not exercised by project_agent_role tests, stub only.
func (f *fakeDiscordClient) CreateChannelDenied(_ context.Context, _, _, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeDiscordClient) ApplyCategoryAgentAllow(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeDiscordClient) SetChannelPermissionDeny(_ context.Context, _, _ string, _ discordgo.PermissionOverwriteType) error {
	return nil
}

// M3/M6 discord.Client methods — not exercised by project_agent_role tests.
func (f *fakeDiscordClient) DeleteCollaboratorOverwrite(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeDiscordClient) RemoveGuildMember(_ context.Context, _, _ string) error {
	return nil
}
func (f *fakeDiscordClient) GetChannelOverwrites(_ context.Context, _ string) ([]*discordgo.PermissionOverwrite, error) {
	return nil, nil
}
func (f *fakeDiscordClient) CreateMerchantRole(_ context.Context, _, _ string) (string, error) {
	return "role-123", nil
}
func (f *fakeDiscordClient) SetRoleChannelAllow(_ context.Context, _, _ string) error { return nil }
func (f *fakeDiscordClient) AssignMerchantRole(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeDiscordClient) RemoveMerchantRole(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeDiscordClient) GetGuildMembersByRole(_ context.Context, _, _ string) ([]string, error) {
	return nil, nil
}
func (f *fakeDiscordClient) EnsureWelcomeChannel(_ context.Context, _, _, _, _ string) (string, error) {
	return "chan-123", nil
}

// M4 discord.Client methods — not exercised by project_agent_role tests.
func (f *fakeDiscordClient) ArchiveChannel(_ context.Context, _, _ string) error   { return nil }
func (f *fakeDiscordClient) UnarchiveChannel(_ context.Context, _, _ string) error { return nil }
func (f *fakeDiscordClient) SetChannelTopic(_ context.Context, _, _ string) error  { return nil }
func (f *fakeDiscordClient) PinMessage(_ context.Context, _, _ string) error       { return nil }
func (f *fakeDiscordClient) EditMessage(_ context.Context, _, _, _ string) error   { return nil }
func (f *fakeDiscordClient) SendMessage(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeDiscordClient) SetNickname(_ context.Context, _, _, _ string) error { return nil }

// workerFakeStore implements store.Store for worker tests.
type workerFakeStore struct {
	users            map[string]*domain.User
	provisionedAt    []string // user ids that had SetUserProvisionedAt called
	provisionedAtErr error
}

func newWorkerFakeStore() *workerFakeStore {
	return &workerFakeStore{users: make(map[string]*domain.User)}
}

func (f *workerFakeStore) GetUserByID(_ context.Context, id string) (*domain.User, error) {
	u, ok := f.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (f *workerFakeStore) SetUserProvisionedAt(_ context.Context, id string) (*domain.User, error) {
	if f.provisionedAtErr != nil {
		return nil, f.provisionedAtErr
	}
	f.provisionedAt = append(f.provisionedAt, id)
	u := f.users[id]
	now := time.Now()
	u.ProvisionedAt = &now
	return u, nil
}

// Full store.Store interface — remaining methods panic.
func (f *workerFakeStore) Ping(_ context.Context) error { panic("Ping") }
func (f *workerFakeStore) CreateMerchant(_ context.Context, _ store.CreateMerchantParams) (*domain.Merchant, error) {
	panic("CreateMerchant")
}
func (f *workerFakeStore) GetMerchantByID(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByID")
}
func (f *workerFakeStore) GetMerchantByExternalRef(_ context.Context, _ string) (*domain.Merchant, error) {
	panic("GetMerchantByExternalRef")
}
func (f *workerFakeStore) ListMerchants(_ context.Context, _ store.ListMerchantsParams) ([]*domain.Merchant, error) {
	panic("ListMerchants")
}
func (f *workerFakeStore) CreateUser(_ context.Context, _ store.CreateUserParams) (*domain.User, error) {
	panic("CreateUser")
}
func (f *workerFakeStore) GetUserByDiscordID(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByDiscordID")
}
func (f *workerFakeStore) GetUserByEmail(_ context.Context, _ string) (*domain.User, error) {
	panic("GetUserByEmail")
}
func (f *workerFakeStore) ListAgents(_ context.Context, _ bool) ([]*domain.User, error) {
	panic("ListAgents")
}
func (f *workerFakeStore) DeactivateUser(_ context.Context, _ string) (*domain.User, error) {
	panic("DeactivateUser")
}
func (f *workerFakeStore) CreateAPIKey(_ context.Context, _ store.CreateAPIKeyParams) (*domain.APIKey, error) {
	panic("CreateAPIKey")
}
func (f *workerFakeStore) ListAPIKeys(_ context.Context, _ bool) ([]*domain.APIKey, error) {
	panic("ListAPIKeys")
}
func (f *workerFakeStore) LookupActiveAPIKeyByHash(_ context.Context, _ []byte) (*domain.APIKey, error) {
	panic("LookupActiveAPIKeyByHash")
}
func (f *workerFakeStore) RevokeAPIKey(_ context.Context, _ string) error { panic("RevokeAPIKey") }
func (f *workerFakeStore) TouchAPIKeyLastUsed(_ context.Context, _ string) error {
	panic("TouchAPIKeyLastUsed")
}
func (f *workerFakeStore) CreateSpace(_ context.Context, _ store.CreateSpaceParams) (*domain.Space, error) {
	panic("CreateSpace")
}
func (f *workerFakeStore) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByID")
}
func (f *workerFakeStore) GetSpaceByMerchantID(_ context.Context, _ string) (*domain.Space, error) {
	panic("GetSpaceByMerchantID")
}
func (f *workerFakeStore) UpdateSpaceDiscordChannel(_ context.Context, _ store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	panic("UpdateSpaceDiscordChannel")
}
func (f *workerFakeStore) UpdateSpaceACLState(_ context.Context, _ string, _ domain.ACLState) (*domain.Space, error) {
	panic("UpdateSpaceACLState")
}
func (f *workerFakeStore) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	panic("CreateJob")
}
func (f *workerFakeStore) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	panic("GetJobByID")
}
func (f *workerFakeStore) UpdateJobStatus(_ context.Context, _ store.UpdateJobStatusParams) (*domain.Job, error) {
	panic("UpdateJobStatus")
}
func (f *workerFakeStore) InsertIdempotencyKey(_ context.Context, _ store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	panic("InsertIdempotencyKey")
}
func (f *workerFakeStore) GetIdempotencyKey(_ context.Context, _ string) (*domain.IdempotencyKey, error) {
	panic("GetIdempotencyKey")
}
func (f *workerFakeStore) UpdateIdempotencyKeyResponse(_ context.Context, _ store.UpdateIdempotencyKeyResponseParams) error {
	panic("UpdateIdempotencyKeyResponse")
}
func (f *workerFakeStore) CreateSpaceWithOutbox(_ context.Context, _ store.CreateSpaceParams, _ store.CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error) {
	panic("CreateSpaceWithOutbox")
}
func (f *workerFakeStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	panic("ListPendingOutbox")
}
func (f *workerFakeStore) StampOutboxEnqueued(_ context.Context, _ []string) error {
	panic("StampOutboxEnqueued")
}
func (f *workerFakeStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	panic("UpdateOutboxPayload")
}
func (f *workerFakeStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	panic("InsertAuditEntry")
}
func (f *workerFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	panic("ListSpaces")
}

// M3 store methods — not exercised by project_agent_role tests; all panic.
func (f *workerFakeStore) CreateSpaceMember(_ context.Context, _ store.CreateSpaceMemberParams) (*domain.SpaceMember, error) {
	panic("CreateSpaceMember")
}
func (f *workerFakeStore) GetSpaceMemberBySpaceAndUser(_ context.Context, _, _ string) (*domain.SpaceMember, error) {
	panic("GetSpaceMemberBySpaceAndUser")
}
func (f *workerFakeStore) StampSpaceMemberInviteSent(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("StampSpaceMemberInviteSent")
}
func (f *workerFakeStore) RevokeSpaceMember(_ context.Context, _ string) (*domain.SpaceMember, error) {
	panic("RevokeSpaceMember")
}
func (f *workerFakeStore) ListSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListSpaceMembers")
}
func (f *workerFakeStore) ListCollaboratorChannels(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListCollaboratorChannels")
}
func (f *workerFakeStore) ListDirectory(_ context.Context, _ store.ListDirectoryParams) ([]*store.DirectoryEntry, error) {
	panic("ListDirectory")
}
func (f *workerFakeStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error {
	panic("UpdateSpaceReconciledAt")
}
func (f *workerFakeStore) ListActiveSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	panic("ListActiveSpaceMembers")
}
func (f *workerFakeStore) UpdateDiscordUserID(_ context.Context, _, _ string) error {
	panic("UpdateDiscordUserID")
}

// M4 store methods — not exercised by project_agent_role tests; all panic.
func (f *workerFakeStore) UpdateSpaceLifecycle(_ context.Context, _ store.UpdateSpaceLifecycleParams) (*domain.Space, error) {
	panic("UpdateSpaceLifecycle")
}
func (f *workerFakeStore) UpdateSpaceWelcomeMessageID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceWelcomeMessageID")
}
func (f *workerFakeStore) ListAuditEntries(_ context.Context, _ store.ListAuditEntriesParams) ([]*domain.AuditEntry, error) {
	panic("ListAuditEntries")
}
func (f *workerFakeStore) GetJobBySpaceIDAndKind(_ context.Context, _, _ string) (*domain.Job, error) {
	return nil, store.ErrNotFound
}

// ListActiveProvisionedSpaces satisfies store.Store (added in M5 for the scheduled sweep).
// Worker tests do not exercise the full-guild reconcile path.
func (f *workerFakeStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	panic("ListActiveProvisionedSpaces")
}

// M6 store methods — not exercised by project_agent_role tests.
func (f *workerFakeStore) SetMerchantInviteLink(_ context.Context, _ string, _ string) (*domain.Merchant, error) {
	panic("SetMerchantInviteLink")
}
func (f *workerFakeStore) UpdateSpaceMerchantRoleID(_ context.Context, _, _ string) (*domain.Space, error) {
	panic("UpdateSpaceMerchantRoleID")
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func makeTask(userID string, add bool) *asynq.Task {
	payload, _ := json.Marshal(queue.ProjectAgentRolePayload{UserID: userID, Add: add})
	return asynq.NewTask(queue.KindProjectAgentRole, payload)
}

func runProjectAgentRole(s *workerFakeStore, d *fakeDiscordClient, task *asynq.Task) error {
	mux := worker.NewServeMux(worker.Config{
		Store:          s,
		DiscordClient:  d,
		DiscordGuildID: "guild-123",
		AgentRoleID:    "role-agent-456",
	})
	return mux.ProcessTask(context.Background(), task)
}

// agentWithDiscordID returns an agent user with a Discord user id set.
func agentWithDiscordID(id, discordID string) *domain.User {
	return &domain.User{
		ID:            id,
		Type:          domain.UserTypeAgent,
		IsAdmin:       false,
		DiscordUserID: &discordID,
		IsActive:      true,
	}
}

// ─── AC-4: assign role ────────────────────────────────────────────────────────

// TestProjectAgentRole_Assign_CallsGuildMemberRoleAdd verifies GuildMemberRoleAdd is called (AC-4).
func TestProjectAgentRole_Assign_CallsGuildMemberRoleAdd(t *testing.T) {
	s := newWorkerFakeStore()
	discordUID := "discord-user-001"
	s.users["user-001"] = agentWithDiscordID("user-001", discordUID)

	d := &fakeDiscordClient{}
	task := makeTask("user-001", true)

	if err := runProjectAgentRole(s, d, task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.assignCalls) != 1 || d.assignCalls[0] != discordUID {
		t.Errorf("want AssignAgentRole called for %q, got assignCalls=%v", discordUID, d.assignCalls)
	}
	if len(d.removeCalls) != 0 {
		t.Errorf("want RemoveAgentRole not called, got removeCalls=%v", d.removeCalls)
	}
}

// TestProjectAgentRole_Assign_StampsProvisionedAt verifies provisioned_at is set after role assign.
func TestProjectAgentRole_Assign_StampsProvisionedAt(t *testing.T) {
	s := newWorkerFakeStore()
	s.users["user-001"] = agentWithDiscordID("user-001", "discord-user-001")
	d := &fakeDiscordClient{}

	if err := runProjectAgentRole(s, d, makeTask("user-001", true)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.provisionedAt) != 1 || s.provisionedAt[0] != "user-001" {
		t.Errorf("want provisioned_at stamped for user-001, got %v", s.provisionedAt)
	}
}

// ─── AC-4: remove role ────────────────────────────────────────────────────────

// TestProjectAgentRole_Remove_CallsGuildMemberRoleRemove verifies GuildMemberRoleRemove is called (AC-4).
func TestProjectAgentRole_Remove_CallsGuildMemberRoleRemove(t *testing.T) {
	s := newWorkerFakeStore()
	discordUID := "discord-user-002"
	s.users["user-002"] = agentWithDiscordID("user-002", discordUID)

	d := &fakeDiscordClient{}
	task := makeTask("user-002", false)

	if err := runProjectAgentRole(s, d, task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.removeCalls) != 1 || d.removeCalls[0] != discordUID {
		t.Errorf("want RemoveAgentRole called for %q, got removeCalls=%v", discordUID, d.removeCalls)
	}
	if len(d.assignCalls) != 0 {
		t.Errorf("want AssignAgentRole not called, got assignCalls=%v", d.assignCalls)
	}
}

// ─── AC-4: reconcile re-assertion ────────────────────────────────────────────

// TestProjectAgentRole_ReconcileReassert_CallsAssignOnSecondRun verifies that
// running the assign handler a second time (e.g. after manual role removal in Discord)
// calls AssignAgentRole again — the reconciler uses the same handler to re-assert (AC-4).
func TestProjectAgentRole_ReconcileReassert_CallsAssignOnSecondRun(t *testing.T) {
	s := newWorkerFakeStore()
	s.users["user-003"] = agentWithDiscordID("user-003", "discord-user-003")

	d := &fakeDiscordClient{}
	task := makeTask("user-003", true)

	// First run (initial projection).
	if err := runProjectAgentRole(s, d, task); err != nil {
		t.Fatalf("first run error: %v", err)
	}
	// Second run (reconcile re-assertion after manual removal).
	if err := runProjectAgentRole(s, d, task); err != nil {
		t.Fatalf("second run error: %v", err)
	}

	if len(d.assignCalls) != 2 {
		t.Errorf("want AssignAgentRole called twice (initial + reconcile), got %d calls", len(d.assignCalls))
	}
}

// ─── No discord_user_id → deferred (retryable) ───────────────────────────────

// TestProjectAgentRole_NoDiscordUserID_RetryableError verifies that an agent without
// a discord_user_id causes a retryable error (they haven't joined yet).
func TestProjectAgentRole_NoDiscordUserID_RetryableError(t *testing.T) {
	s := newWorkerFakeStore()
	s.users["user-004"] = &domain.User{
		ID:            "user-004",
		Type:          domain.UserTypeAgent,
		DiscordUserID: nil, // has not connected Discord yet
		IsActive:      true,
	}

	d := &fakeDiscordClient{}
	task := makeTask("user-004", true)

	err := runProjectAgentRole(s, d, task)
	if err == nil {
		t.Error("want retryable error when discord_user_id is nil, got nil")
	}
	// Should NOT be a SkipRetry error — we want it to retry later.
	if fmt.Sprintf("%v", err) == fmt.Sprintf("%v", asynq.SkipRetry) {
		t.Error("want retryable error, not SkipRetry (agent may connect later)")
	}
	// Discord should not have been called.
	if len(d.assignCalls) != 0 {
		t.Error("AssignAgentRole must not be called when discord_user_id is nil")
	}
}

// ─── Non-agent user → not projected (NFR-13) ─────────────────────────────────

// TestProjectAgentRole_NonAgentType_NoRoleProjected verifies that a collaborator
// is NOT given the Agent role even if somehow queued for projection (NFR-13, AC-3).
// AuthZ is a pure function of Postgres type, not Discord state.
func TestProjectAgentRole_NonAgentType_NoRoleProjected(t *testing.T) {
	s := newWorkerFakeStore()
	discordUID := "discord-collab-001"
	s.users["collab-001"] = &domain.User{
		ID:            "collab-001",
		Type:          domain.UserTypeCollaborator, // NOT an agent
		DiscordUserID: &discordUID,
		IsActive:      true,
	}

	d := &fakeDiscordClient{}
	task := makeTask("collab-001", true)

	// Should succeed (not an error — just silently skip).
	if err := runProjectAgentRole(s, d, task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The Agent role must NOT be assigned.
	if len(d.assignCalls) != 0 {
		t.Errorf("Agent role must NOT be assigned to a collaborator; got assignCalls=%v", d.assignCalls)
	}
}

// ─── User not found → skip silently ─────────────────────────────────────────

// TestProjectAgentRole_UserNotFound_SkipsSilently verifies that a missing user
// causes no error (the agent was deleted before the job ran).
func TestProjectAgentRole_UserNotFound_SkipsSilently(t *testing.T) {
	s := newWorkerFakeStore() // empty — no users
	d := &fakeDiscordClient{}
	task := makeTask("nonexistent-user", true)

	if err := runProjectAgentRole(s, d, task); err != nil {
		t.Errorf("want nil error for missing user, got: %v", err)
	}
	if len(d.assignCalls) != 0 {
		t.Errorf("AssignAgentRole must not be called for a missing user; got %v", d.assignCalls)
	}
}
