// sync_welcome.go implements the KindSyncWelcome worker handler (M4, FR-15 static, AC-4).
//
// Sets the channel topic and idempotently pins a help-desk message:
//   - If welcome_message_id is already recorded on the space, edits the existing message
//     (no duplicate pin). If not recorded, sends a new message and pins it.
//   - Records welcome_message_id on the space row so subsequent re-syncs edit in place.
//
// This is the "static help-desk presence" — a fixed topic + pinned message.
// Dynamic FR-15 (sticky/nudge) is deferred to v1.1.
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

// defaultWelcomeMessage is the fallback help-desk message when none is configured.
const defaultWelcomeMessage = "Welcome! This is your dedicated support channel. A team member will assist you shortly."

// defaultChannelTopic is the fallback channel topic when none is configured.
const defaultChannelTopic = "Support channel — ask us anything"

// discordWelcome is the Discord sub-interface needed by the sync_welcome handler.
type discordWelcome interface {
	SetChannelTopic(ctx context.Context, channelID, topic string) error
	SendMessage(ctx context.Context, channelID, content string) (string, error)
	EditMessage(ctx context.Context, channelID, messageID, content string) error
	PinMessage(ctx context.Context, channelID, messageID string) error
}

// syncWelcomeConfig carries dependencies for the sync_welcome handler.
type syncWelcomeConfig struct {
	store   store.Store
	discord discordWelcome
}

type syncWelcomeHandler struct {
	cfg syncWelcomeConfig
}

func newSyncWelcomeHandler(cfg syncWelcomeConfig) asynq.HandlerFunc {
	if cfg.store == nil || cfg.discord == nil {
		return stubHandler(queue.KindSyncWelcome)
	}
	h := &syncWelcomeHandler{cfg: cfg}
	return h.handle
}

func (h *syncWelcomeHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.SyncWelcomePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: decode sync_welcome payload: %v", asynq.SkipRetry, err)
	}

	if payload.SpaceID == "" {
		return fmt.Errorf("%w: sync_welcome: payload missing space_id", asynq.SkipRetry)
	}

	slog.InfoContext(ctx, "sync_welcome: starting", "space_id", payload.SpaceID)

	sp, err := h.cfg.store.GetSpaceByID(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("sync_welcome: load space %s: %w", payload.SpaceID, err)
	}

	if sp.DiscordChannelID == nil {
		return fmt.Errorf("%w: sync_welcome: space %s has no discord_channel_id yet — cannot sync",
			asynq.SkipRetry, payload.SpaceID)
	}

	channelID := *sp.DiscordChannelID
	topic := defaultChannelTopic
	message := payload.Message
	if message == "" {
		message = defaultWelcomeMessage
	}

	// Step 1: set the channel topic.
	if err := h.cfg.discord.SetChannelTopic(ctx, channelID, topic); err != nil {
		return fmt.Errorf("sync_welcome: set channel topic: %w", err)
	}

	// Step 2: idempotent pin — edit existing message if recorded, else send + pin.
	var messageID string
	if sp.WelcomeMessageID != nil {
		// Re-sync: edit the existing pinned message in place (AC-4: no duplicate).
		if err := h.cfg.discord.EditMessage(ctx, channelID, *sp.WelcomeMessageID, message); err != nil {
			slog.WarnContext(ctx, "sync_welcome: edit existing message failed, will send new",
				"space_id", payload.SpaceID, "message_id", *sp.WelcomeMessageID, "error", err)
			// Fall through: the old pin may have been deleted. Send a new message.
			messageID, err = h.cfg.discord.SendMessage(ctx, channelID, message)
			if err != nil {
				return fmt.Errorf("sync_welcome: send new message after edit failure: %w", err)
			}
			if pinErr := h.cfg.discord.PinMessage(ctx, channelID, messageID); pinErr != nil {
				return fmt.Errorf("sync_welcome: pin new message: %w", pinErr)
			}
		} else {
			messageID = *sp.WelcomeMessageID
		}
	} else {
		// First sync: send the message and pin it.
		messageID, err = h.cfg.discord.SendMessage(ctx, channelID, message)
		if err != nil {
			return fmt.Errorf("sync_welcome: send message: %w", err)
		}
		if err := h.cfg.discord.PinMessage(ctx, channelID, messageID); err != nil {
			return fmt.Errorf("sync_welcome: pin message: %w", err)
		}
	}

	// Step 3: record welcome_message_id on the space row (idempotent on repeat).
	if _, err := h.cfg.store.UpdateSpaceWelcomeMessageID(ctx, payload.SpaceID, messageID); err != nil {
		// Non-fatal: the pin is set; just log and continue.
		slog.WarnContext(ctx, "sync_welcome: could not persist welcome_message_id",
			"space_id", payload.SpaceID, "message_id", messageID, "error", err)
	}

	// Step 4: advance job to completed and write audit entry.
	h.advanceJob(ctx, payload.SpaceID, domain.JobStatusCompleted, nil)

	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:  "space.welcome.sync",
		SpaceID: &payload.SpaceID,
		Detail:  map[string]any{"channel_id": channelID, "message_id": messageID},
	})

	slog.InfoContext(ctx, "sync_welcome: completed",
		"space_id", payload.SpaceID, "channel_id", channelID, "message_id", messageID)
	return nil
}

// advanceJob looks up and updates the jobs mirror row for the space's sync_welcome job.
func (h *syncWelcomeHandler) advanceJob(
	ctx context.Context,
	spaceID string,
	status domain.JobStatus,
	cause *error,
) {
	job, err := h.cfg.store.GetJobBySpaceIDAndKind(ctx, spaceID, queue.KindSyncWelcome)
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
		slog.WarnContext(ctx, "sync_welcome: could not update job status",
			"job_id", job.ID, "status", status, "error", err)
	}
}
