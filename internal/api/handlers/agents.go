package handlers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── ListAgents ──────────────────────────────────────────────────────────────

// ListAgents handles GET /agents (FR-23, M1). Control-plane authority required (Layer B).
// A backoffice-scoped service key or an is_admin user satisfies the gate (§5.2).
func (h *Handlers) ListAgents(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	agents, err := h.store.ListAgents(c.Request.Context(), false)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "list agents: store error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not list agents",
		})
		return
	}

	items := make([]agentResponse, 0, len(agents))
	for _, u := range agents {
		items = append(items, toAgentResponse(u))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// ─── AddAgent ────────────────────────────────────────────────────────────────

// addAgentRequest is the decoded body for POST /agents.
type addAgentRequest struct {
	Email         string  `json:"email" binding:"required,email"`
	DisplayName   *string `json:"display_name"`
	DiscordUserID *string `json:"discord_user_id"`
	IsAdmin       bool    `json:"is_admin"`
}

// AddAgent handles POST /agents (FR-23, M1). Control-plane authority required. Synchronous 201.
// Records the agent in Postgres (the authZ source of truth) and returns a one-time
// Connect-with-Discord URL. A reconcile job projects the Agent role once they join.
func (h *Handlers) AddAgent(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	var req addAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "validation_error", "message": err.Error(),
		})
		return
	}

	// fix(SEC-M4-002): display_name becomes a Discord nickname; reject unsafe runes
	// (ASCII control chars + Unicode Cf/Co) to prevent nickname spoofing via bidi overrides.
	if req.DisplayName != nil {
		if msg := rejectUnsafeRunes(*req.DisplayName); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "validation_error",
				"message": "display_name " + msg,
			})
			return
		}
	}

	email := req.Email
	user, err := h.store.CreateUser(c.Request.Context(), store.CreateUserParams{
		Type:          domain.UserTypeAgent,
		IsAdmin:       req.IsAdmin,
		Email:         &email,
		DisplayName:   req.DisplayName,
		DiscordUserID: req.DiscordUserID,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			c.JSON(http.StatusConflict, gin.H{
				"code":    "conflict",
				"message": "an agent with this Discord user id already exists",
			})
			return
		}
		slog.ErrorContext(c.Request.Context(), "add agent: store error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not create agent",
		})
		return
	}

	// Enqueue a project_agent_role job so the role is applied once the agent joins.
	// This is a best-effort enqueue; if it fails the reconciler will catch the drift.
	if h.queueClient != nil {
		taskID := fmt.Sprintf("project_agent_role:%s:add", user.ID)
		_, err := h.queueClient.Enqueue(
			queue.KindProjectAgentRole,
			queue.QueueMembership,
			queue.ProjectAgentRolePayload{UserID: user.ID, Add: true},
			queue.TaskIDOpt(taskID),
			queue.UniqueOpt(24*time.Hour),
		)
		if err != nil {
			// Non-fatal: reconciler will re-assert the role on its next sweep.
			slog.WarnContext(c.Request.Context(), "add agent: could not enqueue role projection job",
				"user_id", user.ID, "error", err)
		}
	}

	connectURL := buildDiscordOAuthURL(h.discordOAuthClientID, h.discordOAuthRedirectURL, user.ID)

	resp := gin.H{
		"id":              user.ID,
		"type":            user.Type,
		"is_admin":        user.IsAdmin,
		"email":           user.Email,
		"display_name":    user.DisplayName,
		"discord_user_id": user.DiscordUserID,
		"provisioned_at":  user.ProvisionedAt,
		"is_active":       user.IsActive,
		"created_at":      user.CreatedAt,
		"connect_url":     connectURL,
	}
	c.JSON(http.StatusCreated, resp)
}

// ─── RemoveAgent ─────────────────────────────────────────────────────────────

// RemoveAgent handles DELETE /agents/{userId} (FR-23, M1). Control-plane authority required. Async 202.
// Marks the agent inactive in Postgres and enqueues a job to remove the Agent role.
func (h *Handlers) RemoveAgent(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	userID := c.Param("userId")
	if userID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "validation_error", "message": "userId is required",
		})
		return
	}

	// Verify the user exists and is an agent before deactivating.
	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{
				"code": "not_found", "message": "agent not found",
			})
			return
		}
		slog.ErrorContext(c.Request.Context(), "remove agent: store error fetching user", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not fetch agent",
		})
		return
	}
	if user.Type != domain.UserTypeAgent {
		c.JSON(http.StatusNotFound, gin.H{
			"code": "not_found", "message": "agent not found",
		})
		return
	}

	_, err = h.store.DeactivateUser(c.Request.Context(), userID)
	if err != nil {
		slog.ErrorContext(c.Request.Context(), "remove agent: store error deactivating", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not deactivate agent",
		})
		return
	}

	// Enqueue role-removal job.
	var jobID string
	if h.queueClient != nil {
		taskID := fmt.Sprintf("project_agent_role:%s:remove", userID)
		info, err := h.queueClient.Enqueue(
			queue.KindProjectAgentRole,
			queue.QueueMembership,
			queue.ProjectAgentRolePayload{UserID: userID, Add: false},
			queue.TaskIDOpt(taskID),
			queue.UniqueOpt(24*time.Hour),
		)
		if err != nil {
			slog.WarnContext(c.Request.Context(), "remove agent: could not enqueue role removal job",
				"user_id", userID, "error", err)
		} else {
			jobID = info.ID
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job": gin.H{
			"id":     jobID,
			"kind":   queue.KindProjectAgentRole,
			"status": "pending",
		},
	})
}

// ─── Response helpers ─────────────────────────────────────────────────────────

type agentResponse struct {
	ID            string     `json:"id"`
	Type          string     `json:"type"`
	IsAdmin       bool       `json:"is_admin"`
	Email         *string    `json:"email,omitempty"`
	DisplayName   *string    `json:"display_name,omitempty"`
	DiscordUserID *string    `json:"discord_user_id,omitempty"`
	ProvisionedAt *time.Time `json:"provisioned_at,omitempty"`
	IsActive      bool       `json:"is_active"`
	CreatedAt     time.Time  `json:"created_at"`
}

func toAgentResponse(u *domain.User) agentResponse {
	return agentResponse{
		ID:            u.ID,
		Type:          string(u.Type),
		IsAdmin:       u.IsAdmin,
		Email:         u.Email,
		DisplayName:   u.DisplayName,
		DiscordUserID: u.DiscordUserID,
		ProvisionedAt: u.ProvisionedAt,
		IsActive:      u.IsActive,
		CreatedAt:     u.CreatedAt,
	}
}

// buildDiscordOAuthURL constructs the "Connect with Discord" authorize URL.
// The state parameter is the agent's hub user ID (M1 simplified; M3 adds HMAC signing).
// TODO(M3): replace the state with an HMAC-signed, single-use token (CSRF protection).
func buildDiscordOAuthURL(clientID, redirectURL, stateUserID string) string {
	if clientID == "" || redirectURL == "" {
		return ""
	}
	return fmt.Sprintf(
		"https://discord.com/api/oauth2/authorize?client_id=%s&redirect_uri=%s&response_type=code&scope=identify%%20guilds.join&state=%s",
		clientID, redirectURL, stateUserID,
	)
}
