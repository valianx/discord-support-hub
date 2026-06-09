// Package cache_test verifies ValKeyCache Get/Set/Del over miniredis.
//
// The implementation report (02-implementation-m2a.md § Known Limitations) noted
// that internal/cache had no test file. These tests close that gap.
//
// All tests are hermetic — no real Valkey instance is required.
package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/cache"
)

// newTestCache starts a miniredis instance and returns a ValKeyCache wired to it,
// together with the underlying miniredis handle for time manipulation.
func newTestCache(t *testing.T) (*cache.ValKeyCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return cache.New(rdb), mr
}

// ─── Get/Set round-trip ────────────────────────────────────────────────────────

// TestCache_SetAndGet_RoundTrip verifies that a value stored with Set is retrieved
// unchanged by Get (basic round-trip correctness).
func TestCache_SetAndGet_RoundTrip(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)

	want := []byte(`{"id":"space-001","status":"active"}`)
	if err := c.Set(ctx, "space:space-001", want, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := c.Get(ctx, "space:space-001")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Get: want %q, got %q", want, got)
	}
}

// ─── Miss: key absent ─────────────────────────────────────────────────────────

// TestCache_Get_Miss_ReturnsNilNil verifies that a cache miss returns (nil, nil) —
// callers use nil to distinguish a miss from a real error.
func TestCache_Get_Miss_ReturnsNilNil(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)

	val, err := c.Get(ctx, "nonexistent-key")
	if err != nil {
		t.Errorf("Get miss must return nil error, got %v", err)
	}
	if val != nil {
		t.Errorf("Get miss must return nil value, got %q", val)
	}
}

// ─── TTL expiry ───────────────────────────────────────────────────────────────

// TestCache_Set_TTLExpiry verifies that a value becomes unavailable after its TTL
// elapses (miniredis FastForward moves internal time).
func TestCache_Set_TTLExpiry(t *testing.T) {
	ctx := context.Background()
	c, mr := newTestCache(t)

	if err := c.Set(ctx, "expiring-key", []byte("data"), 100*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// FastForward past the TTL.
	mr.FastForward(200 * time.Millisecond)

	val, err := c.Get(ctx, "expiring-key")
	if err != nil {
		t.Fatalf("Get after TTL expiry must not error: %v", err)
	}
	if val != nil {
		t.Errorf("Get after TTL expiry must return nil (cache miss), got %q", val)
	}
}

// ─── Del: write-invalidation ──────────────────────────────────────────────────

// TestCache_Del_RemovesKey verifies that Del removes a previously-stored key
// and the next Get returns a cache miss (write-invalidation strategy, §3.5).
func TestCache_Del_RemovesKey(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)

	if err := c.Set(ctx, "key-to-delete", []byte("value"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if err := c.Del(ctx, "key-to-delete"); err != nil {
		t.Fatalf("Del: %v", err)
	}

	val, err := c.Get(ctx, "key-to-delete")
	if err != nil {
		t.Fatalf("Get after Del must not error: %v", err)
	}
	if val != nil {
		t.Errorf("Get after Del must be a miss (nil), got %q", val)
	}
}

// TestCache_Del_MultipleKeys verifies that Del accepts variadic keys and removes all
// of them in one call (used by handlers to invalidate related entries together).
func TestCache_Del_MultipleKeys(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)

	keys := []string{"k1", "k2", "k3"}
	for _, k := range keys {
		if err := c.Set(ctx, k, []byte("v"), time.Minute); err != nil {
			t.Fatalf("Set %q: %v", k, err)
		}
	}

	if err := c.Del(ctx, keys...); err != nil {
		t.Fatalf("Del multiple keys: %v", err)
	}

	for _, k := range keys {
		val, err := c.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get %q after batch Del: %v", k, err)
		}
		if val != nil {
			t.Errorf("Get %q after Del must be a miss, got %q", k, val)
		}
	}
}

// TestCache_Del_EmptySlice_IsNoOp verifies that Del with no keys is a safe no-op.
func TestCache_Del_EmptySlice_IsNoOp(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestCache(t)

	if err := c.Del(ctx); err != nil {
		t.Errorf("Del with no keys must not error, got %v", err)
	}
}

// ─── NoopCache ────────────────────────────────────────────────────────────────

// TestNoopCache_AlwaysMisses verifies that NoopCache always returns nil on Get
// and does not error on Set/Del (used in M0/M1 before the real impl lands).
func TestNoopCache_AlwaysMisses(t *testing.T) {
	ctx := context.Background()
	c := cache.NoopCache{}

	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Errorf("NoopCache Set must not error: %v", err)
	}
	val, err := c.Get(ctx, "k")
	if err != nil {
		t.Errorf("NoopCache Get must not error: %v", err)
	}
	if val != nil {
		t.Errorf("NoopCache Get must always return nil (cache miss), got %q", val)
	}
	if err := c.Del(ctx, "k"); err != nil {
		t.Errorf("NoopCache Del must not error: %v", err)
	}
}
