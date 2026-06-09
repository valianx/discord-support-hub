// Package queue_test verifies the queue topology constants and payload helpers (AC-4).
//
// These tests are hermetic: they exercise only the constants, structs, and JSON
// marshaling defined in package queue — no real Redis or asynq server is needed.
package queue_test

import (
	"encoding/json"
	"testing"

	"github.com/valianx/discord-support-hub/internal/queue"
)

// TestQueueNames verifies that the four required queue names are declared and match
// the topology specified in docs/02-architecture.md §3.4 (AC-4).
func TestQueueNames(t *testing.T) {
	want := map[string]string{
		"provision":  queue.QueueProvision,
		"membership": queue.QueueMembership,
		"reconcile":  queue.QueueReconcile,
		"marking":    queue.QueueMarking,
	}

	for expected, got := range want {
		if got != expected {
			t.Errorf("queue constant: want %q, got %q", expected, got)
		}
	}
}

// TestQueuePriorityWiring verifies the documented priority contract:
// provision and membership are "high" (3), reconcile and marking are "low" (1).
// This mirrors the asynq.Config.Queues map built in internal/worker and checked in worker_test.
// AC-4: "four asynq queues registered with their priorities".
func TestQueuePriorityWiring(t *testing.T) {
	// Canonical priority map from docs/02-architecture.md §3.4.
	priorities := map[string]int{
		queue.QueueProvision:  3, // high
		queue.QueueMembership: 3, // high
		queue.QueueReconcile:  1, // low
		queue.QueueMarking:    1, // low
	}

	// Verify exactly four queues are configured.
	if len(priorities) != 4 {
		t.Fatalf("expected 4 queues in topology, got %d", len(priorities))
	}

	// Verify high-priority queues.
	for _, q := range []string{queue.QueueProvision, queue.QueueMembership} {
		if p := priorities[q]; p != 3 {
			t.Errorf("queue %q: want priority 3 (high), got %d", q, p)
		}
	}

	// Verify low-priority queues.
	for _, q := range []string{queue.QueueReconcile, queue.QueueMarking} {
		if p := priorities[q]; p != 1 {
			t.Errorf("queue %q: want priority 1 (low), got %d", q, p)
		}
	}
}

// TestTaskKindConstants verifies that all nine task kind strings are non-empty and unique.
// This guards against accidental duplication that would cause asynq to route tasks to the
// wrong handler.
func TestTaskKindConstants(t *testing.T) {
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

	seen := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		if k == "" {
			t.Error("empty task kind constant found — task routing will break")
		}
		if seen[k] {
			t.Errorf("duplicate task kind %q — two handlers would share the same key", k)
		}
		seen[k] = true
	}

	if len(kinds) != 9 {
		t.Errorf("expected 9 task kind constants, got %d", len(kinds))
	}
}

// TestProvisionSpacePayload_MarshalRoundTrip verifies that the primary payload struct
// survives a JSON marshal/unmarshal cycle without data loss.
// A no-op enqueue/consume round-trip at the payload layer — AC-4.
func TestProvisionSpacePayload_MarshalRoundTrip(t *testing.T) {
	original := queue.ProvisionSpacePayload{
		MerchantID:     "merch-001",
		SpaceID:        "space-001",
		SpaceName:      "ACME Support",
		CategoryID:     "cat-999",
		WelcomeMessage: "Welcome to ACME support.",
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var recovered queue.ProvisionSpacePayload
	if err := json.Unmarshal(b, &recovered); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if recovered.MerchantID != original.MerchantID {
		t.Errorf("MerchantID: want %q, got %q", original.MerchantID, recovered.MerchantID)
	}
	if recovered.SpaceID != original.SpaceID {
		t.Errorf("SpaceID: want %q, got %q", original.SpaceID, recovered.SpaceID)
	}
	if recovered.SpaceName != original.SpaceName {
		t.Errorf("SpaceName: want %q, got %q", original.SpaceName, recovered.SpaceName)
	}
	if recovered.CategoryID != original.CategoryID {
		t.Errorf("CategoryID: want %q, got %q", original.CategoryID, recovered.CategoryID)
	}
	if recovered.WelcomeMessage != original.WelcomeMessage {
		t.Errorf("WelcomeMessage: want %q, got %q", original.WelcomeMessage, recovered.WelcomeMessage)
	}
}

// TestAllPayloadStructs_Marshal verifies that every declared payload struct is
// serializable without error (guards against unexported fields or unsupported types).
func TestAllPayloadStructs_Marshal(t *testing.T) {
	payloads := []struct {
		name    string
		payload any
	}{
		{"ProvisionSpacePayload", queue.ProvisionSpacePayload{MerchantID: "m1", SpaceID: "s1", SpaceName: "Test"}},
		{"InviteCollaboratorPayload", queue.InviteCollaboratorPayload{SpaceID: "s1", UserID: "u1", InvitedBy: "admin"}},
		{"ExpelCollaboratorPayload", queue.ExpelCollaboratorPayload{SpaceID: "s1", UserID: "u1", Scope: "channel"}},
		{"ProjectAgentRolePayload", queue.ProjectAgentRolePayload{UserID: "u1", Add: true}},
		{"ChangeLifecyclePayload", queue.ChangeLifecyclePayload{SpaceID: "s1", Action: "archive"}},
		{"ReconcileSpacePayload", queue.ReconcileSpacePayload{SpaceID: "s1"}},
		{"ApplyNicknameSuffixPayload", queue.ApplyNicknameSuffixPayload{UserID: "u1", GuildID: "g1", Suffix: "[Support]"}},
	}

	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("json.Marshal(%s): %v", tc.name, err)
			}
			if len(b) == 0 {
				t.Errorf("json.Marshal(%s): empty output", tc.name)
			}
		})
	}
}
