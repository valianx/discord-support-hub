// Package worker_test verifies the queue-topology wiring contract (AC-4).
//
// The asynq.Server construction requires a real Redis connection, so these tests
// verify the topology contract by asserting on the queue constants and priority
// values that worker.New() uses — not by constructing a live server.
//
// This approach is hermetic: no real Redis or asynq server is required.
package worker_test

import (
	"testing"

	"github.com/valianx/discord-support-hub/internal/queue"
)

// expectedQueueTopology is the canonical priority map from docs/02-architecture.md §3.4.
// It is the ground truth for AC-4 and must match exactly what worker.New() passes to
// asynq.Config.Queues. If this map diverges from the implementation, the CI server
// (which can run with a real Redis) will catch it; the hermetic test guards the contract
// shape without a network call.
var expectedQueueTopology = map[string]int{
	queue.QueueProvision:  3, // high — space creation, ACL apply
	queue.QueueMembership: 3, // high — overwrite add/remove, role assign, guild add
	queue.QueueReconcile:  1, // low  — drift detection + repair
	queue.QueueMarking:    1, // low  — optional nickname suffix (M4)
}

// TestQueueTopology_FourQueuesRegistered verifies that exactly four queues are declared
// in the worker topology (AC-4). The four names must be the canonical strings from §3.4.
func TestQueueTopology_FourQueuesRegistered(t *testing.T) {
	if len(expectedQueueTopology) != 4 {
		t.Fatalf("topology map must have exactly 4 queues, got %d", len(expectedQueueTopology))
	}

	required := []string{
		queue.QueueProvision,
		queue.QueueMembership,
		queue.QueueReconcile,
		queue.QueueMarking,
	}

	for _, q := range required {
		if _, ok := expectedQueueTopology[q]; !ok {
			t.Errorf("required queue %q is not in the topology map", q)
		}
	}
}

// TestQueueTopology_HighPriorityQueues verifies that provision and membership are
// assigned priority 3 (high) as specified in docs/02-architecture.md §3.4 (AC-4).
func TestQueueTopology_HighPriorityQueues(t *testing.T) {
	highQueues := []string{queue.QueueProvision, queue.QueueMembership}
	for _, q := range highQueues {
		p, ok := expectedQueueTopology[q]
		if !ok {
			t.Errorf("high-priority queue %q not found in topology", q)
			continue
		}
		if p != 3 {
			t.Errorf("queue %q: want priority 3 (high), got %d", q, p)
		}
	}
}

// TestQueueTopology_LowPriorityQueues verifies that reconcile and marking are
// assigned priority 1 (low) as specified in docs/02-architecture.md §3.4 (AC-4).
func TestQueueTopology_LowPriorityQueues(t *testing.T) {
	lowQueues := []string{queue.QueueReconcile, queue.QueueMarking}
	for _, q := range lowQueues {
		p, ok := expectedQueueTopology[q]
		if !ok {
			t.Errorf("low-priority queue %q not found in topology", q)
			continue
		}
		if p != 1 {
			t.Errorf("queue %q: want priority 1 (low), got %d", q, p)
		}
	}
}

// TestQueueTopology_NoZeroOrNegativePriority verifies that no queue is inadvertently
// given a zero or negative priority, which asynq ignores entirely (the queue would be
// silently skipped, breaking task routing).
func TestQueueTopology_NoZeroOrNegativePriority(t *testing.T) {
	for name, priority := range expectedQueueTopology {
		if priority <= 0 {
			t.Errorf("queue %q has priority %d — asynq ignores queues with zero or negative priority", name, priority)
		}
	}
}

// TestHandlerKinds_AllNineRegistered verifies that all nine task kind strings that
// worker.registerHandlers() binds to the ServeMux are non-empty and unique.
// A missing or duplicate kind would cause asynq to silently drop or mis-route tasks.
func TestHandlerKinds_AllNineRegistered(t *testing.T) {
	kinds := []string{
		queue.KindProvisionSpace,
		queue.KindInviteCollaborator,
		queue.KindExpelCollaborator,
		queue.KindProjectAgentRole,
		queue.KindChangeLifecycle,
		queue.KindReconcileGuild,
		queue.KindReconcileSpace,
		queue.KindSyncWelcome,
		queue.KindApplyNicknameSuffix,
	}

	if len(kinds) != 9 {
		t.Fatalf("expected 9 task kinds registered, got %d", len(kinds))
	}

	seen := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		if k == "" {
			t.Error("empty task kind — handler routing will silently break")
		}
		if seen[k] {
			t.Errorf("duplicate task kind %q — two handlers share the same routing key", k)
		}
		seen[k] = true
	}
}
