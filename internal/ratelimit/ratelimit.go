// Package ratelimit provides a Valkey-backed distributed token bucket that all workers
// consult before making any Discord API call (NFR-2, docs/02-architecture.md §3.1).
//
// Two bucket levels are enforced:
//   - Global: one shared key across all workers; refilled at globalRefillRate/s.
//   - Per-route: keyed by method+route+majorParam; seeded from Discord response headers.
//
// All take operations are implemented as atomic Lua scripts (EVAL) so concurrent workers
// cannot over-subscribe a bucket in a check-then-act race.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// RateLimitError is returned when a bucket is empty. Callers (asynq RetryDelayFunc)
// extract RetryAfter to schedule the task for exact replay (AC-5, §3.2).
type RateLimitError struct {
	RetryAfter time.Duration
	Bucket     string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("ratelimit: bucket %q exhausted; retry after %s", e.Bucket, e.RetryAfter)
}

// IsRateLimitError reports whether err is (or wraps) a *RateLimitError.
func IsRateLimitError(err error) bool {
	var rle *RateLimitError
	return errors.As(err, &rle)
}

// ExtractRetryAfter returns the RetryAfter duration from a *RateLimitError.
// Returns 0 if err is not a *RateLimitError.
func ExtractRetryAfter(err error) time.Duration {
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	return 0
}

// Limiter is the rate-limit abstraction. The implementation uses atomic Lua scripts
// over Valkey to guard the global Discord budget and per-route buckets (§3.1).
type Limiter interface {
	// TakeGlobal acquires one token from the global (cross-worker) budget.
	// Returns a *RateLimitError with the wait duration when the bucket is empty.
	TakeGlobal(ctx context.Context) error

	// TakeRoute acquires one token from the per-route bucket.
	// routeKey is derived from method + route + majorParameter.
	TakeRoute(ctx context.Context, routeKey string) error

	// UpdateFromHeaders updates a per-route bucket from Discord's X-RateLimit-* headers.
	// Call this after each successful Discord response so sibling workers respect observed limits.
	UpdateFromHeaders(ctx context.Context, routeKey string, limit, remaining int, resetAt time.Time) error

	// PenalizeUntil freezes the global and the named route bucket until retryAfter.
	// Called when a Discord 429 response is received.
	PenalizeUntil(ctx context.Context, routeKey string, retryAfter time.Duration) error
}

// Config controls the token bucket parameters.
type Config struct {
	// GlobalRefillRate is the per-second token refill for the global bucket.
	// Conservative default: 45/s (Discord's global ceiling is ~50/s; we leave headroom).
	GlobalRefillRate float64

	// GlobalCapacity is the burst capacity of the global bucket.
	GlobalCapacity int

	// DefaultRouteCapacity is the default capacity for per-route buckets when no
	// Discord header has been observed yet.
	DefaultRouteCapacity int

	// KeyPrefix is prepended to every Valkey key (e.g. "rl:" for rate-limit).
	KeyPrefix string
}

// DefaultConfig returns a Config with conservative defaults safe for a single bot token.
func DefaultConfig() Config {
	return Config{
		GlobalRefillRate:     45,
		GlobalCapacity:       45,
		DefaultRouteCapacity: 5,
		KeyPrefix:            "rl:",
	}
}

// ValKeyLimiter is the real Valkey-backed implementation of Limiter.
type ValKeyLimiter struct {
	rdb    redis.UniversalClient
	cfg    Config
	script *redis.Script // atomic take script
}

// New creates a ValKeyLimiter connected to the given redis client.
func New(rdb redis.UniversalClient, cfg Config) *ValKeyLimiter {
	return &ValKeyLimiter{
		rdb:    rdb,
		cfg:    cfg,
		script: redis.NewScript(takeLuaScript),
	}
}

// takeLuaScript is an atomic GCRA/token-bucket take implemented in Lua.
//
// KEYS[1] = bucket key
// ARGV[1] = capacity (int)
// ARGV[2] = refill per second (float, tokens/s)
// ARGV[3] = current unix timestamp in milliseconds
//
// Returns an array: {allowed (0|1), remaining_tokens, wait_ms}
// allowed=1 means a token was consumed; allowed=0 means the bucket is empty and
// wait_ms is the number of milliseconds the caller should wait before retrying.
const takeLuaScript = `
local key      = KEYS[1]
local capacity = tonumber(ARGV[1])
local rate     = tonumber(ARGV[2])
local now_ms   = tonumber(ARGV[3])

local state = redis.call('HGETALL', key)
local tokens    = capacity
local last_ms   = now_ms

if #state > 0 then
  for i = 1, #state, 2 do
    if state[i] == 'tokens' then tokens = tonumber(state[i+1]) end
    if state[i] == 'last_ms' then last_ms = tonumber(state[i+1]) end
  end
  -- Refill tokens proportional to elapsed time.
  local elapsed_s = (now_ms - last_ms) / 1000
  tokens = math.min(capacity, tokens + elapsed_s * rate)
end

if tokens >= 1 then
  tokens = tokens - 1
  redis.call('HSET', key, 'tokens', tokens, 'last_ms', now_ms)
  -- TTL = capacity/rate seconds + buffer so idle buckets expire automatically.
  local ttl_ms = math.ceil((capacity / rate) * 1000) + 5000
  redis.call('PEXPIRE', key, ttl_ms)
  return {1, math.floor(tokens), 0}
else
  -- Calculate wait until next token is available.
  local need_s  = (1 - tokens) / rate
  local wait_ms = math.ceil(need_s * 1000)
  return {0, 0, wait_ms}
end
`

