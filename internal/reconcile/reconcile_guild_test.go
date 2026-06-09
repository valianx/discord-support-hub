// Package reconcile_test — reconcile_guild_test.go verifies the scheduled full-guild sweep (M5, AC-5).
//
// Tests are hermetic: no real Postgres or Discord connections. All dependencies are fakes.
// The full-guild sweep must:
//   - Enumerate all active provisioned spaces from the store.
//   - Call ReconcileSpace for each one.
//   - Revoke unbacked Discord overwrites (Postgres wins, NFR-5).
//   - Handle empty guilds (no spaces) gracefully.
//   - Report per-space errors without aborting the remaining spaces.
package reconcile_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/reconcile"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Fakes ────────────────────────────────────────────────────────────────────

// guildStore is a minimal in-memory store used by reconcile_guild tests.
type guildStore struct {
	spaces           []*domain.Space
	membersBySpaceID map[string][]*domain.SpaceMember
	users            map[string]*domain.User
}

func newGuildStore() *guildStore {
	return &guildStore{
		membersBySpaceID: make(map[string][]*domain.SpaceMember),
		users:            make(map[string]*domain.User),
	}
}

func (s *guildStore) addSpace(sp *domain.Space) { s.spaces = append(s.spaces, sp) }
func (s *guildStore) addMember(sm *domain.SpaceMember) {
	s.membersBySpaceID[sm.SpaceID] = append(s.membersBySpaceID[sm.SpaceID], sm)
}
func (s *guildStore) addUser(u *domain.User) { s.users[u.ID] = u }

func (s *guildStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	return s.spaces, nil
}

