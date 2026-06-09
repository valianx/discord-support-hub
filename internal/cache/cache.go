// Package cache wraps Valkey for read-through caching (get/set/invalidate with TTL).
// Valkey is cache and coordination only — never a source of truth (docs/01-mvp-scope.md §4).
// In M0 the interface is defined; the real implementation lands in M2.
package cache

import (
	"context"
	"time"
)

// Cache is the read cache abstraction used by read endpoints.
type Cache interface {
	// Get retrieves a cached value by key. Returns (nil, nil) on a cache miss.
	// TODO(M2): implement with go-redis Get.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores a value with the given TTL.
	// TODO(M2): implement with go-redis Set.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Del invalidates one or more cache keys on a write (write-invalidation strategy).
	// TODO(M2): implement with go-redis Del.
	Del(ctx context.Context, keys ...string) error
}

// NoopCache is a cache that always misses. Used in M0/M1 before the real impl lands.
type NoopCache struct{}

func (NoopCache) Get(_ context.Context, _ string) ([]byte, error)                  { return nil, nil }
func (NoopCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error { return nil }
func (NoopCache) Del(_ context.Context, _ ...string) error                         { return nil }
