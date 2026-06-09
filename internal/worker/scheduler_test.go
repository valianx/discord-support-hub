// scheduler_test.go — verifies the RECONCILE_SWEEP_CRON floor validation (SEC-M5-003).
//
// The scheduler must reject cron expressions whose effective interval between two
// consecutive fires is below minSweepInterval (1 minute), so a misconfigured cron
// cannot spin the full-guild sweep aggressively and amplify Discord rate-limit exposure.
//
// NewScheduler validates the cron expression and its interval floor BEFORE making any
// Valkey connection, so these tests pass without a running Redis instance.
package worker_test

import (
	"strings"
	"testing"

	"github.com/valianx/discord-support-hub/internal/worker"
)

// TestNewScheduler_MalformedCron_ReturnsError verifies that a syntactically invalid
// cron expression is rejected at scheduler construction time (SEC-M5-003).
func TestNewScheduler_MalformedCron_ReturnsError(t *testing.T) {
	_, err := worker.NewScheduler("localhost:6379", "", 0, "not-a-valid-cron", "guild-test")
	if err == nil {
		t.Fatal("expected error for malformed cron expression, got nil")
	}
	if !strings.Contains(err.Error(), "invalid cron expression") {
		t.Errorf("expected error to mention 'invalid cron expression', got: %v", err)
	}
}

// TestNewScheduler_ValidFiveMinuteCron_Accepted verifies that the default
// 5-minute cron ("*/5 * * * *") passes the floor check.
//
// NewScheduler will still fail (no Valkey available), but we expect the failure to
// come from the Valkey dial, not from the cron validation, so the error must NOT
// contain "invalid cron expression" or "minimum allowed interval".
func TestNewScheduler_ValidFiveMinuteCron_Accepted(t *testing.T) {
	_, err := worker.NewScheduler("localhost:1", "", 0, "*/5 * * * *", "guild-test")
	// The cron is valid and above the floor — the error (if any) must come from
	// the Valkey connection, not from cron validation.
	if err != nil {
		if strings.Contains(err.Error(), "invalid cron expression") {
			t.Errorf("valid 5-minute cron must not be rejected as invalid: %v", err)
		}
		if strings.Contains(err.Error(), "minimum allowed interval") {
			t.Errorf("valid 5-minute cron must not trigger the floor check: %v", err)
		}
		// Any other error is a Valkey connection failure — acceptable in unit tests.
		t.Logf("expected Valkey-connection error (no Redis available in unit test): %v", err)
	}
}

// TestNewScheduler_ExactlyOneMinuteCron_Accepted verifies that a cron that fires exactly
// once per minute ("* * * * *") is at the floor and is accepted.
func TestNewScheduler_ExactlyOneMinuteCron_Accepted(t *testing.T) {
	_, err := worker.NewScheduler("localhost:1", "", 0, "* * * * *", "guild-test")
	if err != nil {
		if strings.Contains(err.Error(), "minimum allowed interval") {
			t.Errorf("exactly-1-minute cron must not trigger the floor (it is at the minimum): %v", err)
		}
		// Any other error is infrastructure (no Valkey) — acceptable.
		t.Logf("expected Valkey-connection error: %v", err)
	}
}
