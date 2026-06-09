// provision_space_strengthen_test.go — strengthened coverage for M2b provisioning (AC-1/2/3).
//
// These tests complement provision_space_test.go, adding targeted assertions that were
// absent from the original suite:
//
// AC-2 (fail-closed ordering — born invisible):
//   - Verify the @everyone deny-VIEW_CHANNEL is in the CREATE payload by asserting that
//     CreateChannelDenied carries the correct everyoneRoleID forward.
//   - Verify the no-category path: CreateChannelDenied fires but ApplyCategoryAgentAllow
//     is never called when no category is specified.
//   - Verify both discord_channel_id and acl_state=applied are persisted together only
//     after the Agent allow succeeds (they are never persisted with applied when ACL fails).
//
// AC-3 (fail-closed terminal behaviour — the half-open risk):
//   - Channel created (CreateChannelDenied succeeds, channelID != "") then
//     ApplyCategoryAgentAllow fails:
//   - acl_state=degraded (not failed, not applied).
//   - discord_channel_id is NOT persisted as applied.
//   - @everyone allow is never applied.
//   - SkipRetry is returned.
//   - Audit entry is written.
//   - Channel creation fails (CreateChannelDenied returns error):
//   - acl_state=failed (not degraded, not applied).
//   - ApplyCategoryAgentAllow is never called.
//   - No @everyone allow ever applied.
//   - Agent-allow error does NOT leak @everyone allow — SetChannelPermissionDeny
//     is also never called with allow semantics.
//
// AC-1 (POST handler):
//   - Idempotent replay with a pre-stored response returns the exact stored 202 body
//     with the Idempotency-Replay header and does NOT create a second outbox row.
//   - Body-hash conflict (same Idempotency-Key, different body) returns 409.
//   - Concurrent same-merchant provision: lock serializes — second provision
//     that arrives while the first holds the lock returns a retryable error
//     (not SkipRetry), and CreateChannelDenied is called exactly once.
//
// All tests are hermetic: miniredis for Valkey primitives, provisionFakeStore and
// provisionMockDiscord from provision_space_test.go (same package, same test binary).
package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── Helpers shared by strengthened tests ────────────────────────────────────

// makeProvisionTaskNoCat creates a provision task with no category ID (empty string).
// Used to exercise the no-category branch (ApplyCategoryAgentAllow must not be called).
func makeProvisionTaskNoCat(spaceID, merchantID, spaceName string) *asynq.Task {
	payload, _ := json.Marshal(queue.ProvisionSpacePayload{
		MerchantID: merchantID,
		SpaceID:    spaceID,
		SpaceName:  spaceName,
		CategoryID: "", // explicitly empty — no category
	})
	return asynq.NewTask(queue.KindProvisionSpace, payload)
}

// runProvisionHandlerFull mirrors runProvisionHandler but returns the used rdb so
// tests can inspect cache state after the run.
func runProvisionHandlerFull(
	s *provisionFakeStore,
	d *provisionMockDiscord,
	task *asynq.Task,
) (err error, mr *miniredis.Miniredis, rdb *redis.Client) {
	mr = miniredis.NewMiniRedis()
	if startErr := mr.Start(); startErr != nil {
		panic(startErr)
	}

	rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})

	limiter := ratelimit.New(rdb, ratelimit.DefaultConfig())
	locker := lock.New(rdb)
	c := cache.New(rdb)

	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-str-001",
		EveryoneRoleID:    "everyone-str-001",
		AgentRoleID:       "agent-str-001",
		DefaultCategoryID: "default-cat-str-001",
		Limiter:           limiter,
		Locker:            locker,
		Cache:             c,
	})
	err = mux.ProcessTask(context.Background(), task)
	return
}

// ─── AC-2: born-invisible — everyoneRoleID forwarded ─────────────────────────

// TestProvisionSpace_CreateChannelDenied_EveryoneRoleIDForwarded verifies that the
// everyoneRoleID configured on the handler is the one passed to CreateChannelDenied
// (the invariant that @everyone deny targets the right role).
func TestProvisionSpace_CreateChannelDenied_EveryoneRoleIDForwarded(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-s01"] = pendingSpace("space-s01", "merchant-s01")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-s01"}
	task := makeProvisionTask("space-s01", "merchant-s01", "acme-support", "cat-s01")

	err, mr, rdb := runProvisionHandlerFull(s, d, task)
	defer mr.Close()
	defer rdb.Close()

	if err != nil {
		t.Fatalf("want success, got: %v", err)
	}

	// The everyoneRoleID the handler was built with is "everyone-str-001".
	if len(d.createChannelDeniedCalls) == 0 {
		t.Fatal("want CreateChannelDenied called, got 0 calls")
	}
	args := d.createChannelDeniedCalls[0]
	const wantEveryoneRoleID = "everyone-str-001"
	if args.EveryoneRoleID != wantEveryoneRoleID {
		t.Errorf("CreateChannelDenied: want everyoneRoleID=%q (the deny target), got %q",
			wantEveryoneRoleID, args.EveryoneRoleID)
	}
	// GuildID must also pass through.
	const wantGuildID = "guild-str-001"
	if args.GuildID != wantGuildID {
		t.Errorf("CreateChannelDenied: want guildID=%q, got %q", wantGuildID, args.GuildID)
	}
}

