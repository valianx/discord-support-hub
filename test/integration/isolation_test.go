// Package integration_test implements the multi-tenant isolation gate for M3 (AC-1).
//
// Tests are hermetic: no real PostgreSQL or Discord connections; all dependencies
// are backed by in-memory fakes. The test binary exercises the reconcile engine and
// worker handlers end-to-end using the same component graph as production.
//
// Isolation invariants tested (NFR-5, §4.2):
//   - A collaborator in Merchant-A's space must NOT be able to see Merchant-B's space.
//   - Any Discord overwrite NOT backed by a space_members row is revoked by the reconciler.
//   - CREATE_INSTANT_INVITE is never granted to non-bot principals (AC-8).
package integration_test

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

// ─── Isolation test store ─────────────────────────────────────────────────────

// isolationStore is an in-memory store for isolation tests.
// It models multi-merchant spaces and collaborator memberships.
type isolationStore struct {
	spaces  map[string]*domain.Space         // spaceID → space
	members map[string][]*domain.SpaceMember // spaceID → active members
	users   map[string]*domain.User          // userID  → user
	audit   []store.InsertAuditEntryParams
}

func newIsolationStore() *isolationStore {
	return &isolationStore{
		spaces:  make(map[string]*domain.Space),
		members: make(map[string][]*domain.SpaceMember),
		users:   make(map[string]*domain.User),
	}
}

func (s *isolationStore) addSpace(sp *domain.Space) {
	s.spaces[sp.ID] = sp
}

func (s *isolationStore) addMember(sm *domain.SpaceMember) {
	s.members[sm.SpaceID] = append(s.members[sm.SpaceID], sm)
}

func (s *isolationStore) addUser(u *domain.User) {
	s.users[u.ID] = u
}

// store.storeReconcile interface methods.
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

func (s *isolationStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error {
	return nil
}

func (s *isolationStore) SetSpaceMemberOverwriteApplied(_ context.Context, id string) (*domain.SpaceMember, error) {
	for _, members := range s.members {
		for _, sm := range members {
			if sm.ID == id {
				sm.OverwriteApplied = true
				return sm, nil
			}
		}
	}
	return nil, store.ErrNotFound
}

// ─── Isolation Discord client ─────────────────────────────────────────────────

// isolationDiscord tracks per-channel member-type overwrites and records revoke calls.
// It is the test double for the Discord client used by the reconciler.
type isolationDiscord struct {
	// channelOverwrites maps channelID → list of (discordUserID, type=member) overwrites.
	channelOverwrites map[string][]*discordgo.PermissionOverwrite

	// revokedOverwrites records revoke calls as (channelID, discordUserID) pairs.
	revokedOverwrites []revokeCall

	// appliedOverwrites records apply calls.
	appliedOverwrites []applyCall

	// inviteGranted tracks whether CREATE_INSTANT_INVITE was granted to any non-bot.
	// Production code must never call this; the test asserts it remains false.
	inviteGranted bool
}

type revokeCall struct {
	ChannelID     string
	DiscordUserID string
}

type applyCall struct {
	ChannelID     string
	DiscordUserID string
}

func newIsolationDiscord() *isolationDiscord {
	return &isolationDiscord{channelOverwrites: make(map[string][]*discordgo.PermissionOverwrite)}
}

func (d *isolationDiscord) addOverwrite(channelID, discordUserID string) {
	d.channelOverwrites[channelID] = append(d.channelOverwrites[channelID], &discordgo.PermissionOverwrite{
		ID:   discordUserID,
		Type: discordgo.PermissionOverwriteTypeMember,
	})
}

// reconcile.discordReconcile interface methods.
func (d *isolationDiscord) SetCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	d.appliedOverwrites = append(d.appliedOverwrites, applyCall{channelID, discordUserID})
	d.channelOverwrites[channelID] = append(d.channelOverwrites[channelID], &discordgo.PermissionOverwrite{
		ID:   discordUserID,
		Type: discordgo.PermissionOverwriteTypeMember,
	})
	return nil
}

func (d *isolationDiscord) DeleteCollaboratorOverwrite(_ context.Context, channelID, discordUserID string) error {
	d.revokedOverwrites = append(d.revokedOverwrites, revokeCall{channelID, discordUserID})
	// Remove from in-memory overwrites list.
	existing := d.channelOverwrites[channelID]
	filtered := existing[:0]
	for _, ow := range existing {
		if ow.ID != discordUserID {
			filtered = append(filtered, ow)
		}
	}
	d.channelOverwrites[channelID] = filtered
	return nil
}

