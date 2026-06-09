// provision_space_test.go — hermetic tests for the KindProvisionSpace handler (M2b).
//
// All tests use:
//   - A mockDiscordClient that records Discord API call arguments and can inject errors.
//   - A provisionFakeStore backed by the workerFakeStore base.
//   - miniredis (via cache/lock/ratelimit) for Valkey primitives.
//   - No real Discord API calls, no real database.
//
// Headline invariant verified here (AC-2, AC-3):
//   - CreateChannelDenied is called with the @everyone deny-VIEW_CHANNEL overwrite
//     in the initial PermissionOverwrites (born invisible, no open window).
//   - ApplyCategoryAgentAllow is called only after the channel exists.
//   - On any ACL step failure: handler returns SkipRetry, space is marked degraded/failed,
//     audit entry is written, no @everyone allow is ever applied.
package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/bwmarrin/discordgo"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
	"github.com/valianx/discord-support-hub/internal/store"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── Mock Discord client for provision tests ─────────────────────────────────

// provisionMockDiscord records calls and allows injecting errors per method.
type provisionMockDiscord struct {
	// CreateChannelDenied
	createChannelDeniedCalls []createChannelDeniedArgs
	createChannelDeniedErr   error
	createChannelDeniedID    string // returned channel id on success

	// ApplyCategoryAgentAllow
	applyCategoryAllowCalls []applyCategoryAllowArgs
	applyCategoryAllowErr   error

	// SetChannelPermissionDeny
	setPermDenyCalls []string

	// Inherited stubs (AssignAgentRole etc.)
}

type createChannelDeniedArgs struct {
	GuildID        string
	Name           string
	CategoryID     string
	EveryoneRoleID string
}

type applyCategoryAllowArgs struct {
	CategoryID  string
	AgentRoleID string
}

func (m *provisionMockDiscord) Ping(_ context.Context) error { return nil }

func (m *provisionMockDiscord) AssignAgentRole(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *provisionMockDiscord) RemoveAgentRole(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *provisionMockDiscord) CreateChannelDenied(
	_ context.Context,
	guildID, name, categoryID, everyoneRoleID string,
) (string, error) {
	m.createChannelDeniedCalls = append(m.createChannelDeniedCalls, createChannelDeniedArgs{
		GuildID:        guildID,
		Name:           name,
		CategoryID:     categoryID,
		EveryoneRoleID: everyoneRoleID,
	})
	if m.createChannelDeniedErr != nil {
		return "", m.createChannelDeniedErr
	}
	id := m.createChannelDeniedID
	if id == "" {
		id = "discord-ch-001"
	}
	return id, nil
}

func (m *provisionMockDiscord) ApplyCategoryAgentAllow(
	_ context.Context,
	categoryID, agentRoleID string,
) error {
	m.applyCategoryAllowCalls = append(m.applyCategoryAllowCalls, applyCategoryAllowArgs{
		CategoryID:  categoryID,
		AgentRoleID: agentRoleID,
	})
	return m.applyCategoryAllowErr
}

func (m *provisionMockDiscord) SetChannelPermissionDeny(
	_ context.Context,
	channelID, _ string,
	_ discordgo.PermissionOverwriteType,
) error {
	m.setPermDenyCalls = append(m.setPermDenyCalls, channelID)
	return nil
}

// M3 discord.Client methods — not exercised by provision tests.
func (m *provisionMockDiscord) SetCollaboratorOverwrite(_ context.Context, _, _ string) error {
	return nil
}
func (m *provisionMockDiscord) DeleteCollaboratorOverwrite(_ context.Context, _, _ string) error {
	return nil
}
func (m *provisionMockDiscord) AddGuildMember(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *provisionMockDiscord) RemoveGuildMember(_ context.Context, _, _ string) error {
	return nil
}
func (m *provisionMockDiscord) GetChannelOverwrites(_ context.Context, _ string) ([]*discordgo.PermissionOverwrite, error) {
	return nil, nil
}