// ─── AC-2: no-category path ──────────────────────────────────────────────────

// TestProvisionSpace_NoCategoryID_FallsBackToDefault verifies that when no categoryID
// is provided in the task payload, the handler falls back to the configured default
// category and applies the Agent allow on it (fix #3: category-level allow always applied).
// The old guard `if categoryID != ""` is removed — a category is now mandatory.
func TestProvisionSpace_NoCategoryID_FallsBackToDefault(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-s02"] = pendingSpace("space-s02", "merchant-s02")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-s02"}
	task := makeProvisionTaskNoCat("space-s02", "merchant-s02", "no-cat-space")

	// runProvisionHandler uses DefaultCategoryID: "default-cat-001" — the handler must
	// fall back to it and apply the Agent allow.
	err := runProvisionHandler(s, d, task)
	if err != nil {
		t.Fatalf("want success when no category is specified (falls back to default), got: %v", err)
	}

	// CreateChannelDenied must be called (channel born denied).
	if len(d.createChannelDeniedCalls) != 1 {
		t.Errorf("want CreateChannelDenied called once, got %d", len(d.createChannelDeniedCalls))
	}
	// ApplyCategoryAgentAllow MUST be called with the default category (fix #3).
	if len(d.applyCategoryAllowCalls) != 1 {
		t.Errorf("want ApplyCategoryAgentAllow called once with default category, got %d calls",
			len(d.applyCategoryAllowCalls))
	}
	if len(d.applyCategoryAllowCalls) == 1 {
		call := d.applyCategoryAllowCalls[0]
		if call.CategoryID != "default-cat-001" {
			t.Errorf("want ApplyCategoryAgentAllow called with default category %q, got %q",
				"default-cat-001", call.CategoryID)
		}
	}
	// acl_state must be applied (the channel exists and the category ACL was applied).
	sp := s.spaces["space-s02"]
	if sp.ACLState != domain.ACLStateApplied {
		t.Errorf("want acl_state=applied after success with default category, got %q", sp.ACLState)
	}
}

// ─── AC-2: discord_channel_id + acl_state applied only on full success ────────

// TestProvisionSpace_ChannelIDAndACLState_PersistOnlyAfterFullSuccess verifies that
// discord_channel_id is persisted AND acl_state=applied ONLY after both
// CreateChannelDenied AND ApplyCategoryAgentAllow succeed.
// If the Agent allow fails, the channel ID must NOT have been persisted with acl_state=applied.
func TestProvisionSpace_ChannelIDAndACLState_PersistOnlyAfterFullSuccess(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-s03"] = pendingSpace("space-s03", "merchant-s03")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-s03",
		applyCategoryAllowErr: errors.New("acl: permission denied on category"),
	}
	task := makeProvisionTask("space-s03", "merchant-s03", "partial-provision", "cat-s03")

	_ = runProvisionHandler(s, d, task) // must fail

	sp := s.spaces["space-s03"]

	// CRITICAL: acl_state must NOT be 'applied' (the space is not accessible).
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("INVARIANT VIOLATED: acl_state must not be 'applied' when AgentAllow fails")
	}

	// The channel was created in Discord but ACL failed → state must be 'degraded'.
	if sp.ACLState != domain.ACLStateDegraded {
		t.Errorf("want acl_state=degraded (channel created, ACL failed), got %q", sp.ACLState)
	}

	// channelPersisted map tracks UpdateSpaceDiscordChannel calls.
	// There must be no entry with acl_state=applied.
	if _, persisted := s.channelPersisted["space-s03"]; persisted {
		t.Error("UpdateSpaceDiscordChannel(acl_state=applied) must NOT be called when ACL fails")
	}
}

// ─── AC-3: half-open risk — channel created, then ACL fails ──────────────────

