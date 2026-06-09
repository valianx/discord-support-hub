// project_agent_role.go implements the KindProjectAgentRole worker handler (M1, §6.1).
//
// The handler:
//   - decodes ProjectAgentRolePayload
//   - loads the user from Postgres to confirm they are type=agent and have a discord_user_id
//   - calls GuildMemberRoleAdd (add=true) or GuildMemberRoleRemove (add=false) via the Discord client
//   - on success (add=true): stamps provisioned_at on the user row
//
// AuthZ is always Postgres-resolved: a user row with type!=agent is rejected even if
// the Discord role exists (NFR-13, §5.2).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/discord"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// projectAgentRoleHandler handles KindProjectAgentRole tasks.
type projectAgentRoleHandler struct {
	store       store.Store
	discord     discord.Client
	guildID     string
	agentRoleID string
}

func newProjectAgentRoleHandler(
	s store.Store,
	d discord.Client,
	guildID, agentRoleID string,
) asynq.HandlerFunc {
	if s == nil || d == nil {
		// Fallback to stub when dependencies are not wired (e.g. test or early-stage boot).
		return stubHandler(queue.KindProjectAgentRole)
	}
	h := &projectAgentRoleHandler{
		store:       s,
		discord:     d,
		guildID:     guildID,
		agentRoleID: agentRoleID,
	}
	return h.handle
}

func (h *projectAgentRoleHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.ProjectAgentRolePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		// Bad payload: do not retry — archive immediately.
		return fmt.Errorf("%w: decode payload: %v", asynq.SkipRetry, err)
	}

	user, err := h.store.GetUserByID(ctx, payload.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// User no longer exists — nothing to project.
			slog.WarnContext(ctx, "project_agent_role: user not found, skipping",
				"user_id", payload.UserID)
			return nil
		}
		return fmt.Errorf("project_agent_role: load user: %w", err)
	}

	// AuthZ invariant: only type=agent users get the Agent role (NFR-13).
	if user.Type != domain.UserTypeAgent {
		slog.WarnContext(ctx, "project_agent_role: user is not an agent, skipping",
			"user_id", payload.UserID, "type", user.Type)
		return nil
	}

	// The agent must have joined the guild (i.e. have a discord_user_id) for role projection.
	if user.DiscordUserID == nil || *user.DiscordUserID == "" {
		slog.InfoContext(ctx, "project_agent_role: agent has no discord_user_id yet, deferring",
			"user_id", payload.UserID)
		// Return a retryable error so asynq will retry later via RetryDelayFunc.
		// The reconciler will also catch this drift on its next sweep.
		return fmt.Errorf("project_agent_role: agent %s has not yet connected Discord; retry later",
			payload.UserID)
	}

	if payload.Add {
		return h.assignRole(ctx, user)
	}
	return h.removeRole(ctx, user)
}

func (h *projectAgentRoleHandler) assignRole(ctx context.Context, user *domain.User) error {
	if err := h.discord.AssignAgentRole(ctx, h.guildID, *user.DiscordUserID, h.agentRoleID); err != nil {
		return fmt.Errorf("project_agent_role: assign role: %w", err)
	}

	// Stamp provisioned_at to record that the bot has successfully projected the role.
	if _, err := h.store.SetUserProvisionedAt(ctx, user.ID); err != nil {
		// Non-fatal: the role was assigned. Log and continue; the next reconcile will retry the stamp.
		slog.WarnContext(ctx, "project_agent_role: could not stamp provisioned_at",
			"user_id", user.ID, "error", err)
	}

	slog.InfoContext(ctx, "project_agent_role: agent role assigned",
		"user_id", user.ID, "discord_user_id", *user.DiscordUserID)
	return nil
}

func (h *projectAgentRoleHandler) removeRole(ctx context.Context, user *domain.User) error {
	if err := h.discord.RemoveAgentRole(ctx, h.guildID, *user.DiscordUserID, h.agentRoleID); err != nil {
		return fmt.Errorf("project_agent_role: remove role: %w", err)
	}

	slog.InfoContext(ctx, "project_agent_role: agent role removed",
		"user_id", user.ID, "discord_user_id", *user.DiscordUserID)
	return nil
}
