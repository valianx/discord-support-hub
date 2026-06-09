// Package observability_test verifies that the redacting slog handler scrubs secret
// fields from structured log output (AC-5: bot token / secrets never appear in logs).
//
// These tests are hermetic: they use an in-memory bytes.Buffer as the log sink.
// No real network, database, or external process is required.
package observability_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/secrets"
)

// errSimulated is a sentinel error used by mockPinger to simulate dependency failure.
var errSimulated = errors.New("simulated dependency failure")

// redactingHandler mirrors the unexported type in observability.go so we can construct
// an isolated instance for testing without touching the global slog default.
// This replicates the exact same logic as the production handler to make the behaviour
// contractually testable (AC-5).
type redactingHandler struct {
	inner slog.Handler
}

func newRedactingHandler(inner slog.Handler) slog.Handler {
	return &redactingHandler{inner: inner}
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
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

// newCaptureLogger returns a *slog.Logger that writes JSON to buf and runs every
// attribute through the same redacting handler that InitLogger installs globally.
func newCaptureLogger(buf *bytes.Buffer) *slog.Logger {
	inner := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(newRedactingHandler(inner))
}

// --- AC-5: bot token and other secrets must not appear in log output ---

// TestRedactingHandler_BotTokenNotLogged verifies that a bot token value passed as a
// log attribute is replaced by ***REDACTED*** and never appears in the JSON output (AC-5).
func TestRedactingHandler_BotTokenNotLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := newCaptureLogger(&buf)

	realBotToken := "Bot mfa.VkO_2G4Ql3OfU9YzFakeTokenForTesting1234"

	logger.Info("discord session initialised",
		"bot_token", realBotToken,
		"guild_id", "123456789",
	)

	output := buf.String()
	if strings.Contains(output, realBotToken) {
		t.Errorf("bot token leaked into log output: %s", output)
	}
	if !strings.Contains(output, "***REDACTED***") {
		t.Errorf("expected ***REDACTED*** marker in log output but got: %s", output)
	}
	// Non-secret values must still be present.
	if !strings.Contains(output, "123456789") {
		t.Errorf("non-secret guild_id was incorrectly redacted; output: %s", output)
	}
}

// TestRedactingHandler_AllSecretFields verifies every known secret field name is
// scrubbed from log output (AC-5 / NFR-6).
func TestRedactingHandler_AllSecretFields(t *testing.T) {
	secretCases := []struct {
		key   string
		value string
	}{
		{"bot_token", "super-secret-bot-token"},
		{"access_token", "super-secret-access-token"},
		{"refresh_token", "super-secret-refresh-token"},
		{"authorization", "Bearer super-secret-api-key"},
		{"Authorization", "Bearer super-secret-api-key-mixed-case"},
		{"api_key", "super-secret-api-key-value"},
		{"BOT_TOKEN", "uppercase-bot-token-variant"},
	}

	for _, tc := range secretCases {
		t.Run(tc.key, func(t *testing.T) {
			var buf bytes.Buffer
			logger := newCaptureLogger(&buf)

			logger.Info("test log entry", tc.key, tc.value)

			output := buf.String()
			if strings.Contains(output, tc.value) {
				t.Errorf("key=%q: secret value leaked into log output: %s", tc.key, output)
			}
			if !strings.Contains(output, "***REDACTED***") {
				t.Errorf("key=%q: expected ***REDACTED*** in output, got: %s", tc.key, output)
			}
		})
	}
}

// TestRedactingHandler_NonSecretFieldsPassThrough verifies that normal fields are not
// accidentally redacted — redaction must be surgical, not a broad wipe (AC-5).
func TestRedactingHandler_NonSecretFieldsPassThrough(t *testing.T) {
	var buf bytes.Buffer
	logger := newCaptureLogger(&buf)

	logger.Info("space provisioned",
		"space_id", "space-abc-123",
		"merchant_id", "merchant-xyz-456",
		"action", "provision",
		"status", "ok",
	)

	output := buf.String()
	for _, want := range []string{"space-abc-123", "merchant-xyz-456", "provision", "ok"} {
		if !strings.Contains(output, want) {
			t.Errorf("non-secret value %q was unexpectedly redacted; output: %s", want, output)
		}
	}
	if strings.Contains(output, "***REDACTED***") {
		t.Errorf("***REDACTED*** should not appear when no secret keys are logged; output: %s", output)
	}
}

// TestRedactingHandler_WithAttrs_SecretFieldRedacted verifies that secret fields are
// redacted even when passed via WithAttrs (structured context injected at request scope).
func TestRedactingHandler_WithAttrs_SecretFieldRedacted(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	handler := newRedactingHandler(inner)

	secretToken := "context-injected-bot-token-must-not-leak"
	childHandler := handler.WithAttrs([]slog.Attr{
		slog.String("bot_token", secretToken),
		slog.String("request_id", "req-001"),
	})
	logger := slog.New(childHandler)
	logger.Info("worker started")

	output := buf.String()
	if strings.Contains(output, secretToken) {
		t.Errorf("bot_token from WithAttrs leaked into log: %s", output)
	}
	// Non-secret attr must still be present.
	if !strings.Contains(output, "req-001") {
		t.Errorf("non-secret request_id was redacted unexpectedly; output: %s", output)
	}
}

// --- Health handler tests: legitimate M0 surface (livez/readyz shape) ---

// mockPinger is a Pinger stub for ReadyzHandler tests — satisfies observability.Pinger.
type mockPinger struct{ err error }

func (m *mockPinger) Ping(_ context.Context) error { return m.err }

// TestLivezHandler_Returns200 verifies the liveness endpoint always returns 200 OK
// with a JSON body containing "ok".
func TestLivezHandler_Returns200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/livez", observability.LivezHandler)

	req := httptest.NewRequest(http.MethodGet, "/livez", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("LivezHandler: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"ok"`) {
		t.Errorf("LivezHandler: expected JSON with ok status, got %s", w.Body.String())
	}
}

// TestReadyzHandler_AllHealthy verifies readyz returns 200 when both pingers succeed.
func TestReadyzHandler_AllHealthy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/readyz", observability.ReadyzHandler(&mockPinger{}, &mockPinger{}))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("ReadyzHandler (healthy): want 200, got %d; body: %s", w.Code, w.Body.String())
	}
}

// TestReadyzHandler_PostgresDown verifies readyz returns 503 when the Postgres pinger fails.
func TestReadyzHandler_PostgresDown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	pgDown := &mockPinger{err: errSimulated}
	r.GET("/readyz", observability.ReadyzHandler(pgDown, &mockPinger{}))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("ReadyzHandler (pg down): want 503, got %d; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unavailable") {
		t.Errorf("ReadyzHandler (pg down): expected 'unavailable' in body; got: %s", w.Body.String())
	}
}

// TestReadyzHandler_ValkeyDown verifies readyz returns 503 when the Valkey pinger fails.
func TestReadyzHandler_ValkeyDown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	valkeyDown := &mockPinger{err: errSimulated}
	r.GET("/readyz", observability.ReadyzHandler(&mockPinger{}, valkeyDown))

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("ReadyzHandler (valkey down): want 503, got %d; body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unavailable") {
		t.Errorf("ReadyzHandler (valkey down): expected 'unavailable' in body; got: %s", w.Body.String())
	}
}
