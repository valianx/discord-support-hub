// change_lifecycle.go implements the KindChangeLifecycle worker handler (M4, FR-7).
//
// Lifecycle state transitions (AC-1, AC-6):
//   - archive:  lock + hide the channel (deny @everyone VIEW_CHANNEL + SEND_MESSAGES),
//     set lifecycle_state='archived' + archived_at. History NEVER deleted.
//   - reopen:   remove the archive overwrite, set lifecycle_state='active'.
//   - open:     set lifecycle_state='active' (no Discord change if already active).
//   - resolve:  set lifecycle_state='resolved' (no Discord visibility change).
//
// The handler advances the jobs mirror row (pending→active→completed) so GET /jobs/{id}
// reflects the real outcome (M4 job-mirror fix).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// discordLifecycle is the Discord sub-interface needed by the lifecycle handler.
type discordLifecycle interface {
	ArchiveChannel(ctx context.Context, channelID, everyoneRoleID string) error
	UnarchiveChannel(ctx context.Context, channelID, everyoneRoleID string) error
}

// lifecycleConfig carries dependencies for the change_lifecycle handler.
type lifecycleConfig struct {
	store          store.Store
	discord        discordLifecycle
	guildID        string
	everyoneRoleID string
}

type changeLifecycleHandler struct {
	cfg lifecycleConfig
}

func newChangeLifecycleHandler(cfg lifecycleConfig) asynq.HandlerFunc {
	if cfg.store == nil || cfg.discord == nil {
		return stubHandler(queue.KindChangeLifecycle)
	}
	if cfg.everyoneRoleID == "" {
		cfg.everyoneRoleID = cfg.guildID
	}
	h := &changeLifecycleHandler{cfg: cfg}
	return h.handle
}

func (h *changeLifecycleHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.ChangeLifecyclePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: decode lifecycle payload: %v", asynq.SkipRetry, err)
	}

	if payload.SpaceID == "" || payload.Action == "" {
		return fmt.Errorf("%w: change_lifecycle: payload missing space_id or action", asynq.SkipRetry)
	}

	slog.InfoContext(ctx, "change_lifecycle: starting",
		"space_id", payload.SpaceID, "action", payload.Action)

	// Advance job to active.
	h.advanceJob(ctx, payload.SpaceID, domain.JobStatusActive, nil)

	sp, err := h.cfg.store.GetSpaceByID(ctx, payload.SpaceID)
	if err != nil {
		h.advanceJob(ctx, payload.SpaceID, domain.JobStatusArchived, &err)
		return fmt.Errorf("change_lifecycle: load space %s: %w", payload.SpaceID, err)
	}

	targetState, ok := actionToTargetState(payload.Action)
	if !ok {
		skipErr := fmt.Errorf("%w: change_lifecycle: unknown action %q", asynq.SkipRetry, payload.Action)
		h.advanceJob(ctx, payload.SpaceID, domain.JobStatusArchived, &skipErr)
		return skipErr
	}

	// Apply the Discord-side change when a channel exists.
	if sp.DiscordChannelID != nil {
		if err := h.applyDiscordChange(ctx, *sp.DiscordChannelID, payload.Action); err != nil {
			slog.ErrorContext(ctx, "change_lifecycle: discord change failed",
				"space_id", payload.SpaceID, "action", payload.Action, "error", err)
			h.advanceJob(ctx, payload.SpaceID, domain.JobStatusArchived, &err)
			return fmt.Errorf("change_lifecycle: %s: %w", payload.Action, err)
		}
	}

	// Persist the new lifecycle state (and archived_at when archiving).
	if _, err := h.cfg.store.UpdateSpaceLifecycle(ctx, store.UpdateSpaceLifecycleParams{
		SpaceID:        payload.SpaceID,
		LifecycleState: targetState,
	}); err != nil {
		h.advanceJob(ctx, payload.SpaceID, domain.JobStatusArchived, &err)
		return fmt.Errorf("change_lifecycle: persist lifecycle state: %w", err)
	}

	// Write audit entry.
	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:  "space.lifecycle." + payload.Action,
		SpaceID: &payload.SpaceID,
		Detail:  map[string]any{"action": payload.Action, "new_state": string(targetState)},
	})

	h.advanceJob(ctx, payload.SpaceID, domain.JobStatusCompleted, nil)

	slog.InfoContext(ctx, "change_lifecycle: completed",
		"space_id", payload.SpaceID, "action", payload.Action, "new_state", targetState)
	return nil
}

// applyDiscordChange performs the Discord API call for the given action.
// archive locks/hides the channel; reopen/open restores it.
func (h *changeLifecycleHandler) applyDiscordChange(ctx context.Context, channelID, action string) error {
	switch action {
	case "archive":
		return h.cfg.discord.ArchiveChannel(ctx, channelID, h.cfg.everyoneRoleID)
	case "reopen", "open":
		return h.cfg.discord.UnarchiveChannel(ctx, channelID, h.cfg.everyoneRoleID)
	case "resolve":
		// resolve = no Discord visibility change; only Postgres state changes.
		return nil
	default:
		return fmt.Errorf("unknown lifecycle action: %q", action)
	}
}

// actionToTargetState maps the action string to the domain lifecycle state.
func actionToTargetState(action string) (domain.SpaceLifecycleState, bool) {
	switch action {
	case "open", "reopen":
		return domain.SpaceLifecycleActive, true
	case "resolve":
		return domain.SpaceLifecycleResolved, true
	case "archive":
		return domain.SpaceLifecycleArchived, true
	default:
		return "", false
	}
}

// advanceJob looks up and updates the jobs mirror row for the space's lifecycle job.
func (h *changeLifecycleHandler) advanceJob(
	ctx context.Context,
	spaceID string,
	status domain.JobStatus,
	cause *error,
) {
	job, err := h.cfg.store.GetJobBySpaceIDAndKind(ctx, spaceID, queue.KindChangeLifecycle)
	if err != nil || job == nil {
		return
	}
	params := store.UpdateJobStatusParams{
		JobID:     job.ID,
		Status:    status,
		Completed: status == domain.JobStatusCompleted,
	}
	if cause != nil {
		msg := (*cause).Error()
		params.Error = &msg
	}
	if _, err := h.cfg.store.UpdateJobStatus(ctx, params); err != nil {
		slog.WarnContext(ctx, "change_lifecycle: could not update job status",
			"job_id", job.ID, "status", status, "error", err)
	}
}