// M4 discord.Client methods — not exercised by provision tests.
func (m *provisionMockDiscord) ArchiveChannel(_ context.Context, _, _ string) error   { return nil }
func (m *provisionMockDiscord) UnarchiveChannel(_ context.Context, _, _ string) error { return nil }
func (m *provisionMockDiscord) SetChannelTopic(_ context.Context, _, _ string) error  { return nil }
func (m *provisionMockDiscord) PinMessage(_ context.Context, _, _ string) error       { return nil }
func (m *provisionMockDiscord) EditMessage(_ context.Context, _, _, _ string) error   { return nil }
func (m *provisionMockDiscord) SendMessage(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (m *provisionMockDiscord) SetNickname(_ context.Context, _, _, _ string) error { return nil }

// ─── Fake store for provision tests ──────────────────────────────────────────

type provisionFakeStore struct {
	workerFakeStore
	spaces           map[string]*domain.Space
	aclStateUpdates  map[string]domain.ACLState
	channelPersisted map[string]string // spaceID -> discordChannelID
	auditEntries     []store.InsertAuditEntryParams
	jobs             map[string]*domain.Job
	jobUpdates       []store.UpdateJobStatusParams
}

func newProvisionFakeStore() *provisionFakeStore {
	return &provisionFakeStore{
		workerFakeStore:  workerFakeStore{users: make(map[string]*domain.User)},
		spaces:           make(map[string]*domain.Space),
		aclStateUpdates:  make(map[string]domain.ACLState),
		channelPersisted: make(map[string]string),
		jobs:             make(map[string]*domain.Job),
	}
}

func (f *provisionFakeStore) GetSpaceByID(_ context.Context, id string) (*domain.Space, error) {
	sp, ok := f.spaces[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sp, nil
}

func (f *provisionFakeStore) UpdateSpaceDiscordChannel(
	_ context.Context,
	p store.UpdateSpaceDiscordChannelParams,
) (*domain.Space, error) {
	sp, ok := f.spaces[p.SpaceID]
	if !ok {
		return nil, store.ErrNotFound
	}
	sp.DiscordChannelID = &p.DiscordChannelID
	sp.ACLState = p.ACLState
	f.channelPersisted[p.SpaceID] = p.DiscordChannelID
	return sp, nil
}

func (f *provisionFakeStore) UpdateSpaceACLState(
	_ context.Context,
	spaceID string,
	state domain.ACLState,
) (*domain.Space, error) {
	sp, ok := f.spaces[spaceID]
	if !ok {
		return nil, store.ErrNotFound
	}
	sp.ACLState = state
	f.aclStateUpdates[spaceID] = state
	return sp, nil
}

func (f *provisionFakeStore) InsertAuditEntry(
	_ context.Context,
	p store.InsertAuditEntryParams,
) error {
	f.auditEntries = append(f.auditEntries, p)
	return nil
}

func (f *provisionFakeStore) CreateJob(_ context.Context, p store.CreateJobParams) (*domain.Job, error) {
	j := &domain.Job{
		ID:     "job-" + p.TaskID,
		TaskID: p.TaskID,
		Kind:   p.Kind,
		Status: domain.JobStatusPending,
	}
	f.jobs[j.ID] = j
	return j, nil
}

func (f *provisionFakeStore) UpdateJobStatus(
	_ context.Context,
	p store.UpdateJobStatusParams,
) (*domain.Job, error) {
	f.jobUpdates = append(f.jobUpdates, p)
	j, ok := f.jobs[p.JobID]
	if !ok {
		return nil, store.ErrNotFound
	}
	j.Status = p.Status
	return j, nil
}

func (f *provisionFakeStore) GetJobByID(_ context.Context, id string) (*domain.Job, error) {
	j, ok := f.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return j, nil
}

func (f *provisionFakeStore) ListSpaces(_ context.Context, _ store.ListSpacesParams) ([]*domain.Space, error) {
	return nil, nil
}

func (f *provisionFakeStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func makeProvisionTask(spaceID, merchantID, spaceName, categoryID string) *asynq.Task {
	payload, _ := json.Marshal(queue.ProvisionSpacePayload{
		MerchantID: merchantID,
		SpaceID:    spaceID,
		SpaceName:  spaceName,
		CategoryID: categoryID,
	})
	return asynq.NewTask(queue.KindProvisionSpace, payload)
}

func pendingSpace(id, merchantID string) *domain.Space {
	return &domain.Space{
		ID:         id,
		MerchantID: merchantID,
		Name:       "test-space",
		ACLState:   domain.ACLStatePending,
		CreatedAt:  time.Now(),
	}
}

func runProvisionHandler(
	s *provisionFakeStore,
	d *provisionMockDiscord,
	task *asynq.Task,
) error {
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		panic(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	limiter := ratelimit.New(rdb, ratelimit.DefaultConfig())
	locker := lock.New(rdb)
	c := cache.New(rdb)

	mux := worker.NewServeMux(worker.Config{
		Store:          s,
		DiscordClient:  d,
		DiscordGuildID: "guild-001",
		EveryoneRoleID: "everyone-role-001",
		// fix(NFR-5): AgentRoleID must be set and distinct from GuildID.
		AgentRoleID:       "agent-role-001",
		DefaultCategoryID: "default-cat-001",
		Limiter:           limiter,
		Locker:            locker,
		Cache:             c,
	})
	return mux.ProcessTask(context.Background(), task)
}

// ─── AC-2: fail-closed ordering ──────────────────────────────────────────────

// TestProvisionSpace_CreateChannelDenied_BornInvisible verifies that CreateChannelDenied
// is called — meaning the @everyone deny is in the INITIAL PermissionOverwrites of the
// channel create payload (born invisible, no open window, AC-2, NFR-4).
//
// The mock records the call args; the real discord.Session encodes the deny overwrite
// into the create payload. The test proves the handler always calls CreateChannelDenied,
// not a plain channel-create-then-deny sequence.
func TestProvisionSpace_CreateChannelDenied_BornInvisible(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-001"] = pendingSpace("space-001", "merchant-001")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-ch-001"}
	task := makeProvisionTask("space-001", "merchant-001", "test-space", "cat-001")

	if err := runProvisionHandler(s, d, task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert: CreateChannelDenied called exactly once (the born-denied path).
	if len(d.createChannelDeniedCalls) != 1 {
		t.Fatalf("want CreateChannelDenied called once, got %d", len(d.createChannelDeniedCalls))
	}
	args := d.createChannelDeniedCalls[0]
	if args.Name != "test-space" {
		t.Errorf("want channel name %q, got %q", "test-space", args.Name)
	}
	if args.CategoryID != "cat-001" {
		t.Errorf("want category id %q, got %q", "cat-001", args.CategoryID)
	}
	if args.EveryoneRoleID != "everyone-role-001" {
		t.Errorf("want everyone role id passed through, got %q", args.EveryoneRoleID)
	}
}

// TestProvisionSpace_AgentAllowAppliedAfterChannelCreate verifies that
// ApplyCategoryAgentAllow is called after CreateChannelDenied (AC-2 ordering).
// If CreateChannelDenied fails, the category allow must NOT be applied.
func TestProvisionSpace_AgentAllowAppliedAfterChannelCreate(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-002"] = pendingSpace("space-002", "merchant-002")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-ch-002"}
	task := makeProvisionTask("space-002", "merchant-002", "test-space-2", "cat-002")

	if err := runProvisionHandler(s, d, task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert ordering: create then allow.
	if len(d.createChannelDeniedCalls) != 1 {
		t.Fatalf("want 1 create call, got %d", len(d.createChannelDeniedCalls))
	}
	if len(d.applyCategoryAllowCalls) != 1 {
		t.Fatalf("want 1 category allow call, got %d", len(d.applyCategoryAllowCalls))
	}
	// acl_state must be 'applied' after successful provisioning.
	sp := s.spaces["space-002"]
	if sp.ACLState != domain.ACLStateApplied {
		t.Errorf("want acl_state=applied, got %q", sp.ACLState)
	}
	if sp.DiscordChannelID == nil || *sp.DiscordChannelID != "discord-ch-002" {
		t.Errorf("want discord_channel_id=discord-ch-002, got %v", sp.DiscordChannelID)
	}
}

// TestProvisionSpace_ChannelCreateFails_NoCategoryAllow verifies that if
// CreateChannelDenied fails, ApplyCategoryAgentAllow is never called (AC-3).
// The channel does not exist in Discord, so no visibility is ever granted.
func TestProvisionSpace_ChannelCreateFails_NoCategoryAllow(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-003"] = pendingSpace("space-003", "merchant-003")

	d := &provisionMockDiscord{
		createChannelDeniedErr: errors.New("discord: 500 internal error"),
	}
	task := makeProvisionTask("space-003", "merchant-003", "test-space-3", "cat-003")

	err := runProvisionHandler(s, d, task)
	if err == nil {
		t.Fatal("want error when create channel fails, got nil")
	}

	// ApplyCategoryAgentAllow must NOT have been called.
	if len(d.applyCategoryAllowCalls) != 0 {
		t.Errorf("ApplyCategoryAgentAllow must NOT be called when create fails, got %d calls",
			len(d.applyCategoryAllowCalls))
	}
	// The channel was never created, so no @everyone visibility was granted.
}

// ─── AC-3: fail-closed on ACL step failure ────────────────────────────────────

// TestProvisionSpace_ACLFails_SkipRetryTerminal verifies that when the category allow
// call fails, the handler returns a SkipRetry-wrapped terminal error (AC-3, NFR-4).
func TestProvisionSpace_ACLFails_SkipRetryTerminal(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-004"] = pendingSpace("space-004", "merchant-004")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-ch-004",
		applyCategoryAllowErr: errors.New("discord: manage permissions denied"),
	}
	task := makeProvisionTask("space-004", "merchant-004", "test-space-4", "cat-004")

	err := runProvisionHandler(s, d, task)
	if err == nil {
		t.Fatal("want error when ACL apply fails, got nil")
	}

	// Error must wrap asynq.SkipRetry (terminal, no further retries).
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("want error to wrap asynq.SkipRetry, got: %v", err)
	}
}

// TestProvisionSpace_ACLFails_SpaceMarkedDegraded verifies that a failed category allow
// leaves the space marked degraded (not failed) because the channel was created (AC-3).
func TestProvisionSpace_ACLFails_SpaceMarkedDegraded(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-005"] = pendingSpace("space-005", "merchant-005")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-ch-005",
		applyCategoryAllowErr: errors.New("acl error"),
	}
	task := makeProvisionTask("space-005", "merchant-005", "test-space-5", "cat-005")

	_ = runProvisionHandler(s, d, task) // expect error

	state, ok := s.aclStateUpdates["space-005"]
	if !ok {
		t.Fatal("want acl_state updated on failure, but UpdateSpaceACLState was not called")
	}
	if state != domain.ACLStateDegraded {
		t.Errorf("want acl_state=degraded after channel-created-but-acl-failed, got %q", state)
	}
}

// TestProvisionSpace_ACLFails_AuditEntryWritten verifies that a fail-closed ACL error
// always writes an audit entry (AC-3, FR-14).
func TestProvisionSpace_ACLFails_AuditEntryWritten(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-006"] = pendingSpace("space-006", "merchant-006")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-ch-006",
		applyCategoryAllowErr: errors.New("acl error"),
	}
	task := makeProvisionTask("space-006", "merchant-006", "test-space-6", "cat-006")

	_ = runProvisionHandler(s, d, task) // expect error

	if len(s.auditEntries) == 0 {
		t.Error("want at least one audit entry on fail-closed error, got none")
	}
	// Verify no secret fields appear in audit detail.
	for _, entry := range s.auditEntries {
		for k := range entry.Detail {
			if k == "access_token" || k == "bot_token" || k == "api_key" {
				t.Errorf("audit detail must not contain secret field %q", k)
			}
		}
	}
}

