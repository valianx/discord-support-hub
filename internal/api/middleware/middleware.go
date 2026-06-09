// Package middleware provides Gin middleware for the API layer.
//
// RequestID and Recovery are real implementations used from M0.
// Auth (Layer A) is the real API-key authentication middleware (implemented M1).
// Idempotency is a pass-through stub until M2.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

const (
	requestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
	principalKey    = "principal"
)

// authStore is the minimal store surface Layer A needs, making it easy to mock in tests.
type authStore interface {
	LookupActiveAPIKeyByHash(ctx context.Context, hash []byte) (*domain.APIKey, error)
	TouchAPIKeyLastUsed(ctx context.Context, id string) error
}

// RequestID generates a unique request id for each incoming request and stores it in
// the Gin context and the response header. Downstream handlers use it for log correlation.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		c.Set(requestIDKey, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

// Recovery wraps gin.Recovery to log panics as structured slog errors before returning 500.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				reqID, _ := c.Get(requestIDKey)
				slog.Error("api: panic recovered",
					"request_id", reqID,
					"panic", r,
					"path", c.Request.URL.Path,
					"method", c.Request.Method,
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    "internal_error",
					"message": "an unexpected error occurred",
				})
			}
		}()
		c.Next()
	}
}

// Auth returns the Layer A authentication middleware.
//
// It extracts "Authorization: Bearer <key>", hashes the raw key with SHA-256,
// looks up the active api_keys row in Postgres, and injects a Principal into the
// Gin context. Requests with a missing, invalid, or revoked key are rejected 401
// before any handler executes (docs/02-architecture.md §5.1).
func Auth(s store.Store) gin.HandlerFunc {
	return authMiddleware(s)
}

// authMiddleware accepts the narrow interface for easier unit testing.
func authMiddleware(s authStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		rawKey, ok := extractBearer(c.GetHeader("Authorization"))
		if !ok {
			abortUnauthorized(c, "missing or malformed Authorization header")
			return
		}

		hash := authz.HashAPIKey(rawKey)
		ctx := c.Request.Context()

		apiKey, err := s.LookupActiveAPIKeyByHash(ctx, hash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				abortUnauthorized(c, "invalid or revoked api key")
				return
			}
			// Unexpected store error — fail closed (deny access, log, 500).
			reqID, _ := c.Get(requestIDKey)
			slog.ErrorContext(ctx, "auth: store error during key lookup",
				"request_id", reqID, "error", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "internal_error",
				"message": "authentication service unavailable",
			})
			return
		}

		// Update last_used_at asynchronously; failure is non-fatal.
		go func() {
			if err := s.TouchAPIKeyLastUsed(context.Background(), apiKey.ID); err != nil {
				slog.Warn("auth: could not update last_used_at", "key_id", apiKey.ID, "error", err)
			}
		}()

		p := &authz.Principal{
			Type:     authz.PrincipalTypeService,
			KeyID:    apiKey.ID,
			KeyScope: apiKey.Scope,
			// IsAdmin defaults false for service keys not bound to an admin user.
			// Handler-level enrichment (e.g. user lookup) can set it if needed.
		}
		c.Set(principalKey, p)
		c.Next()
	}
}

// GetPrincipal retrieves the authenticated Principal from the Gin context.
// Returns nil when no principal has been injected (exempt routes, or unauthenticated).
func GetPrincipal(c *gin.Context) *authz.Principal {
	v, exists := c.Get(principalKey)
	if !exists {
		return nil
	}
	p, _ := v.(*authz.Principal)
	return p
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// extractBearer parses "Bearer <token>" from an Authorization header value.
func extractBearer(header string) (string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", false
	}
	return token, true
}

func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"code":    "unauthorized",
		"message": msg,
	})
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
