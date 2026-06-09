// Package api wires the Gin engine, CORS, middleware, and all route groups.
package api

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/oauth"
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

	// Cache is the Valkey read cache. Handlers use it for space reads.
	Cache cache.Cache

	// Discord OAuth2 config needed by handlers.
	DiscordOAuthClientID     string
	DiscordOAuthClientSecret string // NFR-6: never hardcoded, from env only
	DiscordOAuthRedirectURL  string

	// M3: HMAC-signed single-use CSRF state manager for OAuth2 (AC-3).
	StateManager *oauth.StateManager

	// M3: encrypted OAuth2 token store for guilds.join (AC-2, AC-3).
	TokenStore *oauth.TokenStore

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

	// Build the handler struct with all M3 deps wired in.
	h := handlers.NewHandlers(handlers.Config{
		Store:                    cfg.Store,
		QueueClient:              cfg.QueueClient,
		Cache:                    cfg.Cache,
		DiscordOAuthClientID:     cfg.DiscordOAuthClientID,
		DiscordOAuthClientSecret: cfg.DiscordOAuthClientSecret,
		DiscordOAuthRedirectURL:  cfg.DiscordOAuthRedirectURL,
		StateManager:             cfg.StateManager,
		TokenStore:               cfg.TokenStore,
	})

	// OAuth2 callback — exempt from service API key auth (security: [] in OpenAPI).
	// Registered on h (not as a standalone function) so it has access to stateManager and tokenStore.
	r.GET("/v1/oauth/discord/callback", h.OAuthDiscordCallback)

	// All other v1 routes require Layer A authentication.
	// When Store is nil (test mode without real auth), fall back to the no-op stub.
	var authMiddleware gin.HandlerFunc
	if cfg.Store != nil {
		authMiddleware = middleware.Auth(cfg.Store)
	} else {
		authMiddleware = noopAuth()
	}

	v1 := r.Group("/v1", authMiddleware)

	registerV1Routes(v1, h, cfg.Store)

	return r
}

// registerV1Routes attaches all v1 path groups.
// The store is passed to Idempotency() so the middleware can check/replay stored
// responses. When store is nil (test mode) Idempotency is a no-op.
//
// welcome:sync path note (M4): Gin's httprouter uses the colon character as a parameter
// prefix inside a path segment. The OpenAPI contract path is
// POST /v1/channels/{id}/welcome:sync — the literal string "welcome:sync" is a
// static segment (no param), but httprouter would interpret ":sync" as a param named
// "sync" if we wrote "welcome:sync". The fix: register at the engine level (not the
// group) after the group's auth middleware chain by composing the auth + idem handlers
// explicitly. Gin allows `r.Handle("POST", "/v1/channels/:id/welcome:sync", ...)` when
// the colon appears as a suffix of a named static+param hybrid — httprouter accepts this
// because the colon is not the first character of the segment; "welcome:sync" is
// treated as a static segment literal.
func registerV1Routes(v1 *gin.RouterGroup, h *handlers.Handlers, s store.Store) {
	idem := middleware.Idempotency(s)

	// Spaces — provision + reads + lifecycle + welcome sync.
	v1.POST("/merchants/:merchantId/channels", idem, h.ProvisionSpace)
	v1.GET("/channels", h.ListSpaces)
	v1.GET("/channels/:id", h.GetSpace)
	v1.GET("/channels/:id/members", h.ListSpaceMembers)
	v1.POST("/channels/:id/lifecycle", idem, h.ChangeSpaceLifecycle)

	// welcome:sync: The literal colon is NOT the first character of the segment so
	// Gin/httprouter registers it as a static path — no parameter collision.
	// This matches the OpenAPI contract path POST /v1/channels/{id}/welcome:sync exactly.
	v1.POST("/channels/:id/welcome:sync", idem, h.SyncWelcome)

	// Collaborators.
	v1.POST("/channels/:id/collaborators", idem, h.InviteCollaborator)
	v1.DELETE("/channels/:id/collaborators/:userId", idem, h.ExpelCollaborator)
	v1.GET("/collaborators/:userId/channels", h.ListCollaboratorChannels)

	// Agents (Admin only — Layer B enforced inside handlers).
	v1.GET("/agents", h.ListAgents)
	v1.POST("/agents", idem, h.AddAgent)
	v1.DELETE("/agents/:userId", idem, h.RemoveAgent)

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