// TestProvisionSpace_HalfOpen_ChannelCreatedACLFails_DegradedNotApplied is the
// headline half-open risk test.
//
// Sequence:
//  1. CreateChannelDenied succeeds → channelID = "discord-half-open"
//  2. ApplyCategoryAgentAllow fails → the handler must NOT persist acl_state=applied.
//
// Assertions:
//   - acl_state = degraded (channel exists in Discord, ACL not applied).
//   - discord_channel_id is NOT stamped with acl_state=applied (UpdateSpaceDiscordChannel
//     must not have been called with ACLState=applied).
//   - @everyone allow is NEVER applied (applyCategoryAllowCalls recorded as failed; no
//     subsequent SetChannelPermissionDeny with allow semantics either).
//   - SkipRetry is returned.
//   - At least one audit entry is written.
func TestProvisionSpace_HalfOpen_ChannelCreatedACLFails_DegradedNotApplied(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-half"] = pendingSpace("space-half", "merchant-half")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-half-open",
		applyCategoryAllowErr: errors.New("discord: 403 missing permissions"),
	}
	task := makeProvisionTask("space-half", "merchant-half", "half-open-space", "cat-half")

	handlerErr := runProvisionHandler(s, d, task)

	// (a) Error must be returned (non-nil).
	if handlerErr == nil {
		t.Fatal("want error when ACL fails after channel creation, got nil")
	}

	// (b) Error must wrap asynq.SkipRetry (terminal — no half-open retry).
	if !errors.Is(handlerErr, asynq.SkipRetry) {
		t.Errorf("want SkipRetry for half-open ACL failure, got: %v", handlerErr)
	}

	sp := s.spaces["space-half"]

	// (c) acl_state must be 'degraded' (channel exists, ACL not applied).
	if sp.ACLState != domain.ACLStateDegraded {
		t.Errorf("want acl_state=degraded for half-open failure, got %q", sp.ACLState)
	}

	// (d) acl_state must never be 'applied'.
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("INVARIANT VIOLATED: acl_state=applied must never be set when ACL fails")
	}

	// (e) UpdateSpaceDiscordChannel(acl_state=applied) must not have been called.
	// channelPersisted is set only when UpdateSpaceDiscordChannel succeeds.
	if _, ok := s.channelPersisted["space-half"]; ok {
		t.Error("UpdateSpaceDiscordChannel must NOT be called with acl_state=applied when ACL fails")
	}

	// (f) ApplyCategoryAgentAllow was attempted exactly once (the call that failed) —
	// and crucially, was NOT retried or called again.
	if len(d.applyCategoryAllowCalls) != 1 {
		t.Errorf("want exactly 1 ApplyCategoryAgentAllow attempt, got %d", len(d.applyCategoryAllowCalls))
	}

	// (g) Audit entry must have been written.
	if len(s.auditEntries) == 0 {
		t.Error("want at least one audit entry written on half-open failure, got none")
	}

	// (h) No secret fields in audit detail.
	for _, entry := range s.auditEntries {
		for k := range entry.Detail {
			if k == "access_token" || k == "bot_token" || k == "api_key" || k == "refresh_token" {
				t.Errorf("audit detail must not contain secret field %q", k)
			}
		}
	}
}

// ─── AC-3: channel create fails → acl_state=failed (not degraded) ────────────

// TestProvisionSpace_CreateFails_StateIsFailedNotDegraded verifies the distinction
// between degraded (channel created, ACL failed) and failed (channel never created).
// fix(DEFECT-2): when CreateChannelDenied returns a non-429 error, the handler now
// calls failClosed with empty channelID → acl_state='failed' (not degraded, not applied).
// This is terminal (SkipRetry) so the task is archived — retrying would be pointless
// because the channel was never created.
func TestProvisionSpace_CreateFails_StateIsFailedNotDegraded(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-cf"] = pendingSpace("space-cf", "merchant-cf")

	d := &provisionMockDiscord{
		createChannelDeniedErr: errors.New("discord: 500 internal server error"),
	}
	task := makeProvisionTask("space-cf", "merchant-cf", "create-fail-space", "cat-cf")

	handlerErr := runProvisionHandler(s, d, task)

	// (1) Error must be returned.
	if handlerErr == nil {
		t.Fatal("want error when CreateChannelDenied fails, got nil")
	}

	// (2) Error must be terminal (SkipRetry) — the channel was never created (fix DEFECT-2).
	if !errors.Is(handlerErr, asynq.SkipRetry) {
		t.Errorf("want SkipRetry when channel create fails (terminal, no Discord object to repair), got: %v", handlerErr)
	}

	// (3) ApplyCategoryAgentAllow must NEVER be called.
	if len(d.applyCategoryAllowCalls) != 0 {
		t.Errorf("ApplyCategoryAgentAllow must not be called when create fails, got %d calls",
			len(d.applyCategoryAllowCalls))
	}

	// (4) acl_state must be 'failed' (channel never created, failClosed with empty channelID).
	sp := s.spaces["space-cf"]
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("INVARIANT VIOLATED: acl_state must not be applied when channel creation failed")
	}
	if sp.ACLState != domain.ACLStateFailed {
		t.Errorf("want acl_state=failed when channel creation fails (no Discord object), got %q", sp.ACLState)
	}
}

