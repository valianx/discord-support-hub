package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// GetJob handles GET /jobs/{jobId}.
//
// Control-plane authority is required (SEC-002): any authenticated principal can
// otherwise read another tenant's merchant_id/space_id/user_id/kind (IDOR, CWE-639).
// In v1 the only caller is the backoffice (backoffice-scoped service key), so
// RequireControlPlane is the correct gate.
//
// TODO(POC-FE/M3): when a session principal exists, scope job polling to the
// caller's own jobs (per-entitlement) instead of requiring full control-plane authority.
//
// Status is always read from the Postgres jobs table — Valkey is never the source of
// truth (AC-7, docs/02-architecture.md §2.3). Returns 404 when no jobs row exists.
func (h *Handlers) GetJob(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	// fix(SEC-002): gate on control-plane authority to prevent IDOR — any authenticated
	// principal could otherwise enumerate jobs belonging to other tenants.
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	jobID := c.Param("jobId")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "jobId path parameter is required",
		})
		return
	}

	ctx := c.Request.Context()
	job, err := h.store.GetJobByID(ctx, jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{
				"code":    "not_found",
				"message": "job not found",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    "internal_error",
			"message": "failed to retrieve job",
		})
		return
	}

	c.JSON(http.StatusOK, toJobResponse(job))
}

// jobResponse is the JSON shape defined by the OpenAPI Job schema.
type jobResponse struct {
	ID          string           `json:"id"`
	Kind        string           `json:"kind"`
	Status      domain.JobStatus `json:"status"`
	SpaceID     *string          `json:"space_id,omitempty"`
	MerchantID  *string          `json:"merchant_id,omitempty"`
	UserID      *string          `json:"user_id,omitempty"`
	RetryCount  int              `json:"retry_count"`
	Error       *string          `json:"error,omitempty"`
	CreatedAt   string           `json:"created_at"`
	CompletedAt *string          `json:"completed_at,omitempty"`
}

func toJobResponse(j *domain.Job) jobResponse {
	resp := jobResponse{
		ID:         j.ID,
		Kind:       j.Kind,
		Status:     j.Status,
		SpaceID:    j.SpaceID,
		MerchantID: j.MerchantID,
		UserID:     j.UserID,
		RetryCount: j.RetryCount,
		Error:      j.Error,
		CreatedAt:  j.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if j.CompletedAt != nil {
		s := j.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.CompletedAt = &s
	}
	return resp
}
