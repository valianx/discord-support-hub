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
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/secrets"
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
	// M3: the Agent role must be a real, distinct role — not @everyone (NFR-5).
	if err = cfg.RequireAgentRoleID(); err != nil {
		slog.Error("startup: missing required config", "error", err)
		os.Exit(1)
	}
	if err = cfg.ValidateEncryptionKey(); err != nil {
		slog.Error("startup: invalid encryption key — fix ENCRYPTION_KEY before starting", "error", err)
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

	// Valkey (Redis-compatible) client — cache + nonce store + coordination.
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.ValkeyAddr,
		Password: cfg.ValkeyPassword,
		DB:       cfg.ValkeyDB,
	})
	defer rdb.Close()

	// Queue client for handlers that enqueue async jobs.
	queueClient := queue.NewClient(cfg.ValkeyAddr, cfg.ValkeyPassword, cfg.ValkeyDB)
	defer queueClient.Close() //nolint:errcheck

	// M3: AES-256-GCM encrypter for OAuth2 token storage at rest (NFR-6).
	enc, err := secrets.NewEncrypter(cfg.EncryptionKey, 1)
	if err != nil {
		slog.Error("startup: could not initialise encrypter", "error", err)
		os.Exit(1)
	}

	// M3: Valkey-backed nonce store for HMAC state token single-use enforcement (AC-3).
	nonceStore := oauth.NewValkeyNonceStore(rdb)

	// M3: HMAC state manager — requires OAUTH_HMAC_SECRET (32+ bytes hex-encoded).
	var stateManager *oauth.StateManager
	if cfg.OAuthHMACSecret != "" {
		sm, smErr := oauth.NewStateManager(cfg.OAuthHMACSecret, nonceStore)
		if smErr != nil {
			slog.Error("startup: could not initialise OAuth2 state manager", "error", smErr)
			os.Exit(1)
		}
		stateManager = sm
	} else {
		slog.Warn("startup: OAUTH_HMAC_SECRET not set — OAuth2 callback will return 501 (non-fatal for non-OAuth deployments)")
	}

	// M3: token store wraps the encrypter and postgres store (NFR-6, AC-3).
	tokenStore := oauth.NewTokenStore(pg, enc)

	// Build the Gin router with real auth and handler dependencies.
	router := api.NewRouter(api.RouterConfig{
		CORSAllowedOrigins:       cfg.CORSAllowedOrigins,
		Store:                    pg,
		QueueClient:              queueClient,
		DiscordOAuthClientID:     cfg.DiscordOAuthClientID,
		DiscordOAuthClientSecret: cfg.DiscordOAuthClientSecret,
		DiscordOAuthRedirectURL:  cfg.DiscordOAuthRedirectURL,
		StateManager:             stateManager,
		TokenStore:               tokenStore,
		PGPinger:                 pg,
		RedisPinger:              &redisPinger{rdb},
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