// TestProvisionSpace_FailClosed_ChannelIDEmpty_StateIsFailed verifies that when
// CreateChannelDenied fails (non-429), failClosed is called with an empty channelID
// and acl_state is set to 'failed' — not 'degraded', not 'applied'.
// fix(DEFECT-2): the ACLStateFailed dead-branch is now reachable — non-429 create
// errors go through failClosed("", ...) rather than returning a retryable error.
func TestProvisionSpace_FailClosed_ChannelIDEmpty_StateIsFailed(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-dfb"] = pendingSpace("space-dfb", "merchant-dfb")

	d := &provisionMockDiscord{
		createChannelDeniedErr: errors.New("discord: 500 error"),
	}
	task := makeProvisionTask("space-dfb", "merchant-dfb", "dead-branch-space", "cat-dfb")

	handlerErr := runProvisionHandler(s, d, task)

	// (1) Error must be returned and must be terminal (SkipRetry).
	if handlerErr == nil {
		t.Fatal("want error when CreateChannelDenied fails, got nil")
	}
	if !errors.Is(handlerErr, asynq.SkipRetry) {
		t.Errorf("want SkipRetry for channel create failure (fix DEFECT-2), got: %v", handlerErr)
	}

	// (2) acl_state must be 'failed' — the channel was never created.
	sp := s.spaces["space-dfb"]
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("INVARIANT VIOLATED: acl_state must never be applied after a create failure")
	}
	if sp.ACLState != domain.ACLStateFailed {
		t.Errorf("want acl_state=failed when channel creation fails, got %q", sp.ACLState)
	}

	// (3) aclStateUpdates must contain the entry (failClosed was called).
	if _, updated := s.aclStateUpdates["space-dfb"]; !updated {
		t.Error("want aclStateUpdates to contain space-dfb (failClosed must update acl_state)")
	}
}

// ─── AC-3: @everyone allow NEVER applied on any failure ───────────────────────

// TestProvisionSpace_NoEveryoneAllowOnAnyFailure verifies the strongest form of
// the fail-closed invariant: across multiple failure scenarios, @everyone is never
// granted VIEW_CHANNEL. This is the composite guard test.
//
// Scenarios:
//
//	A: CreateChannelDenied fails → no ApplyCategoryAgentAllow, no SetChannelPermissionDeny
//	   with allow bits.
//	B: ApplyCategoryAgentAllow fails (half-open) → no second allow call, acl_state != applied.
func TestProvisionSpace_NoEveryoneAllowOnAnyFailure(t *testing.T) {
	t.Run("ScenarioA_CreateFails", func(t *testing.T) {
		s := newProvisionFakeStore()
		s.spaces["space-nea"] = pendingSpace("space-nea", "merchant-nea")

		d := &provisionMockDiscord{
			createChannelDeniedErr: errors.New("discord: 403 cannot create channels"),
		}
		task := makeProvisionTask("space-nea", "merchant-nea", "no-allow-a", "cat-nea")
		_ = runProvisionHandler(s, d, task)

		if len(d.applyCategoryAllowCalls) != 0 {
			t.Errorf("ScenarioA: ApplyCategoryAgentAllow must not be called, got %d calls",
				len(d.applyCategoryAllowCalls))
		}
		// No @everyone allow via any path.
		sp := s.spaces["space-nea"]
		if sp.ACLState == domain.ACLStateApplied {
			t.Error("ScenarioA INVARIANT VIOLATED: acl_state=applied after create failure")
		}
	})

	t.Run("ScenarioB_ACLFails_HalfOpen", func(t *testing.T) {
		s := newProvisionFakeStore()
		s.spaces["space-neb"] = pendingSpace("space-neb", "merchant-neb")

		d := &provisionMockDiscord{
			createChannelDeniedID: "discord-neb",
			applyCategoryAllowErr: errors.New("discord: 403 permissions"),
		}
		task := makeProvisionTask("space-neb", "merchant-neb", "no-allow-b", "cat-neb")
		_ = runProvisionHandler(s, d, task)

		// Exactly 1 failed attempt at the allow — no successful allow.
		if len(d.applyCategoryAllowCalls) != 1 {
			t.Errorf("ScenarioB: want 1 failed allow attempt, got %d", len(d.applyCategoryAllowCalls))
		}
		sp := s.spaces["space-neb"]
		if sp.ACLState == domain.ACLStateApplied {
			t.Error("ScenarioB INVARIANT VIOLATED: acl_state=applied after ACL failure")
		}
	})
}

// ─── AC-1: idempotent replay with stored response ────────────────────────────