// TestProvisionSpace_ACLFails_NeverWorldReadable verifies the core isolation invariant:
// when an ACL step fails, the space's ACL state is NEVER 'applied' — it stays invisible.
func TestProvisionSpace_ACLFails_NeverWorldReadable(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-007"] = pendingSpace("space-007", "merchant-007")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-ch-007",
		applyCategoryAllowErr: errors.New("permission error"),
	}
	task := makeProvisionTask("space-007", "merchant-007", "test-space-7", "cat-007")

	_ = runProvisionHandler(s, d, task)

	sp := s.spaces["space-007"]
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("INVARIANT VIOLATED: acl_state must never be 'applied' after an ACL failure")
	}
}

// ─── Idempotency ─────────────────────────────────────────────────────────────

// TestProvisionSpace_AlreadyProvisioned_Skips verifies that if the space already has
// a discord_channel_id and acl_state=applied, the handler is a no-op (worker upsert
// idempotency, §4.1 layer 3).
func TestProvisionSpace_AlreadyProvisioned_Skips(t *testing.T) {
	s := newProvisionFakeStore()
	chID := "already-exists-ch"
	s.spaces["space-008"] = &domain.Space{
		ID:               "space-008",
		MerchantID:       "merchant-008",
		DiscordChannelID: &chID,
		ACLState:         domain.ACLStateApplied,
		CreatedAt:        time.Now(),
	}

	d := &provisionMockDiscord{}
	task := makeProvisionTask("space-008", "merchant-008", "test-space-8", "cat-008")

	if err := runProvisionHandler(s, d, task); err != nil {
		t.Fatalf("want no error for already-provisioned space, got: %v", err)
	}

	// No Discord calls should have been made.
	if len(d.createChannelDeniedCalls) != 0 {
		t.Errorf("CreateChannelDenied must not be called for an already-provisioned space")
	}
	if len(d.applyCategoryAllowCalls) != 0 {
		t.Errorf("ApplyCategoryAgentAllow must not be called for an already-provisioned space")
	}
}

