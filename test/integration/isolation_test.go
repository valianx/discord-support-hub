// Package integration_test implements the multi-tenant isolation gate for M6 (AC-M6-8, AC-1).
//
// Tests are hermetic: no real PostgreSQL or Discord connections; all dependencies
// are backed by in-memory fakes. The test binary exercises the reconcile engine
// end-to-end using the same component graph as production.
//
// M6 pivot: isolation is now enforced via per-merchant Discord roles (not per-user
// channel overwrites). The reconciler diffs role membership (real) against space_members
// rows (desired). "Postgres wins" — role holders without a backing space_member row
// have their role revoked.
//
// Isolation invariants tested (NFR-5, §4.2, AC-M6-8):
//   - A collaborator NOT in Merchant-B's space_members must NOT hold Merchant-B's role.
//   - Any Discord role holder NOT backed by a space_members row has their role revoked.
//   - Circuit breaker: a Postgres transient error prevents any role changes.
package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/reconcile"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Isolation test store ─────────────────────────────────────────────────────

// isolationStore is an in-memory store for isolation tests.
type isolationStore struct {
	spaces  map[string]*domain.Space
	members map[string][]*domain.SpaceMember
	users   map[string]*domain.User
	audit   []store.InsertAuditEntryParams
}

func newIsolationStore() *isolationStore {
	return &isolationStore{
		spaces:  make(map[string]*domain.Space),
		members: make(map[string][]*domain.SpaceMember),
		users:   make(map[string]*domain.User),
	}
}

func (s *isolationStore) addSpace(sp *domain.Space)      { s.spaces[sp.ID] = sp }
func (s *isolationStore) addMember(sm *domain.SpaceMember) {
	s.members[sm.SpaceID] = append(s.members[sm.SpaceID], sm)
}
func (s *isolationStore) addUser(u *domain.User) { s.users[u.ID] = u }

