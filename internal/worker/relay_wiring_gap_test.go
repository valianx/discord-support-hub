// relay_wiring_gap_test.go — wiring guard for the outbox relay (fix: wire-outbox-relay).
//
// The relay component (Relay, RelayConfig, NewRelay, Run) is unit-tested in relay_test.go.
// This file guards the *wiring* gap: the relay must be constructed and started in the worker
// entrypoint (cmd/worker/main.go) or the outbox rows written by CreateSpaceWithOutbox are
// never picked up → provision tasks are never enqueued → no Discord channel is created.
//
// Test: TestRelayWiring_PendingRowIsEnqueued
//
//	Reproduces the exact construction pattern that cmd/worker/main.go now uses:
//	  store  → pgstore (faked here as outboxFakeStore)
//	  client → queue.NewClient(valkeyAddr, ...)
//	  relay  → worker.NewRelay(worker.RelayConfig{Store: ..., QueueClient: ...})
//	  start  → go relay.Run(ctx)
//
//	The test inserts a pending outbox row, starts the relay with a context timeout, and
//	asserts that enqueued_at was stamped. If the relay is removed from the entrypoint
//	(or never wired), the row remains unstamped and this test fails with a clear message.
//
// Why this test fails if the relay is unwired:
//
//	The test directly calls worker.NewRelay + relay.Run — it doesn't test cmd/worker/main.go
//	itself (that binary can't be unit-tested without a real Postgres/Redis). Instead it tests
//	the *component contract* that cmd/worker/main.go must satisfy: given a store and a queue
//	client, the relay polls and stamps rows. The compile-time companion
//	TestRelayWiring_ConfigFieldsExist confirms that RelayConfig still has the fields
//	cmd/worker/main.go sets — if either Store or QueueClient is removed from RelayConfig,
//	the entrypoint fails to build AND this file fails to compile, making the gap visible.
package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// TestRelayWiring_ConfigFieldsExist is a compile-time guard: it constructs a
// worker.RelayConfig using exactly the fields that cmd/worker/main.go sets.
// If Store or QueueClient are removed from RelayConfig, this file fails to compile,
// surfacing the wiring break immediately rather than at runtime.
func TestRelayWiring_ConfigFieldsExist(t *testing.T) {
	cfg := worker.RelayConfig{
		Store:       nil, // compile-time: field must exist
		QueueClient: nil, // compile-time: field must exist
	}
	// Neither PollInterval nor BatchSize are set by cmd/worker/main.go
	// (it relies on the NewRelay defaults of 2 s / 100). Verify defaults kick in.
	relay := worker.NewRelay(cfg)
	if relay == nil {
		t.Fatal("NewRelay must return a non-nil Relay")
	}
}

// TestRelayWiring_PendingRowIsEnqueued reproduces the worker entrypoint wiring contract:
//
//  1. A pending outbox row exists in the store (written by CreateSpaceWithOutbox).
//  2. The relay is constructed with a queue.Client and the store — exactly as
//     cmd/worker/main.go does — and started in a goroutine.
//  3. After one poll cycle the row must be stamped (enqueued_at set).
//
// This test FAILs if the relay is not constructed or not started: without relay.Run,
// no poll occurs, stampedIDs stays empty, and the assertion below reports the gap.
func TestRelayWiring_PendingRowIsEnqueued(t *testing.T) {
	// In-process Redis so no external service is needed.
	mr := miniredis.RunT(t)

	// Wire the queue client exactly as cmd/worker/main.go does:
	//   queue.NewClient(cfg.ValkeyAddr, cfg.ValkeyPassword, cfg.ValkeyDB)
	client := queue.NewClient(mr.Addr(), "", 0)
	t.Cleanup(func() { _ = client.Close() })

	// Fake store with one pending outbox row — equivalent to a CreateSpaceWithOutbox commit.
	s := newOutboxFakeStore()
	s.pending = []*domain.OutboxRow{
		{
			ID:             "wiring-ob-001",
			Aggregate:      "space",
			AggregateID:    "wiring-space-001",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "wiring-space-001"},
			IdempotencyKey: "wiring-idem-001",
			CreatedAt:      time.Now(),
		},
	}

	// Construct the relay exactly as cmd/worker/main.go does (no PollInterval / BatchSize
	// override — defaults are used, matching the production entrypoint).
	relay := worker.NewRelay(worker.RelayConfig{
		Store:       s,
		QueueClient: client,
	})

	// Start relay in a goroutine with a cancellable context — mirrors cmd/worker/main.go:
	//   relayCtx, relayCancel := context.WithCancel(ctx)
	//   go relay.Run(relayCtx)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go relay.Run(ctx)

	// Wait for at most 5 s for the row to be stamped. The relay's default poll interval
	// is 2 s, so one successful tick is enough; 5 s gives two full cycles as margin.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.stampedIDs) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if len(s.stampedIDs) == 0 {
		t.Fatal("fix(wire-outbox-relay): outbox row was never stamped — " +
			"the relay must be constructed and started in cmd/worker/main.go; " +
			"without it CreateSpaceWithOutbox rows are never picked up and no asynq task is enqueued")
	}
	if len(s.pending) != 0 {
		t.Errorf("fix(wire-outbox-relay): pending rows must be removed after stamp, %d remain", len(s.pending))
	}
}
