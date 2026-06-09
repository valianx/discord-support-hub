// Package reconcile implements the desired-vs-real diff and repair engine (§4.2).
// In M0 only the interface is defined; the real diff+repair logic lands in M3.
package reconcile

import "context"

// Reconciler performs the Postgres-always-wins reconciliation sweep.
// It reads desired state from Postgres, reads real state from Discord, diffs them,
// and repairs Discord to match (revokes extra access, re-applies missing access).
type Reconciler interface {
	// ReconcileGuild performs a full sweep of the guild against all desired state.
	// Called on a schedule by the low-priority reconcile queue.
	// TODO(M3): implement.
	ReconcileGuild(ctx context.Context, guildID string) error

	// ReconcileSpace performs a targeted sweep of a single space.
	// Called after each successful mutation as a cheap consistency check.
	// TODO(M3): implement.
	ReconcileSpace(ctx context.Context, spaceID string) error
}

// NoopReconciler is a pass-through used in M0/M1/M2 before the real impl lands.
type NoopReconciler struct{}

func (NoopReconciler) ReconcileGuild(_ context.Context, _ string) error { return nil }
func (NoopReconciler) ReconcileSpace(_ context.Context, _ string) error { return nil }
