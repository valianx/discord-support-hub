// transversal.go implements cross-cutting query handlers (M3).
//
// GetDirectory: bidirectional membership directory (user→spaces, space→users, merchant→members).
// OAuthDiscordCallback: Discord OAuth2 callback — validates CSRF state, exchanges code, stores
//
//	encrypted token, and enqueues the invite_collaborator job (AC-3, FR-22).
//
// GetAudit: placeholder for M4 audit log endpoint.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/oauth"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── GetDirectory ─────────────────────────────────────────────────────────────

// GetDirectory handles GET /directory (FR-18, AC-7).
//
// Bidirectional membership directory:
//   - user_id filter  → which spaces does this collaborator belong to?
//   - space_id filter → which collaborators are in this space?
//   - merchant_id filter → which collaborators are in any space of this merchant?
//
// Control-plane gated.
func (h *Handlers) GetDirectory(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	params := store.ListDirectoryParams{Limit: 50}

	if v := c.Query("user_id"); v != "" {
		params.UserID = &v
	}
	if v := c.Query("space_id"); v != "" {
		params.SpaceID = &v
	}
	if v := c.Query("merchant_id"); v != "" {
		params.MerchantID = &v
	}
	if v := c.Query("cursor"); v != "" {
		params.Cursor = &v
	}

	ctx := c.Request.Context()

	entries, err := h.store.ListDirectory(ctx, params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to query directory"})
		return
	}

	items := make([]directoryEntryResponse, 0, len(entries))
	for _, e := range entries {
		items = append(items, directoryEntryResponse{
			SpaceID:         e.SpaceID,
			SpaceName:       e.SpaceName,
			MerchantID:      e.MerchantID,
			MerchantName:    e.MerchantName,
			UserID:          e.UserID,
			UserDisplayName: e.UserDisplayName,
			Role:            string(e.Role),
		})
	}

	var nextCursor *string
	if len(entries) == params.Limit && len(entries) > 0 {
		// Cursor is an opaque token; clients pass it as-is on the next page.
		last := entries[len(entries)-1].SpaceID + ":" + entries[len(entries)-1].UserID
		nextCursor = &last
	}

	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": nextCursor})
}

// directoryEntryResponse is the JSON shape for one Directory entry.
type directoryEntryResponse struct {
	SpaceID         string  `json:"space_id"`
	SpaceName       string  `json:"space_name"`
	MerchantID      string  `json:"merchant_id"`
	MerchantName    string  `json:"merchant_name"`
	UserID          string  `json:"user_id"`
	UserDisplayName *string `json:"user_display_name,omitempty"`
	Role            string  `json:"role"`
}

// ─── GetAudit ─────────────────────────────────────────────────────────────────

// GetAudit handles GET /audit (FR-14, M4).
// TODO(M4): query audit_log with filters; return newest-first with cursor pagination.
func (h *Handlers) GetAudit(c *gin.Context) {
	notImplemented(c)
}

// ─── OAuthDiscordCallback ─────────────────────────────────────────────────────

