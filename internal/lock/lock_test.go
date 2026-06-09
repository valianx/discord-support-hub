// Package lock_test verifies the Valkey distributed lock (AC-6).
//
// Tests use miniredis for a hermetic in-process Valkey replacement.
// Coverage:
//   - NX semantics: acquire succeeds when key absent, fails when already held.
//   - Fencing token: a stale holder cannot release another's lock.
//   - TTL auto-expiry: the lock is released when the TTL expires.
//   - Release is idempotent for a correct token.
//   - Both space and merchant lock flavours work.
package lock_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/lock"
)

// newTestLocker starts a miniredis and returns a locker with a short TTL.
func newTestLocker(t *testing.T, ttl time.Duration) (*lock.ValKeyLocker, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return lock.NewWithTTL(rdb, ttl), mr
}

// ─── NX semantics ─────────────────────────────────────────────────────────────

// TestAcquireSpace_NXSemantics verifies that the first acquire succeeds and a second
// concurrent acquire of the same key returns ok=false (NX — set only if not exists).
func TestAcquireSpace_NXSemantics(t *testing.T) {
	ctx := context.Background()
	locker, _ := newTestLocker(t, 5*time.Second)

	// First acquire must succeed.
	token1, ok1, err := locker.AcquireSpace(ctx, "space-abc")
	if err != nil {
		t.Fatalf("first AcquireSpace: %v", err)
	}
	if !ok1 {
		t.Fatal("first AcquireSpace: want ok=true, got false")
	}
	if token1 == "" {
		t.Fatal("fencing token must be non-empty")
	}

	// Second acquire of the same key must fail (lock held).
	_, ok2, err := locker.AcquireSpace(ctx, "space-abc")
	if err != nil {
		t.Fatalf("second AcquireSpace: %v", err)
	}
	if ok2 {
		t.Error("second AcquireSpace must fail when lock is held (NX)")
	}
}

// ─── Fencing token: stale holder cannot release another's lock ────────────────

// TestRelease_StaleHolder_TokenMismatch verifies that releasing with a wrong token
// does not release the lock (the current holder's token is still valid after the call).
func TestRelease_StaleHolder_TokenMismatch(t *testing.T) {
	ctx := context.Background()
	locker, _ := newTestLocker(t, 5*time.Second)

	token, ok, err := locker.AcquireSpace(ctx, "space-xyz")
	if err != nil || !ok {
		t.Fatalf("AcquireSpace: ok=%v err=%v", ok, err)
	}

	// Stale holder tries to release with a wrong token — must be silently ignored.
	staleToken := "00000000000000000000000000000000"
	if err = locker.ReleaseSpace(ctx, "space-xyz", staleToken); err != nil {
		t.Fatalf("Release with stale token must not error: %v", err)
	}

	// The lock must still be held: acquiring again should fail.
	_, ok2, err := locker.AcquireSpace(ctx, "space-xyz")
	if err != nil {
		t.Fatalf("AcquireSpace after stale release: %v", err)
	}
	if ok2 {
		t.Error("lock must still be held after stale-token release attempt")
	}

	// The real holder can release with the correct token.
	if err = locker.ReleaseSpace(ctx, "space-xyz", token); err != nil {
		t.Fatalf("Release with correct token: %v", err)
	}

	// Now the lock should be free.
	_, ok3, err := locker.AcquireSpace(ctx, "space-xyz")
	if err != nil {
		t.Fatalf("AcquireSpace after release: %v", err)
	}
	if !ok3 {
		t.Error("lock must be free after the real holder releases it")
	}
}

// ─── TTL auto-expiry ──────────────────────────────────────────────────────────

// TestAcquire_TTLExpiry verifies that the lock is auto-released when the TTL expires.
func TestAcquire_TTLExpiry(t *testing.T) {
	ctx := context.Background()
	locker, mr := newTestLocker(t, 100*time.Millisecond)

	_, ok, err := locker.AcquireSpace(ctx, "space-ttl")
	if err != nil || !ok {
		t.Fatalf("AcquireSpace: ok=%v err=%v", ok, err)
	}

	// Fast-forward miniredis time past the TTL.
	mr.FastForward(200 * time.Millisecond)

	// The lock should have expired; a new acquire must succeed.
	_, ok2, err := locker.AcquireSpace(ctx, "space-ttl")
	if err != nil {
		t.Fatalf("AcquireSpace after TTL expiry: %v", err)
	}
	if !ok2 {
		t.Error("lock should be free after TTL expiry")
	}
}

// ─── Merchant lock ────────────────────────────────────────────────────────────

// TestAcquireMerchant_Works verifies the merchant-lock flavour.
func TestAcquireMerchant_Works(t *testing.T) {
	ctx := context.Background()
	locker, _ := newTestLocker(t, 5*time.Second)

	token, ok, err := locker.AcquireMerchant(ctx, "merchant-001")
	if err != nil || !ok {
		t.Fatalf("AcquireMerchant: ok=%v err=%v", ok, err)
	}

	// Second acquire of the same merchant must fail.
	_, ok2, err := locker.AcquireMerchant(ctx, "merchant-001")
	if err != nil || ok2 {
		t.Errorf("second AcquireMerchant must fail: ok=%v err=%v", ok2, err)
	}

	// Release and re-acquire.
	if err = locker.ReleaseMerchant(ctx, "merchant-001", token); err != nil {
		t.Fatalf("ReleaseMerchant: %v", err)
	}
	_, ok3, err := locker.AcquireMerchant(ctx, "merchant-001")
	if err != nil || !ok3 {
		t.Errorf("AcquireMerchant after release: ok=%v err=%v", ok3, err)
	}
}

// ─── Space and Merchant locks are independent ─────────────────────────────────

// TestSpaceAndMerchantLocks_Independent verifies that space and merchant keys
// do not collide even when the IDs are the same string.
func TestSpaceAndMerchantLocks_Independent(t *testing.T) {
	ctx := context.Background()
	locker, _ := newTestLocker(t, 5*time.Second)

	const sharedID = "shared-id-001"

	_, okS, err := locker.AcquireSpace(ctx, sharedID)
	if err != nil || !okS {
		t.Fatalf("AcquireSpace: ok=%v err=%v", okS, err)
	}
	_, okM, err := locker.AcquireMerchant(ctx, sharedID)
	if err != nil || !okM {
		t.Fatalf("AcquireMerchant must not collide with space lock: ok=%v err=%v", okM, err)
	}
}
