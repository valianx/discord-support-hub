// Package reconcile implements the desired-vs-real diff and repair engine (§4.2).
//
// M6 pivot (AC-M6-8): reconciliation is now role-based, not overwrite-based.
//
//   - Desired state: active space_members rows for a space (Postgres always wins).
//   - Real state:    guild members currently holding the merchant role in Discord.
//   - Rule 1: member holds role but is NOT in Postgres → strip role (RemoveMerchantRole).
//   - Rule 2: member IS in Postgres but does NOT hold role → assign role (AssignMerchantRole).
//
// The M5 circuit breaker is preserved (only ErrNotFound omits a member; any other store
// error ABORTS the space reconcile — no revocations on a partial set).
//
// Reconcile triggers (§4.3):
//   - Scheduled full-guild sweep (M5): ReconcileGuild across all active spaces.
//   - Post-mutation targeted sweep: ReconcileSpace for the affected space.
//
// Safety invariant (SEC-M5-001): a transient store error during desired-set construction
// ABORTS the space reconcile without revoking anything.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/store"
)

// discordReconcile is the minimal Discord interface needed by the reconciler (M6 role-based).
// Declared locally to keep the dependency surface small and mockable.
type discordReconcile interface {
	// GetGuildMembersByRole returns Discord user ids of all guild members holding roleID.
	GetGuildMembersByRole(ctx context.Context, guildID, roleID string) ([]string, error)
	// AssignMerchantRole adds the merchant role to a guild member (repair path).
	AssignMerchantRole(ctx context.Context, guildID, discordUserID, roleID string) error
	// RemoveMerchantRole strips the merchant role from a guild member (revocation path).
	RemoveMerchantRole(ctx context.Context, guildID, discordUserID, roleID string) error
}

// storeReconcile is the minimal store interface needed by the reconciler.
type storeReconcile interface {
	GetSpaceByID(ctx context.Context, id string) (*domain.Space, error)
	ListActiveSpaceMembers(ctx context.Context, spaceID string) ([]*domain.SpaceMember, error)
	GetUserByID(ctx context.Context, id string) (*domain.User, error)
	InsertAuditEntry(ctx context.Context, entry store.InsertAuditEntryParams) error
	UpdateSpaceReconciledAt(ctx context.Context, spaceID string) error
	// ListActiveProvisionedSpaces returns all lifecycle=active spaces with a channel ID.
	// Used by the scheduled full-guild sweep (M5, AC-5).
	ListActiveProvisionedSpaces(ctx context.Context) ([]*domain.Space, error)
}

// Engine implements the reconcile engine against real Postgres and Discord dependencies.
type Engine struct {
	store   storeReconcile
	discord discordReconcile
	guildID string
	locker  lock.Locker            // nil → locking disabled (tests)
	metrics *observability.Metrics // nil → no-op (AC-2)
}

// NewEngine creates a reconcile Engine with no distributed lock and no metrics.
// Use NewEngineWithLocker (and optionally WithMetrics) for production instances.
func NewEngine(s storeReconcile, d discordReconcile, guildID string) *Engine {
	return &Engine{store: s, discord: d, guildID: guildID}
}

// NewEngineWithLocker creates a reconcile Engine that acquires a per-space distributed
// lock before each ReconcileSpace call (SEC-M5-002).
func NewEngineWithLocker(s storeReconcile, d discordReconcile, guildID string, l lock.Locker) *Engine {
	return &Engine{store: s, discord: d, guildID: guildID, locker: l}
}

// WithMetrics attaches a Prometheus metrics instance to the Engine.
func (e *Engine) WithMetrics(m *observability.Metrics) *Engine {
	e.metrics = m
	return e
}

