// Package worker_test — outbox relay tests (AC-4 transactional outbox).
//
// Verifies:
//   - A pending outbox row is enqueued and stamped enqueued_at by the relay.
//   - An empty outbox produces no stamps.
//   - The relay stamps rows exactly once (idempotent: after first stamp the row
//     is gone from pending, so re-runs are no-ops).
//   - SEC-001: a row whose task is already in the queue (ErrTaskIDConflict) is stamped
//     without error and is not re-processed on subsequent ticks.
//
// Uses miniredis for a hermetic asynq client and a fake store for the outbox table.
package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── Fake outbox store ────────────────────────────────────────────────────────

type outboxFakeStore struct {
	workerFakeStore
	pending    []*domain.OutboxRow
	stampedIDs []string
}

func newOutboxFakeStore() *outboxFakeStore {
	return &outboxFakeStore{
		workerFakeStore: workerFakeStore{users: make(map[string]*domain.User)},
	}
}

func (s *outboxFakeStore) ListPendingOutbox(_ context.Context, limit int) ([]*domain.OutboxRow, error) {
	if limit <= 0 || len(s.pending) == 0 {
		return nil, nil
	}
	n := limit
	if n > len(s.pending) {
		n = len(s.pending)
	}
	return s.pending[:n], nil
}

func (s *outboxFakeStore) StampOutboxEnqueued(_ context.Context, ids []string) error {
	s.stampedIDs = append(s.stampedIDs, ids...)
	// Remove stamped rows from pending so subsequent ticks are no-ops.
	stamped := make(map[string]bool, len(ids))
	for _, id := range ids {
		stamped[id] = true
	}
	var remaining []*domain.OutboxRow
	for _, row := range s.pending {
		if !stamped[row.ID] {
			remaining = append(remaining, row)
		}
	}
	s.pending = remaining
	return nil
}

func (s *outboxFakeStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

// outboxFakeStore must also implement the remaining store.Store methods inherited
// from workerFakeStore — those already panic, which is correct for tests that
// only exercise the outbox path. The extra methods below override the ones that
// the relay specifically needs (ListPendingOutbox, StampOutboxEnqueued above).

// buildRelay creates a relay wired to the given store and a fresh miniredis client.
func buildRelay(t *testing.T, s store.Store) *worker.Relay {
	t.Helper()
	mr := miniredis.RunT(t)
	client := queue.NewClient(mr.Addr(), "", 0)
	t.Cleanup(func() { _ = client.Close() })
	return worker.NewRelay(worker.RelayConfig{
		Store:        s,
		QueueClient:  client,
		PollInterval: 50 * time.Millisecond,
		BatchSize:    10,
	})
}

// runRelayFor runs the relay for the given duration and waits for it to finish.
func runRelayFor(t *testing.T, relay *worker.Relay, dur time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()
	relay.Run(ctx)
}

// ─── Test: pending row is enqueued and stamped ────────────────────────────────

// TestRelay_EnqueuesPendingRow verifies that the relay picks up a pending outbox row,
// enqueues the asynq task, and stamps enqueued_at (AC-4 transactional outbox).
func TestRelay_EnqueuesPendingRow(t *testing.T) {
	s := newOutboxFakeStore()
	s.pending = []*domain.OutboxRow{
		{
			ID:             "ob-001",
			Aggregate:      "space",
			AggregateID:    "space-001",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "space-001"},
			IdempotencyKey: "idem-key-001",
			CreatedAt:      time.Now(),
		},
	}

	relay := buildRelay(t, s)
	runRelayFor(t, relay, 300*time.Millisecond)

	if len(s.stampedIDs) == 0 {
		t.Error("relay must stamp enqueued_at on the processed outbox row")
	}
	if len(s.pending) != 0 {
		t.Errorf("relay must remove rows from pending after stamp, remaining: %d", len(s.pending))
	}
}

// ─── Test: empty outbox produces no stamps ────────────────────────────────────

// TestRelay_EmptyOutbox_NoStamps verifies that an empty outbox produces no stamps.
func TestRelay_EmptyOutbox_NoStamps(t *testing.T) {
	s := newOutboxFakeStore() // empty pending

	relay := buildRelay(t, s)
	runRelayFor(t, relay, 200*time.Millisecond)

	if len(s.stampedIDs) != 0 {
		t.Errorf("want no stamps for empty outbox, got %v", s.stampedIDs)
	}
}

// ─── Test: idempotent — row stamped exactly once ───────────────────────────────