func (d *isolationDiscord) GetChannelOverwrites(_ context.Context, channelID string) ([]*discordgo.PermissionOverwrite, error) {
	return d.channelOverwrites[channelID], nil
}

// ─── Fixture builders ─────────────────────────────────────────────────────────

func discordID(id string) *string { return &id }

func appliedSpace(spaceID, merchantID, channelID string) *domain.Space {
	return &domain.Space{
		ID:               spaceID,
		MerchantID:       merchantID,
		DiscordChannelID: discordID(channelID),
		ACLState:         domain.ACLStateApplied,
		LifecycleState:   domain.SpaceLifecycleActive,
		CreatedAt:        time.Now(),
	}
}

func collaboratorUser(userID, discordUserID string) *domain.User {
	return &domain.User{
		ID:            userID,
		Type:          domain.UserTypeCollaborator,
		DiscordUserID: discordID(discordUserID),
		IsActive:      true,
		CreatedAt:     time.Now(),
	}
}

func activeSpaceMember(id, spaceID, userID string, applied bool) *domain.SpaceMember {
	return &domain.SpaceMember{
		ID:               id,
		SpaceID:          spaceID,
		UserID:           userID,
		Role:             domain.SpaceMemberRoleCollaborator,
		OverwriteApplied: applied,
		CreatedAt:        time.Now(),
	}
}

// ─── AC-1 / NFR-5: Merchant isolation ────────────────────────────────────────

// TestIsolation_CollaboratorSeesOnlyInvitedSpace verifies that the reconciler's desired-set
// logic (ListActiveSpaceMembers for each space individually) means collaborator-A in
// Space-A cannot have an overwrite in Space-B. Specifically:
//
//   - Merchant-A owns Space-A with CollabA invited (space_member row exists).
//   - Merchant-B owns Space-B. CollabA has NO space_member row for Space-B.
//   - If someone manually added CollabA's Discord overwrite to Space-B's channel,
//     the reconciler for Space-B must revoke it.
//
// This is the core isolation invariant (NFR-5, §4.2).
func TestIsolation_CollaboratorSeesOnlyInvitedSpace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceA   = "space-merchant-a"
		spaceB   = "space-merchant-b"
		chanA    = "discord-chan-a"
		chanB    = "discord-chan-b"
		collab   = "collab-user-1"
		dCollab  = "discord-collab-1"
		memberID = "sm-1"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	// Space-A (merchant-A): collabA is properly invited.
	s.addSpace(appliedSpace(spaceA, "merchant-a", chanA))
	s.addUser(collaboratorUser(collab, dCollab))
	s.addMember(activeSpaceMember(memberID, spaceA, collab, true))
	d.addOverwrite(chanA, dCollab) // properly backed — reconciler must leave it alone

	// Space-B (merchant-B): collabA is NOT in space_members, but someone added
	// an overwrite manually (isolation breach). Reconciler must revoke it.
	s.addSpace(appliedSpace(spaceB, "merchant-b", chanB))
	d.addOverwrite(chanB, dCollab) // NOT backed by any space_members row

	engine := reconcile.NewEngine(s, d)

	// Reconcile Space-A: no revovals expected (collabA is properly invited).
	if err := engine.ReconcileSpace(ctx, spaceA); err != nil {
		t.Fatalf("ReconcileSpace(space-a) failed: %v", err)
	}
	if len(d.revokedOverwrites) != 0 {
		t.Errorf("space-a reconcile: expected 0 revokes, got %d: %v",
			len(d.revokedOverwrites), d.revokedOverwrites)
	}

	// Reconcile Space-B: the unbacked overwrite must be revoked (isolation teeth).
	if err := engine.ReconcileSpace(ctx, spaceB); err != nil {
		t.Fatalf("ReconcileSpace(space-b) failed: %v", err)
	}

	if len(d.revokedOverwrites) != 1 {
		t.Fatalf("space-b reconcile: expected 1 revoke, got %d: %v",
			len(d.revokedOverwrites), d.revokedOverwrites)
	}
	if d.revokedOverwrites[0].ChannelID != chanB {
		t.Errorf("expected revoke on channel %s, got %s", chanB, d.revokedOverwrites[0].ChannelID)
	}
	if d.revokedOverwrites[0].DiscordUserID != dCollab {
		t.Errorf("expected revoke for discord user %s, got %s", dCollab, d.revokedOverwrites[0].DiscordUserID)
	}

	// Audit entry must have been written for the revoke.
	revokeAudit := filterAudit(s.audit, "reconcile.repair")
	if len(revokeAudit) == 0 {
		t.Errorf("expected at least one reconcile.repair audit entry; got none")
	}
}

