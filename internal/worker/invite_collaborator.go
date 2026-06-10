// invite_collaborator.go implements the KindInviteCollaborator worker handler (M3/M6).
//
// M6 pivot: collaborators are granted access via a merchant invite-with-role link, not
// per-user permission overwrites. This handler's responsibility is now limited to:
//  1. Decode InviteCollaboratorPayload.
//  2. Acquire the per-space distributed lock.
//  3. Verify the space is provisioned and the space_member row exists.
//  4. Write an audit entry confirming the invite task was processed.
//  5. Enqueue a post-mutation targeted reconcile for the space.
//
// Actual email delivery is handled by the KindSendInvite task on the notify queue.
// Guild-join and per-user overwrites are removed (AC-M6-9).
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

// inviteCollaboratorConfig carries all dependencies for the invite handler.
type inviteCollaboratorConfig struct {
	store   store.Store
	discord discordInvite
	locker  lock.Locker
	guildID string
}

// discordInvite is the Discord sub-interface needed by the invite handler.
// M6: narrowed — no AddGuildMember or SetCollaboratorOverwrite needed post-pivot.
type discordInvite interface {
	// Placeholder to keep the interface non-empty. Future M7+ methods go here.
	// The provision handler holds the role-assignment surface.
}

type inviteCollaboratorHandler struct {
	cfg inviteCollaboratorConfig
}

func newInviteCollaboratorHandler(cfg inviteCollaboratorConfig) asynq.HandlerFunc {
	if cfg.store == nil {
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

	// Acquire per-space lock to prevent concurrent membership mutations (§3.3).
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

	// Load the space to verify it is provisioned.
	sp, err := h.cfg.store.GetSpaceByID(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("invite_collaborator: load space: %w", err)
	}
	if sp.DiscordChannelID == nil || sp.ACLState != domain.ACLStateApplied {
		// Space not yet provisioned or degraded — retry later.
		return fmt.Errorf("invite_collaborator: space %s not yet provisioned (acl_state=%s); retry later",
			payload.SpaceID, sp.ACLState)
	}

	// Verify the space_member row still exists (desired state).
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

	// Write audit entry — access is granted via the merchant invite link (AC-M6-9).
	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:       "collaborator.invite_processed",
		SpaceID:      &payload.SpaceID,
		TargetUserID: &payload.UserID,
		ActorUserID:  nilIfEmpty(payload.InvitedBy),
		Detail: map[string]any{
			"space_member_id": sm.ID,
			"channel_id":      *sp.DiscordChannelID,
			// M6: access is via merchant invite link, not per-user overwrite
		},
	})

	slog.InfoContext(ctx, "invite_collaborator: processed",
		"space_id", payload.SpaceID, "user_id", payload.UserID,
		"space_member_id", sm.ID)
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
