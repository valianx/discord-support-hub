// Package reconcile_test — reconcile_guild_test.go verifies the scheduled full-guild sweep
// and per-space reconcile logic (M5/M6, AC-5, AC-M6-8).
//
// M6 pivot: reconciliation is now role-based, not per-user-overwrite-based.
// The Engine diffs merchant-role membership in Discord against the Postgres desired set.
//
// Tests are hermetic: no real Postgres or Discord connections. All dependencies are fakes.
// The full-guild sweep must:
//   - Enumerate all active provisioned spaces from the store.
//   - Call ReconcileSpace for each one.
//   - Remove stale role holders (real holds role but not in Postgres).
//   - Assign missing roles (in Postgres but not holding role).
//   - Handle empty guilds (no spaces) gracefully.
//   - Report per-space errors without aborting the remaining spaces.
//   - Circuit breaker: abort space reconcile with zero role changes if desired set is empty
//     but Discord has role holders (SEC-M5-001, applied to role model).
package reconcile_test

import (
	"context"
	"errors"
	"testing"
	"time"

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

// guildDiscord is a role-based Discord fake (M6 model).
// Tracks which members hold the merchant role, and counts assign/remove calls.
type guildDiscord struct {
	roleHolders map[string][]string // guildID+roleID → discordUserIDs
	assignCount int
	removeCount int
}

func newGuildDiscord() *guildDiscord {
	return &guildDiscord{roleHolders: make(map[string][]string)}
}

func (d *guildDiscord) roleKey(guildID, roleID string) string { return guildID + ":" + roleID }

func (d *guildDiscord) addRoleHolder(guildID, roleID, discordUserID string) {
	k := d.roleKey(guildID, roleID)
	d.roleHolders[k] = append(d.roleHolders[k], discordUserID)
}

func (d *guildDiscord) GetGuildMembersByRole(_ context.Context, guildID, roleID string) ([]string, error) {
	return d.roleHolders[d.roleKey(guildID, roleID)], nil
}

func (d *guildDiscord) AssignMerchantRole(_ context.Context, guildID, discordUserID, roleID string) error {
	d.assignCount++
	k := d.roleKey(guildID, roleID)
	d.roleHolders[k] = append(d.roleHolders[k], discordUserID)
	return nil
}

func (d *guildDiscord) RemoveMerchantRole(_ context.Context, guildID, discordUserID, roleID string) error {
	d.removeCount++
	k := d.roleKey(guildID, roleID)
	holders := d.roleHolders[k]
	filtered := holders[:0]
	for _, uid := range holders {
		if uid != discordUserID {
			filtered = append(filtered, uid)
		}
	}
	d.roleHolders[k] = filtered
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

const testGuildID = "guild-test"

func activeProvisionedSpace(id, channelID string) *domain.Space {
	ch := channelID
	roleID := "role-" + id
	return &domain.Space{
		ID:               id,
		MerchantID:       "merchant-" + id,
		DiscordChannelID: &ch,
		MerchantRoleID:   &roleID,
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
		Role:      domain.SpaceMemberRoleCollaborator,
		CreatedAt: time.Now(),
	}
}

// ─── Tests ────────────────────────────────────────────────────────────────────

// TestReconcileGuild_EmptyGuild verifies the sweep is a no-op when there are no spaces.
func TestReconcileGuild_EmptyGuild(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()
	engine := reconcile.NewEngine(s, d, testGuildID)

	if err := engine.ReconcileGuild(context.Background(), testGuildID); err != nil {
		t.Fatalf("ReconcileGuild on empty guild: %v", err)
	}
	if d.assignCount != 0 || d.removeCount != 0 {
		t.Errorf("expected 0 Discord calls on empty guild, got assign=%d remove=%d",
			d.assignCount, d.removeCount)
	}
}

// TestReconcileGuild_AllSynced verifies the sweep makes no changes when
// all Discord role holders match the Postgres desired set.
func TestReconcileGuild_AllSynced(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()

	sp1 := activeProvisionedSpace("sp1", "chan1")
	sp2 := activeProvisionedSpace("sp2", "chan2")
	s.addSpace(sp1)
	s.addSpace(sp2)

	s.addUser(guildCollaborator("u1", "du1"))
	s.addUser(guildCollaborator("u2", "du2"))
	s.addMember(guildSpaceMember("sm1", "sp1", "u1"))
	s.addMember(guildSpaceMember("sm2", "sp2", "u2"))
	// Desired set and real state are in sync.
	d.addRoleHolder(testGuildID, *sp1.MerchantRoleID, "du1")
	d.addRoleHolder(testGuildID, *sp2.MerchantRoleID, "du2")

	engine := reconcile.NewEngine(s, d, testGuildID)
	if err := engine.ReconcileGuild(context.Background(), testGuildID); err != nil {
		t.Fatalf("ReconcileGuild: %v", err)
	}

	if d.assignCount != 0 || d.removeCount != 0 {
		t.Errorf("expected 0 role changes when synced, got assign=%d remove=%d",
			d.assignCount, d.removeCount)
	}
}

// TestReconcileGuild_RemovesStaleRoleHolder verifies the sweep removes a Discord role
// holder that has no backing space_members row (Postgres wins, AC-M6-8).
func TestReconcileGuild_RemovesStaleRoleHolder(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()

	sp := activeProvisionedSpace("sp1", "chan1")
	s.addSpace(sp)

	// Backed member.
	s.addUser(guildCollaborator("u1", "du1"))
	s.addMember(guildSpaceMember("sm1", "sp1", "u1"))
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du1")
	// Stale role holder — not in Postgres.
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du-stale")

	engine := reconcile.NewEngine(s, d, testGuildID)
	if err := engine.ReconcileGuild(context.Background(), testGuildID); err != nil {
		t.Fatalf("ReconcileGuild: %v", err)
	}

	if d.removeCount != 1 {
		t.Errorf("expected 1 role removal for stale holder, got %d", d.removeCount)
	}
	if d.assignCount != 0 {
		t.Errorf("expected 0 role assignments, got %d", d.assignCount)
	}
}

// TestReconcileGuild_AssignsMissingRole verifies that a space_members row holder who
// is missing the merchant role in Discord gets it assigned (AC-M6-8).
func TestReconcileGuild_AssignsMissingRole(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()

	sp := activeProvisionedSpace("sp1", "chan1")
	s.addSpace(sp)

	// Member is in Postgres but NOT holding the role in Discord.
	s.addUser(guildCollaborator("u1", "du1"))
	s.addMember(guildSpaceMember("sm1", "sp1", "u1"))
	// Do not add du1 to roleHolders — triggers repair.

	engine := reconcile.NewEngine(s, d, testGuildID)
	if err := engine.ReconcileGuild(context.Background(), testGuildID); err != nil {
		t.Fatalf("ReconcileGuild: %v", err)
	}

	if d.assignCount != 1 {
		t.Errorf("expected 1 role assignment for missing role, got %d", d.assignCount)
	}
	if d.removeCount != 0 {
		t.Errorf("expected 0 role removals, got %d", d.removeCount)
	}
}

// TestReconcileGuild_ContinuesOnSpaceError verifies that an error in one space's reconcile
// does not abort the sweep for the remaining spaces.
func TestReconcileGuild_ContinuesOnSpaceError(t *testing.T) {
	t.Parallel()

	errStore := &erroringStore{
		guildStore:   newGuildStore(),
		errorSpaceID: "sp-fail",
	}
	d := newGuildDiscord()

	spFail := activeProvisionedSpace("sp-fail", "chan-fail")
	spOK := activeProvisionedSpace("sp-ok", "chan-ok")
	errStore.addSpace(spFail)
	errStore.addSpace(spOK)
	// sp-ok has a stale role holder that should be removed.
	d.addRoleHolder(testGuildID, *spOK.MerchantRoleID, "du-stale")

	engine := reconcile.NewEngine(errStore, d, testGuildID)
	err := engine.ReconcileGuild(context.Background(), testGuildID)
	if err == nil {
		t.Fatal("expected error from ReconcileGuild when a space fails, got nil")
	}
	// sp-ok should still have been reconciled (du-stale removed).
	if d.removeCount != 1 {
		t.Errorf("expected 1 removal from sp-ok despite sp-fail error, got %d", d.removeCount)
	}
}

// TestReconcileSpace_NoMerchantRoleID_Skips verifies that a space without a merchant_role_id
// is skipped (M6: spaces not yet provisioned with a role are not role-reconciled).
func TestReconcileSpace_NoMerchantRoleID_Skips(t *testing.T) {
	t.Parallel()
	s := newGuildStore()
	d := newGuildDiscord()

	ch := "chan-norole"
	sp := &domain.Space{
		ID:               "sp-norole",
		MerchantID:       "m1",
		DiscordChannelID: &ch,
		MerchantRoleID:   nil, // not yet assigned
		ACLState:         domain.ACLStateApplied,
		LifecycleState:   domain.SpaceLifecycleActive,
		CreatedAt:        time.Now(),
	}
	s.addSpace(sp)

	engine := reconcile.NewEngine(s, d, testGuildID)
	if err := engine.ReconcileSpace(context.Background(), "sp-norole"); err != nil {
		t.Fatalf("ReconcileSpace on no-role space should not error: %v", err)
	}
	if d.assignCount != 0 || d.removeCount != 0 {
		t.Errorf("expected 0 Discord calls for no-role space")
	}
}

// ─── Error-injecting store ────────────────────────────────────────────────────

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

// ─── SEC-M5-001 safety tests (circuit breaker) ───────────────────────────────

// TestReconcileSpace_TransientStoreError_ZeroRoleChanges verifies that a transient store
// error during desired-set construction aborts the reconcile with zero role changes
// (SEC-M5-001: circuit breaker preserved in M6 role model).
func TestReconcileSpace_TransientStoreError_ZeroRoleChanges(t *testing.T) {
	t.Parallel()

	s := &transientUserStore{
		guildStore:      newGuildStore(),
		transientUserID: "u-transient",
	}

	const spaceID = "sp-transient"
	sp := activeProvisionedSpace(spaceID, "chan-transient")
	s.addSpace(sp)
	// Member whose GetUserByID call will return a transient error.
	s.addMember(guildSpaceMember("sm-transient", spaceID, "u-transient"))
	s.addUser(guildCollaborator("u-transient", "du-transient"))

	d := newGuildDiscord()
	// Discord has a stale role holder — must NOT be removed on a transient error.
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du-transient")

	engine := reconcile.NewEngine(s, d, testGuildID)
	err := engine.ReconcileSpace(context.Background(), spaceID)

	if err == nil {
		t.Fatal("SEC-M5-001: expected error when GetUserByID returns transient error, got nil")
	}
	if d.removeCount != 0 {
		t.Errorf("SEC-M5-001: expected 0 role removals on transient store error, got %d", d.removeCount)
	}
}

type transientUserStore struct {
	*guildStore
	transientUserID string
}

var errTransient = errors.New("store: connection pool exhausted (simulated transient error)")

func (s *transientUserStore) GetUserByID(ctx context.Context, id string) (*domain.User, error) {
	if id == s.transientUserID {
		return nil, errTransient
	}
	return s.guildStore.GetUserByID(ctx, id)
}

// TestReconcileSpace_ErrNotFound_MemberOmitted verifies that a genuine store.ErrNotFound
// for a member's user row causes the member to be omitted from the desired set (legitimate).
// The member's role is removed because they're no longer in the desired set.
func TestReconcileSpace_ErrNotFound_MemberOmitted(t *testing.T) {
	t.Parallel()

	s := newGuildStore()

	const spaceID = "sp-notfound"
	sp := activeProvisionedSpace(spaceID, "chan-notfound")
	s.addSpace(sp)
	// Member row exists but user row does NOT — simulates a deleted user.
	s.addMember(guildSpaceMember("sm-nf", spaceID, "u-deleted"))
	// Intentionally do NOT call s.addUser — GetUserByID will return ErrNotFound.

	// Healthy member.
	s.addUser(guildCollaborator("u-ok", "du-ok"))
	s.addMember(guildSpaceMember("sm-ok", spaceID, "u-ok"))

	d := newGuildDiscord()
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du-ok")
	// du-deleted still holds the role but is not in the desired set — should be removed.
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du-deleted")

	engine := reconcile.NewEngine(s, d, testGuildID)
	err := engine.ReconcileSpace(context.Background(), spaceID)

	if err != nil {
		t.Fatalf("unexpected error for ErrNotFound member: %v", err)
	}
	// du-deleted: role removed (not in desired set). du-ok: no change.
	if d.removeCount != 1 {
		t.Errorf("expected 1 role removal for deleted user, got %d", d.removeCount)
	}
	if d.assignCount != 0 {
		t.Errorf("expected 0 role assignments, got %d", d.assignCount)
	}
}

// TestReconcileSpace_CircuitBreaker_EmptyDesiredSet verifies that the circuit breaker fires
// when the desired set is empty but Discord has role holders (SEC-M5-001 preserved in M6).
func TestReconcileSpace_CircuitBreaker_EmptyDesiredSet(t *testing.T) {
	t.Parallel()

	s := &alwaysNotFoundUserStore{guildStore: newGuildStore()}

	const spaceID = "sp-cb"
	sp := activeProvisionedSpace(spaceID, "chan-cb")
	s.addSpace(sp)
	s.addMember(guildSpaceMember("sm-cb", spaceID, "u-cb"))
	// User row deliberately absent — alwaysNotFoundUserStore returns ErrNotFound.

	d := newGuildDiscord()
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du-cb-1")
	d.addRoleHolder(testGuildID, *sp.MerchantRoleID, "du-cb-2")

	engine := reconcile.NewEngine(s, d, testGuildID)
	err := engine.ReconcileSpace(context.Background(), spaceID)

	if err == nil {
		t.Fatal("SEC-M5-001 circuit breaker: expected error when desired set is empty but Discord has role holders")
	}
	if d.removeCount != 0 {
		t.Errorf("SEC-M5-001 circuit breaker: expected 0 role removals, got %d", d.removeCount)
	}
}

type alwaysNotFoundUserStore struct{ *guildStore }

func (s *alwaysNotFoundUserStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	return nil, store.ErrNotFound
}
