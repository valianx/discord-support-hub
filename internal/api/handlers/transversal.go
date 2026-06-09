package handlers

import "github.com/gin-gonic/gin"

// GetDirectory handles GET /directory (FR-18, M3).
// Bidirectional search: filter by user_id or space_id or merchant_id.
// TODO(M3): query space_members joined to spaces and users with cursor pagination.
func (h *Handlers) GetDirectory(c *gin.Context) {
	notImplemented(c)
}

// GetAudit handles GET /audit (FR-14, M4).
// TODO(M4): query audit_log with filters; return newest-first with cursor pagination.
func (h *Handlers) GetAudit(c *gin.Context) {
	notImplemented(c)
}

// OAuthDiscordCallback handles GET /oauth/discord/callback (FR-22, M3).
// Not protected by the service API key (security: [] in OpenAPI); CSRF-protected via
// a signed state parameter. The endpoint is reached via browser redirect.
// TODO(M3): validate state (CSRF), exchange code for token, store encrypted, enqueue add-to-guild.
func OAuthDiscordCallback(c *gin.Context) {
	notImplemented(c)
}