// ReconcileGuild performs a full sweep across all active provisioned spaces (M5, AC-5).
// It loads all lifecycle=active spaces with a discord_channel_id from Postgres and runs
// ReconcileSpace for each one.
func (e *Engine) ReconcileGuild(ctx context.Context, guildID string) error {
	spaces, err := e.store.ListActiveProvisionedSpaces(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: guild sweep: list spaces: %w", err)
	}

	// fix(AC-2): update the active-spaces gauge each sweep.
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

// ReconcileSpace performs a targeted sweep for a single space (§4.2, §4.3, AC-M6-8).
//
// Algorithm (M6 role-based):
//  1. Acquire a per-space lock (SEC-M5-002). Skip if another sweep holds the lock.
//  2. Load the space. If not provisioned or merchant role absent, skip.
//  3. Load all active space_members rows (desired state).
//  4. Build desired set: discordUserID set from resolved user rows.
//     ErrNotFound = member genuinely gone → omit. Any other error → ABORT (circuit breaker).
//  5. Fetch real state: guild members currently holding the merchant role.
//  6. Rule 1: member holds role but NOT in desired → RemoveMerchantRole.
//  7. Rule 2: member in desired but NOT holding role → AssignMerchantRole.
func (e *Engine) ReconcileSpace(ctx context.Context, spaceID string) error {
	// fix(SEC-M5-002): per-space lock prevents overlapping sweeps.
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
		slog.InfoContext(ctx, "reconcile: space not provisioned, skipping",
			"space_id", spaceID, "acl_state", sp.ACLState)
		return nil
	}
	if sp.MerchantRoleID == nil || *sp.MerchantRoleID == "" {
		// No merchant role yet — provision worker has not created it. Skip gracefully.
		slog.InfoContext(ctx, "reconcile: space has no merchant_role_id yet, skipping",
			"space_id", spaceID)
		return nil
	}
	merchantRoleID := *sp.MerchantRoleID

	// Load desired state: active space_members rows.
	members, err := e.store.ListActiveSpaceMembers(ctx, spaceID)
	if err != nil {
		return fmt.Errorf("reconcile: list active space_members for %s: %w", spaceID, err)
	}

	// Build desired set: discordUserID → struct{} (AC-M6-8 role-based).
	//
	// Circuit breaker (SEC-M5-001 preserved):
	//   - ErrNotFound on a user row → member genuinely gone → omit from desired set.
	//   - Any other error → ABORT the space reconcile to prevent unsafe revocations.
	desiredDiscordIDs, err := e.buildDesiredSet(ctx, spaceID, members)
	if err != nil {
		return err
	}

	// Fetch real state: guild members currently holding the merchant role.
	realDiscordIDs, err := e.discord.GetGuildMembersByRole(ctx, e.guildID, merchantRoleID)
	if err != nil {
		return fmt.Errorf("reconcile: get guild members by role %s for space %s: %w",
			merchantRoleID, spaceID, err)
	}
	realSet := make(map[string]struct{}, len(realDiscordIDs))
	for _, id := range realDiscordIDs {
		realSet[id] = struct{}{}
	}

	// Circuit breaker: if desired set is empty but real set is non-empty AND we have
	// member rows, the desired set build may have silently failed — abort.
	if len(members) > 0 && len(desiredDiscordIDs) == 0 && len(realSet) > 0 {
		slog.ErrorContext(ctx, "reconcile: circuit breaker — members in Postgres but desired set is empty; aborting",
			"space_id", spaceID,
			"member_rows", len(members),
			"real_role_holders", len(realSet))
		return fmt.Errorf("reconcile: circuit breaker: space %s has %d member row(s) but desired Discord set is empty while role has %d holder(s); "+
			"aborting to prevent mass-revocation",
			spaceID, len(members), len(realSet))
	}

	driftFound, driftRepaired := 0, 0

	// Rule 1: real holds role but NOT in desired → strip role (Postgres wins).
	for discordUserID := range realSet {
		if _, wanted := desiredDiscordIDs[discordUserID]; !wanted {
			driftFound++
			slog.WarnContext(ctx, "reconcile: stripping merchant role from non-member",
				"space_id", spaceID, "discord_user_id", discordUserID)
			if rErr := e.discord.RemoveMerchantRole(ctx, e.guildID, discordUserID, merchantRoleID); rErr != nil {
				slog.ErrorContext(ctx, "reconcile: failed to remove merchant role",
					"space_id", spaceID, "discord_user_id", discordUserID, "error", rErr)
				continue
			}
			_ = e.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
				Action:  "reconcile.repair",
				SpaceID: &spaceID,
				Detail: map[string]any{
					"action":           "remove_unbacked_role",
					"discord_user_id":  discordUserID,
					"merchant_role_id": merchantRoleID,
				},
			})
			driftRepaired++
		}
	}

	// Rule 2: in desired but NOT holding role → assign role (repair).
	for discordUserID := range desiredDiscordIDs {
		if _, hasRole := realSet[discordUserID]; !hasRole {
			driftFound++
			slog.WarnContext(ctx, "reconcile: assigning missing merchant role",
				"space_id", spaceID, "discord_user_id", discordUserID)
			if rErr := e.discord.AssignMerchantRole(ctx, e.guildID, discordUserID, merchantRoleID); rErr != nil {
				slog.ErrorContext(ctx, "reconcile: failed to assign merchant role",
					"space_id", spaceID, "discord_user_id", discordUserID, "error", rErr)
				continue
			}
			_ = e.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
				Action:  "reconcile.repair",
				SpaceID: &spaceID,
				Detail: map[string]any{
					"action":           "assign_missing_role",
					"discord_user_id":  discordUserID,
					"merchant_role_id": merchantRoleID,
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

// buildDesiredSet resolves the active space_members rows to a Discord user id set.
// ErrNotFound on a user row → member is genuinely gone → omit.
// Any other error → return it immediately (ABORT, circuit breaker, SEC-M5-001).
func (e *Engine) buildDesiredSet(
	ctx context.Context,
	spaceID string,
	members []*domain.SpaceMember,
) (map[string]struct{}, error) {
	desired := make(map[string]struct{}, len(members))
	for _, sm := range members {
		u, uErr := e.store.GetUserByID(ctx, sm.UserID)
		if uErr != nil {
			if errors.Is(uErr, store.ErrNotFound) {
				// User row genuinely gone — omit; their role will be revoked by Rule 1.
				continue
			}
			// Transient error — abort space reconcile to prevent unsafe revocations (SEC-M5-001).
			return nil, fmt.Errorf("reconcile: get user %s for space %s: %w (aborting space reconcile)",
				sm.UserID, spaceID, uErr)
		}
		if u.DiscordUserID == nil {
			// User has not yet claimed a Discord identity — skip (no role to assign/revoke).
			continue
		}
		desired[*u.DiscordUserID] = struct{}{}
	}
	return desired, nil
}

// NoopReconciler is a pass-through used before the real impl is wired.
type NoopReconciler struct{}

func (NoopReconciler) ReconcileGuild(_ context.Context, _ string) error { return nil }
func (NoopReconciler) ReconcileSpace(_ context.Context, _ string) error  { return nil }
