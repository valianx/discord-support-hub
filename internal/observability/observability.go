// Package observability configures structured logging and exposes health-check handlers.
package observability

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/secrets"
)

// InitLogger initialises the global slog logger with JSON output at the requested level.
// All log attributes that match secret field names are automatically redacted (NFR-6).
func InitLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := &redactingHandler{
		inner: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
	}
	slog.SetDefault(slog.New(handler))
}

// redactingHandler wraps a slog.Handler and scrubs known secret attribute keys.
type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	// Build a new record with secret fields redacted.
	clean := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if secrets.IsSecretKey(a.Key) {
			clean.AddAttrs(slog.String(a.Key, "***REDACTED***"))
		} else {
			clean.AddAttrs(a)
		}
		return true
	})
	return h.inner.Handle(ctx, clean)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		if secrets.IsSecretKey(a.Key) {
			redacted[i] = slog.String(a.Key, "***REDACTED***")
		} else {
			redacted[i] = a
		}
	}
	return &redactingHandler{inner: h.inner.WithAttrs(redacted)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

// Pinger is satisfied by any type that can check its own connectivity.
type Pinger interface {
	Ping(ctx context.Context) error
}

// LivezHandler responds 200 OK for Kubernetes liveness probes.
// It only verifies that the process is running, not that dependencies are healthy.
func LivezHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ReadyzHandler responds 200 when all dependencies are reachable, 503 otherwise.
// It pings Postgres and Valkey; either failure returns 503 to prevent traffic routing
// to an instance that cannot serve requests (NFR-7).
func ReadyzHandler(pgPinger Pinger, redisPinger Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		body := gin.H{"postgres": "ok", "valkey": "ok"}
		healthy := true

		if err := pgPinger.Ping(ctx); err != nil {
			slog.ErrorContext(ctx, "readyz: postgres ping failed", "error", err)
			body["postgres"] = "unavailable"
			healthy = false
		}

		if err := redisPinger.Ping(ctx); err != nil {
			slog.ErrorContext(ctx, "readyz: valkey ping failed", "error", err)
			body["valkey"] = "unavailable"
			healthy = false
		}

		if healthy {
			c.JSON(http.StatusOK, body)
		} else {
			c.JSON(http.StatusServiceUnavailable, body)
		}
	}
}