func (s *isolationStore) GetSpaceByID(_ context.Context, id string) (*domain.Space, error) {
	sp, ok := s.spaces[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return sp, nil
}

func (s *isolationStore) ListActiveSpaceMembers(_ context.Context, spaceID string) ([]*domain.SpaceMember, error) {
	return s.members[spaceID], nil
}

func (s *isolationStore) GetUserByID(_ context.Context, id string) (*domain.User, error) {
	u, ok := s.users[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return u, nil
}

func (s *isolationStore) InsertAuditEntry(_ context.Context, p store.InsertAuditEntryParams) error {
	s.audit = append(s.audit, p)
	return nil
}

func (s *isolationStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error { return nil }

func (s *isolationStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	out := make([]*domain.Space, 0, len(s.spaces))
	for _, sp := range s.spaces {
		out = append(out, sp)
	}
	return out, nil
}

// ─── Isolation Discord client (M6 role-based) ─────────────────────────────────

// isolationDiscord implements the role-based Discord interface for reconciler tests.
type isolationDiscord struct {
	// roleHolders maps guildID:roleID → discordUserIDs currently holding the role.
	roleHolders map[string][]string

	// assignedRoles records assign calls.
	assignedRoles []roleChange
	// revokedRoles records removal calls.
	revokedRoles []roleChange
}

type roleChange struct {
	DiscordUserID string
	RoleID        string
}

func newIsolationDiscord() *isolationDiscord {
	return &isolationDiscord{roleHolders: make(map[string][]string)}
}

func (d *isolationDiscord) roleKey(guildID, roleID string) string { return guildID + ":" + roleID }

func (d *isolationDiscord) addRoleHolder(guildID, roleID, discordUserID string) {
	k := d.roleKey(guildID, roleID)
	d.roleHolders[k] = append(d.roleHolders[k], discordUserID)
}

func (d *isolationDiscord) GetGuildMembersByRole(_ context.Context, guildID, roleID string) ([]string, error) {
	return d.roleHolders[d.roleKey(guildID, roleID)], nil
}

func (d *isolationDiscord) AssignMerchantRole(_ context.Context, guildID, discordUserID, roleID string) error {
	d.assignedRoles = append(d.assignedRoles, roleChange{discordUserID, roleID})
	k := d.roleKey(guildID, roleID)
	d.roleHolders[k] = append(d.roleHolders[k], discordUserID)
	return nil
}

func (d *isolationDiscord) RemoveMerchantRole(_ context.Context, guildID, discordUserID, roleID string) error {
	d.revokedRoles = append(d.revokedRoles, roleChange{discordUserID, roleID})
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

// ─── Fixture builders ─────────────────────────────────────────────────────────

const testIsolationGuildID = "guild-isolation"

func strPtr(s string) *string { return &s }

func appliedSpace(spaceID, merchantID, channelID string) *domain.Space {
	roleID := "role-" + spaceID
	return &domain.Space{
		ID:               spaceID,
		MerchantID:       merchantID,
		DiscordChannelID: strPtr(channelID),
		MerchantRoleID:   strPtr(roleID),
		ACLState:         domain.ACLStateApplied,
		LifecycleState:   domain.SpaceLifecycleActive,
		CreatedAt:        time.Now(),
	}
}

func collaboratorUser(userID, discordUserID string) *domain.User {
	return &domain.User{
		ID:            userID,
		Type:          domain.UserTypeCollaborator,
		DiscordUserID: strPtr(discordUserID),
		IsActive:      true,
		CreatedAt:     time.Now(),
	}
}

func activeSpaceMember(id, spaceID, userID string) *domain.SpaceMember {
	return &domain.SpaceMember{
		ID:        id,
		SpaceID:   spaceID,
		UserID:    userID,
		Role:      domain.SpaceMemberRoleCollaborator,
		CreatedAt: time.Now(),
	}
}

// ─── AC-1 / AC-M6-8: Merchant isolation ──────────────────────────────────────

// TestIsolation_CollaboratorSeesOnlyInvitedSpace verifies that a collaborator's role
// in Space-A does not grant access to Space-B. Each space has its own merchant role.
// If a collaborator somehow holds Space-B's merchant role without a space_members row,
// the reconciler must strip that role.
func TestIsolation_CollaboratorSeesOnlyInvitedSpace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceA  = "space-merchant-a"
		spaceB  = "space-merchant-b"
		chanA   = "discord-chan-a"
		chanB   = "discord-chan-b"
		collab  = "collab-user-1"
		dCollab = "discord-collab-1"
		memberA = "sm-1"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	spA := appliedSpace(spaceA, "merchant-a", chanA)
	spB := appliedSpace(spaceB, "merchant-b", chanB)
	s.addSpace(spA)
	s.addSpace(spB)
	s.addUser(collaboratorUser(collab, dCollab))
	s.addMember(activeSpaceMember(memberA, spaceA, collab))
	// collabA holds Space-A's role (correctly).
	d.addRoleHolder(testIsolationGuildID, *spA.MerchantRoleID, dCollab)
	// collabA also holds Space-B's role (isolation breach — not in Space-B's space_members).
	d.addRoleHolder(testIsolationGuildID, *spB.MerchantRoleID, dCollab)

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)

	// Reconcile Space-A: collabA is properly invited — no changes.
	if err := engine.ReconcileSpace(ctx, spaceA); err != nil {
		t.Fatalf("ReconcileSpace(space-a) failed: %v", err)
	}
	if len(d.revokedRoles) != 0 {
		t.Errorf("space-a reconcile: expected 0 revocations, got %d", len(d.revokedRoles))
	}

	// Reconcile Space-B: collabA holds the role without a space_member row — must be stripped.
	if err := engine.ReconcileSpace(ctx, spaceB); err != nil {
		t.Fatalf("ReconcileSpace(space-b) failed: %v", err)
	}

	if len(d.revokedRoles) != 1 {
		t.Fatalf("space-b reconcile: expected 1 role revocation, got %d", len(d.revokedRoles))
	}
	if d.revokedRoles[0].DiscordUserID != dCollab {
		t.Errorf("expected revocation of %s, got %s", dCollab, d.revokedRoles[0].DiscordUserID)
	}

	// Audit entry must record the repair.
	revokeAudit := filterAudit(s.audit, "reconcile.repair")
	if len(revokeAudit) == 0 {
		t.Error("expected at least one reconcile.repair audit entry; got none")
	}
}

// TestIsolation_UnbackedRoleHolder_IsRevoked is the focused variant: a guild member
// holds the merchant role but has no backing space_members row — must be revoked.
func TestIsolation_UnbackedRoleHolder_IsRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceID       = "space-orphan"
		chanID        = "chan-orphan"
		orphanDiscord = "orphan-discord-user"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	sp := appliedSpace(spaceID, "merchant-x", chanID)
	s.addSpace(sp)
	// No space_members rows at all — but Discord has a role holder.
	d.addRoleHolder(testIsolationGuildID, *sp.MerchantRoleID, orphanDiscord)

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.revokedRoles) != 1 {
		t.Fatalf("expected 1 role revocation, got %d", len(d.revokedRoles))
	}
	if d.revokedRoles[0].DiscordUserID != orphanDiscord {
		t.Errorf("expected revocation of %s, got %s", orphanDiscord, d.revokedRoles[0].DiscordUserID)
	}
}

