// transversal.go implements cross-cutting query handlers.
//
// GetDirectory: bidirectional membership directory (user→spaces, space→users, merchant→members).
// GetAudit: audit log endpoint (FR-14, M4).
// ListSpaceMembers: lists active space_member rows for a space.
//
// M6: OAuthDiscordCallback, enqueuePendingInvites, linkDiscordUserID, and the standalone shim
//     are removed. OAuth2 onboarding is fully removed (AC-M6-9).
package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── GetDirectory ─────────────────────────────────────────────────────────────

// GetDirectory handles GET /directory (FR-18, AC-7).
//
// Bidirectional membership directory:
//   - user_id filter     → which spaces does this collaborator belong to?
//   - space_id filter    → which collaborators are in this space?
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

// GetAudit handles GET /audit (FR-14, M4 AC-2).
//
// Returns audit_log entries newest-first with optional filters:
//   - merchant_id: filter to a specific merchant
//   - space_id:    filter to a specific space
//   - action:      filter to a specific action type (e.g. "space.provision")
//   - since:       ISO-8601 timestamp; only entries after this time
//
// Cursor pagination via the `cursor` query param (last seen id as a string).
// Control-plane gated. No secrets appear in the audit detail (NFR-6).
func (h *Handlers) GetAudit(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	params := store.ListAuditEntriesParams{Limit: 50}

	if v := c.Query("merchant_id"); v != "" {
		params.MerchantID = &v
	}
	if v := c.Query("space_id"); v != "" {
		params.SpaceID = &v
	}
	if v := c.Query("action"); v != "" {
		params.Action = &v
	}
	if v := c.Query("since"); v != "" {
		params.Since = &v
	}
	if v := c.Query("cursor"); v != "" {
		var cursorID int64
		if _, err := fmt.Sscanf(v, "%d", &cursorID); err == nil {
			params.Cursor = &cursorID
		}
	}

	ctx := c.Request.Context()

	entries, err := h.store.ListAuditEntries(ctx, params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to list audit entries"})
		return
	}

	items := make([]auditEntryResponse, 0, len(entries))
	for _, e := range entries {
		items = append(items, toAuditEntryResponse(e))
	}

	var nextCursor *string
	if len(entries) == params.Limit && len(entries) > 0 {
		last := entries[len(entries)-1].ID
		s := fmt.Sprintf("%d", last)
		nextCursor = &s
	}

	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": nextCursor})
}

// auditEntryResponse is the JSON shape for one audit_log entry.
// No secrets appear in this response (NFR-6, FR-14).
type auditEntryResponse struct {
	ID            int64          `json:"id"`
	ActorAPIKeyID *string        `json:"actor_api_key_id,omitempty"`
	ActorUserID   *string        `json:"actor_user_id,omitempty"`
	Action        string         `json:"action"`
	MerchantID    *string        `json:"merchant_id,omitempty"`
	SpaceID       *string        `json:"space_id,omitempty"`
	TargetUserID  *string        `json:"target_user_id,omitempty"`
	Scope         *string        `json:"scope,omitempty"`
	Detail        map[string]any `json:"detail,omitempty"`
	CreatedAt     string         `json:"created_at"`
}

func toAuditEntryResponse(e *domain.AuditEntry) auditEntryResponse {
	r := auditEntryResponse{
		ID:            e.ID,
		ActorAPIKeyID: e.ActorAPIKeyID,
		ActorUserID:   e.ActorUserID,
		Action:        e.Action,
		MerchantID:    e.MerchantID,
		SpaceID:       e.SpaceID,
		TargetUserID:  e.TargetUserID,
		Detail:        e.Detail,
		CreatedAt:     e.CreatedAt.UTC().Format(time.RFC3339),
	}
	if e.Scope != nil {
		s := string(*e.Scope)
		r.Scope = &s
	}
	return r
}

// ─── ListSpaceMembers ─────────────────────────────────────────────────────────

// spaceMemberResponse is the JSON shape for one space_member row (M6: invite_sent_at replaces overwrite_applied).
type spaceMemberResponse struct {
	ID           string  `json:"id"`
	UserID       string  `json:"user_id"`
	Role         string  `json:"role"`
	InviteSentAt *string `json:"invite_sent_at,omitempty"` // stamped when invite email was sent (AC-M6-5)
	InvitedBy    *string `json:"invited_by,omitempty"`
	CreatedAt    string  `json:"created_at"`
	RevokedAt    *string `json:"revoked_at,omitempty"`
}

// listSpaceMembers is the real implementation for GET /channels/{id}/members (FR-17, AC-7).
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
			ID:        sm.ID,
			UserID:    sm.UserID,
			Role:      string(sm.Role),
			InvitedBy: sm.InvitedBy,
			CreatedAt: sm.CreatedAt.UTC().Format(time.RFC3339),
		}
		if sm.InviteSentAt != nil {
			s := sm.InviteSentAt.UTC().Format(time.RFC3339)
			r.InviteSentAt = &s
		}
		if sm.RevokedAt != nil {
			s := sm.RevokedAt.UTC().Format(time.RFC3339)
			r.RevokedAt = &s
		}
		items = append(items, r)
	}

	c.JSON(http.StatusOK, gin.H{"items": items})
}
