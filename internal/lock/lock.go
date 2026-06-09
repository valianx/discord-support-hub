// Package lock provides per-space and per-merchant distributed locks over Valkey (§3.3).
// Locks are coordination only — never authoritative. The reconciler converges state
// regardless of whether a lock was held.
// In M0 the interface is defined; the SET NX PX implementation lands in M2.
package lock

import "context"

// Locker acquires and releases distributed locks keyed by resource id.
type Locker interface {
	// AcquireSpace acquires the lock for a given space id.
	// Returns a release function and nil on success, or an error (with a
	// suggested wait duration embedded) when the lock is held.
	// TODO(M2): implement with Valkey SET NX PX + fencing token.
	AcquireSpace(ctx context.Context, spaceID string) (release func(), err error)

	// AcquireMerchant acquires the lock for a given merchant id.
	// TODO(M2): implement with Valkey SET NX PX + fencing token.
	AcquireMerchant(ctx context.Context, merchantID string) (release func(), err error)
}

// NoopLocker is a pass-through that always succeeds with a no-op release function.
// Used in M0/M1 before the real implementation lands.
type NoopLocker struct{}

func (NoopLocker) AcquireSpace(_ context.Context, _ string) (func(), error) {
	return func() {}, nil
}

func (NoopLocker) AcquireMerchant(_ context.Context, _ string) (func(), error) {
	return func() {}, nil
}
