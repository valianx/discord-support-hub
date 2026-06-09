// cmd/migrate applies SQL migrations in lexical order.
// It is idempotent: already-applied migrations are skipped via a schema_migrations table.
// No external library is used — the runner is intentionally minimal and auditable.
package main

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valianx/discord-support-hub/internal/config"
	"github.com/valianx/discord-support-hub/internal/observability"
)

const migrationsDir = "migrations"

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	observability.InitLogger(cfg.LogLevel)

	if err = cfg.RequirePostgresDSN(); err != nil {
		slog.Error("startup: missing required config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, cfg.PostgresDSN)
	if err != nil {
		slog.Error("migrate: connect failed", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err = run(ctx, pool); err != nil {
		slog.Error("migrate: failed", "error", err)
		os.Exit(1)
	}

	slog.Info("migrate: done")
}

// run applies all unapplied migrations from the migrations/ directory in lexical order.
func run(ctx context.Context, pool *pgxpool.Pool) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return err
	}

	files, err := collectMigrationFiles(migrationsDir)
	if err != nil {
		return err
	}

	for _, f := range files {
		applied, err := isApplied(ctx, pool, f)
		if err != nil {
			return err
		}
		if applied {
			slog.Info("migrate: already applied, skipping", "file", f)
			continue
		}

		if err = applyMigration(ctx, pool, f); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
		slog.Info("migrate: applied", "file", f)
	}

	return nil
}

// ensureMigrationsTable creates the schema_migrations tracking table if absent.
// The table is idempotent (IF NOT EXISTS).
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			filename    TEXT PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	return err
}

// collectMigrationFiles returns *.sql files in dir sorted lexically.
func collectMigrationFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".sql") {
			files = append(files, d.Name())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}

// isApplied reports whether the named migration has already been applied.
func isApplied(ctx context.Context, pool *pgxpool.Pool, filename string) (bool, error) {
	var count int
	err := pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = $1",
		filename,
	).Scan(&count)
	return count > 0, err
}

// applyMigration executes the SQL file within a transaction and records it.
func applyMigration(ctx context.Context, pool *pgxpool.Pool, filename string) error {
	sql, err := os.ReadFile(filepath.Join(migrationsDir, filename))
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (filename) VALUES ($1)",
			filename,
		); err != nil {
			return fmt.Errorf("record migration: %w", err)
		}
		return nil
	})
}
