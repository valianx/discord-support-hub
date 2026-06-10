// collaborators.go implements the collaborator membership handlers (M6 onboarding pivot).
//
// All endpoints are control-plane gated (RequireControlPlane).
// Collaborators themselves can never reach invite/expel (FR-20, AC-4).
//
// RegisterCollaborator: POST /channels/{id}/collaborators (synchronous 201, AC-M6-4)
// SendCollaboratorInvite: POST /channels/{id}/collaborators/{userId}:send-invite (202, AC-M6-5)
// ExpelCollaborator: DELETE /channels/{id}/collaborators/{userId}
// ListCollaboratorChannels: GET /collaborators/{userId}/channels
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

// ─── RegisterCollaborator ─────────────────────────────────────────────────────

// registerCollaboratorRequest is the JSON body for POST /channels/{id}/collaborators (AC-M6-4).
// Requires name and email; no Discord identity needed at registration time.
type registerCollaboratorRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// RegisterCollaborator handles POST /channels/{id}/collaborators (AC-M6-4).
//
// Control-plane gated. Accepts {name, email} only — no Discord identity needed.
// Creates the user row if no matching email exists.
// Inserts the desired space_member row synchronously and returns 201.
// No async task is enqueued here; the operator calls :send-invite separately.
func (h *Handlers) RegisterCollaborator(c *gin.Context) {
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

	var req registerCollaboratorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": err.Error()})
		return
	}

	if req.Name == "" || req.Email == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "name and email are required",
		})
		return
	}

	if msg := validateEmailFormat(req.Email); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": msg})
		return
	}

	if msg := rejectUnsafeRunes(req.Name); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "name " + msg})
		return
	}

	ctx := c.Request.Context()

	// Verify space exists.
	if _, err := h.store.GetSpaceByID(ctx, spaceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "space not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load space"})
		return
	}

	// Resolve or create the collaborator user by email.
	displayName := req.Name
	user, httpStatus, resolveMsg := resolveOrCreateByEmailWithName(ctx, h.store, req.Email, &displayName)
	if resolveMsg != "" {
		c.JSON(httpStatus, gin.H{"code": "internal_error", "message": resolveMsg})
		return
	}

	invitedBy := p.UserID

	// Insert the desired space_member row (idempotent on conflict).
	sm, smErr := h.store.CreateSpaceMember(ctx, store.CreateSpaceMemberParams{
		SpaceID:   spaceID,
		UserID:    user.ID,
		Role:      domain.SpaceMemberRoleCollaborator,
		InvitedBy: nilIfEmpty(invitedBy),
	})
	if smErr != nil {
		if errors.Is(smErr, store.ErrConflict) {
			// Already registered — fetch and return the existing row.
			sm, smErr = h.store.GetSpaceMemberBySpaceAndUser(ctx, spaceID, user.ID)
			if smErr != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load existing membership"})
				return
			}
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to record membership"})
			return
		}
	}

	// Write audit entry.
	_ = h.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:       "collaborator.register",
		SpaceID:      &spaceID,
		TargetUserID: &user.ID,
		ActorUserID:  nilIfEmpty(invitedBy),
		Detail: map[string]any{
			"email": req.Email, // email in audit is acceptable (not a secret)
		},
	})

	c.Header("Location", fmt.Sprintf("/v1/channels/%s/collaborators/%s", spaceID, user.ID))
	c.JSON(http.StatusCreated, gin.H{
		"space_member_id": sm.ID,
		"space_id":        spaceID,
		"user_id":         user.ID,
		"role":            string(sm.Role),
		"created_at":      sm.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// ─── SendCollaboratorInvite ───────────────────────────────────────────────────

// SendCollaboratorInvite handles POST /channels/{id}/collaborators/{userId}:send-invite (AC-M6-5).
//
// Control-plane gated. Enqueues a KindSendInvite task on the notify queue.
// Returns 409 when the merchant has no invite link stored (AC-M6-5 precondition).
// Returns 202 when the task is enqueued.
func (h *Handlers) SendCollaboratorInvite(c *gin.Context) {
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

	ctx := c.Request.Context()

	// Load space to get the merchant id.
	sp, err := h.store.GetSpaceByID(ctx, spaceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "space not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load space"})
		return
	}

	// Verify the merchant has an invite link stored (fail 409 if not — AC-M6-5).
	merchant, err := h.store.GetMerchantByID(ctx, sp.MerchantID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load merchant"})
		return
	}
	if merchant.InviteLink == nil || *merchant.InviteLink == "" {
		c.JSON(http.StatusConflict, gin.H{
			"code":    "no_invite_link",
			"message": "merchant has no invite link stored; set one via PUT /merchants/{id}/invite first",
		})
		return
	}

	// Verify the space_member exists.
	sm, err := h.store.GetSpaceMemberBySpaceAndUser(ctx, spaceID, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "collaborator not registered in this space"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load space member"})
		return
	}

	idemKey := middleware.GetIdempotencyKey(c)
	if idemKey == "" {
		idemKey = fmt.Sprintf("send-invite:%s:%s", spaceID, userID)
	}

	jobID := uuid.New().String()
	job, jobErr := h.store.CreateJob(ctx, store.CreateJobParams{
		TaskID:  idemKey,
		Kind:    queue.KindSendInvite,
		Queue:   queue.QueueNotify,
		SpaceID: &spaceID,
		UserID:  &userID,
		Payload: map[string]any{
			"space_member_id": sm.ID,
			"space_id":        spaceID,
			"user_id":         userID,
			"merchant_id":     sp.MerchantID,
		},
	})
	if jobErr == nil {
		jobID = job.ID
	}

	if h.queueClient != nil {
		_, _ = h.queueClient.Enqueue(
			queue.KindSendInvite,
			queue.QueueNotify,
			queue.SendInvitePayload{
				SpaceMemberID: sm.ID,
				SpaceID:       spaceID,
				UserID:        userID,
				MerchantID:    sp.MerchantID,
			},
			queue.TaskIDOpt(idemKey),
			queue.UniqueOpt(24*time.Hour),
		)
	}

	c.Header("Location", fmt.Sprintf("/v1/jobs/%s", jobID))
	c.JSON(http.StatusAccepted, gin.H{
		"job": map[string]any{
			"id":          jobID,
			"kind":        queue.KindSendInvite,
			"status":      string(domain.JobStatusPending),
			"space_id":    spaceID,
			"user_id":     userID,
			"retry_count": 0,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		},
	})
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

// resolveOrCreateByEmailWithName looks up a collaborator by email, creating one if absent.
// Returns (user, 0, "") on success; (nil, httpStatus, message) on failure.
func resolveOrCreateByEmailWithName(
	ctx context.Context,
	s store.Store,
	emailAddr string,
	displayName *string,
) (*domain.User, int, string) {
	user, err := s.GetUserByEmail(ctx, emailAddr)
	if err == nil {
		return user, 0, ""
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, http.StatusInternalServerError, "failed to look up user by email"
	}

	e := emailAddr
	user, err = s.CreateUser(ctx, store.CreateUserParams{
		Type:        domain.UserTypeCollaborator,
		Email:       &e,
		DisplayName: displayName,
	})
	if err == nil {
		return user, 0, ""
	}

	// Race: another request created the same email concurrently — re-fetch.
	if errors.Is(err, store.ErrConflict) {
		user, err = s.GetUserByEmail(ctx, emailAddr)
		if err == nil {
			return user, 0, ""
		}
		return nil, http.StatusInternalServerError, "failed to resolve collaborator after conflict"
	}
	return nil, http.StatusInternalServerError, "failed to create collaborator"
}

// nilIfEmpty returns nil when s is empty, otherwise a pointer to s.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// rejectUnsafeRunes is declared in spaces.go (shared within the handlers package).

// ─── ExpelCollaborator ────────────────────────────────────────────────────────

// ExpelCollaborator handles DELETE /channels/{id}/collaborators/{userId} (FR-19, AC-5).
//
// Control-plane gated. scope=channel (default) revokes role access;
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

	c.Header("Location", fmt.Sprintf("/v1/jobs/%s", jobID))
	c.JSON(http.StatusAccepted, gin.H{
		"job": map[string]any{
			"id":          jobID,
			"kind":        queue.KindExpelCollaborator,
			"status":      string(domain.JobStatusPending),
			"space_id":    spaceID,
			"user_id":     userID,
			"retry_count": 0,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		},
	})
}

// ─── ListCollaboratorChannels ─────────────────────────────────────────────────

// ListCollaboratorChannels handles GET /collaborators/{userId}/channels (FR-21, AC-7).
//
// Control-plane gated. Lists all active spaces a collaborator has been registered in.
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