// OAuthDiscordCallback handles GET /oauth/discord/callback (FR-22, AC-3).
//
// This endpoint is NOT protected by the service API key (security: [] in OpenAPI).
// It is reached via a browser redirect from Discord after the user authorises.
//
// Flow:
//  1. Validate CSRF state token (HMAC-signed, single-use — AC-3).
//  2. Exchange the authorization code for Discord access + refresh tokens.
//  3. Call /users/@me to obtain the Discord user id.
//  4. Link the Discord user id to the hub user (SetUserDiscordID if not already set).
//  5. Store tokens encrypted (AES-256-GCM, NFR-6).
//  6. Enqueue any pending invite_collaborator jobs for that user.
//  7. Redirect the browser to a success page.
//
// This is a gin.HandlerFunc method so it can access h.stateManager and h.tokenStore.
func (h *Handlers) OAuthDiscordCallback(c *gin.Context) {
	// The CSRF state gate runs unconditionally — even without a tokenStore wired,
	// we must validate the state before touching any other resource.
	if h.stateManager == nil {
		notImplemented(c)
		return
	}

	ctx := c.Request.Context()

	// Step 1: validate CSRF state (single-use HMAC — AC-3).
	state := c.Query("state")
	userID, err := h.stateManager.Validate(ctx, state)
	if err != nil {
		slog.WarnContext(ctx, "oauth: callback state validation failed", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "invalid_state",
			"message": "OAuth2 state token is missing, invalid, or already used",
		})
		return
	}

	// Require tokenStore before any code exchange (only reachable after state passes).
	if h.tokenStore == nil {
		notImplemented(c)
		return
	}

	// Step 2: exchange code for tokens.
	code := c.Query("code")
	if code == "" {
		// Discord sends error + error_description when the user denies access.
		discordErr := c.Query("error")
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "oauth_error",
			"message": fmt.Sprintf("OAuth2 callback received no code (discord error: %s)", discordErr),
		})
		return
	}

	tokenResp, err := oauth.ExchangeCode(ctx, oauth.ExchangeConfig{
		ClientID:     h.discordOAuthClientID,
		ClientSecret: h.discordOAuthClientSecret,
		RedirectURL:  h.discordOAuthRedirectURL,
	}, code, h.oauthHTTPClient)
	if err != nil {
		slog.ErrorContext(ctx, "oauth: code exchange failed", "user_id", userID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{
			"code":    "token_exchange_failed",
			"message": "failed to exchange authorization code for tokens",
		})
		return
	}

	// Step 3: fetch Discord user id.
	discordUser, err := oauth.FetchDiscordUser(ctx, tokenResp.AccessToken, h.oauthHTTPClient)
	if err != nil {
		slog.ErrorContext(ctx, "oauth: fetch discord user failed", "user_id", userID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{
			"code":    "discord_user_fetch_failed",
			"message": "failed to retrieve Discord user identity",
		})
		return
	}

	// Step 4: link the verified Discord identity to the hub user named in the signed state.
	//
	// SEC-M3-001 binding guarantee: the token + discord_user_id are bound to:
	//   - state.userID  — the hub user who initiated the connect (HMAC-signed, single-use)
	//   - discordUser.ID — the Discord identity that ACTUALLY authorised (from /users/@me)
	// No client-controllable field chooses the link. The UNIQUE constraint on
	// users.discord_user_id (enforced below via ErrConflict) ensures one Discord
	// identity cannot be bound to more than one hub user.
	if err := h.linkDiscordUserID(ctx, userID, discordUser.ID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Another hub user already holds this Discord id — reject to prevent
			// account-linking confusion (CWE-290 / SEC-M3-001).
			slog.WarnContext(ctx, "oauth: discord_user_id already linked to another account",
				"user_id", userID, "discord_user_id", discordUser.ID)
			c.JSON(http.StatusConflict, gin.H{
				"code":    "discord_id_conflict",
				"message": "This Discord account is already linked to another hub user",
			})
			return
		}
		// Log other failures but continue — the token is stored regardless.
		// The reconciler or a future retry will re-attempt the link.
		slog.WarnContext(ctx, "oauth: link discord user id failed",
			"user_id", userID, "discord_user_id", discordUser.ID, "error", err)
	}

	// Step 5: store tokens encrypted (never plaintext in DB — NFR-6).
	var expiresAt *time.Time
	if tokenResp.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
		expiresAt = &t
	}
	if err := h.tokenStore.Store(ctx,
		userID,
		tokenResp.AccessToken,
		tokenResp.RefreshToken,
		tokenResp.Scope,
		expiresAt,
	); err != nil {
		slog.ErrorContext(ctx, "oauth: store token failed", "user_id", userID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    "token_store_failed",
			"message": "failed to persist tokens",
		})
		return
	}

	// Step 6: enqueue any pending invite_collaborator job for this user.
	// The worker already checks for existing space_member rows — this is a best-effort
	// trigger so the overwrite is applied promptly after account connection.
	if h.queueClient != nil {
		h.enqueuePendingInvites(ctx, userID)
	}

	// Step 7: redirect to success page (or JSON success for headless callers).
	acceptHeader := c.GetHeader("Accept")
	if strings.Contains(acceptHeader, "application/json") {
		c.JSON(http.StatusOK, gin.H{
			"code":            "connected",
			"message":         "Discord account connected successfully",
			"discord_user_id": discordUser.ID,
		})
		return
	}

	// Browser redirect to success page. Callers configure the redirect via env.
	c.Redirect(http.StatusFound, "/oauth/success")
}

