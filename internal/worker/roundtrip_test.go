// Package worker_test — enqueue→consume round-trip test (AC-4).
//
// This test starts an in-process miniredis server, wires a real asynq Client
// and Server against it, enqueues one representative task per queue
// (provision, membership, reconcile, marking), and asserts that each handler
// is actually invoked before a hard deadline.
//
// The production queue topology (queue names + priorities) is reused directly
// from the queue package constants so this test stays in sync with the real
// implementation. The handler mux is built fresh for the test — asynq panics
// on double-registration, so we cannot re-use NewServeMux() and then override
// individual handlers. Instead we wire a dedicated test mux whose handlers
// signal a WaitGroup, exercising the enqueue→consume path end-to-end.
//
// The test is hermetic: no external Redis, no network, no sleeps-as-sync.
package worker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/queue"
)

// TestRoundTrip_EnqueueConsume exercises a real enqueue→consume path across
// all four queues using representative task kinds (AC-4). The test passes only
// when every enqueued task is dequeued and its handler invoked.
func TestRoundTrip_EnqueueConsume(t *testing.T) {
	// ── 1. In-process Redis replacement ──────────────────────────────────────
	mr := miniredis.RunT(t)
	redisOpt := asynq.RedisClientOpt{Addr: mr.Addr()}

	// ── 2. One representative task per queue ──────────────────────────────────
	// Covers all four queues defined in docs/02-architecture.md §3.4.
	type taskSpec struct {
		kind  string
		queue string
	}
	tasks := []taskSpec{
		{queue.KindProvisionSpace, queue.QueueProvision},
		{queue.KindInviteCollaborator, queue.QueueMembership},
		{queue.KindReconcileGuild, queue.QueueReconcile},
		{queue.KindApplyNicknameSuffix, queue.QueueMarking},
	}

	// ── 3. Build a test mux that signals wg.Done() on each invocation ─────────
	// A dedicated mux is required because asynq panics on duplicate registration;
	// we cannot layer on top of the production mux.
	var wg sync.WaitGroup
	wg.Add(len(tasks))

	mux := asynq.NewServeMux()
	for _, ts := range tasks {
		mux.HandleFunc(ts.kind, func(ctx context.Context, task *asynq.Task) error {
			wg.Done()
			return nil
		})
	}

	// ── 4. Start the asynq Server using the real queue topology ───────────────
	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: 4,
		// Queue names and priorities mirror worker.New() / docs/02-architecture.md §3.4.
		Queues: map[string]int{
			queue.QueueProvision:  3,
			queue.QueueMembership: 3,
			queue.QueueReconcile:  1,
			queue.QueueMarking:    1,
		},
	})

	if err := srv.Start(mux); err != nil {
		t.Fatalf("asynq server Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown() })

	// ── 5. Enqueue one task per queue ─────────────────────────────────────────
	client := asynq.NewClient(redisOpt)
	t.Cleanup(func() { _ = client.Close() })

	for _, ts := range tasks {
		task := asynq.NewTask(ts.kind, []byte(`{}`))
		if _, err := client.Enqueue(task, asynq.Queue(ts.queue)); err != nil {
			t.Fatalf("enqueue %s → %s: %v", ts.kind, ts.queue, err)
		}
	}

	// ── 6. Assert all handlers are invoked within 5 s ─────────────────────────
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All four handlers invoked — enqueue→consume round-trip verified.
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: not all task handlers were invoked within 5 s")
	}
}
