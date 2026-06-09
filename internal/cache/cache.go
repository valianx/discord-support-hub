// Package cache wraps Valkey for read-through caching (get/set/invalidate with TTL).
// Valkey is cache and coordination only — never a source of truth (docs/01-mvp-scope.md §4).
// Implemented in M2; M3/M4 endpoints wire it for directory/listing reads.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Cache is the read cache abstraction used by read endpoints.
type Cache interface {
	// Get retrieves a cached value by key. Returns (nil, nil) on a cache miss.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores a value with the given TTL.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Del invalidates one or more cache keys on a write (write-invalidation strategy).
	Del(ctx context.Context, keys ...string) error
}

// ValKeyCache is the real Valkey-backed implementation of Cache.
type ValKeyCache struct {
	rdb redis.UniversalClient
}

// New creates a ValKeyCache backed by the provided redis client.
func New(rdb redis.UniversalClient) *ValKeyCache {
	return &ValKeyCache{rdb: rdb}
}

// Get returns the cached value, or (nil, nil) on a miss.
func (c *ValKeyCache) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil // cache miss
	}
	if err != nil {
		return nil, fmt.Errorf("cache: get %q: %w", key, err)
	}
	return val, nil
}

// Set stores value under key with the given TTL.
func (c *ValKeyCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

// Del removes one or more keys from the cache (write-invalidation).
func (c *ValKeyCache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("cache: del: %w", err)
	}
	return nil
}

// NoopCache is a cache that always misses. Used in M0/M1 before the real impl lands.
type NoopCache struct{}

func (NoopCache) Get(_ context.Context, _ string) ([]byte, error)                  { return nil, nil }
func (NoopCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error { return nil }
func (NoopCache) Del(_ context.Context, _ ...string) error                         { return nil }
