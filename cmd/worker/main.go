// cmd/worker is the asynq worker entrypoint.
// It boots the asynq.Server with the four-queue topology, registers handlers,
// and shuts down gracefully on SIGINT/SIGTERM.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"
	"github.com/valianx/discord-support-hub/internal/config"
	"github.com/valianx/discord-support-hub/internal/discord"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/oauth"
	obsv "github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/reconcile"
	"github.com/valianx/discord-support-hub/internal/secrets"
	pgstore "github.com/valianx/discord-support-hub/internal/store/postgres"
	"github.com/valianx/discord-support-hub/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	obsv.InitLogger(cfg.LogLevel)

	// M5: initialise the Prometheus metrics registry (AC-2) so /metrics reflects real outcomes.
	metrics := obsv.InitMetrics()

	if err = cfg.RequireDiscordToken(); err != nil {
		slog.Error("startup: missing required config", "error", err)
		os.Exit(1)
	}
	// M3: the Agent role must be a real, distinct role — not @everyone (NFR-5).
	if err = cfg.RequireAgentRoleID(); err != nil {
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

	// Postgres pool — required for M1+ worker handlers.
	ctx := context.Background()
	pg, err := pgstore.New(ctx, cfg.PostgresDSN)
	if err != nil {
		slog.Error("startup: postgres connect failed", "error", err)
		os.Exit(1)
	}
	defer pg.Close()

	// M3: AES-256-GCM encrypter for loading OAuth2 tokens needed by guilds.join (NFR-6).
	// If ENCRYPTION_KEY is absent the token store is omitted (invite_collaborator falls back
	// to not calling GuildMemberAdd — the bot must add the user manually or via another flow).
	var tokenStore *oauth.TokenStore
	if cfg.EncryptionKey != "" {
		enc, encErr := secrets.NewEncrypter(cfg.EncryptionKey, 1)
		if encErr != nil {
			slog.Error("startup: could not initialise encrypter", "error", encErr)
			os.Exit(1)
		}
		tokenStore = oauth.NewTokenStore(pg, enc)
	} else {
		slog.Warn("startup: ENCRYPTION_KEY not set — guilds.join will be skipped in invite_collaborator (non-fatal if OAuth2 not used)")
	}

	// Valkey client — used for the distributed reconcile lock (SEC-M5-002).
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.ValkeyAddr,
		Password: cfg.ValkeyPassword,
		DB:       cfg.ValkeyDB,
	})
	defer rdb.Close() //nolint:errcheck

	// M3/M5: reconcile engine — desired-state vs real Discord diff and repair (§4.2).
	// fix(SEC-M5-002): use NewEngineWithLocker so concurrent scheduled sweeps acquire a
	// per-space lock before reconciling, preventing doubled Discord calls.
	// fix(AC-2): WithMetrics so the guild sweep updates hub_active_spaces_total each run.
	reconcileEngine := reconcile.NewEngineWithLocker(pg, discordSession, lock.New(rdb)).
		WithMetrics(metrics)

	// M5: wire the asynq Scheduler for the scheduled full-guild reconcile sweep (AC-5).
	// The scheduler enqueues a reconcile:guild task on the low-priority reconcile queue at
	// the cron interval from config (default every 5 minutes). An empty cron disables it.
	if cfg.ReconcileSweepCron != "" {
		scheduler, schedErr := worker.NewScheduler(
			cfg.ValkeyAddr, cfg.ValkeyPassword, cfg.ValkeyDB,
			cfg.ReconcileSweepCron, cfg.DiscordGuildID,
		)
		if schedErr != nil {
			slog.Error("startup: reconcile scheduler init failed", "error", schedErr)
			os.Exit(1)
		}
		go func() {
			slog.Info("scheduler: starting", "cron", cfg.ReconcileSweepCron)
			if err := scheduler.Start(); err != nil {
				slog.Error("scheduler: start error", "error", err)
			}
		}()
		defer scheduler.Stop()
	}

	// Build and start the asynq server.
	// fix(NFR-5): AgentRoleID and DefaultCategoryID are now wired so the provision handler
	// uses the real Agent role (not @everyone) and can apply the category allow consistently.
	// fix(AC-5): AgentNicknameSuffix wired so AGENT_NICKNAME_SUFFIX env var can enable
	// nickname marking at runtime (was always disabled regardless of env var).
	srv := worker.New(worker.Config{
		RedisAddr:           cfg.ValkeyAddr,
		RedisPassword:       cfg.ValkeyPassword,
		RedisDB:             cfg.ValkeyDB,
		Concurrency:         cfg.WorkerConcurrency,
		Store:               pg,
		DiscordClient:       discordSession,
		DiscordGuildID:      cfg.DiscordGuildID,
		AgentRoleID:         cfg.DiscordAgentRoleID,
		DefaultCategoryID:   cfg.DiscordCategoryID,
		TokenStore:          tokenStore,
		ReconcileEngine:     reconcileEngine,
		AgentNicknameSuffix: cfg.AgentNicknameSuffix,
		Metrics:             metrics, // fix(AC-2): wire real metrics to provision worker
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
