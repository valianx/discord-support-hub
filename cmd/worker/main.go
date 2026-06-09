// cmd/worker is the asynq worker entrypoint.
// It boots the asynq.Server with the four-queue topology, registers stub handlers,
// and shuts down gracefully on SIGINT/SIGTERM.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/valianx/discord-support-hub/internal/config"
	"github.com/valianx/discord-support-hub/internal/discord"
	"github.com/valianx/discord-support-hub/internal/observability"
	pgstore "github.com/valianx/discord-support-hub/internal/store/postgres"
	"github.com/valianx/discord-support-hub/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	observability.InitLogger(cfg.LogLevel)

	if err = cfg.RequireDiscordToken(); err != nil {
		slog.Error("startup: missing required config", "error", err)
		os.Exit(1)
	}

	// Open the Discord session — bot token from env only (NFR-6).
	discordSession, err := discord.New(cfg.DiscordBotToken, cfg.DiscordGuildID)
	if err != nil {
		slog.Error("startup: discord session failed", "error", err)
		os.Exit(1)
	}
	defer discordSession.Close() //nolint:errcheck

	// Postgres pool — required for M1 worker handlers.
	ctx := context.Background()
	pg, err := pgstore.New(ctx, cfg.PostgresDSN)
	if err != nil {
		slog.Error("startup: postgres connect failed", "error", err)
		os.Exit(1)
	}
	defer pg.Close()

	// Build and start the asynq server.
	srv := worker.New(worker.Config{
		RedisAddr:      cfg.ValkeyAddr,
		RedisPassword:  cfg.ValkeyPassword,
		RedisDB:        cfg.ValkeyDB,
		Concurrency:    cfg.WorkerConcurrency,
		Store:          pg,
		DiscordClient:  discordSession,
		DiscordGuildID: cfg.DiscordGuildID,
		AgentRoleID:    cfg.DiscordAgentRoleID,
	})

	// Start the worker in a goroutine; it blocks until Shutdown is called.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("worker: starting", "concurrency", cfg.WorkerConcurrency)
		if err := srv.Start(); err != nil {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
		slog.Info("worker: shutting down...")
	case err := <-errCh:
		slog.Error("worker: server error", "error", err)
	}

	srv.Shutdown()
	slog.Info("worker: stopped")
}