// TakeGlobal acquires one token from the global bucket.
func (l *ValKeyLimiter) TakeGlobal(ctx context.Context) error {
	key := l.cfg.KeyPrefix + "global"
	return l.take(ctx, key, l.cfg.GlobalCapacity, l.cfg.GlobalRefillRate)
}

// TakeRoute acquires one token from the named per-route bucket.
func (l *ValKeyLimiter) TakeRoute(ctx context.Context, routeKey string) error {
	key := l.cfg.KeyPrefix + "route:" + routeKey
	return l.take(ctx, key, l.cfg.DefaultRouteCapacity, float64(l.cfg.DefaultRouteCapacity))
}

func (l *ValKeyLimiter) take(ctx context.Context, key string, capacity int, rate float64) error {
	nowMS := time.Now().UnixMilli()

	res, err := l.script.Run(ctx, l.rdb, []string{key},
		capacity, rate, nowMS,
	).Slice()
	if err != nil {
		return fmt.Errorf("ratelimit: eval take script: %w", err)
	}
	if len(res) < 3 {
		return fmt.Errorf("ratelimit: unexpected script result length %d", len(res))
	}

	allowed := toInt64(res[0])
	if allowed == 1 {
		return nil
	}

	waitMS := toInt64(res[2])
	retryAfter := time.Duration(waitMS) * time.Millisecond
	if retryAfter < time.Millisecond {
		retryAfter = time.Millisecond
	}
	return &RateLimitError{RetryAfter: retryAfter, Bucket: key}
}

// maxPenaltyDuration caps how far into the future PenalizeUntil or UpdateFromHeaders
// may freeze a bucket. A malicious or erroneous Discord header value must not be able
// to lock the bucket indefinitely (medium security: input clamping).
const maxPenaltyDuration = 5 * time.Minute

// UpdateFromHeaders updates a per-route bucket using values observed from Discord's
// X-RateLimit-Remaining and X-RateLimit-Reset headers.
// This lets sibling workers respect the actual Discord-side quota, not just an estimate.
//
// Non-positive remaining values are ignored (a negative remaining from a malformed
// header must not drive the bucket below zero). The reset TTL is clamped to
// maxPenaltyDuration so an absurd future timestamp cannot freeze the bucket forever.
func (l *ValKeyLimiter) UpdateFromHeaders(
	ctx context.Context,
	routeKey string,
	limit, remaining int,
	resetAt time.Time,
) error {
	// clamp: ignore non-positive remaining (malformed/hostile header).
	if remaining < 0 {
		remaining = 0
	}

	key := l.cfg.KeyPrefix + "route:" + routeKey
	ttl := time.Until(resetAt) + 5*time.Second
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	// clamp: cap TTL so an absurd reset timestamp cannot freeze the bucket forever.
	if ttl > maxPenaltyDuration {
		ttl = maxPenaltyDuration
	}

	pipe := l.rdb.Pipeline()
	pipe.HSet(ctx, key,
		"tokens", remaining,
		"last_ms", time.Now().UnixMilli(),
		"capacity", limit,
	)
	pipe.PExpire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("ratelimit: update from headers: %w", err)
	}
	return nil
}

// PenalizeUntil freezes the global and named route bucket until retryAfter elapses.
// It sets tokens=0 and stamps last_ms so the refill calculation starts from now+retryAfter.
//
// Non-positive retryAfter values are ignored — a zero or negative Retry-After header
// must not be acted upon. The duration is capped at maxPenaltyDuration so an absurd
// value cannot freeze the bucket indefinitely.
func (l *ValKeyLimiter) PenalizeUntil(ctx context.Context, routeKey string, retryAfter time.Duration) error {
	// clamp: ignore non-positive retry-after (malformed/hostile header).
	if retryAfter <= 0 {
		return nil
	}
	// clamp: cap to sane maximum so a huge value cannot freeze the bucket forever.
	if retryAfter > maxPenaltyDuration {
		retryAfter = maxPenaltyDuration
	}

	futureMS := time.Now().Add(retryAfter).UnixMilli()
	ttlMS := retryAfter + 10*time.Second

	keys := []string{
		l.cfg.KeyPrefix + "global",
		l.cfg.KeyPrefix + "route:" + routeKey,
	}
	pipe := l.rdb.Pipeline()
	for _, k := range keys {
		pipe.HSet(ctx, k, "tokens", 0, "last_ms", futureMS)
		pipe.PExpire(ctx, k, ttlMS)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("ratelimit: penalize: %w", err)
	}
	return nil
}

// NoopLimiter is a pass-through that always allows calls.
// Used in M0/M1 before the real implementation lands, and in tests that do not
// exercise the rate-limit path.
type NoopLimiter struct{}

func (NoopLimiter) TakeGlobal(_ context.Context) error          { return nil }
func (NoopLimiter) TakeRoute(_ context.Context, _ string) error { return nil }
func (NoopLimiter) UpdateFromHeaders(_ context.Context, _ string, _, _ int, _ time.Time) error {
	return nil
}
func (NoopLimiter) PenalizeUntil(_ context.Context, _ string, _ time.Duration) error { return nil }

// ─── helpers ──────────────────────────────────────────────────────────────────

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(math.Round(n))
	default:
		return 0
	}
}