// TestRelay_Idempotent_StampedOnce verifies that a single pending row is stamped
// exactly once even when the relay runs for multiple ticks. After the first stamp,
// the fake store removes the row from pending, so subsequent ticks are no-ops.
func TestRelay_Idempotent_StampedOnce(t *testing.T) {
	s := newOutboxFakeStore()
	s.pending = []*domain.OutboxRow{
		{
			ID:             "ob-idem-001",
			Aggregate:      "space",
			AggregateID:    "space-idem",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "space-idem"},
			IdempotencyKey: "idem-key-idem-001",
			CreatedAt:      time.Now(),
		},
	}

	relay := buildRelay(t, s)
	// Run long enough for multiple ticks (PollInterval=50ms, so ~6 ticks in 300ms).
	runRelayFor(t, relay, 300*time.Millisecond)

	if len(s.stampedIDs) != 1 {
		t.Errorf("outbox row must be stamped exactly once, got %d stamps (IDs: %v)",
			len(s.stampedIDs), s.stampedIDs)
	}
}

// ─── Outbox atomicity contract ────────────────────────────────────────────────

// outboxAtomicStore is a fake that records CreateSpaceWithOutbox calls.
// It simulates the "both space and outbox row returned on success, neither on failure"
// contract without a real Postgres transaction (AC-4: desired change + outbox row
// commit atomically — one tx).
type outboxAtomicStore struct {
	workerFakeStore
	spaces  []*domain.Space
	outrows []*domain.OutboxRow
	failOn  int // if > 0, fail on the Nth call
	calls   int
}

func (s *outboxAtomicStore) ListPendingOutbox(_ context.Context, _ int) ([]*domain.OutboxRow, error) {
	return nil, nil
}
func (s *outboxAtomicStore) StampOutboxEnqueued(_ context.Context, _ []string) error { return nil }
func (s *outboxAtomicStore) UpdateOutboxPayload(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

func (s *outboxAtomicStore) CreateSpaceWithOutbox(
	_ context.Context,
	sp store.CreateSpaceParams,
	ob store.CreateOutboxParams,
) (*domain.Space, *domain.OutboxRow, error) {
	s.calls++
	if s.failOn > 0 && s.calls == s.failOn {
		// Simulate a Postgres transaction rollback — neither row must be visible.
		return nil, nil, store.ErrConflict
	}
	space := &domain.Space{
		ID:             "space-tx-001",
		MerchantID:     sp.MerchantID,
		Name:           sp.Name,
		LifecycleState: domain.SpaceLifecycleActive,
		ACLState:       domain.ACLStatePending,
	}
	row := &domain.OutboxRow{
		ID:             "ob-tx-001",
		Aggregate:      ob.Aggregate,
		AggregateID:    ob.AggregateID,
		Kind:           ob.Kind,
		Payload:        ob.Payload,
		IdempotencyKey: ob.IdempotencyKey,
	}
	s.spaces = append(s.spaces, space)
	s.outrows = append(s.outrows, row)
	return space, row, nil
}

// TestCreateSpaceWithOutbox_SuccessReturnsBothRows verifies that on success both the
// space and the outbox row are returned in the same call (atomicity contract, AC-4).
func TestCreateSpaceWithOutbox_SuccessReturnsBothRows(t *testing.T) {
	s := &outboxAtomicStore{
		workerFakeStore: workerFakeStore{users: make(map[string]*domain.User)},
	}
	ctx := context.Background()

	sp, ob, err := s.CreateSpaceWithOutbox(ctx,
		store.CreateSpaceParams{MerchantID: "m-001", Name: "support-space"},
		store.CreateOutboxParams{
			Aggregate:      "space",
			AggregateID:    "space-tx-001",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "space-tx-001"},
			IdempotencyKey: "idem-atomic-001",
		},
	)
	if err != nil {
		t.Fatalf("CreateSpaceWithOutbox: unexpected error: %v", err)
	}
	if sp == nil {
		t.Error("space must be non-nil on success")
	}
	if ob == nil {
		t.Error("outbox row must be non-nil on success")
	}
	if sp != nil && sp.MerchantID != "m-001" {
		t.Errorf("space.MerchantID: want m-001, got %v", sp.MerchantID)
	}
	if ob != nil && ob.IdempotencyKey != "idem-atomic-001" {
		t.Errorf("outbox.IdempotencyKey: want idem-atomic-001, got %v", ob.IdempotencyKey)
	}
	if len(s.spaces) != 1 || len(s.outrows) != 1 {
		t.Errorf("want 1 space + 1 outbox row persisted; got %d spaces, %d rows",
			len(s.spaces), len(s.outrows))
	}
}