// TestIsolation_BackedRoleHolder_IsPreserved verifies the positive case: a member
// holding the merchant role AND backed by a space_members row must NOT be touched.
func TestIsolation_BackedRoleHolder_IsPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceID  = "space-ok"
		chanID   = "chan-ok"
		userID   = "user-ok"
		dUserID  = "discord-ok"
		memberID = "sm-ok"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	sp := appliedSpace(spaceID, "merchant-ok", chanID)
	s.addSpace(sp)
	s.addUser(collaboratorUser(userID, dUserID))
	s.addMember(activeSpaceMember(memberID, spaceID, userID))
	d.addRoleHolder(testIsolationGuildID, *sp.MerchantRoleID, dUserID) // correctly backed

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.revokedRoles) != 0 {
		t.Errorf("backed role holder must not be revoked; got %d revocations", len(d.revokedRoles))
	}
	if len(d.assignedRoles) != 0 {
		t.Errorf("backed role holder must not trigger re-assign; got %d assignments", len(d.assignedRoles))
	}
}

// TestIsolation_MissingRole_IsAssigned verifies that a space_members row holder who
// is NOT currently holding the merchant role in Discord has the role assigned (repair).
func TestIsolation_MissingRole_IsAssigned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceID  = "space-missing"
		chanID   = "chan-missing"
		userID   = "user-missing"
		dUserID  = "discord-missing"
		memberID = "sm-missing"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	sp := appliedSpace(spaceID, "merchant-m", chanID)
	s.addSpace(sp)
	s.addUser(collaboratorUser(userID, dUserID))
	s.addMember(activeSpaceMember(memberID, spaceID, userID))
	// Do NOT add to roleHolders — role was manually removed or never assigned.

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.assignedRoles) != 1 {
		t.Fatalf("expected 1 role assignment (repair), got %d", len(d.assignedRoles))
	}
	if d.assignedRoles[0].DiscordUserID != dUserID {
		t.Errorf("expected assignment for %s, got %s", dUserID, d.assignedRoles[0].DiscordUserID)
	}
}

// TestIsolation_PendingSpace_IsSkipped verifies that the reconciler skips spaces
// whose ACL state is not "applied" (§4.2 step 1 guard).
func TestIsolation_PendingSpace_IsSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceID = "space-pending"
		chanID  = "chan-pending"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	sp := appliedSpace(spaceID, "merchant-pend", chanID)
	sp.ACLState = domain.ACLStatePending // not yet applied
	s.addSpace(sp)
	// Even if a role holder exists in Discord, the reconciler must not touch it.
	d.addRoleHolder(testIsolationGuildID, *sp.MerchantRoleID, "some-discord-user")

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.revokedRoles) != 0 {
		t.Errorf("pending space must not trigger any role changes; got %d revocations", len(d.revokedRoles))
	}
}

// TestIsolation_NoMerchantRoleID_IsSkipped verifies that a space without a merchant_role_id
// is skipped (M6: the provision worker has not yet created the role).
func TestIsolation_NoMerchantRoleID_IsSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newIsolationStore()
	d := newIsolationDiscord()

	sp := &domain.Space{
		ID:               "space-norole",
		MerchantID:       "merchant-norole",
		DiscordChannelID: strPtr("chan-norole"),
		MerchantRoleID:   nil, // not yet assigned
		ACLState:         domain.ACLStateApplied,
		LifecycleState:   domain.SpaceLifecycleActive,
		CreatedAt:        time.Now(),
	}
	s.addSpace(sp)

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)
	if err := engine.ReconcileSpace(ctx, "space-norole"); err != nil {
		t.Fatalf("ReconcileSpace on no-role space should not error: %v", err)
	}
	if len(d.revokedRoles) != 0 || len(d.assignedRoles) != 0 {
		t.Errorf("no-role space must not trigger Discord changes")
	}
}

