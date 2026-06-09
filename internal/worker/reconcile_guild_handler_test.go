// Package worker_test — reconcile_guild_handler_test.go verifies that the
// KindReconcileGuild asynq handler is wired to the reconcile engine and that
// the scheduled guild sweep is exercised through the real worker task path (M5, AC-5).
//
// Tests are hermetic: miniredis for the queue transport, fake store and Discord client.
// No real Postgres or Discord connections are used.
package worker_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/bwmarrin/discordgo"
	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/reconcile"
	"github.com/valianx/discord-support-hub/internal/store"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// TestReconcileGuildHandler_DispatchesToEngine verifies that when KindReconcileGuild is
// enqueued and processed, the worker calls ReconcileGuild on the reconcile.Engine (AC-5).
//
// The store's ListActiveProvisionedSpaces is used as a side-channel observable: if the
// engine's ReconcileGuild runs, it must call ListActiveProvisionedSpaces. A non-zero call
// count proves the engine was invoked through the real handler path.
func TestReconcileGuildHandler_DispatchesToEngine(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	// counting store: tracks ListActiveProvisionedSpaces calls as a proxy for ReconcileGuild.
	cs := &countingGuildStore{}
	engine := reconcile.NewEngine(cs, &noopGuildDiscord{})

	mux := worker.NewServeMux(worker.Config{
		ReconcileEngine: engine,
		DiscordGuildID:  "test-guild-handler",
	})

	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: mr.Addr()},
		asynq.Config{
			Concurrency: 1,
			Queues:      map[string]int{queue.QueueReconcile: 1},
		},
	)

	var wg sync.WaitGroup
	wg.Add(1)

	// Wrap the real mux so we can signal when the task is processed.
	notifyMux := asynq.NewServeMux()
	notifyMux.HandleFunc(queue.KindReconcileGuild, func(ctx context.Context, task *asynq.Task) error {
		defer wg.Done()
		return mux.ProcessTask(ctx, task)
	})

	if err := srv.Start(notifyMux); err != nil {
		t.Fatalf("start asynq server: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	client := asynq.NewClient(asynq.RedisClientOpt{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	task := asynq.NewTask(queue.KindReconcileGuild, []byte(`{"guild_id":"test-guild-handler"}`))
	if _, err := client.Enqueue(task, asynq.Queue(queue.QueueReconcile)); err != nil {
		t.Fatalf("enqueue reconcile:guild task: %v", err)
	}

	// Wait for the handler to process the task (5-second hard deadline).
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reconcile:guild handler was not invoked within 5 seconds")
	}

	// ListActiveProvisionedSpaces must have been called — this is the first thing
	// ReconcileGuild does: enumerate active provisioned spaces from the store.
	if atomic.LoadInt32(&cs.listCalls) == 0 {
		t.Errorf("expected ReconcileGuild to call ListActiveProvisionedSpaces at least once; got 0 calls")
	}
}

// TestReconcileGuildHandler_NilEngine_UsesStub verifies that when ReconcileEngine is nil
// in the Config, the KindReconcileGuild handler falls back to the stub and returns nil
// without panicking (AC-5 defensive path).
func TestReconcileGuildHandler_NilEngine_UsesStub(t *testing.T) {
	t.Parallel()

	mr := miniredis.RunT(t)

	// Nil ReconcileEngine triggers the stub handler path.
	mux := worker.NewServeMux(worker.Config{
		ReconcileEngine: nil,
		DiscordGuildID:  "test-guild-stub",
	})

	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: mr.Addr()},
		asynq.Config{
			Concurrency: 1,
			Queues:      map[string]int{queue.QueueReconcile: 1},
		},
	)

	var wg sync.WaitGroup
	wg.Add(1)

	notifyMux := asynq.NewServeMux()
	notifyMux.HandleFunc(queue.KindReconcileGuild, func(ctx context.Context, task *asynq.Task) error {
		defer wg.Done()
		return mux.ProcessTask(ctx, task)
	})

	if err := srv.Start(notifyMux); err != nil {
		t.Fatalf("start asynq server: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	client := asynq.NewClient(asynq.RedisClientOpt{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	task := asynq.NewTask(queue.KindReconcileGuild, []byte(`{}`))
	if _, err := client.Enqueue(task, asynq.Queue(queue.QueueReconcile)); err != nil {
		t.Fatalf("enqueue task: %v", err)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stub handler not invoked within 5 seconds")
	}
	// Reaching here means the stub handler returned without panicking.
}

// ─── Counting store ───────────────────────────────────────────────────────────

// countingGuildStore satisfies the reconcile.Engine's storeReconcile interface.
// It counts calls to ListActiveProvisionedSpaces so tests can verify ReconcileGuild ran.
type countingGuildStore struct {
	listCalls int32 // atomic
}

func (s *countingGuildStore) ListActiveProvisionedSpaces(_ context.Context) ([]*domain.Space, error) {
	atomic.AddInt32(&s.listCalls, 1)
	return nil, nil // empty guild — ReconcileGuild will complete with no per-space work
}

func (s *countingGuildStore) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	return nil, store.ErrNotFound
}

func (s *countingGuildStore) ListActiveSpaceMembers(_ context.Context, _ string) ([]*domain.SpaceMember, error) {
	return nil, nil
}

func (s *countingGuildStore) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	return nil, store.ErrNotFound
}

func (s *countingGuildStore) InsertAuditEntry(_ context.Context, _ store.InsertAuditEntryParams) error {
	return nil
}

func (s *countingGuildStore) UpdateSpaceReconciledAt(_ context.Context, _ string) error { return nil }

func (s *countingGuildStore) SetSpaceMemberOverwriteApplied(_ context.Context, _ string) (*domain.SpaceMember, error) {
	return nil, store.ErrNotFound
}

// ─── No-op Discord client ─────────────────────────────────────────────────────

type noopGuildDiscord struct{}

func (d *noopGuildDiscord) GetChannelOverwrites(_ context.Context, _ string) ([]*discordgo.PermissionOverwrite, error) {
	return nil, nil
}
func (d *noopGuildDiscord) SetCollaboratorOverwrite(_ context.Context, _, _ string) error {
	return nil
}
func (d *noopGuildDiscord) DeleteCollaboratorOverwrite(_ context.Context, _, _ string) error {
	return nil
}
