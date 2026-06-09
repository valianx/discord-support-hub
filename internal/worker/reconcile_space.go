// reconcile_space.go implements KindReconcileSpace and KindReconcileGuild worker handlers.
//
// KindReconcileSpace (M3, §4.2/§4.3): targeted single-space pass triggered post-mutation.
// KindReconcileGuild (M5, AC-5): full-guild sweep triggered by the asynq Scheduler on a
// periodic cron interval. Postgres always wins (NFR-5).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/reconcile"
)

// reconcileSpaceHandler is the KindReconcileSpace asynq handler.
type reconcileSpaceHandler struct {
	engine *reconcile.Engine
}

func newReconcileSpaceHandler(engine *reconcile.Engine) asynq.HandlerFunc {
	if engine == nil {
		return stubHandler(queue.KindReconcileSpace)
	}
	h := &reconcileSpaceHandler{engine: engine}
	return h.handle
}

func (h *reconcileSpaceHandler) handle(ctx context.Context, task *asynq.Task) error {
	var payload queue.ReconcileSpacePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return fmt.Errorf("%w: decode reconcile_space payload: %v", asynq.SkipRetry, err)
	}

	if payload.SpaceID == "" {
		return fmt.Errorf("%w: reconcile_space: payload missing space_id", asynq.SkipRetry)
	}

	slog.InfoContext(ctx, "reconcile_space: starting", "space_id", payload.SpaceID)
	if err := h.engine.ReconcileSpace(ctx, payload.SpaceID); err != nil {
		return fmt.Errorf("reconcile_space: %w", err)
	}
	slog.InfoContext(ctx, "reconcile_space: completed", "space_id", payload.SpaceID)
	return nil
}

// ─── reconcile:guild handler (M5, AC-5) ───────────────────────────────────────

// reconcileGuildHandler is the KindReconcileGuild asynq handler.
// Triggered by the asynq Scheduler at the configured cron interval.
type reconcileGuildHandler struct {
	engine  *reconcile.Engine
	guildID string
}

func newReconcileGuildHandler(engine *reconcile.Engine, guildID string) asynq.HandlerFunc {
	if engine == nil {
		return stubHandler(queue.KindReconcileGuild)
	}
	h := &reconcileGuildHandler{engine: engine, guildID: guildID}
	return h.handle
}

func (h *reconcileGuildHandler) handle(ctx context.Context, _ *asynq.Task) error {
	slog.InfoContext(ctx, "reconcile_guild: full sweep starting", "guild_id", h.guildID)
	if err := h.engine.ReconcileGuild(ctx, h.guildID); err != nil {
		// Partial sweep errors are logged inside ReconcileGuild; return retryable error here
		// so asynq will retry the sweep on transient failures (e.g. Discord downtime).
		return fmt.Errorf("reconcile_guild: %w", err)
	}
	slog.InfoContext(ctx, "reconcile_guild: full sweep complete", "guild_id", h.guildID)
	return nil
}
