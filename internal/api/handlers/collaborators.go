// collaborators.go implements the collaborator membership handlers (M3).
//
// All three endpoints are control-plane gated (RequireControlPlane).
// Collaborators themselves can never reach invite/expel (FR-20, AC-4).
//
// Invite: POST /channels/{id}/collaborators
// Expel:  DELETE /channels/{id}/collaborators/{userId}?scope=channel|server
// List:   GET /collaborators/{userId}/channels
package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── InviteCollaborator ───────────────────────────────────────────────────────

// inviteCollaboratorRequest is the JSON body for POST /channels/{id}/collaborators.
// At least one of user_id, discord_user_id, or email must be non-empty (OpenAPI anyOf).
type inviteCollaboratorRequest struct {
	UserID        string  `json:"user_id"`
	DiscordUserID string  `json:"discord_user_id"`
	Email         string  `json:"email"`
	DisplayName   *string `json:"display_name"`
}

// InviteCollaborator handles POST /channels/{id}/collaborators (FR-4, AC-2, AC-4).
//
// Control-plane gated. Only Agents/Admins may invite (FR-20) — collaborators cannot (AC-4).
// Accepts user_id, discord_user_id, or email to identify the collaborator; creates the user
// row when identified by discord_user_id or email and no matching user exists.
// Records the desired space_member row and enqueues an invite_collaborator job.
// Returns 202 + connect_url if the user must connect Discord first (AC-2).
func (h *Handlers) InviteCollaborator(c *gin.Context) {
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
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "space id is required"})
		return
	}

	var req inviteCollaboratorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": err.Error()})
		return
	}

	// OpenAPI anyOf: at least one identifier must be provided.
	if req.UserID == "" && req.DiscordUserID == "" && req.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "one of user_id, discord_user_id, or email is required",
		})
		return
	}

	// Input hygiene — validated before any store call.
	if msg := validateInviteInputs(&req); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": msg})
		return
	}

	ctx := c.Request.Context()

	// Verify space exists.
	sp, err := h.store.GetSpaceByID(ctx, spaceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "space not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load space"})
		return
	}
	_ = sp // existence verification only

	// Resolve the collaborator user via the three-path lookup / create flow.
	user, httpStatus, resolveMsg := resolveOrCreateCollaborator(ctx, h.store, &req)
	if resolveMsg != "" {
		c.JSON(httpStatus, gin.H{"code": "not_found", "message": resolveMsg})
		return
	}
	if user == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to resolve collaborator"})
		return
	}

	invitedBy := p.UserID

	// Insert the desired space_member row (idempotent on conflict).
	_, smErr := h.store.CreateSpaceMember(ctx, store.CreateSpaceMemberParams{
		SpaceID:   spaceID,
		UserID:    user.ID,
		Role:      domain.SpaceMemberRoleCollaborator,
		InvitedBy: nilIfEmpty(invitedBy),
	})
	if smErr != nil && !errors.Is(smErr, store.ErrConflict) {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to record membership"})
		return
	}

	// Build connect_url when the user has not yet linked their Discord account (AC-2).
	var connectURL *string
	if user.DiscordUserID == nil && h.stateManager != nil {
		state, stateErr := h.stateManager.Issue(ctx, user.ID, h.discordOAuthRedirectURL)
		if stateErr == nil {
			cu := buildConnectURL(h.discordOAuthClientID, h.discordOAuthRedirectURL, state)
			connectURL = &cu
		}
	}

	idemKey := middleware.GetIdempotencyKey(c)
	if idemKey == "" {
		idemKey = fmt.Sprintf("invite:%s:%s", spaceID, user.ID)
	}

	jobID := uuid.New().String()
	job, jobErr := h.store.CreateJob(ctx, store.CreateJobParams{
		TaskID:  idemKey,
		Kind:    queue.KindInviteCollaborator,
		Queue:   queue.QueueMembership,
		SpaceID: &spaceID,
		UserID:  &user.ID,
		Payload: map[string]any{
			"space_id":   spaceID,
			"user_id":    user.ID,
			"invited_by": invitedBy,
		},
	})
	if jobErr == nil {
		jobID = job.ID
	}

	if h.queueClient != nil {
		_, _ = h.queueClient.Enqueue(
			queue.KindInviteCollaborator,
			queue.QueueMembership,
			queue.InviteCollaboratorPayload{
				SpaceID:   spaceID,
				UserID:    user.ID,
				InvitedBy: invitedBy,
			},
			queue.TaskIDOpt(idemKey),
			queue.UniqueOpt(24*time.Hour),
		)
	}

	respBody := map[string]any{
		"job": map[string]any{
			"id":          jobID,
			"kind":        queue.KindInviteCollaborator,
			"status":      string(domain.JobStatusPending),
			"space_id":    spaceID,
			"user_id":     user.ID,
			"retry_count": 0,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		},
		"connect_url": connectURL,
	}

	c.Header("Location", fmt.Sprintf("/v1/jobs/%s", jobID))
	c.JSON(http.StatusAccepted, respBody)
}