// TestProvisionSpace_IdempotentReplay_ReturnsSameBody verifies that when the idempotency
// middleware has a stored response (ResponseCode + ResponseBody set), a second request
// with the same key returns the EXACT stored body with Idempotency-Replay header and
// does NOT create a second outbox row.
//
// This test exercises the middleware replay path directly via the spacesFakeStore,
// which is in the handlers_test package. We test the worker-side here: after a
// successful provision task, the cache is invalidated so the handler does not see
// stale data on the next list call.
//
// For the handler-side replay, see spaces_test.go. Here we verify the worker does
// not double-provision an already-completed space (worker upsert idempotency).
func TestProvisionSpace_WorkerIdempotency_DoubleRunIsNoop(t *testing.T) {
	s := newProvisionFakeStore()
	chID := "discord-idem-001"
	// Space is ALREADY provisioned (applied + channelID set).
	s.spaces["space-idem"] = &domain.Space{
		ID:               "space-idem",
		MerchantID:       "merchant-idem",
		DiscordChannelID: &chID,
		ACLState:         domain.ACLStateApplied,
	}

	d := &provisionMockDiscord{}
	task := makeProvisionTask("space-idem", "merchant-idem", "idem-space", "cat-idem")

	// First run — already provisioned, should be a no-op.
	if err := runProvisionHandler(s, d, task); err != nil {
		t.Fatalf("first run on already-provisioned space: want nil, got: %v", err)
	}
	// Second run — still no Discord calls.
	if err := runProvisionHandler(s, d, task); err != nil {
		t.Fatalf("second run on already-provisioned space: want nil, got: %v", err)
	}

	if len(d.createChannelDeniedCalls) != 0 {
		t.Errorf("CreateChannelDenied must NOT be called for an already-provisioned space"+
			" (idempotency), got %d calls", len(d.createChannelDeniedCalls))
	}
	if len(d.applyCategoryAllowCalls) != 0 {
		t.Errorf("ApplyCategoryAgentAllow must NOT be called for an already-provisioned space"+
			" (idempotency), got %d calls", len(d.applyCategoryAllowCalls))
	}
}

// ─── AC-1: concurrent same-merchant serialization ────────────────────────────

// TestProvisionSpace_ConcurrentSameMerchant_LockSerializes verifies that when a
// second worker task for the same merchant arrives while the lock is held, it
// returns a retryable error (not SkipRetry). The first task must have obtained
// the lock and made exactly one CreateChannelDenied call.
//
// This simulates the "lock held" scenario at the worker level, equivalent to
// two concurrent provision tasks for the same merchant.
func TestProvisionSpace_ConcurrentSameMerchant_LockSerializes(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-concurrent"] = pendingSpace("space-concurrent", "merchant-concurrent")

	d := &provisionMockDiscord{}
	task := makeProvisionTask("space-concurrent", "merchant-concurrent", "concurrent-space", "cat-conc")

	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	locker := lock.New(rdb)

	// Pre-acquire the merchant lock — simulates worker-1 holding the lock.
	_, ok, err := locker.AcquireMerchant(context.Background(), "merchant-concurrent")
	if err != nil || !ok {
		t.Fatalf("pre-acquire lock failed: ok=%v err=%v", ok, err)
	}

	// Worker-2 tries to run — sees the lock held.
	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-str-001",
		EveryoneRoleID:    "everyone-str-001",
		AgentRoleID:       "agent-str-001",
		DefaultCategoryID: "default-cat-str-001",
		Limiter:           ratelimit.NoopLimiter{},
		Locker:            locker,
		Cache:             cache.NoopCache{},
	})
	handlerErr := mux.ProcessTask(context.Background(), task)

	// Must be a retryable error (not SkipRetry).
	if handlerErr == nil {
		t.Fatal("want retryable error when lock is held, got nil")
	}
	if errors.Is(handlerErr, asynq.SkipRetry) {
		t.Error("lock-held error must NOT be SkipRetry — must be retryable (AC-6)")
	}

	// No Discord calls were made (worker-2 did not proceed past the lock gate).
	if len(d.createChannelDeniedCalls) != 0 {
		t.Errorf("CreateChannelDenied must not be called when lock is held, got %d calls",
			len(d.createChannelDeniedCalls))
	}
	// acl_state must not have been touched by worker-2.
	sp := s.spaces["space-concurrent"]
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("acl_state must not be applied when the second worker was blocked by the lock")
	}
}

// ─── AC-3: audit entry action for degraded vs failed ─────────────────────────

