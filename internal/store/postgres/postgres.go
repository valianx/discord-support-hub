// Package postgres implements the store.Store interface using pgx/pgxpool (NFR-8).
package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valianx/discord-support-hub/internal/domain"
)

// Store is the PostgreSQL-backed implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New connects a pgxpool using the provided DSN and returns a Store.
// The caller is responsible for calling Close when the store is no longer needed.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	// Validate connectivity immediately so the caller knows the DSN is good.
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping on connect: %w", err)
	}

	slog.InfoContext(ctx, "postgres: connected", "dsn_host", dsn[:min(len(dsn), 40)])
	return &Store{pool: pool}, nil
}

// Close releases all pooled connections.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool exposes the underlying pgxpool for direct queries in later milestones.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Ping implements store.Store. Used by the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// GetMerchantByID implements store.Store.
// TODO(M1): implement the real query.
func (s *Store) GetMerchantByID(_ context.Context, _ string) (*domain.Merchant, error) {
	return nil, fmt.Errorf("GetMerchantByID: not implemented") // TODO(M1)
}

// GetUserByID implements store.Store.
// TODO(M1): implement the real query.
func (s *Store) GetUserByID(_ context.Context, _ string) (*domain.User, error) {
	return nil, fmt.Errorf("GetUserByID: not implemented") // TODO(M1)
}

// GetSpaceByID implements store.Store.
// TODO(M2): implement the real query.
func (s *Store) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	return nil, fmt.Errorf("GetSpaceByID: not implemented") // TODO(M2)
}

// GetJobByID implements store.Store.
// TODO(M2): implement the real query.
func (s *Store) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	return nil, fmt.Errorf("GetJobByID: not implemented") // TODO(M2)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
