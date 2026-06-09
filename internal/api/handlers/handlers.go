// Package handlers contains one Gin handler per OpenAPI operationId.
// Handlers are methods on the Handlers struct, which holds all runtime dependencies.
// This enables injection of real implementations in production and fakes in tests.
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// Config carries the runtime dependencies handlers need.
type Config struct {
	Store                    store.Store
	QueueClient              *queue.Client
	Cache                    cache.Cache
	DiscordOAuthClientID     string
	DiscordOAuthClientSecret string
	DiscordOAuthRedirectURL  string

	// M3: HMAC-signed single-use CSRF state tokens for OAuth2 (AC-3).
	StateManager *oauth.StateManager

	// M3: encrypted token persistence for guilds.join (AC-2, AC-3).
	TokenStore *oauth.TokenStore

	// OAuthHTTPClient is used for Discord token-exchange and identity requests.
	// When nil the handlers use the default http.Client (production path).
	// Tests inject a fake transport to avoid real network calls.
	OAuthHTTPClient *http.Client
}

// Handlers groups all API handler methods and their shared dependencies.
type Handlers struct {
	store                    store.Store
	queueClient              *queue.Client
	cache                    cache.Cache
	discordOAuthClientID     string
	discordOAuthClientSecret string
	discordOAuthRedirectURL  string

	// M3 deps.
	stateManager    *oauth.StateManager
	tokenStore      *oauth.TokenStore
	oauthHTTPClient *http.Client // nil = production default; tests inject a fake transport
}

// NewHandlers creates a Handlers instance from the provided config.
func NewHandlers(cfg Config) *Handlers {
	c := cfg.Cache
	if c == nil {
		c = cache.NoopCache{}
	}
	return &Handlers{
		store:                    cfg.Store,
		queueClient:              cfg.QueueClient,
		cache:                    c,
		discordOAuthClientID:     cfg.DiscordOAuthClientID,
		discordOAuthClientSecret: cfg.DiscordOAuthClientSecret,
		discordOAuthRedirectURL:  cfg.DiscordOAuthRedirectURL,
		stateManager:             cfg.StateManager,
		tokenStore:               cfg.TokenStore,
		oauthHTTPClient:          cfg.OAuthHTTPClient,
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