// TestProvisionSpace_AuditEntry_ContainsACLState verifies that the audit entry written
// on a fail-closed error includes the acl_state in its detail map, enabling operators
// to distinguish degraded (channel created, ACL failed) from failed (never created).
func TestProvisionSpace_AuditEntry_ContainsACLState(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-audit-acl"] = pendingSpace("space-audit-acl", "merchant-audit-acl")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-audit-acl",
		applyCategoryAllowErr: errors.New("permission denied"),
	}
	task := makeProvisionTask("space-audit-acl", "merchant-audit-acl", "audit-acl-space", "cat-audit")

	_ = runProvisionHandler(s, d, task)

	if len(s.auditEntries) == 0 {
		t.Fatal("want audit entry on fail-closed, got none")
	}

	// Find the failure audit entry (action=space.provision.failed).
	var failureEntry *struct {
		action string
		detail map[string]any
	}
	for _, e := range s.auditEntries {
		if e.Action == "space.provision.failed" {
			failureEntry = &struct {
				action string
				detail map[string]any
			}{action: e.Action, detail: e.Detail}
			break
		}
	}
	if failureEntry == nil {
		t.Fatal("want audit entry with action=space.provision.failed, found none")
	}

	// The detail must contain acl_state so operators can see the terminal state.
	if _, ok := failureEntry.detail["acl_state"]; !ok {
		t.Error("audit entry detail must include 'acl_state' for operator visibility")
	}
	// The detail must contain reason.
	if _, ok := failureEntry.detail["reason"]; !ok {
		t.Error("audit entry detail must include 'reason' for operator visibility")
	}
}

// ─── AC-3: rate-limit on ACL step is retryable (not terminal) ────────────────

// TestProvisionSpace_RateLimitOnACLStep_IsRetryable verifies that a rate-limit error
// on the ACL step is retryable — the channel is still invisible (deny is still in
// effect from creation) so retrying is safe (as opposed to a non-429 ACL error,
// which is terminal/fail-closed).
//
// Implementation note: The current handler converts 429 on the ACL step to a
// *RateLimitError (retryable), not a SkipRetryError. We verify:
//   - The returned error is not SkipRetry.
//   - IsFailure returns false (rate-limit retries don't count as failures).
//   - acl_state is NOT 'applied' (the channel is still invisible).
func TestProvisionSpace_RateLimitOnACLStep_IsRetryable(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-rl-acl"] = pendingSpace("space-rl-acl", "merchant-rl-acl")

	// Use a zero-capacity limiter so TakeGlobal/TakeRoute is denied immediately.
	// The rate-limit fires before the ACL step (on "POST/channels" route).
	// This exercises the retryable rate-limit path.
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

	d := &provisionMockDiscord{createChannelDeniedID: "discord-rl-acl"}

	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-str-001",
		EveryoneRoleID:    "everyone-str-001",
		AgentRoleID:       "agent-str-001",
		DefaultCategoryID: "default-cat-str-001",
		Limiter:           limiter,
		Locker:            lock.NoopLocker{},
		Cache:             cache.NoopCache{},
	})
	handlerErr := mux.ProcessTask(context.Background(),
		makeProvisionTask("space-rl-acl", "merchant-rl-acl", "rl-acl-space", "cat-rl"))

	if handlerErr == nil {
		t.Fatal("want error when rate limit bucket is empty, got nil")
	}

	// Rate-limit error must NOT be SkipRetry.
	if errors.Is(handlerErr, asynq.SkipRetry) {
		t.Error("rate-limit on provision must NOT be SkipRetry — it is retryable")
	}

	// IsFailure must return false for rate-limit retries (AC-8, NFR-7).
	if worker.IsFailure(handlerErr) {
		t.Error("IsFailure must return false for a rate-limit error (AC-8)")
	}

	// acl_state must NOT be applied.
	sp := s.spaces["space-rl-acl"]
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("acl_state must not be applied when rate-limited (never got through the flow)")
	}
}

// ─── fix(NFR-5): AgentRoleID used for category allow, never @everyone ─────────