func (s *guildStore) GetSpaceByID(_ context.Context, id string) (*domain.Space, error) {
	for _, sp := range s.spaces {
		if sp.ID == id {
			return sp, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *guildStore) ListActiveSpaceMembers(_ context.Context, spaceID string) ([]*domain.SpaceMember, error) {
	return s.membersBySpaceID[spaceID], nil
}

func (s *guildStore) GetUserByID(_ context.Context, id string) (*domain.User, error) {
	u, ok := s.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (s *guildStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	return nil
}

func (s *guildStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error { return nil }

func (s *guildStore) SetSpaceMemberOverwriteApplied(_ context.Context, id string) (*domain.SpaceMember, error) {
	for _, members := range s.membersBySpaceID {
		for _, sm := range members {
			if sm.ID == id {
				sm.OverwriteApplied = true
				return sm, nil
			}
		}
	}
	return nil, store.ErrNotFound
}

// guildDiscord is a simple Discord fake tracking overwrites and revokes.
type guildDiscord struct {
	overwrites    map[string][]*discordgo.PermissionOverwrite // channelID → overwrites
	revokeCount   int
	applyCount    int
	listCallCount int
}

func newGuildDiscord() *guildDiscord {
	return &guildDiscord{overwrites: make(map[string][]*discordgo.PermissionOverwrite)}
}

func (d *guildDiscord) addOverwrite(channelID, discordUserID string) {
	d.overwrites[channelID] = append(d.overwrites[channelID], &discordgo.PermissionOverwrite{
		ID:   discordUserID,
		Type: discordgo.PermissionOverwriteTypeMember,
	})
}

func (d *guildDiscord) GetChannelOverwrites(_ context.Context, channelID string) ([]*discordgo.PermissionOverwrite, error) {
	d.listCallCount++
	return d.overwrites[channelID], nil
}

func (d *guildDiscord) SetCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	d.applyCount++
	d.overwrites[channelID] = append(d.overwrites[channelID], &discordgo.PermissionOverwrite{
		ID: discordUserID, Type: discordgo.PermissionOverwriteTypeMember,
	})
	return nil
}

func (d *guildDiscord) DeleteCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	d.revokeCount++
	existing := d.overwrites[channelID]
	filtered := existing[:0]
	for _, ow := range existing {
		if ow.ID != discordUserID {
			filtered = append(filtered, ow)
		}
	}
	d.overwrites[channelID] = filtered
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func activeProvisionedSpace(id, channelID string) *domain.Space {
	ch := channelID
	return &domain.Space{
		ID:               id,
		MerchantID:       "merchant-" + id,
		DiscordChannelID: &ch,
		ACLState:         domain.ACLStateApplied,
		LifecycleState:   domain.SpaceLifecycleActive,
		CreatedAt:        time.Now(),
	}
}

func guildCollaborator(userID, discordID string) *domain.User {
	d := discordID
	return &domain.User{
		ID:            userID,
		Type:          domain.UserTypeCollaborator,
		DiscordUserID: &d,
		IsActive:      true,
		CreatedAt:     time.Now(),
	}
}

func guildSpaceMember(id, spaceID, userID string) *domain.SpaceMember {
	return &domain.SpaceMember{
		ID: id, SpaceID: spaceID, UserID: userID,
		Role: domain.SpaceMemberRoleCollaborator, OverwriteApplied: true, CreatedAt: time.Now(),
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestReconcileGuild_EmptyGuild verifies the sweep is a no-op when there are no spaces.
func TestReconcileGuild_EmptyGuild(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()
	engine := reconcile.NewEngine(s, d)

	if err := engine.ReconcileGuild(context.Background(), "guild-empty"); err != nil {
		t.Fatalf("ReconcileGuild on empty guild: %v", err)
	}
	if d.listCallCount != 0 {
		t.Errorf("expected 0 Discord calls on empty guild, got %d", d.listCallCount)
	}
}

// TestReconcileGuild_AllBackedOverwrites verifies the sweep makes no revokes when
// all Discord overwrites are backed by space_members rows.
func TestReconcileGuild_AllBackedOverwrites(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()

	// Two spaces, each with one properly-backed collaborator.
	s.addSpace(activeProvisionedSpace("sp1", "chan1"))
	s.addSpace(activeProvisionedSpace("sp2", "chan2"))

	s.addUser(guildCollaborator("u1", "du1"))
	s.addUser(guildCollaborator("u2", "du2"))
	s.addMember(guildSpaceMember("sm1", "sp1", "u1"))
	s.addMember(guildSpaceMember("sm2", "sp2", "u2"))
	d.addOverwrite("chan1", "du1")
	d.addOverwrite("chan2", "du2")

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileGuild(context.Background(), "guild-ok"); err != nil {
		t.Fatalf("ReconcileGuild: %v", err)
	}

	if d.revokeCount != 0 {
		t.Errorf("expected 0 revokes, got %d", d.revokeCount)
	}
}

// TestReconcileGuild_RevokeUnbackedOverwrites verifies the sweep revokes Discord overwrites
// that have no backing space_members row across all spaces (Postgres always wins, AC-5).
func TestReconcileGuild_RevokeUnbackedOverwrites(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()

	// Space 1: one backed overwrite. Space 2: one unbacked overwrite.
	s.addSpace(activeProvisionedSpace("sp1", "chan1"))
	s.addSpace(activeProvisionedSpace("sp2", "chan2"))

	s.addUser(guildCollaborator("u1", "du1"))
	s.addMember(guildSpaceMember("sm1", "sp1", "u1"))
	d.addOverwrite("chan1", "du1")         // backed — must NOT be revoked
	d.addOverwrite("chan2", "orphan-user") // unbacked — must be revoked

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileGuild(context.Background(), "guild-drift"); err != nil {
		t.Fatalf("ReconcileGuild: %v", err)
	}

	if d.revokeCount != 1 {
		t.Errorf("expected 1 revoke (unbacked overwrite), got %d", d.revokeCount)
	}
}

// TestReconcileGuild_ContinuesOnSpaceError verifies that an error in one space's reconcile
// does not abort the sweep for the remaining spaces.
func TestReconcileGuild_ContinuesOnSpaceError(t *testing.T) {
	t.Parallel()

	// Use a store that returns an error for GetSpaceByID on a specific space.
	errStore := &erroringStore{
		guildStore:   newGuildStore(),
		errorSpaceID: "sp-fail",
	}
	d := newGuildDiscord()

	errStore.addSpace(activeProvisionedSpace("sp-fail", "chan-fail"))
	errStore.addSpace(activeProvisionedSpace("sp-ok", "chan-ok"))
	d.addOverwrite("chan-ok", "unbacked-ok") // unbacked — should be revoked even though sp-fail errors

	engine := reconcile.NewEngine(errStore, d)
	// ReconcileGuild should return an error (sp-fail failed) but still process sp-ok.
	err := engine.ReconcileGuild(context.Background(), "guild-partial")
	if err == nil {
		t.Fatal("expected error from ReconcileGuild when a space fails, got nil")
	}
	if d.revokeCount != 1 {
		t.Errorf("expected 1 revoke from sp-ok despite sp-fail error, got %d revokes", d.revokeCount)
	}
}

// ─── Error-injecting store ────────────────────────────────────────────────────

// erroringStore wraps guildStore and injects a GetSpaceByID error for a specific space.
type erroringStore struct {
	*guildStore
	errorSpaceID string
}

var errInjected = errors.New("injected store error")

func (s *erroringStore) GetSpaceByID(ctx context.Context, id string) (*domain.Space, error) {
	if id == s.errorSpaceID {
		return nil, errInjected
	}
	return s.guildStore.GetSpaceByID(ctx, id)
}

// ─── SEC-M5-001 safety tests (the dangerous path) ────────────────────────────

// transientUserStore wraps guildStore and injects a non-NotFound error for GetUserByID
// on a specific user id. Used to simulate a Postgres connection blip during desired-set
// construction (SEC-M5-001: transient error must abort the space reconcile with zero revokes).
type transientUserStore struct {
	*guildStore
	transientUserID string // GetUserByID returns errTransient for this user id
}

var errTransient = errors.New("store: connection pool exhausted (simulated transient error)")

func (s *transientUserStore) GetUserByID(ctx context.Context, id string) (*domain.User, error) {
	if id == s.transientUserID {
		return nil, errTransient
	}
	return s.guildStore.GetUserByID(ctx, id)
}

// TestReconcileSpace_TransientStoreError_ZeroRevocations is the headline dangerous-path test
// for SEC-M5-001.
//
// Scenario: a space has one member. GetUserByID returns a transient (non-NotFound) error
// while building the desired set. The reconciler MUST:
//   - Return a non-nil error so asynq retries later.
//   - Execute ZERO Discord revocations (the desired set must not be treated as empty).
func TestReconcileSpace_TransientStoreError_ZeroRevocations(t *testing.T) {
	t.Parallel()

	s := &transientUserStore{
		guildStore:      newGuildStore(),
		transientUserID: "u-transient",
	}

	const (
		spaceID    = "sp-transient"
		channelID  = "chan-transient"
		discordUID = "du-transient"
	)

	sp := activeProvisionedSpace(spaceID, channelID)
	s.addSpace(sp)
	// One member whose GetUserByID call will return a transient error.
	s.addMember(guildSpaceMember("sm-transient", spaceID, "u-transient"))
	// The user row exists in the base store but is intercepted by transientUserStore.
	s.addUser(guildCollaborator("u-transient", discordUID))

	d := newGuildDiscord()
	// The channel has one overwrite matching this user — must NOT be revoked.
	d.addOverwrite(channelID, discordUID)

	engine := reconcile.NewEngine(s, d)
	err := engine.ReconcileSpace(context.Background(), spaceID)

	// (1) Must return a non-nil retryable error — not silent success.
	if err == nil {
		t.Fatal("SEC-M5-001 VIOLATED: expected error when GetUserByID returns transient error, got nil")
	}

	// (2) CRITICAL: zero Discord revocations — the desired set must not have been treated
	// as empty and then used to revoke the legitimate member's overwrite.
	if d.revokeCount != 0 {
		t.Errorf("SEC-M5-001 VIOLATED: expected 0 revocations on transient GetUserByID error, got %d; "+
			"a transient error MUST abort the reconcile without revoking (mass-revocation risk)",
			d.revokeCount)
	}
}

// TestReconcileSpace_ErrNotFound_MemberRevoked verifies that a genuine store.ErrNotFound
// (member's user row deleted) causes the member to be omitted from the desired set and
// their Discord overwrite to be revoked (legitimate revoke behavior — preserved).
func TestReconcileSpace_ErrNotFound_MemberRevoked(t *testing.T) {
	t.Parallel()

	s := newGuildStore()

	const (
		spaceID    = "sp-notfound"
		channelID  = "chan-notfound"
		discordUID = "du-notfound"
	)

	sp := activeProvisionedSpace(spaceID, channelID)
	s.addSpace(sp)
	// Member row exists but user row does NOT — simulates a deleted user.
	s.addMember(guildSpaceMember("sm-nf", spaceID, "u-deleted"))
	// Intentionally do NOT call s.addUser("u-deleted") — GetUserByID will return ErrNotFound.

	d := newGuildDiscord()
	// Discord has an overwrite for the deleted user — it must be revoked.
	d.addOverwrite(channelID, discordUID)
	// The space also has a backed member (u2) to confirm unbacked-only revocations.
	s.addUser(guildCollaborator("u-ok", "du-ok"))
	s.addMember(guildSpaceMember("sm-ok", spaceID, "u-ok"))
	d.addOverwrite(channelID, "du-ok") // backed — must NOT be revoked

	engine := reconcile.NewEngine(s, d)
	err := engine.ReconcileSpace(context.Background(), spaceID)

	// No error expected — ErrNotFound is handled gracefully (member genuinely gone).
	if err != nil {
		t.Fatalf("unexpected error for ErrNotFound member: %v", err)
	}

	// The unbacked overwrite (du-notfound) must be revoked; the backed one (du-ok) must not.
	if d.revokeCount != 1 {
		t.Errorf("expected exactly 1 revoke for the ErrNotFound member's overwrite, got %d", d.revokeCount)
	}
	// Apply count: 0 (du-ok already has an overwrite).
	if d.applyCount != 0 {
		t.Errorf("expected 0 re-applies (du-ok overwrite already in place), got %d", d.applyCount)
	}
}

// TestReconcileSpace_CircuitBreaker_EmptyDesiredSet verifies that when the desired set
// comes back completely empty while Discord has member overwrites, the circuit breaker
// aborts the reconcile with an error instead of revoking every overwrite (SEC-M5-001).
//
// This scenario can arise from a misconfigured store, a race during member deletion,
// or any partial-read scenario where the desired set looks empty but the legitimate
// members are still active. The circuit breaker is the last line of defense.
func TestReconcileSpace_CircuitBreaker_EmptyDesiredSet(t *testing.T) {
	t.Parallel()

	// Use a store that has a space with a member but GetUserByID always returns
	// ErrNotFound — forcing an empty desired set while Discord has overwrites.
	s := &alwaysNotFoundUserStore{guildStore: newGuildStore()}

	const (
		spaceID   = "sp-cb"
		channelID = "chan-cb"
	)

	sp := activeProvisionedSpace(spaceID, channelID)
	s.addSpace(sp)
	s.addMember(guildSpaceMember("sm-cb", spaceID, "u-cb"))
	// The user row is deliberately absent — alwaysNotFoundUserStore returns ErrNotFound.

	d := newGuildDiscord()
	// Discord has overwrites for two members — circuit breaker must prevent revoking them.
	d.addOverwrite(channelID, "du-cb-1")
	d.addOverwrite(channelID, "du-cb-2")

	engine := reconcile.NewEngine(s, d)
	err := engine.ReconcileSpace(context.Background(), spaceID)

	// Circuit breaker must fire: desired set empty, Discord has 2 overwrites.
	if err == nil {
		t.Fatal("SEC-M5-001 circuit breaker VIOLATED: expected error when desired set is empty but Discord has overwrites, got nil")
	}

	// Zero revocations — no mass-revoke.
	if d.revokeCount != 0 {
		t.Errorf("SEC-M5-001 circuit breaker VIOLATED: expected 0 revocations, got %d", d.revokeCount)
	}
}

// alwaysNotFoundUserStore forces GetUserByID → ErrNotFound for every call, producing
// an empty desired set even when space_members rows exist.
type alwaysNotFoundUserStore struct{ *guildStore }

func (s *alwaysNotFoundUserStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	return nil, store.ErrNotFound
}
