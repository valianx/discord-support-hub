// cmd/worker is the asynq worker entrypoint.
// It boots the asynq.Server with the five-queue topology, registers handlers,
// and shuts down gracefully on SIGINT/SIGTERM.
// M6: OAuth2 token store removed (AC-M6-9). Email sender (SMTP) added (AC-M6-5).
//     notify queue added (AC-M6-6). Welcome channel config wired (AC-M6-7).
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
	"github.com/valianx/discord-support-hub/internal/email"
	"github.com/valianx/discord-support-hub/internal/lock"
	obsv "github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/reconcile"
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
	// The Agent role must be a real, distinct role — not @everyone (NFR-5).
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

	// M6: SMTP email sender for the notify queue (AC-M6-5).
	// Validation occurs at send time; the sender is always constructed even if SMTP_HOST is empty
	// (the send_invite handler will log a warning and skip the send if config is incomplete).
	emailSender := email.NewSender(email.Config{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword, // never logged
		From:     cfg.SMTPFrom,
	})

	// Valkey client — used for the distributed reconcile lock (SEC-M5-002).
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.ValkeyAddr,
		Password: cfg.ValkeyPassword,
		DB:       cfg.ValkeyDB,
	})
	defer rdb.Close() //nolint:errcheck

	// M6: reconcile engine — role-based diff and repair (AC-M6-8).
	// fix(SEC-M5-002): use NewEngineWithLocker so concurrent scheduled sweeps acquire a
	// per-space lock before reconciling, preventing doubled Discord calls.
	// fix(AC-2): WithMetrics so the guild sweep updates hub_active_spaces_total each run.
	reconcileEngine := reconcile.NewEngineWithLocker(pg, discordSession, cfg.DiscordGuildID, lock.New(rdb)).
		WithMetrics(metrics)

	// M5: wire the asynq Scheduler for the scheduled full-guild reconcile sweep (AC-5).
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

	// Transactional outbox relay — enqueues pending outbox rows as asynq tasks (NFR-3).
	relayCtx, relayCancel := context.WithCancel(ctx)
	queueClient := queue.NewClient(cfg.ValkeyAddr, cfg.ValkeyPassword, cfg.ValkeyDB)
	defer func() {
		relayCancel()
		_ = queueClient.Close()
	}()
	relay := worker.NewRelay(worker.RelayConfig{
		Store:       pg,
		QueueClient: queueClient,
	})
	go func() {
		slog.Info("outbox relay: starting")
		relay.Run(relayCtx)
		slog.Info("outbox relay: stopped")
	}()

	// Build and start the asynq server.
	srv := worker.New(worker.Config{
		RedisAddr:             cfg.ValkeyAddr,
		RedisPassword:         cfg.ValkeyPassword,
		RedisDB:               cfg.ValkeyDB,
		Concurrency:           cfg.WorkerConcurrency,
		Store:                 pg,
		DiscordClient:         discordSession,
		DiscordGuildID:        cfg.DiscordGuildID,
		AgentRoleID:           cfg.DiscordAgentRoleID,
		DefaultCategoryID:     cfg.DiscordCategoryID,
		ReconcileEngine:       reconcileEngine,
		AgentNicknameSuffix:   cfg.AgentNicknameSuffix,
		Metrics:               metrics,
		EmailSender:           emailSender,           // AC-M6-5
		WelcomeChannelName:    cfg.WelcomeChannelName, // AC-M6-7
		WelcomeChannelMessage: cfg.WelcomeMessage,     // AC-M6-7
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
