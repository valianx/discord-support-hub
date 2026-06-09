package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── RegisterMerchant ─────────────────────────────────────────────────────────

// registerMerchantRequest is the validated JSON body for POST /merchants.
type registerMerchantRequest struct {
	ExternalRef string  `json:"external_ref"`
	Name        string  `json:"name"`
	HelpDeskURL *string `json:"help_desk_url"`
}

// RegisterMerchant handles POST /merchants (AC-1, AC-2, AC-3). Synchronous 201.
//
// Control-plane gated. Records the merchant in Postgres (the authZ source of truth)
// and returns 201 with the created Merchant. No Discord side-effect.
// Duplicate external_ref returns 409 (UNIQUE constraint → ErrConflict).
func (h *Handlers) RegisterMerchant(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	var req registerMerchantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": "validation_error", "message": err.Error(),
		})
		return
	}

	// Required field validation (ShouldBindJSON does not enforce required on non-tagged fields).
	req.ExternalRef = strings.TrimSpace(req.ExternalRef)
	req.Name = strings.TrimSpace(req.Name)

	if req.ExternalRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "external_ref is required and must not be blank",
		})
		return
	}
	if req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "name is required and must not be blank",
		})
		return
	}

	// Reject unsafe runes (ASCII control chars + Unicode Cf/Co) to prevent UI spoofing.
	if msg := rejectUnsafeRunes(req.ExternalRef); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "external_ref " + msg,
		})
		return
	}
	if msg := rejectUnsafeRunes(req.Name); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "validation_error",
			"message": "name " + msg,
		})
		return
	}

	// Validate help_desk_url when provided: must be an absolute http/https URL.
	if req.HelpDeskURL != nil {
		if msg := validateHelpDeskURL(*req.HelpDeskURL); msg != "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "validation_error",
				"message": "help_desk_url " + msg,
			})
			return
		}
	}

	ctx := c.Request.Context()

	m, err := h.store.CreateMerchant(ctx, store.CreateMerchantParams{
		ExternalRef: req.ExternalRef,
		Name:        req.Name,
		HelpDeskURL: req.HelpDeskURL,
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			c.JSON(http.StatusConflict, gin.H{
				"code":    "conflict",
				"message": "a merchant with this external_ref already exists",
			})
			return
		}
		slog.ErrorContext(ctx, "register merchant: store error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not create merchant",
		})
		return
	}

	c.JSON(http.StatusCreated, toMerchantResponse(m))
}

// ─── ListMerchants ────────────────────────────────────────────────────────────

// ListMerchants handles GET /merchants (AC-5, AC-6). Control-plane gated.
// Returns a cursor-paginated list of merchants ordered by created_at ASC.
// Optional query param: is_active (boolean filter).
func (h *Handlers) ListMerchants(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	params := store.ListMerchantsParams{Limit: 50}

	if v := c.Query("is_active"); v != "" {
		switch v {
		case "true":
			t := true
			params.IsActive = &t
		case "false":
			f := false
			params.IsActive = &f
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"code":    "validation_error",
				"message": "is_active must be 'true' or 'false'",
			})
			return
		}
	}

	if lv := c.Query("limit"); lv != "" {
		var n int
		if _, err := parseQueryInt(lv, &n); err == nil && n > 0 && n <= 200 {
			params.Limit = n
		}
	}

	if cursor := c.Query("cursor"); cursor != "" {
		params.Cursor = &cursor
	}

	ctx := c.Request.Context()

	merchants, err := h.store.ListMerchants(ctx, params)
	if err != nil {
		slog.ErrorContext(ctx, "list merchants: store error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not list merchants",
		})
		return
	}

	items := make([]merchantResponse, 0, len(merchants))
	for _, m := range merchants {
		items = append(items, toMerchantResponse(m))
	}

	var nextCursor *string
	if len(merchants) == params.Limit {
		last := merchants[len(merchants)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
		nextCursor = &last
	}

	c.JSON(http.StatusOK, gin.H{"items": items, "next_cursor": nextCursor})
}

// ─── GetMerchant ──────────────────────────────────────────────────────────────

// GetMerchant handles GET /merchants/{merchantId} (AC-7). Control-plane gated.
// Returns the merchant identified by the UUID merchantId.
// Returns 404 for absent or malformed (non-UUID) ids.
func (h *Handlers) GetMerchant(c *gin.Context) {
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	merchantID, ok := parseUUIDParam(c, "merchantId")
	if !ok {
		return // parseUUIDParam already wrote the 404 response
	}

	ctx := c.Request.Context()

	m, err := h.store.GetMerchantByID(ctx, merchantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "merchant not found"})
			return
		}
		slog.ErrorContext(ctx, "get merchant: store error", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": "internal_error", "message": "could not load merchant",
		})
		return
	}

	c.JSON(http.StatusOK, toMerchantResponse(m))
}

// ─── Response type ────────────────────────────────────────────────────────────

// merchantResponse is the JSON shape defined by the OpenAPI Merchant schema.
type merchantResponse struct {
	ID          string  `json:"id"`
	ExternalRef string  `json:"external_ref"`
	Name        string  `json:"name"`
	HelpDeskURL *string `json:"help_desk_url"`
	IsActive    bool    `json:"is_active"`
	CreatedAt   string  `json:"created_at"`
}

func toMerchantResponse(m *domain.Merchant) merchantResponse {
	return merchantResponse{
		ID:          m.ID,
		ExternalRef: m.ExternalRef,
		Name:        m.Name,
		HelpDeskURL: m.HelpDeskURL,
		IsActive:    m.IsActive,
		CreatedAt:   m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// ─── Shared helpers ───────────────────────────────────────────────────────────

// parseUUIDParam extracts and validates a path parameter as a UUID.
// When the value is empty or not a valid UUID format, it writes a 404 response
// and returns ("", false). The caller must return immediately on false.
//
// Using 404 (not 400) intentionally: a non-UUID path segment is indistinguishable
// from a well-formed UUID that maps to no row — both should be not_found to the
// caller (plan §Decisions, AC-8). This also prevents SQLSTATE 22P02 from reaching
// the generic 500 handler (the reported defect).
func parseUUIDParam(c *gin.Context, paramName string) (string, bool) {
	v := c.Param(paramName)
	if v == "" || !isValidUUID(v) {
		c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "not found"})
		return "", false
	}
	return v, true
}

// isValidUUID reports whether s is a canonical UUID (8-4-4-4-12 hex, case-insensitive).
// We inline a lightweight check rather than importing a uuid library so this helper
// stays dependency-free within the handlers package.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, ch := range s {
		switch i {
		case 8, 13, 18, 23:
			if ch != '-' {
				return false
			}
		default:
			if !isHexRune(ch) {
				return false
			}
		}
	}
	return true
}

func isHexRune(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// validateHelpDeskURL returns a non-empty error message when rawURL is not an
// absolute http/https URL. The URL is a display link only (never fetched server-side),
// so scheme validation is the security boundary here.
func validateHelpDeskURL(rawURL string) string {
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return "must be a valid URL"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "must use http or https scheme"
	}
	if u.Host == "" {
		return "must include a host"
	}
	return ""
}

// parseQueryInt parses a decimal integer from a query string value.
// Returns an error string (non-empty) when parsing fails.
func parseQueryInt(s string, out *int) (string, error) {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return "not a valid integer", errors.New("invalid")
		}
		n = n*10 + int(ch-'0')
	}
	*out = n
	return "", nil
}