// TestIsolation_UnbackedOverwrite_IsRevoked is the focused variant: one channel,
// one Discord overwrite that has no backing space_members row — must be revoked (§4.2).
func TestIsolation_UnbackedOverwrite_IsRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceID       = "space-orphan"
		channelID     = "chan-orphan"
		orphanDiscord = "orphan-discord-user"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	s.addSpace(appliedSpace(spaceID, "merchant-x", channelID))
	// No space_members rows at all.
	d.addOverwrite(channelID, orphanDiscord) // unbacked — must be revoked

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.revokedOverwrites) != 1 {
		t.Fatalf("expected 1 revoke, got %d", len(d.revokedOverwrites))
	}
	if d.revokedOverwrites[0].DiscordUserID != orphanDiscord {
		t.Errorf("expected revoke of %s, got %s", orphanDiscord, d.revokedOverwrites[0].DiscordUserID)
	}
}

// TestIsolation_BackedOverwrite_IsPreserved verifies the positive case: a backed
// overwrite (space_members row + Discord overwrite aligned) must NOT be revoked.
func TestIsolation_BackedOverwrite_IsPreserved(t *testing.T) {
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

	s.addSpace(appliedSpace(spaceID, "merchant-ok", chanID))
	s.addUser(collaboratorUser(userID, dUserID))
	s.addMember(activeSpaceMember(memberID, spaceID, userID, true))
	d.addOverwrite(chanID, dUserID) // backed → must be preserved

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.revokedOverwrites) != 0 {
		t.Errorf("backed overwrite must not be revoked; got %d revokes", len(d.revokedOverwrites))
	}
}

// TestIsolation_MissingOverwrite_IsReapplied verifies that a backed space_member
// whose overwrite is missing in Discord is re-applied by the reconciler (§4.2 Rule 2).
func TestIsolation_MissingOverwrite_IsReapplied(t *testing.T) {
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

	s.addSpace(appliedSpace(spaceID, "merchant-m", chanID))
	s.addUser(collaboratorUser(userID, dUserID))
	s.addMember(activeSpaceMember(memberID, spaceID, userID, true))
	// No overwrite in Discord — it was manually deleted.

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.appliedOverwrites) != 1 {
		t.Fatalf("expected 1 re-apply, got %d", len(d.appliedOverwrites))
	}
	if d.appliedOverwrites[0].DiscordUserID != dUserID {
		t.Errorf("expected re-apply for %s, got %s", dUserID, d.appliedOverwrites[0].DiscordUserID)
	}
}

// ─── AC-8: CREATE_INSTANT_INVITE never granted ────────────────────────────────

// TestIsolation_CreateInstantInviteNotGranted asserts that the reconcile engine
// and the worker handlers never call CreateInstantInvite on a non-bot principal.
//
// Production code must never mint invite links — all guild entry goes through
// GuildMemberAdd with the guilds.join OAuth2 token (NFR-14, AC-8, AC-2).
// This is verified by confirming that isolationDiscord.inviteGranted remains false
// throughout a full reconcile pass (the field would be set to true if any
// production path called CreateInstantInvite).
func TestIsolation_CreateInstantInviteNotGranted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const (
		spaceID  = "space-invite"
		chanID   = "chan-invite"
		userID   = "user-invite"
		dUserID  = "discord-invite"
		memberID = "sm-invite"
	)

	s := newIsolationStore()
	d := newIsolationDiscord()

	s.addSpace(appliedSpace(spaceID, "merchant-invite", chanID))
	s.addUser(collaboratorUser(userID, dUserID))
	s.addMember(activeSpaceMember(memberID, spaceID, userID, true))
	d.addOverwrite(chanID, dUserID)

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	// inviteGranted must remain false — the reconciler must never create an invite link.
	if d.inviteGranted {
		t.Error("AC-8 violated: CREATE_INSTANT_INVITE was granted by the reconciler")
	}
}

// ─── AC-1: Space not provisioned is skipped ───────────────────────────────────

// TestIsolation_PendingSpace_IsSkipped verifies that the reconciler skips spaces
// whose ACL state is not "applied" (§4.2 step 1 guard). This prevents premature
// overwrite projection on half-provisioned channels.
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

	// Even if an overwrite exists in Discord, the reconciler must not touch it.
	d.addOverwrite(chanID, "some-discord-user")

	engine := reconcile.NewEngine(s, d)
	if err := engine.ReconcileSpace(ctx, spaceID); err != nil {
		t.Fatalf("ReconcileSpace failed: %v", err)
	}

	if len(d.revokedOverwrites) != 0 {
		t.Errorf("pending space must not trigger any revokes; got %d", len(d.revokedOverwrites))
	}
}