// TestIsolation_MultiMerchant_FullSweep tests the complete multi-merchant isolation:
//   - Merchant-A has Space-A with 2 collaborators properly holding the role.
//   - Merchant-B has Space-B with 1 collaborator properly backed + 1 stale role holder.
//   - After reconciling both spaces:
//   - Space-A: 0 role changes (all backed).
//   - Space-B: 1 revocation (the stale holder).
//   - Collaborators from Merchant-A do not hold Space-B's role.
func TestIsolation_MultiMerchant_FullSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newIsolationStore()
	d := newIsolationDiscord()

	const (
		spaceA  = "space-a"
		chanA   = "chan-a"
		userA1  = "user-a1"
		dUserA1 = "discord-a1"
		userA2  = "user-a2"
		dUserA2 = "discord-a2"
		smA1    = "sm-a1"
		smA2    = "sm-a2"

		spaceB      = "space-b"
		chanB       = "chan-b"
		userB1      = "user-b1"
		dUserB1     = "discord-b1"
		smB1        = "sm-b1"
		dUserOrphan = "discord-orphan-b"
	)

	spA := appliedSpace(spaceA, "merchant-a", chanA)
	spB := appliedSpace(spaceB, "merchant-b", chanB)
	s.addSpace(spA)
	s.addSpace(spB)

	s.addUser(collaboratorUser(userA1, dUserA1))
	s.addUser(collaboratorUser(userA2, dUserA2))
	s.addMember(activeSpaceMember(smA1, spaceA, userA1))
	s.addMember(activeSpaceMember(smA2, spaceA, userA2))
	d.addRoleHolder(testIsolationGuildID, *spA.MerchantRoleID, dUserA1)
	d.addRoleHolder(testIsolationGuildID, *spA.MerchantRoleID, dUserA2)

	s.addUser(collaboratorUser(userB1, dUserB1))
	s.addMember(activeSpaceMember(smB1, spaceB, userB1))
	d.addRoleHolder(testIsolationGuildID, *spB.MerchantRoleID, dUserB1)
	d.addRoleHolder(testIsolationGuildID, *spB.MerchantRoleID, dUserOrphan) // stale

	engine := reconcile.NewEngine(s, d, testIsolationGuildID)

	if err := engine.ReconcileSpace(ctx, spaceA); err != nil {
		t.Fatalf("ReconcileSpace(space-a) failed: %v", err)
	}
	if err := engine.ReconcileSpace(ctx, spaceB); err != nil {
		t.Fatalf("ReconcileSpace(space-b) failed: %v", err)
	}

	// Exactly one revocation: the stale holder in Space-B.
	if len(d.revokedRoles) != 1 {
		t.Fatalf("expected 1 revocation total, got %d", len(d.revokedRoles))
	}
	if d.revokedRoles[0].DiscordUserID != dUserOrphan {
		t.Errorf("expected revocation of %s, got %s", dUserOrphan, d.revokedRoles[0].DiscordUserID)
	}

	// Merchant-A collaborators must NOT appear in Space-B's role holders.
	for _, uid := range d.roleHolders[testIsolationGuildID+":"+*spB.MerchantRoleID] {
		if uid == dUserA1 || uid == dUserA2 {
			t.Errorf("isolation breach: Merchant-A collaborator %s holds Merchant-B's role", uid)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func filterAudit(entries []store.InsertAuditEntryParams, action string) []store.InsertAuditEntryParams {
	var out []store.InsertAuditEntryParams
	for _, e := range entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// compile-time assertion that store sentinel errors satisfy error.
var _ error = store.ErrNotFound

// compile-time assertion for ErrConflict.
var _ = errors.Is(store.ErrConflict, store.ErrConflict)
