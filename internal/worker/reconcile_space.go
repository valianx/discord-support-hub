// reconcile_space.go implements the KindReconcileSpace worker handler (M3, §4.2/§4.3).
//
// The handler decodes a ReconcileSpacePayload, delegates to the reconcile.Engine,
// and returns nil on success. Errors from the reconciler are returned as retryable
// so asynq will retry the sweep on transient failures.
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
