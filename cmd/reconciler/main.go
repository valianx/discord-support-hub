// cmd/reconciler is the reconciler entrypoint.
// In M0 it boots and idles — the real reconcile loop and asynq scheduler land in M3.
// It can optionally be folded into cmd/worker (both architectures are valid; they are
// kept separate in M0 to match the §8 layout exactly).
package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/valianx/discord-support-hub/internal/config"
	"github.com/valianx/discord-support-hub/internal/observability"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	observability.InitLogger(cfg.LogLevel)

	slog.Info("reconciler: started (idle in M0; real loop lands in M3)")

	// TODO(M3): boot asynq.Scheduler for periodic full-guild reconcile sweeps.
	// TODO(M3): register KindReconcileGuild task on the reconcile queue.

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("reconciler: stopped")
}
