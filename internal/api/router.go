// Package api wires the Gin engine, CORS, middleware, and all route groups.
package api

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// RouterConfig carries the runtime dependencies the router needs.
type RouterConfig struct {
	// CORSAllowedOrigins is the allowlist of permitted request origins.
	// An empty slice locks the API down (no cross-origin requests allowed).
	// Never use "*" with credentials (docs/02-architecture.md §5.3).
	CORSAllowedOrigins []string

	// Store is the Postgres-backed store, used by Layer A (auth) and handlers.
	Store store.Store

	// QueueClient is the asynq client used by handlers that enqueue jobs.
	QueueClient *queue.Client

	// Discord config needed by handlers.
	DiscordOAuthClientID    string
	DiscordOAuthRedirectURL string

	// Health check pingable dependencies.
	PGPinger    observability.Pinger
	RedisPinger observability.Pinger
}

// NewRouter builds and returns the configured Gin engine.
// The engine is suitable for passing to http.Server.
func NewRouter(cfg RouterConfig) *gin.Engine {
	r := gin.New()

	// Middleware: recovery first so panics in other middleware are caught.
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())
	r.Use(corsMiddleware(cfg.CORSAllowedOrigins))

	// Health endpoints — exempt from authentication.
	r.GET("/livez", observability.LivezHandler)
	r.GET("/readyz", observability.ReadyzHandler(cfg.PGPinger, cfg.RedisPinger))

	// OAuth2 callback — exempt from service API key auth (security: [] in OpenAPI).
	r.GET("/v1/oauth/discord/callback", handlers.OAuthDiscordCallback)

	// All other v1 routes require Layer A authentication.
	// When Store is nil (test mode without real auth), fall back to the no-op stub.
	var authMiddleware gin.HandlerFunc
	if cfg.Store != nil {
		authMiddleware = middleware.Auth(cfg.Store)
	} else {
		authMiddleware = noopAuth()
	}

	v1 := r.Group("/v1", authMiddleware)

	h := handlers.NewHandlers(handlers.Config{
		Store:                   cfg.Store,
		QueueClient:             cfg.QueueClient,
		DiscordOAuthClientID:    cfg.DiscordOAuthClientID,
		DiscordOAuthRedirectURL: cfg.DiscordOAuthRedirectURL,
	})

	registerV1Routes(v1, h)

	return r
}

// registerV1Routes attaches all v1 path groups.
func registerV1Routes(v1 *gin.RouterGroup, h *handlers.Handlers) {
	// Spaces — provision + reads + lifecycle + welcome sync.
	v1.POST("/merchants/:merchantId/channels", middleware.Idempotency(), h.ProvisionSpace)
	v1.GET("/channels", h.ListSpaces)
	v1.GET("/channels/:id", h.GetSpace)
	v1.GET("/channels/:id/members", h.ListSpaceMembers)
	v1.POST("/channels/:id/lifecycle", middleware.Idempotency(), h.ChangeSpaceLifecycle)
	// Note: "welcome:sync" contains a literal colon which Gin would misparse as a param.
	// We register it as a static segment. The OpenAPI path is POST /v1/channels/{id}/welcome:sync.
	// TODO(M4): reconsider path if Gin adds literal-colon support.
	v1.POST("/channels/:id/welcomesync", middleware.Idempotency(), h.SyncWelcome)

	// Collaborators.
	v1.POST("/channels/:id/collaborators", middleware.Idempotency(), h.InviteCollaborator)
	v1.DELETE("/channels/:id/collaborators/:userId", middleware.Idempotency(), h.ExpelCollaborator)
	v1.GET("/collaborators/:userId/channels", h.ListCollaboratorChannels)

	// Agents (Admin only — Layer B enforced inside handlers).
	v1.GET("/agents", h.ListAgents)
	v1.POST("/agents", middleware.Idempotency(), h.AddAgent)
	v1.DELETE("/agents/:userId", middleware.Idempotency(), h.RemoveAgent)

	// Transversal.
	v1.GET("/directory", h.GetDirectory)
	v1.GET("/audit", h.GetAudit)

	// Jobs.
	v1.GET("/jobs/:jobId", h.GetJob)
}

// corsMiddleware returns a gin-contrib/cors middleware with locked-down defaults.
// allowedOrigins comes from config (FR-13); credentials are never allowed with "*".
func corsMiddleware(allowedOrigins []string) gin.HandlerFunc {
	if len(allowedOrigins) == 0 {
		// No allowlist configured: omit CORS headers so the browser enforces
		// same-origin policy. Operators add origins via CORS_ALLOWED_ORIGINS.
		return func(c *gin.Context) { c.Next() }
	}

	cfg := cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Request-ID"},
		ExposeHeaders:    []string{"Location", "X-Request-ID"},
		AllowCredentials: false, // only set true on the session path in POC-FE (§5.3)
	}
	return cors.New(cfg)
}

// noopAuth is the M0-style pass-through used in tests that don't exercise auth.
func noopAuth() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}
