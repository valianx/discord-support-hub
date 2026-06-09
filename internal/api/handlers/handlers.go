// Package handlers contains one Gin handler per OpenAPI operationId.
// Handlers are methods on the Handlers struct, which holds all runtime dependencies.
// This enables injection of real implementations in production and fakes in tests.
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// Config carries the runtime dependencies handlers need.
type Config struct {
	Store                   store.Store
	QueueClient             *queue.Client
	Cache                   cache.Cache
	DiscordOAuthClientID    string
	DiscordOAuthRedirectURL string
}

// Handlers groups all API handler methods and their shared dependencies.
type Handlers struct {
	store                   store.Store
	queueClient             *queue.Client
	cache                   cache.Cache
	discordOAuthClientID    string
	discordOAuthRedirectURL string
}

// NewHandlers creates a Handlers instance from the provided config.
func NewHandlers(cfg Config) *Handlers {
	c := cfg.Cache
	if c == nil {
		c = cache.NoopCache{}
	}
	return &Handlers{
		store:                   cfg.Store,
		queueClient:             cfg.QueueClient,
		cache:                   c,
		discordOAuthClientID:    cfg.DiscordOAuthClientID,
		discordOAuthRedirectURL: cfg.DiscordOAuthRedirectURL,
	}
}

// notImplemented returns the contract Error shape with HTTP 501.
func notImplemented(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"code":    "not_implemented",
		"message": "this endpoint is not yet implemented",
	})
}

// forbidden returns the contract Error shape with HTTP 403.
func forbidden(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{
		"code":    "forbidden",
		"message": "you are not authorized to perform this action",
	})
}