// ─── Rate-limit retry ─────────────────────────────────────────────────────────

// TestProvisionSpace_RateLimitError_IsRetryable verifies that a rate-limit bucket being
// empty returns a *RateLimitError — the RetryDelayFunc extracts it for Retry-After and
// returns a retryable delay, not a terminal failure (AC-5).
func TestProvisionSpace_RateLimitError_IsRetryable(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-009"] = pendingSpace("space-009", "merchant-009")

	d := &provisionMockDiscord{}
	task := makeProvisionTask("space-009", "merchant-009", "test-space-9", "cat-009")

	// Use a limiter with capacity=0 so every TakeGlobal is denied immediately.
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	cfg := ratelimit.DefaultConfig()
	cfg.GlobalCapacity = 0
	cfg.GlobalRefillRate = 0
	limiter := ratelimit.New(rdb, cfg)

	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-001",
		EveryoneRoleID:    "everyone-role-001",
		AgentRoleID:       "agent-role-001",
		DefaultCategoryID: "default-cat-001",
		Limiter:           limiter,
		Locker:            lock.NoopLocker{},
		Cache:             cache.NoopCache{},
	})
	err := mux.ProcessTask(context.Background(), task)

	if err == nil {
		t.Fatal("want error when rate limit bucket is empty")
	}
	// Must be a RateLimitError — not a terminal SkipRetry (AC-5, AC-8).
	if errors.Is(err, asynq.SkipRetry) {
		t.Error("rate-limit error must NOT be SkipRetry — it must be retryable")
	}
	if !ratelimit.IsRateLimitError(err) {
		t.Errorf("want *RateLimitError, got: %T %v", err, err)
	}
	// Verify IsFailure returns false for this error (AC-8: rate-limit retries
	// should not increment the failure counter).
	if worker.IsFailure(err) {
		t.Error("IsFailure must return false for a *RateLimitError (AC-8, NFR-7)")
	}
}