// linkDiscordUserID sets the discord_user_id on the hub user.
// Returns store.ErrConflict when another hub user already holds that Discord id.
// Returns store.ErrNotFound when the hub user does not exist.
// Returns nil when the link is already set to the same value (idempotent via no-op update).
func (h *Handlers) linkDiscordUserID(ctx context.Context, userID, discordUserID string) error {
	if h.store == nil {
		return nil
	}
	return h.store.UpdateDiscordUserID(ctx, userID, discordUserID)
}

// enqueuePendingInvites enqueues KindInviteCollaborator tasks for every space_member
// row belonging to the user that has not yet had its Discord overwrite applied
// (overwrite_applied = false). Called immediately after a successful OAuth2 link so
// the per-user channel overwrite is projected without waiting for the asynq retry window.
func (h *Handlers) enqueuePendingInvites(ctx context.Context, userID string) {
	if h.store == nil || h.queueClient == nil {
		return
	}

	members, err := h.store.ListCollaboratorChannels(ctx, userID)
	if err != nil {
		slog.WarnContext(ctx, "oauth: list collaborator channels failed; pending invites not enqueued",
			"user_id", userID, "error", err)
		return
	}

	for _, sm := range members {
		if sm.OverwriteApplied {
			continue // already projected — skip
		}
		payload := queue.InviteCollaboratorPayload{
			SpaceID: sm.SpaceID,
			UserID:  userID,
		}
		if _, enqErr := h.queueClient.Enqueue(queue.KindInviteCollaborator, queue.QueueMembership, payload); enqErr != nil {
			// Non-fatal: the asynq retry window will eventually re-process the invite.
			slog.WarnContext(ctx, "oauth: enqueue pending invite failed",
				"user_id", userID, "space_id", sm.SpaceID, "error", enqErr)
		}
	}
}

// ─── standalone OAuthDiscordCallback shim (router compatibility) ──────────────

// OAuthDiscordCallbackStandalone is the function-level variant of OAuthDiscordCallback
// that was originally registered as a plain function before M3 wired the handler struct.
// The router now calls h.OAuthDiscordCallback after M3; this shim remains for callers
// that still reference it by the old signature.
func OAuthDiscordCallback(c *gin.Context) {
	// Before M3 wiring this returned 501. After M3, the router resolves to
	// h.OAuthDiscordCallback(c) via the Handlers method. This standalone variant
	// is kept for backward compatibility with any test that imports it directly.
	notImplemented(c)
}

// ─── ListSpaceMembers ─────────────────────────────────────────────────────────
// (moved from spaces.go to be co-located with directory query logic)

// listSpaceMembersForDirectory is the response type for listing space members.
type spaceMemberResponse struct {
	ID               string  `json:"id"`
	UserID           string  `json:"user_id"`
	Role             string  `json:"role"`
	OverwriteApplied bool    `json:"overwrite_applied"`
	InvitedBy        *string `json:"invited_by,omitempty"`
	CreatedAt        string  `json:"created_at"`
	RevokedAt        *string `json:"revoked_at,omitempty"`
}

// ListSpaceMembersHandler is the real implementation extracted for use by spaces.go.
// It lists all active space_member rows for a space (FR-17, AC-7).
// Control-plane gated.
func (h *Handlers) listSpaceMembers(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	spaceID := c.Param("id")
	if spaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "id is required"})
		return
	}

	ctx := c.Request.Context()

	if _, err := h.store.GetSpaceByID(ctx, spaceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "space not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load space"})
		return
	}

	members, err := h.store.ListSpaceMembers(ctx, spaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to list space members"})
		return
	}

	items := make([]spaceMemberResponse, 0, len(members))
	for _, sm := range members {
		r := spaceMemberResponse{
			ID:               sm.ID,
			UserID:           sm.UserID,
			Role:             string(sm.Role),
			OverwriteApplied: sm.OverwriteApplied,
			InvitedBy:        sm.InvitedBy,
			CreatedAt:        sm.CreatedAt.UTC().Format(time.RFC3339),
		}
		if sm.RevokedAt != nil {
			s := sm.RevokedAt.UTC().Format(time.RFC3339)
			r.RevokedAt = &s
		}
		items = append(items, r)
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}