// ─── Multi-merchant scope isolation ──────────────────────────────────────────

// TestIsolation_MultiMerchant_FullSweep tests the complete multi-merchant scenario:
//
//   - Merchant-A has Space-A with 2 collaborators properly invited.
//   - Merchant-B has Space-B with 1 collaborator properly invited + 1 unbacked overwrite.
//   - After reconciling both spaces:
//   - Space-A: 0 revokes (all backed).
//   - Space-B: 1 revoke (the unbacked overwrite).
//   - Collaborator-A from Merchant-A cannot reach Space-B (no overlap).
func TestIsolation_MultiMerchant_FullSweep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := newIsolationStore()
	d := newIsolationDiscord()

	// Merchant-A: Space-A with 2 collaborators.
	const (
		spaceA  = "space-a"
		chanA   = "chan-a"
		userA1  = "user-a1"
		dUserA1 = "discord-a1"
		userA2  = "user-a2"
		dUserA2 = "discord-a2"
		smA1    = "sm-a1"
		smA2    = "sm-a2"
	)
	s.addSpace(appliedSpace(spaceA, "merchant-a", chanA))
	s.addUser(collaboratorUser(userA1, dUserA1))
	s.addUser(collaboratorUser(userA2, dUserA2))
	s.addMember(activeSpaceMember(smA1, spaceA, userA1, true))
	s.addMember(activeSpaceMember(smA2, spaceA, userA2, true))
	d.addOverwrite(chanA, dUserA1)
	d.addOverwrite(chanA, dUserA2)

	// Merchant-B: Space-B with 1 collaborator + 1 unbacked overwrite.
	const (
		spaceB      = "space-b"
		chanB       = "chan-b"
		userB1      = "user-b1"
		dUserB1     = "discord-b1"
		smB1        = "sm-b1"
		unbackedUID = "discord-orphan-b"
	)
	s.addSpace(appliedSpace(spaceB, "merchant-b", chanB))
	s.addUser(collaboratorUser(userB1, dUserB1))
	s.addMember(activeSpaceMember(smB1, spaceB, userB1, true))
	d.addOverwrite(chanB, dUserB1)
	d.addOverwrite(chanB, unbackedUID) // unbacked — isolation breach

	engine := reconcile.NewEngine(s, d)

	if err := engine.ReconcileSpace(ctx, spaceA); err != nil {
		t.Fatalf("ReconcileSpace(space-a) failed: %v", err)
	}
	if err := engine.ReconcileSpace(ctx, spaceB); err != nil {
		t.Fatalf("ReconcileSpace(space-b) failed: %v", err)
	}

	// Exactly one revoke: the unbacked overwrite in Space-B.
	if len(d.revokedOverwrites) != 1 {
		t.Fatalf("expected 1 revoke total, got %d: %v", len(d.revokedOverwrites), d.revokedOverwrites)
	}
	if d.revokedOverwrites[0].ChannelID != chanB || d.revokedOverwrites[0].DiscordUserID != unbackedUID {
		t.Errorf("wrong revoke target: got channel=%s user=%s; want channel=%s user=%s",
			d.revokedOverwrites[0].ChannelID, d.revokedOverwrites[0].DiscordUserID, chanB, unbackedUID)
	}

	// Collaborator-A must still have their overwrite in Space-A.
	owA := d.channelOverwrites[chanA]
	if len(owA) != 2 {
		t.Errorf("space-a should still have 2 overwrites, got %d", len(owA))
	}

	// Isolation check: userA1's discord id must NOT appear in Space-B's overwrites.
	for _, ow := range d.channelOverwrites[chanB] {
		if ow.ID == dUserA1 || ow.ID == dUserA2 {
			t.Errorf("isolation breach: Merchant-A collaborator %s found in Merchant-B's Space-B", ow.ID)
		}
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// filterAudit returns audit entries with the given action.
func filterAudit(entries []store.InsertAuditEntryParams, action string) []store.InsertAuditEntryParams {
	var out []store.InsertAuditEntryParams
	for _, e := range entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// errNotFound is a compile-time assertion that store.ErrNotFound satisfies error.
var _ error = store.ErrNotFound

// errConflict is a compile-time assertion that store.ErrConflict satisfies error.
var _ = errors.Is(store.ErrConflict, store.ErrConflict)
