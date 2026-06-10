// Package api wires the Gin engine, CORS, middleware, and all route groups.
// M6: OAuth2 callback route removed (AC-M6-9). New routes:
//   - PUT  /v1/merchants/:merchantId/invite (AC-M6-3)
//   - POST /v1/channels/:id/collaborators (synchronous 201, AC-M6-4)
//   - POST /v1/channels/:id/collaborators/:userId:send-invite (AC-M6-5)
package api

import (
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/cache"
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

	// Metrics is the Prometheus metrics instance. When non-nil, /metrics is exposed.
	Metrics *observability.Metrics

	// Store is the Postgres-backed store, used by Layer A (auth) and handlers.
	Store store.Store

	// QueueClient is the asynq client used by handlers that enqueue jobs.
	QueueClient *queue.Client

	// Cache is the Valkey read cache. Handlers use it for space reads.
	Cache cache.Cache

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

	// Prometheus metrics endpoint — exempt from authentication (scraper access).
	// Omitted when Metrics is nil (e.g. tests that do not need metrics).
	if cfg.Metrics != nil {
		r.GET("/metrics", gin.WrapH(cfg.Metrics.Handler()))
	}

	// Build the handler struct with all M6 deps wired in.
	h := handlers.NewHandlers(handlers.Config{
		Store:       cfg.Store,
		QueueClient: cfg.QueueClient,
		Cache:       cfg.Cache,
	})

	// All v1 routes require Layer A authentication.
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
// Colon-in-path notes (Gin/httprouter):
//   - "welcome:sync" and "send-invite" contain colons; Gin treats a colon as a parameter
//     prefix ONLY when it is the first character of a path segment. In these paths the colon
//     is a suffix of a static+param hybrid, so httprouter registers the literal string.
//     E.g. ":userId:send-invite" — httprouter reads ":userId" as the param and ":send-invite"
//     does NOT start with a bare colon (it follows a segment boundary), so the registration works.
//     In practice, Gin registers "/channels/:id/collaborators/:userId:send-invite" cleanly.
func registerV1Routes(v1 *gin.RouterGroup, h *handlers.Handlers, s store.Store) {
	idem := middleware.Idempotency(s)

	// Merchants — register, list, detail, invite link.
	v1.POST("/merchants", idem, h.RegisterMerchant)
	v1.GET("/merchants", h.ListMerchants)
	v1.GET("/merchants/:merchantId", h.GetMerchant)
	v1.PUT("/merchants/:merchantId/invite", h.SetMerchantInviteLink) // AC-M6-3

	// Spaces — provision + reads + lifecycle + welcome sync.
	v1.POST("/merchants/:merchantId/channels", idem, h.ProvisionSpace)
	v1.GET("/channels", h.ListSpaces)
	v1.GET("/channels/:id", h.GetSpace)
	v1.GET("/channels/:id/members", h.ListSpaceMembers)
	v1.POST("/channels/:id/lifecycle", idem, h.ChangeSpaceLifecycle)

	// welcome:sync: The literal colon is NOT the first character of the segment so
	// Gin/httprouter registers it as a static path — no parameter collision.
	v1.POST("/channels/:id/welcome:sync", idem, h.SyncWelcome)

	// Collaborators — register (sync 201), send invite (async 202), expel, list.
	// AC-M6-4: POST /channels/{id}/collaborators → synchronous 201
	// AC-M6-5: POST /channels/{id}/collaborators/{userId}/send-invite → async 202
	// Note: the action is a separate path segment (/send-invite) because Gin/httprouter
	// does not allow a literal colon after a wildcard in the same segment (:userId:send-invite
	// would be parsed as two wildcards and panic at startup).
	v1.POST("/channels/:id/collaborators", idem, h.RegisterCollaborator)
	v1.POST("/channels/:id/collaborators/:userId/send-invite", idem, h.SendCollaboratorInvite)
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
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Request-ID"},
		ExposeHeaders:    []string{"Location", "X-Request-ID"},
		AllowCredentials: false,
	}
	return cors.New(cfg)
}

// noopAuth is the M0-style pass-through used in tests that don't exercise auth.
func noopAuth() gin.HandlerFunc {
	return func(c *gin.Context) { c.Next() }
}
