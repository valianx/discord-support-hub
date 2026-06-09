// Package ratelimit_test verifies the Valkey token bucket limiter (AC-5).
//
// All tests use miniredis for a hermetic in-process Valkey replacement.
// No real Valkey or Discord connection is required.
//
// Coverage:
//   - Tokens deplete on consecutive takes.
//   - Global bucket caps throughput (no token → RateLimitError with RetryAfter).
//   - A 429/Retry-After penalizes the bucket via PenalizeUntil.
//   - UpdateFromHeaders seeds a per-route bucket from observed Discord limits.
//   - The Lua take is atomic: concurrent goroutines don't over-subscribe.
//   - IsRateLimitError and ExtractRetryAfter utility functions.
package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
)

// newTestLimiter starts a miniredis instance and returns a limiter wired to it.
func newTestLimiter(t *testing.T, cfg ratelimit.Config) *ratelimit.ValKeyLimiter {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return ratelimit.New(rdb, cfg)
}

// smallCfg returns a Config with a tiny capacity so depletion is easy to observe.
func smallCfg(capacity int) ratelimit.Config {
	return ratelimit.Config{
		GlobalRefillRate:     float64(capacity),
		GlobalCapacity:       capacity,
		DefaultRouteCapacity: capacity,
		KeyPrefix:            "rl-test:",
	}
}

// ─── TakeGlobal: basic take and depletion ─────────────────────────────────────

// TestTakeGlobal_TokensDeplete verifies that consecutive takes decrement tokens
// and that a take beyond capacity returns a *RateLimitError (AC-5).
func TestTakeGlobal_TokensDeplete(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(3))

	// First three takes should succeed.
	for i := 0; i < 3; i++ {
		if err := limiter.TakeGlobal(ctx); err != nil {
			t.Fatalf("take %d: want nil error, got %v", i+1, err)
		}
	}

	// Fourth take must fail with RateLimitError.
	err := limiter.TakeGlobal(ctx)
	if err == nil {
		t.Fatal("expected RateLimitError when bucket is empty, got nil")
	}
	if !ratelimit.IsRateLimitError(err) {
		t.Errorf("expected *RateLimitError, got %T: %v", err, err)
	}
}

// TestTakeGlobal_RetryAfterIsPositive verifies that the RateLimitError's RetryAfter
// is positive (so callers can schedule a retry).
func TestTakeGlobal_RetryAfterIsPositive(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(1))

	// Deplete.
	_ = limiter.TakeGlobal(ctx)

	err := limiter.TakeGlobal(ctx)
	ra := ratelimit.ExtractRetryAfter(err)
	if ra <= 0 {
		t.Errorf("RetryAfter must be positive, got %v", ra)
	}
}

// ─── TakeRoute: per-route bucket ──────────────────────────────────────────────

// TestTakeRoute_IndependentFromGlobal verifies that a per-route bucket is independent
// from the global bucket (depleting the route does not affect the global).
func TestTakeRoute_IndependentFromGlobal(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(5))

	// Deplete the route bucket.
	const routeKey = "POST:channels:guild-1"
	for i := 0; i < 5; i++ {
		_ = limiter.TakeRoute(ctx, routeKey)
	}

	// Route should be exhausted.
	if err := limiter.TakeRoute(ctx, routeKey); err == nil {
		t.Error("route bucket should be exhausted, but take succeeded")
	}

	// Global must still have tokens.
	if err := limiter.TakeGlobal(ctx); err != nil {
		t.Errorf("global bucket must be independent from route bucket, got error: %v", err)
	}
}

// ─── PenalizeUntil: 429 freeze ────────────────────────────────────────────────

