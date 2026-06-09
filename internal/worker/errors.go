package worker

import (
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
)

// TerminalError wraps an error to signal that the task should be archived immediately
// without further retries. Use this for fail-closed ACL errors (NFR-4, §3.2).
// Wrapping with asynq.SkipRetry causes asynq to archive the task.
type TerminalError struct {
	cause error
}

// NewTerminalError wraps cause as a terminal (non-retryable) worker error.
func NewTerminalError(cause error) error {
	return &TerminalError{cause: cause}
}

func (e *TerminalError) Error() string {
	return "worker: terminal error (fail-closed): " + e.cause.Error()
}

func (e *TerminalError) Unwrap() error { return e.cause }

// IsTerminalError reports whether err is (or wraps) a *TerminalError.
func IsTerminalError(err error) bool {
	var te *TerminalError
	return errors.As(err, &te)
}

// RetryDelayFunc returns the delay before the next retry attempt for the given error.
//
// For *ratelimit.RateLimitError: returns exactly the RetryAfter duration that Discord
// told us to wait (AC-5, AC-8, §3.2).
//
// For all other errors: exponential backoff with a 1-second base and jitter, capped
// at 10 minutes.
//
// This function signature matches asynq.Config.RetryDelayFunc.
func RetryDelayFunc(n int, err error, task *asynq.Task) time.Duration {
	// Rate-limit retry: honor Discord's Retry-After exactly.
	if ra := ratelimit.ExtractRetryAfter(err); ra > 0 {
		return ra
	}
	// Exponential backoff: base=1s, doubles per attempt, capped at 10 minutes.
	base := time.Second
	delay := base << min(uint(n), 10) // 1s, 2s, 4s, ... up to ~17 min; capped below
	const maxDelay = 10 * time.Minute
	if delay > maxDelay || delay < 0 {
		delay = maxDelay
	}
	return delay
}

// IsFailure reports whether err should count as a task failure for metrics.
//
// Rate-limit retries are expected flow — they do NOT increment the failure counter
// so error-rate dashboards stay honest (AC-8, NFR-7, §3.2).
//
// Terminal errors (fail-closed) are real failures and DO increment the counter.
//
// This function signature matches asynq.Config.IsFailure.
func IsFailure(err error) bool {
	return !ratelimit.IsRateLimitError(err)
}

// SkipRetryError wraps a cause to produce an error that tells asynq to archive
// the task immediately. Use it at call sites that need to return the sentinel.
func SkipRetryError(cause error) error {
	return fmt.Errorf("%w: %w", asynq.SkipRetry, cause)
}

// min returns the smaller of a and b (generic uint helper; Go 1.21+ has built-in min).
func min(a, b uint) uint {
	if a < b {
		return a
	}
	return b
}
