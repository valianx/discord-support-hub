// cmd/api is the HTTP API server entrypoint.
// It loads config, initialises dependencies, builds the Gin router,
// and serves until SIGINT or SIGTERM triggers a graceful shutdown.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/api"
	"github.com/valianx/discord-support-hub/internal/config"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/store/postgres"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// Config errors must fail loudly at startup (no silent fallbacks, NFR-6).
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	observability.InitLogger(cfg.LogLevel)

	if err = cfg.RequirePostgresDSN(); err != nil {
		slog.Error("startup: missing required config", "error", err)
		os.Exit(1)
	}
	if err = cfg.RequireDiscordToken(); err != nil {
		slog.Error("startup: missing required config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// Postgres pool.
	pg, err := postgres.New(ctx, cfg.PostgresDSN)
	if err != nil {
		slog.Error("startup: postgres connect failed", "error", err)
		os.Exit(1)
	}
	defer pg.Close()

	// Valkey (Redis-compatible) client — cache + coordination only, never source of truth.
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.ValkeyAddr,
		Password: cfg.ValkeyPassword,
		DB:       cfg.ValkeyDB,
	})
	defer rdb.Close()

	// Build the Gin router with the real pingers for the readiness probe.
	router := api.NewRouter(api.RouterConfig{
		CORSAllowedOrigins: cfg.CORSAllowedOrigins,
		PGPinger:           pg,
		RedisPinger:        &redisPinger{rdb},
	})

	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start serving in a goroutine so we can wait for the shutdown signal.
	go func() {
		slog.Info("api: listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("api: serve error", "error", err)
			os.Exit(1)
		}
	}()

	// Wait for OS signal to initiate graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("api: shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("api: shutdown error", "error", err)
	}

	slog.Info("api: stopped")
}

// redisPinger wraps the go-redis client to satisfy observability.Pinger.
type redisPinger struct {
	client *redis.Client
}

func (p *redisPinger) Ping(ctx context.Context) error {
	return p.client.Ping(ctx).Err()
}