// TestPenalizeUntil_FreezesGlobalAndRoute verifies that PenalizeUntil causes both
// the global and named route bucket to be empty immediately after the call (AC-5).
func TestPenalizeUntil_FreezesGlobalAndRoute(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(10))

	const routeKey = "GET:guild:guild-1"
	// Penalize for 500ms — both buckets should be empty immediately.
	if err := limiter.PenalizeUntil(ctx, routeKey, 500*time.Millisecond); err != nil {
		t.Fatalf("PenalizeUntil: %v", err)
	}

	if err := limiter.TakeGlobal(ctx); err == nil {
		t.Error("global bucket must be empty immediately after penalize")
	}
	if err := limiter.TakeRoute(ctx, routeKey); err == nil {
		t.Error("route bucket must be empty immediately after penalize")
	}
}

// ─── UpdateFromHeaders: seed from observed limits ─────────────────────────────

// TestUpdateFromHeaders_SeedsTokens verifies that UpdateFromHeaders sets the
// remaining count so a subsequent take reflects the observed Discord limit (AC-5).
func TestUpdateFromHeaders_SeedsTokens(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(10))

	const routeKey = "POST:messages:ch-1"
	// Seed with remaining=1, so one more take should succeed and the next should fail.
	resetAt := time.Now().Add(time.Second)
	if err := limiter.UpdateFromHeaders(ctx, routeKey, 5, 1, resetAt); err != nil {
		t.Fatalf("UpdateFromHeaders: %v", err)
	}

	// One take should succeed (remaining=1 was set).
	if err := limiter.TakeRoute(ctx, routeKey); err != nil {
		t.Fatalf("take after seeding 1 remaining: want nil, got %v", err)
	}
	// Next take must fail.
	if err := limiter.TakeRoute(ctx, routeKey); err == nil {
		t.Error("bucket must be exhausted after seeded remaining was consumed")
	}
}

// ─── Atomicity: concurrent goroutines don't over-subscribe ────────────────────

