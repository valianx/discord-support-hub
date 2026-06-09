// Package reconcile implements the desired-vs-real diff and repair engine (§4.2).
//
// Postgres always wins: any Discord overwrite not backed by a space_members row is revoked.
// Any space_members row without a corresponding Discord overwrite is re-applied.
//
// Reconcile triggers (§4.3):
//   - Scheduled full-guild sweep (M5): ReconcileGuild across all active spaces.
//   - Post-mutation targeted sweep: ReconcileSpace for the affected space.
//   - On-failure sweep (called by workers after a job archives).
//
// The reconciler operates on the discord.Client interface so it can be mocked in tests.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// discordReconcile is the minimal Discord interface needed by the reconciler.
// Declared locally to keep the dependency surface small and mockable.
type discordReconcile interface {
	SetCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error
	DeleteCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error
	GetChannelOverwrites(ctx context.Context, channelID string) ([]*discordgo.PermissionOverwrite, error)
}

// storeReconcile is the minimal store interface needed by the reconciler.
type storeReconcile interface {
	GetSpaceByID(ctx context.Context, id string) (*domain.Space, error)
	ListActiveSpaceMembers(ctx context.Context, spaceID string) ([]*domain.SpaceMember, error)
	GetUserByID(ctx context.Context, id string) (*domain.User, error)
	InsertAuditEntry(ctx context.Context, entry store.InsertAuditEntryParams) error
	UpdateSpaceReconciledAt(ctx context.Context, spaceID string) error
	SetSpaceMemberOverwriteApplied(ctx context.Context, id string) (*domain.SpaceMember, error)
}

// Engine implements Reconciler against real Postgres and Discord dependencies.
type Engine struct {
	store   storeReconcile
	discord discordReconcile
}

// NewEngine creates a reconcile Engine.
func NewEngine(s storeReconcile, d discordReconcile) *Engine {
	return &Engine{store: s, discord: d}
}

// ReconcileGuild performs a full sweep across all provided space IDs.
// In M3 this is a thin wrapper around per-space sweeps. A full scheduled sweep
// enumerating all guild spaces lands in M5.
func (e *Engine) ReconcileGuild(ctx context.Context, _ string) error {
	// Full sweep implementation deferred to M5 (scheduler not yet wired).
	// The M3 reconcile path is post-mutation targeted (ReconcileSpace).
	return nil
}

// ReconcileSpace performs a targeted sweep for a single space (§4.2, §4.3).
//
// Algorithm:
//  1. Load the space. If not provisioned or failed, skip.
//  2. Load all active space_members rows (desired state).
//  3. Fetch all PermissionOverwrite entries from Discord for the channel (real state).
//  4. For each Discord member-type overwrite (PermissionOverwriteTypeMember):
//     a. If backed by an active space_members row → no action.
//     b. If NOT backed by any space_members row → revoke (Postgres wins, isolation teeth).
//  5. For each active space_members row with a known discord_user_id:
//     a. If no corresponding Discord overwrite exists → re-apply.
func (e *Engine) ReconcileSpace(ctx context.Context, spaceID string) error {
	sp, err := e.store.GetSpaceByID(ctx, spaceID)
	if err != nil {
		return fmt.Errorf("reconcile: load space %s: %w", spaceID, err)
	}
	if sp.DiscordChannelID == nil || sp.ACLState != domain.ACLStateApplied {
		// Space not yet provisioned or in a failed state — nothing to reconcile.
		slog.InfoContext(ctx, "reconcile: space not provisioned, skipping",
			"space_id", spaceID, "acl_state", sp.ACLState)
		return nil
	}

	channelID := *sp.DiscordChannelID

	// Load desired state: active space_members rows.
	members, err := e.store.ListActiveSpaceMembers(ctx, spaceID)
	if err != nil {
		return fmt.Errorf("reconcile: list active space_members for %s: %w", spaceID, err)
	}

	// Build desired set: discordUserID → space_member id.
	desiredByDiscordID := make(map[string]string, len(members))
	for _, sm := range members {
		u, uErr := e.store.GetUserByID(ctx, sm.UserID)
		if uErr != nil || u.DiscordUserID == nil {
			continue
		}
		desiredByDiscordID[*u.DiscordUserID] = sm.ID
	}

	// Fetch real state from Discord.
	overwrites, err := e.discord.GetChannelOverwrites(ctx, channelID)
	if err != nil {
		return fmt.Errorf("reconcile: get channel overwrites for %s: %w", channelID, err)
	}

	// Build real set: discordUserID → exists (member-type overwrites only).
	realMemberOverwrites := make(map[string]bool, len(overwrites))
	for _, ow := range overwrites {
		if ow.Type == discordgo.PermissionOverwriteTypeMember {
			realMemberOverwrites[ow.ID] = true
		}
	}

	driftFound, driftRepaired := 0, 0

	// Rule 1: extra in Discord → revoke (isolation teeth, §4.2).
	for discordUserID := range realMemberOverwrites {
		if _, blessed := desiredByDiscordID[discordUserID]; !blessed {
			driftFound++
			slog.WarnContext(ctx, "reconcile: revoking unbacked overwrite (isolation breach risk)",
				"space_id", spaceID, "discord_user_id", discordUserID)
			if err := e.discord.DeleteCollaboratorOverwrite(ctx, channelID, discordUserID); err != nil {
				slog.ErrorContext(ctx, "reconcile: failed to revoke unbacked overwrite",
					"space_id", spaceID, "discord_user_id", discordUserID, "error", err)
				continue
			}
			_ = e.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
				Action:  "reconcile.repair",
				SpaceID: &spaceID,
				Detail: map[string]any{
					"action":          "revoke_unbacked_overwrite",
					"discord_user_id": discordUserID,
				},
			})
			driftRepaired++
		}
	}

	// Rule 2: in Postgres but missing in Discord → re-apply.
	for discordUserID, smID := range desiredByDiscordID {
		if !realMemberOverwrites[discordUserID] {
			driftFound++
			slog.WarnContext(ctx, "reconcile: re-applying missing overwrite",
				"space_id", spaceID, "discord_user_id", discordUserID)
			if err := e.discord.SetCollaboratorOverwrite(ctx, channelID, discordUserID); err != nil {
				slog.ErrorContext(ctx, "reconcile: failed to re-apply overwrite",
					"space_id", spaceID, "discord_user_id", discordUserID, "error", err)
				continue
			}
			// Mark overwrite_applied=true in the space_member row.
			if _, markErr := e.store.SetSpaceMemberOverwriteApplied(ctx, smID); markErr != nil {
				slog.WarnContext(ctx, "reconcile: could not mark overwrite_applied",
					"space_member_id", smID, "error", markErr)
			}
			_ = e.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
				Action:  "reconcile.repair",
				SpaceID: &spaceID,
				Detail: map[string]any{
					"action":          "reapply_missing_overwrite",
					"discord_user_id": discordUserID,
				},
			})
			driftRepaired++
		}
	}

	_ = e.store.UpdateSpaceReconciledAt(ctx, spaceID)

	slog.InfoContext(ctx, "reconcile: space sweep complete",
		"space_id", spaceID,
		"drift_found", driftFound,
		"drift_repaired", driftRepaired)
	return nil
}

// NoopReconciler is a pass-through used before the real impl is wired.
type NoopReconciler struct{}

func (NoopReconciler) ReconcileGuild(_ context.Context, _ string) error { return nil }
func (NoopReconciler) ReconcileSpace(_ context.Context, _ string) error { return nil }