// validateInviteInputs runs input hygiene checks on a parsed invite request.
// Returns a non-empty message when any field fails validation; empty string on success.
func validateInviteInputs(req *inviteCollaboratorRequest) string {
	if req.Email != "" {
		if msg := validateEmailFormat(req.Email); msg != "" {
			return msg
		}
	}
	if req.DiscordUserID != "" {
		if !isNumericSnowflake(req.DiscordUserID) {
			return "discord_user_id must be a numeric snowflake"
		}
	}
	if req.DisplayName != nil {
		if msg := rejectUnsafeRunes(*req.DisplayName); msg != "" {
			return "display_name " + msg
		}
	}
	return ""
}

// validateEmailFormat performs a basic structural check on an email address.
// It does not do DNS resolution — it enforces the presence of exactly one "@"
// with non-empty local and domain parts, and a dot in the domain part.
func validateEmailFormat(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "email is not a valid email address"
	}
	if !strings.Contains(parts[1], ".") {
		return "email is not a valid email address"
	}
	// Reject control characters in the email for the same reason as display_name.
	for _, ch := range email {
		if ch < 0x20 || ch == 0x7F || unicode.Is(unicode.Cf, ch) || unicode.Is(unicode.Co, ch) {
			return "email contains disallowed characters"
		}
	}
	return ""
}

// isNumericSnowflake returns true when s is a non-empty string of ASCII digits only.
// Discord snowflake IDs are 64-bit integers serialised as decimal strings.
func isNumericSnowflake(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// resolveOrCreateCollaborator implements the three-path user resolution for InviteCollaborator.
//
// Priority:
//  1. user_id present  — GetUserByID; 404 if absent.
//  2. discord_user_id  — GetUserByDiscordID; create type=collaborator user if not found.
//  3. email            — GetUserByEmail; create type=collaborator user if not found.
//
// UNIQUE conflict on create (race condition) is resolved by re-fetching the conflicting row.
// Returns (user, 0, "") on success; (nil, httpStatus, message) on failure.
func resolveOrCreateCollaborator(
	ctx context.Context,
	s store.Store,
	req *inviteCollaboratorRequest,
) (*domain.User, int, string) {
	switch {
	case req.UserID != "":
		return resolveByUserID(ctx, s, req.UserID)
	case req.DiscordUserID != "":
		return resolveOrCreateByDiscordID(ctx, s, req)
	default:
		return resolveOrCreateByEmail(ctx, s, req)
	}
}

func resolveByUserID(ctx context.Context, s store.Store, userID string) (*domain.User, int, string) {
	user, err := s.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, http.StatusNotFound, "user not found"
		}
		return nil, http.StatusInternalServerError, "failed to load user"
	}
	return user, 0, ""
}

func resolveOrCreateByDiscordID(
	ctx context.Context,
	s store.Store,
	req *inviteCollaboratorRequest,
) (*domain.User, int, string) {
	user, err := s.GetUserByDiscordID(ctx, req.DiscordUserID)
	if err == nil {
		return user, 0, ""
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, http.StatusInternalServerError, "failed to look up user by discord_user_id"
	}

	// User does not exist — create a collaborator row.
	discordID := req.DiscordUserID
	params := store.CreateUserParams{
		Type:          domain.UserTypeCollaborator,
		DiscordUserID: &discordID,
		Email:         nilIfEmpty(req.Email),
		DisplayName:   req.DisplayName,
	}
	user, err = s.CreateUser(ctx, params)
	if err == nil {
		return user, 0, ""
	}

	// Race: another request created the same discord_user_id concurrently — re-fetch.
	if errors.Is(err, store.ErrConflict) {
		user, err = s.GetUserByDiscordID(ctx, req.DiscordUserID)
		if err == nil {
			return user, 0, ""
		}
		return nil, http.StatusInternalServerError, "failed to resolve collaborator after conflict"
	}
	return nil, http.StatusInternalServerError, "failed to create collaborator"
}

func resolveOrCreateByEmail(
	ctx context.Context,
	s store.Store,
	req *inviteCollaboratorRequest,
) (*domain.User, int, string) {
	user, err := s.GetUserByEmail(ctx, req.Email)
	if err == nil {
		return user, 0, ""
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, http.StatusInternalServerError, "failed to look up user by email"
	}

	// User does not exist — create a collaborator row.
	email := req.Email
	params := store.CreateUserParams{
		Type:        domain.UserTypeCollaborator,
		Email:       &email,
		DisplayName: req.DisplayName,
	}
	user, err = s.CreateUser(ctx, params)
	if err == nil {
		return user, 0, ""
	}

	// Race: another request created the same email concurrently — re-fetch.
	if errors.Is(err, store.ErrConflict) {
		user, err = s.GetUserByEmail(ctx, req.Email)
		if err == nil {
			return user, 0, ""
		}
		return nil, http.StatusInternalServerError, "failed to resolve collaborator after conflict"
	}
	return nil, http.StatusInternalServerError, "failed to create collaborator"
}