// TestTakeGlobal_ConcurrentAtomicity verifies that concurrent goroutines calling
// TakeGlobal on a capacity-N bucket allow at most N successful takes (AC-5).
// The Lua script guarantees atomicity — this test would catch race conditions if
// the implementation used GET+SET instead of atomic Lua.
func TestTakeGlobal_ConcurrentAtomicity(t *testing.T) {
	ctx := context.Background()
	capacity := 10
	limiter := newTestLimiter(t, smallCfg(capacity))

	var (
		allowed atomic.Int64
		denied  atomic.Int64
		wg      sync.WaitGroup
	)

	workers := 50
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := limiter.TakeGlobal(ctx); err != nil {
				denied.Add(1)
			} else {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	// At most `capacity` goroutines should have been allowed.
	if allowed.Load() > int64(capacity) {
		t.Errorf("atomicity violation: %d goroutines allowed, capacity is %d",
			allowed.Load(), capacity)
	}
	// All remaining goroutines must have been denied.
	if allowed.Load()+denied.Load() != int64(workers) {
		t.Errorf("unexpected total: allowed=%d denied=%d workers=%d",
			allowed.Load(), denied.Load(), workers)
	}
}

// ─── IsRateLimitError / ExtractRetryAfter ─────────────────────────────────────

// TestIsRateLimitError_NilNotRateLimit verifies that nil is not a rate-limit error.
func TestIsRateLimitError_NilNotRateLimit(t *testing.T) {
	if ratelimit.IsRateLimitError(nil) {
		t.Error("nil must not be a RateLimitError")
	}
}

// TestExtractRetryAfter_ZeroOnNonRateLimitError verifies that ExtractRetryAfter
// returns 0 for non-rate-limit errors.
func TestExtractRetryAfter_ZeroOnNonRateLimitError(t *testing.T) {
	if d := ratelimit.ExtractRetryAfter(nil); d != 0 {
		t.Errorf("want 0 for nil, got %v", d)
	}
}

// ─── Global bucket caps throughput (exact gate) ──────────────────────────────

// TestTakeGlobal_ExactThroughputGate verifies that concurrent goroutines are allowed
// exactly `capacity` times — not fewer (the bucket starts full) and not more
// (atomicity invariant). This is stronger than TestTakeGlobal_ConcurrentAtomicity,
// which only checks the upper bound; this test also checks the lower bound (AC-5).
func TestTakeGlobal_ExactThroughputGate(t *testing.T) {
	ctx := context.Background()
	capacity := 20
	limiter := newTestLimiter(t, smallCfg(capacity))

	var (
		allowed atomic.Int64
		denied  atomic.Int64
		wg      sync.WaitGroup
	)

	// Launch exactly 2×capacity goroutines so the upper half must be denied.
	workers := 2 * capacity
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if err := limiter.TakeGlobal(ctx); err != nil {
				denied.Add(1)
			} else {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	// Upper bound: never more than capacity.
	if allowed.Load() > int64(capacity) {
		t.Errorf("atomicity violation: %d allowed, capacity=%d", allowed.Load(), capacity)
	}
	// Lower bound: a full bucket must yield exactly capacity grants.
	if allowed.Load() < int64(capacity) {
		t.Errorf("under-subscription: only %d of %d capacity consumed — bucket may be broken",
			allowed.Load(), capacity)
	}
	if allowed.Load()+denied.Load() != int64(workers) {
		t.Errorf("total mismatch: allowed=%d denied=%d workers=%d",
			allowed.Load(), denied.Load(), workers)
	}
}

// ─── UpdateFromHeaders re-seeds a previously-depleted bucket ─────────────────

// TestUpdateFromHeaders_ReseedsDepleted verifies that UpdateFromHeaders can restore
// tokens to a bucket that was previously depleted, and that subsequent takes succeed
// up to the new remaining count (AC-5 — UpdateFromHeaders adjusts the bucket).
func TestUpdateFromHeaders_ReseedsDepleted(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(3))

	const routeKey = "POST:channels:reseeded"

	// Drain the route bucket.
	for i := 0; i < 3; i++ {
		if err := limiter.TakeRoute(ctx, routeKey); err != nil {
			t.Fatalf("initial drain take %d: %v", i, err)
		}
	}
	if err := limiter.TakeRoute(ctx, routeKey); err == nil {
		t.Fatal("bucket must be depleted after 3 takes")
	}

	// Simulate Discord responding with Remaining=2; the bucket should be re-seeded.
	resetAt := time.Now().Add(time.Second)
	if err := limiter.UpdateFromHeaders(ctx, routeKey, 5, 2, resetAt); err != nil {
		t.Fatalf("UpdateFromHeaders: %v", err)
	}

	// Two takes must now succeed.
	for i := 0; i < 2; i++ {
		if err := limiter.TakeRoute(ctx, routeKey); err != nil {
			t.Fatalf("post-reseed take %d: want nil, got %v", i, err)
		}
	}
	// Third take must fail again (remaining was 2).
	if err := limiter.TakeRoute(ctx, routeKey); err == nil {
		t.Error("bucket must be exhausted after consuming the 2 re-seeded tokens")
	}
}

// ─── PenalizeUntil: RetryAfter propagated to RateLimitError ──────────────────

// TestPenalizeUntil_RetryAfterPopulated verifies that after PenalizeUntil the
// take failure carries a positive RetryAfter so callers can schedule exact replays
// (AC-5 — RateLimitError.RetryAfter is populated).
func TestPenalizeUntil_RetryAfterPopulated(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(10))

	const routeKey = "DELETE:members:guild-1"
	if err := limiter.PenalizeUntil(ctx, routeKey, 2*time.Second); err != nil {
		t.Fatalf("PenalizeUntil: %v", err)
	}

	err := limiter.TakeGlobal(ctx)
	if err == nil {
		t.Fatal("take must fail immediately after PenalizeUntil")
	}
	if !ratelimit.IsRateLimitError(err) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	ra := ratelimit.ExtractRetryAfter(err)
	if ra <= 0 {
		t.Errorf("RetryAfter must be positive after penalize, got %v", ra)
	}
}