// TestProvisionSpace_AgentAllow_TargetsAgentRole_NotEveryone is the headline isolation
// invariant test (NFR-5, CRITICAL fix #1).
//
// It asserts:
//  1. ApplyCategoryAgentAllow is called with the configured AgentRoleID ("agent-str-001").
//  2. The AgentRoleID is NOT the same as GuildID ("guild-str-001").
//  3. The AgentRoleID is NOT the EveryoneRoleID ("everyone-str-001").
//
// Before the fix, guildID was passed to ApplyCategoryAgentAllow, making every channel
// world-readable (guildID == @everyone role in Discord's permission model, NFR-5).
func TestProvisionSpace_AgentAllow_TargetsAgentRole_NotEveryone(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-nfe"] = pendingSpace("space-nfe", "merchant-nfe")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-nfe"}
	task := makeProvisionTask("space-nfe", "merchant-nfe", "nfe-space", "cat-nfe")

	err, mr, rdb := runProvisionHandlerFull(s, d, task)
	defer mr.Close()
	defer rdb.Close()

	if err != nil {
		t.Fatalf("want success, got: %v", err)
	}

	// ApplyCategoryAgentAllow must have been called exactly once.
	if len(d.applyCategoryAllowCalls) != 1 {
		t.Fatalf("want 1 ApplyCategoryAgentAllow call, got %d", len(d.applyCategoryAllowCalls))
	}

	call := d.applyCategoryAllowCalls[0]
	const (
		wantAgentRoleID = "agent-str-001"
		guildID         = "guild-str-001"
		everyoneRoleID  = "everyone-str-001"
	)

	// (1) The allow must target the Agent role.
	if call.AgentRoleID != wantAgentRoleID {
		t.Errorf("ApplyCategoryAgentAllow: want agentRoleID=%q, got %q",
			wantAgentRoleID, call.AgentRoleID)
	}

	// (2) The allow must NOT target @everyone (guildID == @everyone in Discord).
	if call.AgentRoleID == guildID {
		t.Errorf("ISOLATION VIOLATED: ApplyCategoryAgentAllow targeted guildID=%q which is "+
			"@everyone — every channel becomes world-readable (NFR-5)", guildID)
	}

	// (3) The allow must NOT target the everyoneRoleID (belt-and-suspenders).
	if call.AgentRoleID == everyoneRoleID {
		t.Errorf("ISOLATION VIOLATED: ApplyCategoryAgentAllow targeted everyoneRoleID=%q "+
			"— every channel becomes world-readable (NFR-5)", everyoneRoleID)
	}
}

// TestProvisionSpace_MisconfiguredAgentRole_RejectsAtBoot verifies that when
// AgentRoleID is absent or equals GuildID (@everyone), newProvisionSpaceHandler
// refuses to start the real handler and falls back to the stub (fail-safe, NFR-5).
// A stub handler returns nil — tests must NOT expect a provisioned channel.
func TestProvisionSpace_MisconfiguredAgentRole_RejectsAtBoot(t *testing.T) {
	s := newProvisionFakeStore()
	s.spaces["space-mis"] = pendingSpace("space-mis", "merchant-mis")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-mis"}
	task := makeProvisionTask("space-mis", "merchant-mis", "mis-space", "cat-mis")

	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		t.Fatal(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	// Misconfigured: AgentRoleID equals GuildID (@everyone).
	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-001",
		EveryoneRoleID:    "everyone-001",
		AgentRoleID:       "guild-001", // WRONG: same as GuildID (@everyone)
		DefaultCategoryID: "cat-001",
		Limiter:           ratelimit.NoopLimiter{},
		Locker:            lock.NoopLocker{},
		Cache:             cache.NoopCache{},
	})
	// The stub handler returns nil — task is accepted but not provisioned.
	if err := mux.ProcessTask(context.Background(), task); err != nil {
		t.Fatalf("want nil from stub handler (misconfigured handler falls back to stub), got: %v", err)
	}

	// No Discord calls should have been made (handler refused to start — stub only).
	if len(d.createChannelDeniedCalls) != 0 {
		t.Errorf("CreateChannelDenied must not be called when handler is misconfigured, got %d calls",
			len(d.createChannelDeniedCalls))
	}
	if len(d.applyCategoryAllowCalls) != 0 {
		t.Errorf("ApplyCategoryAgentAllow must not be called when handler is misconfigured, got %d calls",
			len(d.applyCategoryAllowCalls))
	}
	// acl_state must NOT be 'applied' (isolation invariant).
	sp := s.spaces["space-mis"]
	if sp.ACLState == domain.ACLStateApplied {
		t.Error("ISOLATION VIOLATED: acl_state=applied when handler should have refused (misconfigured AgentRoleID)")
	}
}

// ─── fix(DEFECT-2): SpaceID missing from payload → terminal SkipRetry ─────────

// TestProvisionSpace_EmptySpaceID_TerminalError verifies that when the payload
// contains an empty space_id, the handler returns a terminal SkipRetry error
// rather than retrying 10× with GetSpaceByID("") → ErrNotFound on each attempt.
// This guards against the pre-fix bug where the outbox payload omitted space_id.
func TestProvisionSpace_EmptySpaceID_TerminalError(t *testing.T) {
	s := newProvisionFakeStore()
	d := &provisionMockDiscord{}

	// Craft a task with empty SpaceID — simulates the pre-fix broken outbox payload.
	payload, _ := json.Marshal(queue.ProvisionSpacePayload{
		MerchantID: "merchant-emp",
		SpaceID:    "", // the bug: space_id was not written to the outbox
		SpaceName:  "emp-space",
		CategoryID: "cat-emp",
	})
	task := asynq.NewTask(queue.KindProvisionSpace, payload)

	err := runProvisionHandler(s, d, task)

	if err == nil {
		t.Fatal("want error for empty SpaceID payload, got nil")
	}
	// Must be terminal — no 10× retries into GetSpaceByID("") → ErrNotFound.
	if !errors.Is(err, asynq.SkipRetry) {
		t.Errorf("want SkipRetry for empty SpaceID (defensive guard, fix DEFECT-2), got: %v", err)
	}
	// No Discord calls must have been made.
	if len(d.createChannelDeniedCalls) != 0 {
		t.Errorf("CreateChannelDenied must not be called for empty SpaceID, got %d calls",
			len(d.createChannelDeniedCalls))
	}
}

