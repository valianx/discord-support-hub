// cmd/keygen is the admin tool for minting service API keys.
//
// It generates a cryptographically random 32-byte opaque bearer token, prints the
// raw key ONCE (this is the only time the plaintext is visible), stores only the
// SHA-256 hash in api_keys, and records the key name and scope.
//
// Usage:
//
//	POSTGRES_DSN="..." go run ./cmd/keygen --name backoffice-prod [--scope backoffice]
//
// The raw key printed to stdout must be stored in the backoffice's secret manager
// immediately and never logged. The hub stores only the hash.
//
// Security contract (§5.1, NFR-6):
//   - The raw key is printed exactly once and never persisted.
//   - The hash (SHA-256) is stored in api_keys.key_hash.
//   - The key can be revoked instantly by running: go run ./cmd/keygen --revoke <key-id>
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/config"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/store"
	pgstore "github.com/valianx/discord-support-hub/internal/store/postgres"
)

func main() {
	name := flag.String("name", "", "Human-readable label for the key, e.g. 'backoffice-prod' (required)")
	scope := flag.String("scope", "backoffice", "Key scope: 'backoffice' or a narrower future scope")
	revoke := flag.String("revoke", "", "Revoke an existing key by its UUID (incompatible with --name)")
	flag.Parse()

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
	pg, err := pgstore.New(ctx, cfg.PostgresDSN)
	if err != nil {
		slog.Error("keygen: postgres connect failed", "error", err)
		os.Exit(1)
	}
	defer pg.Close()

	if *revoke != "" {
		if err := revokeKey(ctx, pg, *revoke); err != nil {
			slog.Error("keygen: revoke failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if *name == "" {
		fmt.Fprintln(os.Stderr, "usage: keygen --name <label> [--scope backoffice]")
		os.Exit(1)
	}

	if err := mintKey(ctx, pg, *name, *scope); err != nil {
		slog.Error("keygen: mint failed", "error", err)
		os.Exit(1)
	}
}

func mintKey(ctx context.Context, s *pgstore.Store, name, scope string) error {
	rawKey, err := authz.GenerateAPIKey()
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	hash := authz.HashAPIKey(rawKey)

	apiKey, err := s.CreateAPIKey(ctx, store.CreateAPIKeyParams{
		Name:    name,
		KeyHash: hash,
		Scope:   scope,
	})
	if err != nil {
		return fmt.Errorf("store key: %w", err)
	}

	// Print the raw key ONCE. Nothing else should print it.
	fmt.Printf("─────────────────────────────────────────────────────────\n")
	fmt.Printf("API key created (store the raw key now — it will not be shown again)\n")
	fmt.Printf("  ID:    %s\n", apiKey.ID)
	fmt.Printf("  Name:  %s\n", apiKey.Name)
	fmt.Printf("  Scope: %s\n", apiKey.Scope)
	fmt.Printf("\n")
	fmt.Printf("  Raw key (copy to your secret manager immediately):\n")
	fmt.Printf("  %s\n", rawKey)
	fmt.Printf("\n")
	fmt.Printf("  Use as:  Authorization: Bearer %s\n", rawKey)
	fmt.Printf("─────────────────────────────────────────────────────────\n")
	return nil
}

func revokeKey(ctx context.Context, s *pgstore.Store, id string) error {
	if err := s.RevokeAPIKey(ctx, id); err != nil {
		return err
	}
	fmt.Printf("API key %s revoked.\n", id)
	return nil
}