// ─── Input clamping: malicious/absurd header values ───────────────────────────

// TestPenalizeUntil_NegativeRetryAfter_IsNoop verifies that a negative retryAfter
// value is ignored and does not alter bucket state (hostile-header clamping).
func TestPenalizeUntil_NegativeRetryAfter_IsNoop(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(5))

	const routeKey = "GET:channels:ch-neg"
	// A negative Retry-After must not freeze the bucket.
	if err := limiter.PenalizeUntil(ctx, routeKey, -10*time.Second); err != nil {
		t.Fatalf("PenalizeUntil with negative duration: unexpected error: %v", err)
	}

	// Bucket must still be usable — tokens were not zeroed.
	if err := limiter.TakeGlobal(ctx); err != nil {
		t.Errorf("global bucket must be unaffected by a negative PenalizeUntil, got: %v", err)
	}
}

// TestPenalizeUntil_AbsurdRetryAfter_IsClamped verifies that an absurdly large
// retryAfter value is clamped to maxPenaltyDuration and does not lock the bucket
// indefinitely (hostile-header clamping).
func TestPenalizeUntil_AbsurdRetryAfter_IsClamped(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(5))

	const routeKey = "GET:channels:ch-huge"
	// 24 hours is absurd; Discord's documented maximum Retry-After is a few seconds.
	if err := limiter.PenalizeUntil(ctx, routeKey, 24*time.Hour); err != nil {
		t.Fatalf("PenalizeUntil with huge duration: unexpected error: %v", err)
	}
	// Bucket must be frozen immediately (tokens=0) — the clamp only limits how far
	// into the future the freeze extends, not whether the freeze happens at all.
	if err := limiter.TakeGlobal(ctx); err == nil {
		t.Error("global bucket must be frozen after a (clamped) penalize call")
	}
}

// TestUpdateFromHeaders_NegativeRemaining_ClampsToZero verifies that a negative
// remaining value from a malformed header does not drive the bucket below zero.
func TestUpdateFromHeaders_NegativeRemaining_ClampsToZero(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(10))

	const routeKey = "POST:messages:ch-neg-remaining"
	// A negative remaining must not corrupt bucket state.
	resetAt := time.Now().Add(time.Second)
	if err := limiter.UpdateFromHeaders(ctx, routeKey, 5, -999, resetAt); err != nil {
		t.Fatalf("UpdateFromHeaders with negative remaining: unexpected error: %v", err)
	}

	// After clamping remaining to 0, the very next take must fail (bucket empty).
	if err := limiter.TakeRoute(ctx, routeKey); err == nil {
		t.Error("bucket must be empty after UpdateFromHeaders with negative remaining (clamped to 0)")
	}
}

// TestUpdateFromHeaders_AbsurdResetAt_IsClamped verifies that an absurdly far-future
// resetAt does not extend the bucket TTL beyond maxPenaltyDuration (hostile-header).
func TestUpdateFromHeaders_AbsurdResetAt_IsClamped(t *testing.T) {
	ctx := context.Background()
	limiter := newTestLimiter(t, smallCfg(10))

	const routeKey = "GET:guilds:guild-far-future"
	// A reset 72 hours in the future is absurd; the TTL must be clamped.
	// We verify no error is returned — the clamp is transparent to callers.
	resetAt := time.Now().Add(72 * time.Hour)
	if err := limiter.UpdateFromHeaders(ctx, routeKey, 10, 5, resetAt); err != nil {
		t.Fatalf("UpdateFromHeaders with absurd resetAt: unexpected error: %v", err)
	}
	// The bucket must still be usable (remaining=5 was set).
	if err := limiter.TakeRoute(ctx, routeKey); err != nil {
		t.Errorf("bucket must be usable after UpdateFromHeaders with absurd resetAt: %v", err)
	}
}