// buildConnectURL constructs the Discord OAuth2 authorization URL with CSRF state.
func buildConnectURL(clientID, redirectURL, state string) string {
	return fmt.Sprintf(
		"https://discord.com/api/oauth2/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=identify+guilds.join&state=%s",
		clientID, redirectURL, state,
	)
}

// nilIfEmpty returns nil when s is empty, otherwise a pointer to s.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ─── ExpelCollaborator ────────────────────────────────────────────────────────

// ExpelCollaborator handles DELETE /channels/{id}/collaborators/{userId} (FR-19, AC-5).
//
// Control-plane gated. scope=channel (default) revokes overwrite only;
// scope=server also removes from guild. Both write an audit entry (AC-5).
func (h *Handlers) ExpelCollaborator(c *gin.Context) {
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
	userID := c.Param("userId")
	if spaceID == "" || userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "space id and user id are required"})
		return
	}

	scope := c.DefaultQuery("scope", string(domain.ExpulsionScopeChannel))
	if scope != string(domain.ExpulsionScopeChannel) && scope != string(domain.ExpulsionScopeServer) {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "scope must be 'channel' or 'server'",
		})
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

	if _, err := h.store.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load user"})
		return
	}

	idemKey := middleware.GetIdempotencyKey(c)
	if idemKey == "" {
		idemKey = fmt.Sprintf("expel:%s:%s:%s", spaceID, userID, scope)
	}

	jobID := uuid.New().String()
	job, jobErr := h.store.CreateJob(ctx, store.CreateJobParams{
		TaskID:  idemKey,
		Kind:    queue.KindExpelCollaborator,
		Queue:   queue.QueueMembership,
		SpaceID: &spaceID,
		UserID:  &userID,
		Payload: map[string]any{
			"space_id": spaceID,
			"user_id":  userID,
			"scope":    scope,
		},
	})
	if jobErr == nil {
		jobID = job.ID
	}

	if h.queueClient != nil {
		_, _ = h.queueClient.Enqueue(
			queue.KindExpelCollaborator,
			queue.QueueMembership,
			queue.ExpelCollaboratorPayload{
				SpaceID: spaceID,
				UserID:  userID,
				Scope:   scope,
			},
			queue.TaskIDOpt(idemKey),
			queue.UniqueOpt(24*time.Hour),
		)
	}

	respBody := map[string]any{
		"job": map[string]any{
			"id":          jobID,
			"kind":        queue.KindExpelCollaborator,
			"status":      string(domain.JobStatusPending),
			"space_id":    spaceID,
			"user_id":     userID,
			"retry_count": 0,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		},
	}

	c.Header("Location", fmt.Sprintf("/v1/jobs/%s", jobID))
	c.JSON(http.StatusAccepted, respBody)
}

// ─── ListCollaboratorChannels ─────────────────────────────────────────────────

// ListCollaboratorChannels handles GET /collaborators/{userId}/channels (FR-21, AC-7).
//
// Control-plane gated. Lists all active spaces a collaborator has access to.
func (h *Handlers) ListCollaboratorChannels(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	userID := c.Param("userId")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "userId is required"})
		return
	}

	ctx := c.Request.Context()

	if _, err := h.store.GetUserByID(ctx, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load user"})
		return
	}

	members, err := h.store.ListCollaboratorChannels(ctx, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to list collaborator channels"})
		return
	}

	items := buildCollaboratorChannelItems(ctx, h.store, members)
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// buildCollaboratorChannelItems fetches space and merchant metadata for each space_member row.
// Rows that fail to load (race with deletion) are skipped non-fatally.
func buildCollaboratorChannelItems(ctx context.Context, s store.Store, members []*domain.SpaceMember) []collaboratorChannelResponse {
	items := make([]collaboratorChannelResponse, 0, len(members))
	for _, sm := range members {
		sp, spErr := s.GetSpaceByID(ctx, sm.SpaceID)
		if spErr != nil {
			continue
		}
		m, mErr := s.GetMerchantByID(ctx, sp.MerchantID)
		if mErr != nil {
			continue
		}
		items = append(items, collaboratorChannelResponse{
			SpaceID:        sp.ID,
			SpaceName:      sp.Name,
			MerchantID:     sp.MerchantID,
			MerchantName:   m.Name,
			Role:           string(sm.Role),
			LifecycleState: string(sp.LifecycleState),
		})
	}
	return items
}

// collaboratorChannelResponse is the JSON shape for one CollaboratorChannel item.
type collaboratorChannelResponse struct {
	SpaceID        string `json:"space_id"`
	SpaceName      string `json:"space_name"`
	MerchantID     string `json:"merchant_id"`
	MerchantName   string `json:"merchant_name"`
	Role           string `json:"role"`
	LifecycleState string `json:"lifecycle_state"`
}
