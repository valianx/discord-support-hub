// Package ratelimit provides a Valkey-backed distributed token bucket that all workers
// consult before making any Discord API call (NFR-2, docs/02-architecture.md §3.1).
// In M0 the interface is defined; the Lua-based implementation lands in M2.
package ratelimit

import "context"

// Limiter is the rate-limit abstraction. The implementation uses atomic Lua scripts
// over Valkey to guard the global Discord budget and per-route buckets (§3.1).
type Limiter interface {
	// TakeGlobal acquires one token from the global (cross-worker) budget.
	// Returns an error with the wait duration when the bucket is empty.
	// TODO(M2): implement with Valkey EVAL + Lua.
	TakeGlobal(ctx context.Context) error

	// TakeRoute acquires one token from the per-route bucket.
	// routeKey is derived from method + route + majorParameter.
	// TODO(M2): implement with Valkey EVAL + Lua.
	TakeRoute(ctx context.Context, routeKey string) error
}

// NoopLimiter is a pass-through that always allows calls.
// Used in M0/M1 before the real implementation lands.
type NoopLimiter struct{}

func (NoopLimiter) TakeGlobal(_ context.Context) error          { return nil }
func (NoopLimiter) TakeRoute(_ context.Context, _ string) error { return nil }