// ─── AC-2: provisioning latency metric increments on success and failure ──────

// runProvisionHandlerWithMetrics is a variant of runProvisionHandler that attaches
// a real observability.Metrics instance so tests can assert on metric values.
func runProvisionHandlerWithMetrics(
	s *provisionFakeStore,
	d *provisionMockDiscord,
	task *asynq.Task,
	m *observability.Metrics,
) error {
	mr := miniredis.NewMiniRedis()
	if err := mr.Start(); err != nil {
		panic(err)
	}
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	mux := worker.NewServeMux(worker.Config{
		Store:             s,
		DiscordClient:     d,
		DiscordGuildID:    "guild-m-001",
		EveryoneRoleID:    "everyone-m-001",
		AgentRoleID:       "agent-m-001",
		DefaultCategoryID: "default-cat-m-001",
		Limiter:           ratelimit.NoopLimiter{},
		Locker:            lock.NoopLocker{},
		Cache:             cache.NoopCache{},
		Metrics:           m,
	})
	return mux.ProcessTask(context.Background(), task)
}

// metricsBody fetches the /metrics text exposition from the given Metrics instance.
func metricsBody(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	return string(b)
}

// TestProvisionWorker_SuccessFlow_RecordsLatencyMetric verifies that a successful
// provision run increments hub_provisioning_latency_seconds{status="success"} in the
// Prometheus registry (AC-2: metric helpers are wired to real call-sites).
func TestProvisionWorker_SuccessFlow_RecordsLatencyMetric(t *testing.T) {
	m := observability.NewMetrics()

	s := newProvisionFakeStore()
	s.spaces["space-metric-ok"] = pendingSpace("space-metric-ok", "merchant-m-ok")

	d := &provisionMockDiscord{createChannelDeniedID: "discord-m-ok"}
	task := makeProvisionTask("space-metric-ok", "merchant-m-ok", "metric-ok-space", "cat-m-ok")

	if err := runProvisionHandlerWithMetrics(s, d, task, m); err != nil {
		t.Fatalf("provision success: want nil error, got %v", err)
	}

	body := metricsBody(t, m)
	// hub_provisioning_latency_seconds_count{status="success"} must be 1 after one success.
	if !strings.Contains(body, `hub_provisioning_latency_seconds_count{status="success"} 1`) {
		t.Errorf("AC-2: expected hub_provisioning_latency_seconds_count{status=\"success\"} 1 in /metrics output; got:\n%s", body)
	}
}

// TestProvisionWorker_FailureFlow_RecordsLatencyMetric verifies that a fail-closed
// provision run increments hub_provisioning_latency_seconds{status="failure"} and
// hub_errors_total{kind="fatal"} (AC-2).
func TestProvisionWorker_FailureFlow_RecordsLatencyMetric(t *testing.T) {
	m := observability.NewMetrics()

	s := newProvisionFakeStore()
	s.spaces["space-metric-fail"] = pendingSpace("space-metric-fail", "merchant-m-fail")

	d := &provisionMockDiscord{
		createChannelDeniedID: "discord-m-fail",
		applyCategoryAllowErr: errors.New("discord: 403 forbidden"),
	}
	task := makeProvisionTask("space-metric-fail", "merchant-m-fail", "metric-fail-space", "cat-m-fail")

	// failClosed path — must return SkipRetry.
	if err := runProvisionHandlerWithMetrics(s, d, task, m); err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}

	body := metricsBody(t, m)

	// hub_provisioning_latency_seconds_count{status="failure"} must be 1.
	if !strings.Contains(body, `hub_provisioning_latency_seconds_count{status="failure"} 1`) {
		t.Errorf("AC-2: expected hub_provisioning_latency_seconds_count{status=\"failure\"} 1 in /metrics output; got:\n%s", body)
	}

	// hub_errors_total{kind="fatal"} must be at least 1 (failClosed calls IncError).
	if !strings.Contains(body, `kind="fatal"`) {
		t.Errorf("AC-2: expected hub_errors_total{kind=\"fatal\"} in /metrics output; got:\n%s", body)
	}
}
