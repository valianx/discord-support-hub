// Package worker_test — retry/backoff and IsFailure tests (AC-8).
//
// Verifies:
//   - RetryDelayFunc returns Retry-After for *RateLimitError (not exponential backoff).
//   - RetryDelayFunc returns exponential backoff for non-rate-limit errors.
//   - IsFailure returns false for *RateLimitError (rate-limit retries are not failures).
//   - IsFailure returns true for all other errors.
//   - SkipRetryError wraps a cause with asynq.SkipRetry.
package worker_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
	"github.com/valianx/discord-support-hub/internal/worker"
)

// ─── RetryDelayFunc ───────────────────────────────────────────────────────────

// TestRetryDelayFunc_RateLimitError_ReturnsRetryAfter verifies that a *RateLimitError
// causes RetryDelayFunc to return exactly RetryAfter (AC-5, AC-8, §3.2).
func TestRetryDelayFunc_RateLimitError_ReturnsRetryAfter(t *testing.T) {
	want := 42 * time.Second
	rlErr := &ratelimit.RateLimitError{RetryAfter: want, Bucket: "rl-test:global"}

	got := worker.RetryDelayFunc(1, rlErr, nil)
	if got != want {
		t.Errorf("RetryDelayFunc: want %v, got %v", want, got)
	}
}

// TestRetryDelayFunc_WrappedRateLimitError_ReturnsRetryAfter verifies that a wrapped
// *RateLimitError (via fmt.Errorf %w) still triggers the RetryAfter path.
func TestRetryDelayFunc_WrappedRateLimitError_ReturnsRetryAfter(t *testing.T) {
	want := 15 * time.Second
	wrapped := fmt.Errorf("discord: %w", &ratelimit.RateLimitError{RetryAfter: want, Bucket: "route"})

	got := worker.RetryDelayFunc(3, wrapped, nil)
	if got != want {
		t.Errorf("RetryDelayFunc: want %v, got %v for wrapped error", want, got)
	}
}

// TestRetryDelayFunc_OtherError_ExponentialBackoff verifies that generic errors
// receive exponential backoff (doubles each attempt, capped at 10 min).
func TestRetryDelayFunc_OtherError_ExponentialBackoff(t *testing.T) {
	err := errors.New("transient discord error")

	prev := worker.RetryDelayFunc(0, err, nil)
	for n := 1; n <= 5; n++ {
		curr := worker.RetryDelayFunc(n, err, nil)
		if curr <= prev {
			t.Errorf("attempt %d: want backoff > %v, got %v", n, prev, curr)
		}
		prev = curr
	}
}

// TestRetryDelayFunc_BackoffCap_10min verifies the cap at 10 minutes.
func TestRetryDelayFunc_BackoffCap_10min(t *testing.T) {
	err := errors.New("generic error")
	const maxDelay = 10 * time.Minute

	// After enough attempts the backoff should be capped.
	got := worker.RetryDelayFunc(100, err, nil)
	if got > maxDelay {
		t.Errorf("backoff cap: want <= 10m, got %v", got)
	}
}

// ─── IsFailure ────────────────────────────────────────────────────────────────

// TestIsFailure_RateLimitError_False verifies that a *RateLimitError does NOT
// increment the failure counter — rate limiting is expected flow (AC-8, NFR-7).
func TestIsFailure_RateLimitError_False(t *testing.T) {
	err := &ratelimit.RateLimitError{RetryAfter: time.Second, Bucket: "global"}
	if worker.IsFailure(err) {
		t.Error("IsFailure must return false for *RateLimitError (rate-limit is not a failure)")
	}
}

// TestIsFailure_WrappedRateLimitError_False verifies that a wrapped *RateLimitError
// is also not counted as a failure.
func TestIsFailure_WrappedRateLimitError_False(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", &ratelimit.RateLimitError{RetryAfter: 5 * time.Second, Bucket: "route"})
	if worker.IsFailure(wrapped) {
		t.Error("IsFailure must return false for wrapped *RateLimitError")
	}
}

// TestIsFailure_OtherError_True verifies that generic errors ARE counted as failures.
func TestIsFailure_OtherError_True(t *testing.T) {
	tests := []error{
		errors.New("discord: internal server error"),
		fmt.Errorf("timeout"),
		asynq.SkipRetry,
	}
	for _, err := range tests {
		if !worker.IsFailure(err) {
			t.Errorf("IsFailure(%v): want true, got false", err)
		}
	}
}

// ─── SkipRetryError ───────────────────────────────────────────────────────────

// TestSkipRetryError_WrapsAsynqSkipRetry verifies that SkipRetryError produces an
// error that wraps asynq.SkipRetry so asynq archives the task without further retries.
func TestSkipRetryError_WrapsAsynqSkipRetry(t *testing.T) {
	cause := errors.New("ACL apply failed: permission denied")
	err := worker.SkipRetryError(cause)

	if !errors.Is(err, asynq.SkipRetry) {
		t.Error("SkipRetryError must wrap asynq.SkipRetry")
	}
	if !errors.Is(err, cause) {
		t.Error("SkipRetryError must also wrap the original cause")
	}
}
