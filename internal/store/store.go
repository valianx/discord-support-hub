// Package store defines the storage interface and its adapters (NFR-8 pluggable storage).
// All database access goes through the Store interface so implementations can be swapped.
package store

import (
	"context"

	"github.com/valianx/discord-support-hub/internal/domain"
)

// Store is the primary storage abstraction. Only methods needed by M0 stubs are listed;
// later milestones add the real implementations.
type Store interface {
	// Ping checks whether the database is reachable. Used by the readiness probe.
	Ping(ctx context.Context) error

	// --- Merchants ---

	// GetMerchantByID returns the merchant for the given id.
	// TODO(M1): implement
	GetMerchantByID(ctx context.Context, id string) (*domain.Merchant, error)

	// --- Users ---

	// GetUserByID returns the user for the given id.
	// TODO(M1): implement
	GetUserByID(ctx context.Context, id string) (*domain.User, error)

	// --- Spaces ---

	// GetSpaceByID returns the space for the given id.
	// TODO(M2): implement
	GetSpaceByID(ctx context.Context, id string) (*domain.Space, error)

	// --- Jobs ---

	// GetJobByID returns the job for the given id (Postgres mirror of asynq state).
	// TODO(M2): implement
	GetJobByID(ctx context.Context, id string) (*domain.Job, error)
}
