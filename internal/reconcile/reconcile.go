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
//
// Safety invariant (SEC-M5-001): a transient store error (connection blip, timeout,
// pool exhaustion) during desired-set construction ABORTS the space reconcile without
// revoking anything for that space. Only store.ErrNotFound is treated as "member
// genuinely gone". Any other error is returned immediately so asynq retries later.
//
// Circuit breaker: before applying revocations, ReconcileSpace verifies that the
// desired set is not suspiciously empty relative to the real Discord overwrites.
// If the desired set is completely empty while Discord has overwrites the space
// reconcile is aborted with an error to prevent mass-revocation caused by a
// mis-built desired set.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/store"
)

// maxSafeRevokeFraction is the circuit-breaker threshold: if the fraction of
// Discord member overwrites that would be revoked equals or exceeds this value
// (i.e. all of them), the reconcile is aborted. This guards against a desired
// set that came back unexpectedly empty/partial after a partial store read.
//
// A value of 1.0 means "abort only when we would revoke every single overwrite
// AND the desired set is entirely empty" — the most conservative guard.
const maxSafeRevokeFraction = 1.0

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
	// ListActiveProvisionedSpaces returns all lifecycle=active spaces with a channel ID.
	// Used by the scheduled full-guild sweep (M5, AC-5).
	ListActiveProvisionedSpaces(ctx context.Context) ([]*domain.Space, error)
}

// Engine implements Reconciler against real Postgres and Discord dependencies.
type Engine struct {
	store   storeReconcile
	discord discordReconcile
	locker  lock.Locker            // nil → locking disabled (tests that do not exercise the lock path)
	metrics *observability.Metrics // nil → no-op metric recording (AC-2)
}

// NewEngine creates a reconcile Engine with no distributed lock and no metrics.
// Use NewEngineWithLocker (and optionally WithMetrics) for production instances.
func NewEngine(s storeReconcile, d discordReconcile) *Engine {
	return &Engine{store: s, discord: d, locker: nil}
}

// NewEngineWithLocker creates a reconcile Engine that acquires a per-space distributed
// lock before each ReconcileSpace call (SEC-M5-002). Overlapping sweeps skip the space
// rather than doubling Discord calls — the next sweep picks it up.
func NewEngineWithLocker(s storeReconcile, d discordReconcile, l lock.Locker) *Engine {
	return &Engine{store: s, discord: d, locker: l}
}

// WithMetrics attaches a Prometheus metrics instance to the Engine so ReconcileGuild
// can update the hub_active_spaces_total gauge (AC-2).
func (e *Engine) WithMetrics(m *observability.Metrics) *Engine {
	e.metrics = m
	return e
}

// ReconcileGuild performs a full sweep across all active provisioned spaces in the guild
// (M5, AC-5). It loads all lifecycle=active spaces with a discord_channel_id from Postgres
// and runs ReconcileSpace for each one. Postgres always wins — any Discord access not
// backed by a space_members row is revoked.
//
// The guildID parameter is retained for future multi-guild support; in v1 there is one guild
// so it is used only for logging context.
func (e *Engine) ReconcileGuild(ctx context.Context, guildID string) error {
	spaces, err := e.store.ListActiveProvisionedSpaces(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: guild sweep: list spaces: %w", err)
	}

	// fix(AC-2): update the active-spaces gauge each sweep so /metrics reflects the
	// current number of provisioned spaces without requiring a separate DB query.
	observability.SetActiveSpaces(e.metrics, float64(len(spaces)))

	slog.InfoContext(ctx, "reconcile: guild sweep started",
		"guild_id", guildID,
		"space_count", len(spaces))

	var errs []error
	for _, sp := range spaces {
		if err := e.ReconcileSpace(ctx, sp.ID); err != nil {
			slog.ErrorContext(ctx, "reconcile: guild sweep: space error",
				"space_id", sp.ID, "error", err)
			errs = append(errs, fmt.Errorf("space %s: %w", sp.ID, err))
		}
	}

	if len(errs) > 0 {
		slog.WarnContext(ctx, "reconcile: guild sweep completed with errors",
			"guild_id", guildID,
			"total", len(spaces),
			"errors", len(errs))
		return fmt.Errorf("reconcile: guild sweep: %d space(s) failed", len(errs))
	}

	slog.InfoContext(ctx, "reconcile: guild sweep complete",
		"guild_id", guildID,
		"total", len(spaces))
	return nil
}

