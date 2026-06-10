// Package worker — outbox relay.
//
// The relay polls the outbox table for rows where enqueued_at IS NULL, enqueues
// each asynq task using the row's idempotency_key as the TaskID (NFR-3, §4), and
// stamps enqueued_at so the row is never processed twice.
//
// The relay guarantees that a desired-state change committed in Postgres is never
// lost before the task reaches the queue, even if the process dies between the DB
// commit and the Enqueue call (transactional outbox, NFR-3).
package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// RelayConfig carries the dependencies the outbox relay needs.
type RelayConfig struct {
	Store       store.Store
	QueueClient *queue.Client

	// PollInterval controls how often the relay checks for pending outbox rows.
	// Default: 2 seconds.
	PollInterval time.Duration

	// BatchSize is the maximum number of outbox rows processed per poll tick.
	// Default: 100.
	BatchSize int
}

// Relay is the outbox relay that enqueues pending asynq tasks.
type Relay struct {
	cfg RelayConfig
}

// NewRelay creates an outbox Relay.
func NewRelay(cfg RelayConfig) *Relay {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	return &Relay{cfg: cfg}
}

// Run polls the outbox table at cfg.PollInterval and enqueues pending rows.
// It blocks until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.tick(ctx); err != nil {
				slog.ErrorContext(ctx, "outbox relay: tick error", "error", err)
			}
		}
	}
}

// tick processes one batch of pending outbox rows.
func (r *Relay) tick(ctx context.Context) error {
	rows, err := r.cfg.Store.ListPendingOutbox(ctx, r.cfg.BatchSize)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	var stamped []string
	for _, ob := range rows {
		if enqueueErr := r.enqueueRow(ctx, ob); enqueueErr != nil {
			// Log and continue — a failed enqueue is retried on the next tick.
			slog.ErrorContext(ctx, "outbox relay: enqueue failed",
				"outbox_id", ob.ID, "kind", ob.Kind,
				"idempotency_key", ob.IdempotencyKey, "error", enqueueErr,
			)
			continue
		}
		stamped = append(stamped, ob.ID)
	}

	if len(stamped) == 0 {
		return nil
	}
	if err = r.cfg.Store.StampOutboxEnqueued(ctx, stamped); err != nil {
		slog.ErrorContext(ctx, "outbox relay: stamp enqueued failed",
			"ids", stamped, "error", err)
		return err
	}
	slog.InfoContext(ctx, "outbox relay: enqueued batch", "count", len(stamped))
	return nil
}

// enqueueRow enqueues a single outbox row as an asynq task.
// Uses TaskID(idempotencyKey) + Unique so duplicate enqueues collapse to one task.
//
// When asynq returns ErrTaskIDConflict the task already exists in the queue under
// the same TaskID — the idempotency key acts as a deduplication handle (NFR-3).
// Treat this as "already enqueued": return nil so the caller stamps the row and
// stops re-processing it, preserving the exactly-once guarantee.
func (r *Relay) enqueueRow(ctx context.Context, ob *domain.OutboxRow) error {
	q := queueForKind(ob.Kind)
	_, err := r.cfg.QueueClient.Enqueue(
		ob.Kind, q,
		ob.Payload,
		queue.TaskIDOpt(ob.IdempotencyKey),
		queue.UniqueOpt(24*time.Hour),
		asynq.MaxRetry(provisionMaxRetry),
	)
	if err != nil {
		// fix(SEC-001): both ErrTaskIDConflict and ErrDuplicateTask mean the task is
		// already in the queue — the relay uses TaskID + Unique together, so either
		// sentinel can fire depending on which constraint is checked first.
		// Treat both as "already enqueued": return nil so the caller stamps the row
		// and stops re-processing it, preserving the exactly-once guarantee.
		// Returning the raw error caused the row to be re-queued every relay tick
		// (self-inflicted DoS, broken exactly-once).
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			return nil
		}
		return err
	}
	return nil
}

// queueForKind maps a task kind to its queue name.
// Mirrors the queue topology in docs/02-architecture.md §3.4.
func queueForKind(kind string) string {
	switch kind {
	case queue.KindProvisionSpace:
		return queue.QueueProvision
	case queue.KindInviteCollaborator, queue.KindExpelCollaborator, queue.KindProjectAgentRole:
		return queue.QueueMembership
	case queue.KindReconcileGuild, queue.KindReconcileSpace:
		return queue.QueueReconcile
	case queue.KindSyncWelcome, queue.KindApplyNicknameSuffix:
		return queue.QueueMarking
	case queue.KindSendInvite:
		return queue.QueueNotify // AC-M6-6: notify queue is isolated from provision/membership
	default:
		return queue.QueueProvision // safe default
	}
}