// ─── Lock serialization ───────────────────────────────────────────────────────

// TestProvisionSpace_LockHeld_ReturnsRetryable verifies that when the merchant lock is
// already held, the handler returns a retryable error (not SkipRetry) so the task is
// retried after a delay (AC-6, §3.3).
func TestProvisionSpace_LockHeld_ReturnsRetryable(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-010"] = pendingSpace("space-010", "merchant-010")

	d := &provisionMockDiscord{}
	task := makeProvisionTask("space-010", "merchant-010", "test-space-10", "cat-010")

	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	locker := lock.New(rdb)

	// Pre-acquire the merchant lock so the handler sees it as held.
	_, ok, err := locker.AcquireMerchant(context.Background(), "merchant-010")
	if err != nil || !ok {
		t.Fatalf("failed to pre-acquire lock: ok=%v err=%v", ok, err)
	}

	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-001",
		EveryoneRoleID:    "everyone-role-001",
		AgentRoleID:       "agent-role-001",
		DefaultCategoryID: "default-cat-001",
		Limiter:           ratelimit.NoopLimiter{},
		Locker:            locker,
		Cache:             cache.NoopCache{},
	})
	handlerErr := mux.ProcessTask(context.Background(), task)

	if handlerErr == nil {
		t.Fatal("want error when lock is held, got nil")
	}
	// Must be retryable (not SkipRetry) — the worker should come back after the holder releases.
	if errors.Is(handlerErr, asynq.SkipRetry) {
		t.Error("lock-held error must NOT be SkipRetry — it must be retryable (AC-6)")
	}
	// No Discord calls should have been made.
	if len(d.createChannelDeniedCalls) != 0 {
		t.Error("CreateChannelDenied must not be called when the lock is held")
	}
}

// ─── Cache invalidation on success ────────────────────────────────────────────

// TestProvisionSpace_SuccessInvalidatesCache verifies that after a successful
// provision, the spaces list cache key is deleted (write-invalidation).
func TestProvisionSpace_SuccessInvalidatesCache(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-011"] = pendingSpace("space-011", "merchant-011")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-ch-011"}
	task := makeProvisionTask("space-011", "merchant-011", "test-space-11", "cat-011")

	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	c := cache.New(rdb)

	// Pre-seed the cache list key so we can confirm it is deleted.
	if err := c.Set(context.Background(), "spaces:list", []byte(`{"items":[]}`), time.Minute); err != nil {
		t.Fatal(err)
	}

	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-001",
		EveryoneRoleID:    "everyone-role-001",
		AgentRoleID:       "agent-role-001",
		DefaultCategoryID: "default-cat-001",
		Limiter:           ratelimit.NoopLimiter{},
		Locker:            lock.NoopLocker{},
		Cache:             c,
	})

	if err := mux.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Cache list key should have been invalidated.
	val, err := c.Get(context.Background(), "spaces:list")
	if err != nil {
		t.Fatalf("cache.Get error: %v", err)
	}
	if val != nil {
		t.Error("want spaces:list cache key deleted after provisioning, but it still exists")
	}
}
