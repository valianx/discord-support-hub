// expel_collaborator.go implements the KindExpelCollaborator worker handler (M3, §6.3).
//
// Expulsion scopes (FR-19):
//   - scope=channel (default): revoke the per-user overwrite on the space channel.
//     The user stays in the guild. The space_member row is soft-deleted (revoked_at set).
//   - scope=server: also remove the user from the guild entirely (GuildMemberRemove).
//
// Both scopes write an audit entry. The reconciler will verify the Discord state matches
// on its next targeted pass (§4.3 post-mutation reconcile).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// discordExpel is the Discord sub-interface needed by the expel handler.
type discordExpel interface {
	DeleteCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error
	RemoveGuildMember(ctx context.Context, guildID, discordUserID string) error
}

type expelCollaboratorConfig struct {
	store   store.Store
	discord discordExpel
	locker  lock.Locker
	guildID string
}

type expelCollaboratorHandler struct {
	cfg expelCollaboratorConfig
}

func newExpelCollaboratorHandler(cfg expelCollaboratorConfig) asynq.HandlerFunc {
	if cfg.store == nil || cfg.discord == nil {
		return stubHandler(queue.KindExpelCollaborator)
	}
	if cfg.locker == nil {
		cfg.locker = lock.NoopLocker{}
	}
	h := &expelCollaboratorHandler{cfg: cfg}
	return h.handle
}

func (h *expelCollaboratorHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.ExpelCollaboratorPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: decode expel payload: %v", asynq.SkipRetry, err)
	}

	if payload.SpaceID == "" || payload.UserID == "" {
		return fmt.Errorf("%w: expel_collaborator: payload missing space_id or user_id", asynq.SkipRetry)
	}

	slog.InfoContext(ctx, "expel_collaborator: starting",
		"space_id", payload.SpaceID, "user_id", payload.UserID, "scope", payload.Scope)

	// Acquire per-space lock.
	token, ok, err := h.cfg.locker.AcquireSpace(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("expel_collaborator: acquire space lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("expel_collaborator: space %s lock held; retry later", payload.SpaceID)
	}
	defer func() { _ = h.cfg.locker.ReleaseSpace(ctx, payload.SpaceID, token) }()

	// Load the space.
	sp, err := h.cfg.store.GetSpaceByID(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("expel_collaborator: load space: %w", err)
	}

	// Load the user for their discord_user_id.
	user, err := h.cfg.store.GetUserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: expel_collaborator: user %s not found", asynq.SkipRetry, payload.UserID)
		}
		return fmt.Errorf("expel_collaborator: load user: %w", err)
	}

	// Load the space_member row (need its ID to revoke).
	sm, err := h.cfg.store.GetSpaceMemberBySpaceAndUser(ctx, payload.SpaceID, payload.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.InfoContext(ctx, "expel_collaborator: space_member not found (already expelled?), skipping",
				"space_id", payload.SpaceID, "user_id", payload.UserID)
			return nil
		}
		return fmt.Errorf("expel_collaborator: load space_member: %w", err)
	}

	// Step 1: revoke the overwrite (always, for both scopes).
	if sp.DiscordChannelID != nil && user.DiscordUserID != nil {
		if overwErr := h.cfg.discord.DeleteCollaboratorOverwrite(
			ctx, *sp.DiscordChannelID, *user.DiscordUserID,
		); overwErr != nil {
			slog.WarnContext(ctx, "expel_collaborator: delete overwrite failed (may not exist)",
				"space_id", payload.SpaceID, "user_id", payload.UserID, "error", overwErr)
			// Non-fatal: the overwrite may have already been deleted; continue.
		}
	}

	// Step 2: server scope — also remove from guild.
	if payload.Scope == string(domain.ExpulsionScopeServer) {
		if user.DiscordUserID != nil {
			if rmErr := h.cfg.discord.RemoveGuildMember(ctx, h.cfg.guildID, *user.DiscordUserID); rmErr != nil {
				slog.WarnContext(ctx, "expel_collaborator: remove guild member failed",
					"user_id", payload.UserID, "error", rmErr)
				// Non-fatal: log and continue. The audit entry records the attempt.
			}
		}
	}

	// Step 3: soft-delete the space_member row (revoked_at set, row kept for audit).
	if sm.RevokedAt == nil {
		if _, err := h.cfg.store.RevokeSpaceMember(ctx, sm.ID); err != nil {
			return fmt.Errorf("expel_collaborator: revoke space_member: %w", err)
		}
	}

	// Step 4: write audit entry.
	scope := domain.ExpulsionScope(payload.Scope)
	if scope == "" {
		scope = domain.ExpulsionScopeChannel
	}
	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:       "collaborator.expel",
		SpaceID:      &payload.SpaceID,
		TargetUserID: &payload.UserID,
		Scope:        &scope,
		Detail: map[string]any{
			"scope": payload.Scope,
		},
	})

	slog.InfoContext(ctx, "expel_collaborator: completed",
		"space_id", payload.SpaceID, "user_id", payload.UserID, "scope", payload.Scope)
	return nil
}
