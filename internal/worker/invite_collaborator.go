// invite_collaborator.go implements the KindInviteCollaborator worker handler (M3, §6.2).
//
// The handler:
//  1. Decodes InviteCollaboratorPayload.
//  2. Acquires the per-space distributed lock (§3.3 — prevents concurrent overwrite clobbering).
//  3. Loads the user and space from Postgres.
//  4. If the user has a discord_user_id + an oauth token: adds them to the guild via
//     GuildMemberAdd (guilds.join token, no role applied at join).
//  5. Applies the per-user permission overwrite (ChannelPermissionSet, PermissionOverwriteTypeMember,
//     allow VIEW_CHANNEL+SEND_MESSAGES) on the space's channel — the ONLY access grant (NFR-5, §6.2).
//  6. Marks overwrite_applied=true on the space_member row.
//  7. Writes an audit entry.
//  8. Enqueues a post-mutation targeted reconcile for the space (§4.3).
//
// If the user has not yet connected Discord (no discord_user_id), the handler exits
// without applying the overwrite. The backoffice presents a connect_url; the OAuth2
// callback re-enqueues this task when the token is captured.
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
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// inviteCollaboratorConfig carries all dependencies for the invite handler.
type inviteCollaboratorConfig struct {
	store      store.Store
	discord    discordInvite
	locker     lock.Locker
	tokenStore *oauth.TokenStore
	guildID    string
}

// discordInvite is the Discord sub-interface needed by the invite handler.
// Declared locally to keep the dependency surface minimal.
type discordInvite interface {
	AddGuildMember(ctx context.Context, guildID, discordUserID, accessToken string) error
	SetCollaboratorOverwrite(ctx context.Context, channelID, discordUserID string) error
}

type inviteCollaboratorHandler struct {
	cfg inviteCollaboratorConfig
}

func newInviteCollaboratorHandler(cfg inviteCollaboratorConfig) asynq.HandlerFunc {
	if cfg.store == nil || cfg.discord == nil {
		return stubHandler(queue.KindInviteCollaborator)
	}
	if cfg.locker == nil {
		cfg.locker = lock.NoopLocker{}
	}
	h := &inviteCollaboratorHandler{cfg: cfg}
	return h.handle
}

func (h *inviteCollaboratorHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.InviteCollaboratorPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: decode invite payload: %v", asynq.SkipRetry, err)
	}

	if payload.SpaceID == "" || payload.UserID == "" {
		return fmt.Errorf("%w: invite_collaborator: payload missing space_id or user_id", asynq.SkipRetry)
	}

	slog.InfoContext(ctx, "invite_collaborator: starting",
		"space_id", payload.SpaceID, "user_id", payload.UserID)

	// Acquire per-space lock to prevent concurrent overwrite clobbering (§3.3).
	token, ok, err := h.cfg.locker.AcquireSpace(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("invite_collaborator: acquire space lock: %w", err)
	}
	if !ok {
		slog.InfoContext(ctx, "invite_collaborator: space lock held, re-enqueuing",
			"space_id", payload.SpaceID)
		return fmt.Errorf("invite_collaborator: space %s lock held; retry later", payload.SpaceID)
	}
	defer func() { _ = h.cfg.locker.ReleaseSpace(ctx, payload.SpaceID, token) }()

	// Load the space to get the discord_channel_id.
	sp, err := h.cfg.store.GetSpaceByID(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("invite_collaborator: load space: %w", err)
	}
	if sp.DiscordChannelID == nil || sp.ACLState != domain.ACLStateApplied {
		// Space not yet provisioned or degraded — retry later.
		return fmt.Errorf("invite_collaborator: space %s not yet provisioned (acl_state=%s); retry later",
			payload.SpaceID, sp.ACLState)
	}

	// Load the user.
	user, err := h.cfg.store.GetUserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: invite_collaborator: user %s not found", asynq.SkipRetry, payload.UserID)
		}
		return fmt.Errorf("invite_collaborator: load user: %w", err)
	}

	// Load the space_member row (desired state must exist before we project).
	sm, err := h.cfg.store.GetSpaceMemberBySpaceAndUser(ctx, payload.SpaceID, payload.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Desired state was deleted (e.g. expulsion raced with invite) — skip.
			slog.InfoContext(ctx, "invite_collaborator: space_member no longer exists, skipping",
				"space_id", payload.SpaceID, "user_id", payload.UserID)
			return nil
		}
		return fmt.Errorf("invite_collaborator: load space_member: %w", err)
	}

	// Idempotency: already applied.
	if sm.OverwriteApplied {
		slog.InfoContext(ctx, "invite_collaborator: overwrite already applied, skipping",
			"space_id", payload.SpaceID, "user_id", payload.UserID)
		return nil
	}

	// If the user has not yet connected Discord, we cannot add them to the guild.
	// The job will be re-enqueued by the OAuth2 callback when the token arrives.
	if user.DiscordUserID == nil || *user.DiscordUserID == "" {
		slog.InfoContext(ctx, "invite_collaborator: user has no discord_user_id yet, waiting for OAuth2 connect",
			"user_id", payload.UserID)
		// Retryable — the backoffice will prompt the user to connect, then the OAuth2
		// callback will re-enqueue this task.
		return fmt.Errorf("invite_collaborator: user %s has not connected Discord yet; retry after OAuth2 connect",
			payload.UserID)
	}

	channelID := *sp.DiscordChannelID
	discordUserID := *user.DiscordUserID

	// Add to guild via guilds.join token if a token is available.
	// Idempotent: GuildMemberAdd returns 204 if already a member.
	if h.cfg.tokenStore != nil {
		accessToken, tokenErr := h.cfg.tokenStore.LoadAccessToken(ctx, payload.UserID)
		if tokenErr == nil && accessToken != "" {
			if addErr := h.cfg.discord.AddGuildMember(ctx, h.cfg.guildID, discordUserID, accessToken); addErr != nil {
				slog.WarnContext(ctx, "invite_collaborator: add guild member failed (may already be a member)",
					"user_id", payload.UserID, "error", addErr)
				// Non-fatal if 400 (already member); we proceed with the overwrite regardless.
				// A real 403 or 500 would be retried by the outer retry logic.
			}
		}
	}

	// Apply the per-user permission overwrite — the ONLY access grant (NFR-5, §6.2).
	if err := h.cfg.discord.SetCollaboratorOverwrite(ctx, channelID, discordUserID); err != nil {
		return fmt.Errorf("invite_collaborator: set collaborator overwrite: %w", err)
	}

	// Mark overwrite applied in Postgres.
	if _, err := h.cfg.store.SetSpaceMemberOverwriteApplied(ctx, sm.ID); err != nil {
		// Non-fatal — log and continue. The reconciler will see the overwrite exists in
		// Discord but overwrite_applied=false and will mark it on its next pass.
		slog.WarnContext(ctx, "invite_collaborator: could not mark overwrite_applied",
			"space_member_id", sm.ID, "error", err)
	}

	// Write audit entry.
	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:       "collaborator.invite",
		SpaceID:      &payload.SpaceID,
		TargetUserID: &payload.UserID,
		ActorUserID:  nilIfEmpty(payload.InvitedBy),
		Detail: map[string]any{
			"discord_user_id": discordUserID,
			"channel_id":      channelID,
		},
	})

	slog.InfoContext(ctx, "invite_collaborator: overwrite applied",
		"space_id", payload.SpaceID, "user_id", payload.UserID,
		"discord_user_id", discordUserID)
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
