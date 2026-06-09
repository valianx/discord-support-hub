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
	"time"

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
type inviteCollaboratorRequest struct {
	UserID      string  `json:"user_id"`
	DisplayName *string `json:"display_name"`
}

// InviteCollaborator handles POST /channels/{id}/collaborators (FR-4, AC-2, AC-4).
//
// Control-plane gated. Only Agents/Admins may invite (FR-20) — collaborators cannot (AC-4).
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
	if req.UserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "user_id is required"})
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
	_ = sp // used for existence verification

	// Verify user exists.
	user, err := h.store.GetUserByID(ctx, req.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "user not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load user"})
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
