// Package lock provides per-space and per-merchant distributed locks over Valkey (§3.3).
//
// Locks are acquired via SET key val NX PX ttl with a fencing token (random UUID).
// The release script uses compare-and-delete Lua to ensure a stale holder cannot
// release a lock that was re-acquired by another worker after the TTL expired.
//
// Locks are coordination only — never authoritative. If a lock is lost, the
// reconciler still converges state; the lock avoids wasted/conflicting Discord calls.
package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLockHeld is returned when Acquire finds the key already set (another holder).
var ErrLockHeld = errors.New("lock: already held by another worker")

// Locker acquires and releases distributed locks keyed by resource id.
type Locker interface {
	// AcquireSpace acquires the lock for a given space id.
	// Returns a fencing token and true on success, or ("", false, nil) when the
	// lock is already held. An error is returned only on infrastructure failure.
	AcquireSpace(ctx context.Context, spaceID string) (token string, ok bool, err error)

	// ReleaseSpace releases the lock for a space only when the provided token matches.
	// A token mismatch is silently ignored (fencing invariant — stale holder).
	ReleaseSpace(ctx context.Context, spaceID, token string) error

	// AcquireMerchant acquires the lock for a given merchant id.
	AcquireMerchant(ctx context.Context, merchantID string) (token string, ok bool, err error)

	// ReleaseMerchant releases the merchant lock only when the provided token matches.
	ReleaseMerchant(ctx context.Context, merchantID, token string) error
}

const (
	defaultTTL        = 30 * time.Second
	keyPrefixSpace    = "lock:space:"
	keyPrefixMerchant = "lock:merchant:"
)

// releaseScript is an atomic compare-and-delete. It deletes the key only when the
// stored value matches the provided token. This prevents a revived stale worker from
// releasing a lock it no longer holds (the fencing invariant, §3.3).
//
// KEYS[1] = lock key
// ARGV[1] = expected fencing token
// Returns 1 if released, 0 if token mismatch (stale holder), -1 if key absent.
const releaseScript = `
local v = redis.call('GET', KEYS[1])
if v == false then return -1 end
if v ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1])
return 1
`

// ValKeyLocker is the real Valkey-backed implementation of Locker.
type ValKeyLocker struct {
	rdb    redis.UniversalClient
	ttl    time.Duration
	script *redis.Script
}

// New creates a ValKeyLocker with the default 30-second TTL.
func New(rdb redis.UniversalClient) *ValKeyLocker {
	return NewWithTTL(rdb, defaultTTL)
}

// NewWithTTL creates a ValKeyLocker with a configurable TTL.
func NewWithTTL(rdb redis.UniversalClient, ttl time.Duration) *ValKeyLocker {
	return &ValKeyLocker{
		rdb:    rdb,
		ttl:    ttl,
		script: redis.NewScript(releaseScript),
	}
}

// AcquireSpace acquires the lock for the named space.
func (l *ValKeyLocker) AcquireSpace(ctx context.Context, spaceID string) (string, bool, error) {
	return l.acquire(ctx, keyPrefixSpace+spaceID)
}

// ReleaseSpace releases the space lock if the token matches.
func (l *ValKeyLocker) ReleaseSpace(ctx context.Context, spaceID, token string) error {
	return l.release(ctx, keyPrefixSpace+spaceID, token)
}

// AcquireMerchant acquires the lock for the named merchant.
func (l *ValKeyLocker) AcquireMerchant(ctx context.Context, merchantID string) (string, bool, error) {
	return l.acquire(ctx, keyPrefixMerchant+merchantID)
}

// ReleaseMerchant releases the merchant lock if the token matches.
func (l *ValKeyLocker) ReleaseMerchant(ctx context.Context, merchantID, token string) error {
	return l.release(ctx, keyPrefixMerchant+merchantID, token)
}

func (l *ValKeyLocker) acquire(ctx context.Context, key string) (string, bool, error) {
	token, err := newFencingToken()
	if err != nil {
		return "", false, fmt.Errorf("lock: generate fencing token: %w", err)
	}

	// SET key token NX PX ttl_ms — atomic: sets only if key does not exist.
	ok, err := l.rdb.SetNX(ctx, key, token, l.ttl).Result()
	if err != nil {
		return "", false, fmt.Errorf("lock: acquire %q: %w", key, err)
	}
	if !ok {
		return "", false, nil // lock held by another worker
	}
	return token, true, nil
}

func (l *ValKeyLocker) release(ctx context.Context, key, token string) error {
	res, err := l.script.Run(ctx, l.rdb, []string{key}, token).Int64()
	if err != nil {
		return fmt.Errorf("lock: release %q: %w", key, err)
	}
	// res=0 means token mismatch (stale holder) — silently ignore per fencing invariant.
	// res=-1 means key absent (already expired) — also fine.
	_ = res
	return nil
}

// newFencingToken generates a 16-byte (32 hex chars) cryptographically random token.
func newFencingToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NoopLocker is a pass-through that always succeeds with a no-op release.
// Used in M0/M1 before the real implementation lands.
type NoopLocker struct{}

func (NoopLocker) AcquireSpace(_ context.Context, _ string) (string, bool, error) {
	return "noop", true, nil
}
func (NoopLocker) ReleaseSpace(_ context.Context, _, _ string) error { return nil }
func (NoopLocker) AcquireMerchant(_ context.Context, _ string) (string, bool, error) {
	return "noop", true, nil
}
func (NoopLocker) ReleaseMerchant(_ context.Context, _, _ string) error { return nil }