// ReconcileSpace performs a targeted sweep for a single space (§4.2, §4.3).
//
// Algorithm:
//  1. Acquire a per-space lock (SEC-M5-002). Skip if another sweep holds the lock.
//  2. Load the space. If not provisioned or failed, skip.
//  3. Load all active space_members rows (desired state).
//  4. Fetch all PermissionOverwrite entries from Discord for the channel (real state).
//  5. For each Discord member-type overwrite (PermissionOverwriteTypeMember):
//     a. If backed by an active space_members row → no action.
//     b. If NOT backed by any space_members row → revoke (Postgres wins, isolation teeth).
//  6. For each active space_members row with a known discord_user_id:
//     a. If no corresponding Discord overwrite exists → re-apply.
func (e *Engine) ReconcileSpace(ctx context.Context, spaceID string) error {
	// fix(SEC-M5-002): acquire a per-space lock to prevent overlapping sweeps from
	// doubling Discord calls. If the lock is held, skip this space — the current sweep
	// will pick it up. The locker is nil in tests that do not exercise the lock path.
	if e.locker != nil {
		token, ok, lockErr := e.locker.AcquireSpace(ctx, spaceID)
		if lockErr != nil {
			return fmt.Errorf("reconcile: acquire space lock %s: %w", spaceID, lockErr)
		}
		if !ok {
			slog.InfoContext(ctx, "reconcile: space lock held by another sweep, skipping",
				"space_id", spaceID)
			return nil
		}
		defer func() { _ = e.locker.ReleaseSpace(ctx, spaceID, token) }()
	}

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
	//
	// fix(SEC-M5-001): distinguish between "member not found in store" (ErrNotFound —
	// genuinely gone, omit from desired set) and any other error (transient store failure
	// — abort the whole space reconcile so we don't revoke against a partial set).
	desiredByDiscordID := make(map[string]string, len(members))
	for _, sm := range members {
		u, uErr := e.store.GetUserByID(ctx, sm.UserID)
		if uErr != nil {
			if errors.Is(uErr, store.ErrNotFound) {
				// Member's user row is genuinely gone — omit from desired set.
				// Their Discord overwrite will be revoked by Rule 1 below.
				continue
			}
			// Transient error (connection blip, timeout, pool exhaustion, etc.).
			// Abort this space's reconcile — do NOT revoke anything on a partial set.
			return fmt.Errorf("reconcile: get user %s for space %s: %w (aborting space reconcile to prevent unsafe revocations)",
				sm.UserID, spaceID, uErr)
		}
		if u.DiscordUserID == nil {
			// User has no Discord identity yet — not yet eligible for overwrites, skip.
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

	// Circuit breaker (SEC-M5-001): if the desired set is entirely empty WHILE the
	// space_members list is non-empty AND Discord has member overwrites, the desired set
	// is suspiciously empty — the per-member user resolutions all returned ErrNotFound
	// or had no DiscordUserID, which is implausible when members are registered.
	//
	// We do NOT fire when len(members)==0: an empty member list genuinely means no one
	// is supposed to have access, so revoking all Discord overwrites is the correct outcome.
	//
	// Fire condition: space_members rows exist, but NONE of them resolved to a Discord
	// user ID, AND Discord still has overwrites. This is the "partial/silent read failure"
	// fingerprint that precedes a mass-revocation event.
	if len(members) > 0 && len(desiredByDiscordID) == 0 && len(realMemberOverwrites) > 0 {
		slog.ErrorContext(ctx, "reconcile: circuit breaker tripped — space has members but desired set is empty and Discord has overwrites; aborting to prevent mass-revocation",
			"space_id", spaceID,
			"member_row_count", len(members),
			"real_overwrite_count", len(realMemberOverwrites))
		return fmt.Errorf("reconcile: circuit breaker: space %s has %d member row(s) but desired set is empty while Discord has %d overwrite(s); "+
			"aborting space reconcile to prevent mass-revocation (retry will rebuild the desired set)",
			spaceID, len(members), len(realMemberOverwrites))
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