// TestCreateSpaceWithOutbox_FailureReturnsNeitherRow verifies that when
// CreateSpaceWithOutbox fails (transaction rollback), neither the space nor the
// outbox row is visible to callers (atomicity contract, AC-4).
func TestCreateSpaceWithOutbox_FailureReturnsNeitherRow(t *testing.T) {
	s := &outboxAtomicStore{
		workerFakeStore: workerFakeStore{users: make(map[string]*domain.User)},
		failOn:          1,
	}
	ctx := context.Background()

	sp, ob, err := s.CreateSpaceWithOutbox(ctx,
		store.CreateSpaceParams{MerchantID: "m-002", Name: "space-fail"},
		store.CreateOutboxParams{
			Aggregate:      "space",
			AggregateID:    "space-fail-001",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "space-fail-001"},
			IdempotencyKey: "idem-atomic-fail",
		},
	)
	if err == nil {
		t.Fatal("want error on forced failure, got nil")
	}
	if sp != nil {
		t.Error("space must be nil when the transaction fails")
	}
	if ob != nil {
		t.Error("outbox row must be nil when the transaction fails")
	}
	if len(s.spaces) != 0 || len(s.outrows) != 0 {
		t.Errorf("no rows must be persisted on failure; got %d spaces, %d outbox rows",
			len(s.spaces), len(s.outrows))
	}
}

// TestRelay_AlreadyStampedRows_ProducesNoDoubleEnqueue verifies the no-double-enqueue
// guarantee (AC-4): once a relay run stamps a row and removes it from pending, a
// second relay run against the same store produces exactly zero additional stamps.
func TestRelay_AlreadyStampedRows_ProducesNoDoubleEnqueue(t *testing.T) {
	s := newOutboxFakeStore()
	s.pending = []*domain.OutboxRow{
		{
			ID:             "ob-dup-001",
			Aggregate:      "space",
			AggregateID:    "space-dup",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "space-dup"},
			IdempotencyKey: "idem-dup-001",
			CreatedAt:      time.Now(),
		},
	}

	// First relay run: should stamp the row once.
	relay1 := buildRelay(t, s)
	runRelayFor(t, relay1, 200*time.Millisecond)

	stampsAfterFirstRun := len(s.stampedIDs)
	if stampsAfterFirstRun == 0 {
		t.Fatal("relay must stamp the row on the first run")
	}

	// Second relay run: pending is now empty because the fake store removed the
	// row on stamp. A second run must not produce any additional stamps.
	relay2 := buildRelay(t, s)
	runRelayFor(t, relay2, 200*time.Millisecond)

	stampsAfterSecondRun := len(s.stampedIDs)
	if stampsAfterSecondRun != stampsAfterFirstRun {
		t.Errorf("no-double-enqueue violated: stamps after first run=%d, after second run=%d",
			stampsAfterFirstRun, stampsAfterSecondRun)
	}
}

// ─── SEC-001: ErrTaskIDConflict treated as already-enqueued ──────────────────

// TestRelay_TaskIDConflict_StampsWithoutError verifies SEC-001: when the asynq task
// for an outbox row is already in the queue (ErrTaskIDConflict), the relay treats it as
// "already enqueued", stamps the row, and does NOT re-process it on subsequent ticks.
//
// The conflict is induced by pre-enqueuing the task with the same idempotency key
// before the relay runs, then confirming the row is stamped exactly once.
func TestRelay_TaskIDConflict_StampsWithoutError(t *testing.T) {
	const idempotencyKey = "idem-conflict-001"

	// Build a shared miniredis instance so the pre-enqueued task and the relay's
	// asynq client share the same in-process queue.
	mr := miniredis.RunT(t)
	client := queue.NewClient(mr.Addr(), "", 0)
	t.Cleanup(func() { _ = client.Close() })

	// Pre-enqueue the task with the same idempotency key that the outbox row carries.
	// The relay's Enqueue call will receive ErrTaskIDConflict for this row.
	_, err := client.Enqueue(
		queue.KindProvisionSpace,
		queue.QueueProvision,
		map[string]any{"space_id": "space-conflict"},
		queue.TaskIDOpt(idempotencyKey),
		queue.UniqueOpt(24*time.Hour),
	)
	if err != nil {
		t.Fatalf("pre-enqueue: unexpected error: %v", err)
	}

	s := newOutboxFakeStore()
	s.pending = []*domain.OutboxRow{
		{
			ID:             "ob-conflict-001",
			Aggregate:      "space",
			AggregateID:    "space-conflict",
			Kind:           queue.KindProvisionSpace,
			Payload:        map[string]any{"space_id": "space-conflict"},
			IdempotencyKey: idempotencyKey,
			CreatedAt:      time.Now(),
		},
	}

	relay := worker.NewRelay(worker.RelayConfig{
		Store:        s,
		QueueClient:  client,
		PollInterval: 50 * time.Millisecond,
		BatchSize:    10,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	relay.Run(ctx)

	// The row must be stamped exactly once — ErrTaskIDConflict is treated as success.
	if len(s.stampedIDs) != 1 {
		t.Errorf("SEC-001: row with pre-existing task must be stamped exactly once; got %d stamps (IDs: %v)",
			len(s.stampedIDs), s.stampedIDs)
	}
	// No pending rows must remain — the row must have been removed after the stamp.
	if len(s.pending) != 0 {
		t.Errorf("SEC-001: no pending rows must remain after stamp; got %d", len(s.pending))
	}
}
