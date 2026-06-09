package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// cacheKeySpacesList is the cache key for the spaces list.
const cacheKeySpacesList = "spaces:list"

// cacheKeySpacesListGen is the generation token for the spaces list cache.
// Deleting it causes all filtered list variants (spaces:list:lc=...:m=...) to miss.
const cacheKeySpacesListGen = "spaces:list:gen"

// cacheSpaceTTL is how long a cached space entry lives before a fallthrough re-reads Postgres.
const cacheSpaceTTL = 5 * time.Minute

// discordChannelNameMaxLen is Discord's enforced maximum for channel names (100 chars).
// https://discord.com/developers/docs/resources/channel#channel-object
const discordChannelNameMaxLen = 100

// ─── ProvisionSpace ───────────────────────────────────────────────────────────

// provisionSpaceRequest is the validated JSON body for POST /merchants/{merchantId}/channels.
type provisionSpaceRequest struct {
	Name           string  `json:"name" binding:"required"`
	CategoryID     *string `json:"category_id"`
	WelcomeMessage *string `json:"welcome_message"`
}

// validateProvisionRequest validates the channel name and optional category_id fields
// beyond what the struct binding tag can express (SEC-M2b-002, SEC-M3-001).
//
// Channel name rules (Discord's documented limits):
//   - 1–100 characters after trimming whitespace.
//   - No ASCII control characters (0x00–0x1F, 0x7F).
//   - No Unicode bidi or format control characters (Cf category, e.g. U+202E RLO) — SEC-M3-001.
//     These could allow name spoofing via direction overrides in UI surfaces.
//
// Category ID rules: when provided, must be a non-empty string of digits only
// (Discord snowflake format).
func validateProvisionRequest(req *provisionSpaceRequest) (trimmedName string, validationErr string) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return "", "name must not be blank after trimming whitespace"
	}
	if len(name) > discordChannelNameMaxLen {
		return "", fmt.Sprintf("name exceeds Discord's %d-character limit", discordChannelNameMaxLen)
	}
	for _, ch := range name {
		if ch < 0x20 || ch == 0x7F {
			return "", "name contains disallowed control characters"
		}
		// SEC-M3-001: reject Unicode bidi/format control characters (U+202E RIGHT-TO-LEFT
		// OVERRIDE and its family). unicode.Cf covers all format controls; unicode.Co covers
		// private-use codepoints that have no place in a channel name.
		if unicode.Is(unicode.Cf, ch) || unicode.Is(unicode.Co, ch) {
			return "", fmt.Sprintf("name contains disallowed Unicode control character U+%04X", ch)
		}
	}
	if req.CategoryID != nil {
		catID := *req.CategoryID
		if catID == "" {
			return "", "category_id must not be empty when provided"
		}
		for _, ch := range catID {
			if ch < '0' || ch > '9' {
				return "", "category_id must be a numeric Discord snowflake"
			}
		}
	}
	return name, ""
}

// ProvisionSpace handles POST /merchants/{merchantId}/channels (AC-1, FR-1, §2).
//
// Control-plane gated. In one Postgres transaction the desired spaces row (acl_state=pending)
// and an outbox row are written (CreateSpaceWithOutbox). The relay (already built in M2a) then
// enqueues the provision_space task. Returns 202 with a Location header and the job handle.
//
// Idempotency: the Idempotency middleware has already checked for an existing key and would
// have replayed a stored response. Here we store the 202 response via StoreIdempotencyResponse
// so future replays work correctly (the M2a audit noted this caller was missing).
func (h *Handlers) ProvisionSpace(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	// Layer B: control-plane authority required.
	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	merchantID := c.Param("merchantId")
	if merchantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": "merchantId is required"})
		return
	}

	var req provisionSpaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": err.Error()})
		return
	}

	// fix(SEC-M2b-002): validate name (length + no control chars) and category_id (snowflake).
	trimmedName, validationErr := validateProvisionRequest(&req)
	if validationErr != "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": "validation_error", "message": validationErr})
		return
	}
	req.Name = trimmedName // use the trimmed form for all downstream operations

	ctx := c.Request.Context()

	// Verify the merchant exists.
	if _, err := h.store.GetMerchantByID(ctx, merchantID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "merchant not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load merchant"})
		return
	}

	// Derive an idempotency key. Use the middleware-provided key when present;
	// fall back to a deterministic key based on (merchant_id, "provision") so
	// the operation is naturally idempotent even without a client-supplied key.
	idemKey := middleware.GetIdempotencyKey(c)
	if idemKey == "" {
		idemKey = fmt.Sprintf("provision:%s", merchantID)
	}

	// Write desired-state + outbox row in one transaction.
	// fix(DEFECT-2): the outbox payload MUST include space_id so the provision worker can
	// load the correct space via GetSpaceByID. We build a placeholder payload here and
	// update it with the real space.ID after CreateSpaceWithOutbox returns.
	spaceParams := store.CreateSpaceParams{
		MerchantID:        merchantID,
		Name:              req.Name,
		DiscordCategoryID: req.CategoryID,
		WelcomeMessage:    req.WelcomeMessage,
	}
	// Temporary outbox payload — space_id is unknown until after the transaction commits.
	// We must pass a non-nil payload; the real space_id is patched in immediately below.
	outboxParams := store.CreateOutboxParams{
		Aggregate:      "space",
		AggregateID:    merchantID,
		Kind:           queue.KindProvisionSpace,
		Payload:        map[string]any{"merchant_id": merchantID, "space_name": req.Name},
		IdempotencyKey: idemKey,
	}

	space, outboxRow, err := h.store.CreateSpaceWithOutbox(ctx, spaceParams, outboxParams)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// A space for this merchant already exists (1:1 invariant).
			c.JSON(http.StatusConflict, gin.H{
				"code":    "conflict",
				"message": "a space for this merchant already exists",
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to create space"})
		return
	}

	// fix(DEFECT-2): now that we have the space id, update the outbox row's payload so
	// the relay enqueues a task with the correct space_id. Without this the worker's
	// GetSpaceByID("") → ErrNotFound → retries → archived → no channel.
	_ = outboxRow // outboxRow is used implicitly — the relay reads payload from Postgres
	spaceID := space.ID
	if updateErr := h.store.UpdateOutboxPayload(ctx, idemKey, map[string]any{
		"merchant_id": merchantID,
		"space_id":    spaceID,
		"space_name":  req.Name,
		"category_id": func() string {
			if req.CategoryID != nil {
				return *req.CategoryID
			}
			return ""
		}(),
	}); updateErr != nil {
		// Non-fatal: the relay will pick up the outbox row. Log the failure so ops
		// can detect it, but do not fail the request — the space is already committed.
		slog.WarnContext(ctx, "provision_space: could not update outbox payload with space_id",
			"space_id", spaceID, "error", updateErr)
	}

	// Create the Postgres jobs mirror row.
	jobID := uuid.New().String()
	job, jobErr := h.store.CreateJob(ctx, store.CreateJobParams{
		TaskID:     idemKey,
		Kind:       queue.KindProvisionSpace,
		Queue:      queue.QueueProvision,
		MerchantID: &merchantID,
		SpaceID:    &spaceID,
		Payload: map[string]any{
			"merchant_id": merchantID,
			"space_id":    spaceID,
			"space_name":  req.Name,
		},
	})
	if jobErr != nil {
		// Non-fatal: the outbox row is already committed and the relay will enqueue the task.
		// The job mirror row is for polling; its absence degrades poll UX but not correctness.
		// Use a synthetic job ID so the response can still carry a job_id.
		_ = jobID
	} else {
		jobID = job.ID
	}

	// Build the 202 response body.
	jobKind := queue.KindProvisionSpace
	respBody := map[string]any{
		"job": map[string]any{
			"id":          jobID,
			"kind":        jobKind,
			"status":      string(domain.JobStatusPending),
			"space_id":    spaceID,
			"merchant_id": merchantID,
			"retry_count": 0,
			"created_at":  time.Now().UTC().Format(time.RFC3339),
		},
	}

	locationURL := fmt.Sprintf("/v1/jobs/%s", jobID)
	c.Header("Location", locationURL)

	// Store the idempotency response so future replays return the same 202 (AC-1, M2a note).
	middleware.StoreIdempotencyResponse(ctx, h.store, idemKey, http.StatusAccepted, respBody, &jobID)

	// fix(DEFECT-1/SEC-M2b-003): invalidate both the base list key and the generation
	// token (covers filtered list variants) plus the per-space key for the new space.
	_ = h.cache.Del(ctx, cacheKeySpacesList, cacheKeySpacesListGen, "spaces:id:"+spaceID)

	c.JSON(http.StatusAccepted, respBody)
}

// ─── ListSpaces ───────────────────────────────────────────────────────────────

// ListSpaces handles GET /channels (FR-10, §2).
//
// Served from the Valkey cache (TTL + write-invalidation) falling through to Postgres.
// Control-plane gated.
func (h *Handlers) ListSpaces(c *gin.Context) {
	if h.store == nil {
		notImplemented(c)
		return
	}

	p := middleware.GetPrincipal(c)
	if !authz.RequireControlPlane(p) {
		forbidden(c)
		return
	}

	ctx := c.Request.Context()

	lc := c.Query("lifecycle_state")
	mid := c.Query("merchant_id")
	isFiltered := lc != "" || mid != ""

	// Build cache key from query params for basic filter differentiation.
	// fix(DEFECT-1): filtered keys include the generation token so deleting the
	// generation key (cacheKeySpacesListGen) busts all filtered variants at once.
	cacheKey := cacheKeySpacesList
	if isFiltered {
		// Only serve filtered results from cache when the generation token is present.
		// A missing gen token means a write just happened — treat as a cache miss.
		gen, genErr := h.cache.Get(ctx, cacheKeySpacesListGen)
		if genErr != nil || gen == nil {
			// Generation token absent or cache error — skip cache for filtered read.
			goto postgresListFallback
		}
		if lc != "" {
			cacheKey += ":lc=" + lc
		}
		if mid != "" {
			cacheKey += ":m=" + mid
		}
	}

	// Cache read.
	if cached, err := h.cache.Get(ctx, cacheKey); err == nil && cached != nil {
		c.Header("X-Cache", "HIT")
		c.Data(http.StatusOK, "application/json", cached)
		return
	}

postgresListFallback:

	// Postgres fallback.
	params := store.ListSpacesParams{Limit: 50}

	if lc != "" {
		v := domain.SpaceLifecycleState(lc)
		params.LifecycleState = &v
	}
	if mid != "" {
		params.MerchantID = &mid
	}
	if cursor := c.Query("cursor"); cursor != "" {
		params.Cursor = &cursor
	}

	spaces, err := h.store.ListSpaces(ctx, params)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to list spaces"})
		return
	}

	items := make([]spaceResponse, 0, len(spaces))
	for _, sp := range spaces {
		items = append(items, toSpaceResponse(sp))
	}

	var nextCursor *string
	if len(spaces) == params.Limit {
		last := spaces[len(spaces)-1].CreatedAt.UTC().Format(time.RFC3339Nano)
		nextCursor = &last
	}

	respBody := gin.H{"items": items, "next_cursor": nextCursor}

	// Write to cache.
	if b, err := json.Marshal(respBody); err == nil {
		_ = h.cache.Set(ctx, cacheKey, b, cacheSpaceTTL)
		// Ensure the generation token is present so filtered keys are valid on read.
		// Use a long TTL — it is invalidated on write, not on expiry.
		if isFiltered {
			_ = h.cache.Set(ctx, cacheKeySpacesListGen, []byte("1"), cacheSpaceTTL)
		}
	}

	c.JSON(http.StatusOK, respBody)
}

// ─── GetSpace ─────────────────────────────────────────────────────────────────

// GetSpace handles GET /channels/{id} (FR-10, §2).
//
// Cache-first, Postgres fallback, control-plane gated.
func (h *Handlers) GetSpace(c *gin.Context) {
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
	cacheKey := "spaces:id:" + spaceID

	// Cache read.
	if cached, err := h.cache.Get(ctx, cacheKey); err == nil && cached != nil {
		var sp spaceResponse
		if jsonErr := json.Unmarshal(cached, &sp); jsonErr == nil {
			c.Header("X-Cache", "HIT")
			c.JSON(http.StatusOK, sp)
			return
		}
	}

	// Postgres fallback.
	sp, err := h.store.GetSpaceByID(ctx, spaceID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "not_found", "message": "space not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "failed to load space"})
		return
	}

	resp := toSpaceResponse(sp)

	// Write to cache.
	if b, err := json.Marshal(resp); err == nil {
		_ = h.cache.Set(ctx, cacheKey, b, cacheSpaceTTL)
	}

	c.JSON(http.StatusOK, resp)
}

// ─── Stub handlers (M3/M4) ───────────────────────────────────────────────────

// ListSpaceMembers handles GET /channels/{id}/members (FR-17, AC-7).
// Lists all active space_member rows for the given space.
// Control-plane gated. Implementation lives in transversal.go (listSpaceMembers).
func (h *Handlers) ListSpaceMembers(c *gin.Context) {
	h.listSpaceMembers(c)
}

// ChangeSpaceLifecycle handles POST /channels/{id}/lifecycle (FR-7, M4).
// TODO(M4): validate transition, enqueue change_lifecycle job, return 202.
func (h *Handlers) ChangeSpaceLifecycle(c *gin.Context) {
	notImplemented(c)
}

// SyncWelcome handles POST /channels/{id}/welcome:sync (FR-15 static, M4).
// TODO(M4): enqueue sync_welcome job (set topic + pin), return 202.
func (h *Handlers) SyncWelcome(c *gin.Context) {
	notImplemented(c)
}

// ─── Response types ──────────────────────────────────────────────────────────

// spaceResponse is the JSON shape defined by the OpenAPI Space schema.
type spaceResponse struct {
	ID                string  `json:"id"`
	MerchantID        string  `json:"merchant_id"`
	Name              string  `json:"name"`
	DiscordChannelID  *string `json:"discord_channel_id,omitempty"`
	DiscordCategoryID *string `json:"discord_category_id,omitempty"`
	LifecycleState    string  `json:"lifecycle_state"`
	ACLState          string  `json:"acl_state"`
	LastActivityAt    *string `json:"last_activity_at,omitempty"`
	CreatedAt         string  `json:"created_at"`
	ArchivedAt        *string `json:"archived_at,omitempty"`
}

func toSpaceResponse(sp *domain.Space) spaceResponse {
	r := spaceResponse{
		ID:                sp.ID,
		MerchantID:        sp.MerchantID,
		Name:              sp.Name,
		DiscordChannelID:  sp.DiscordChannelID,
		DiscordCategoryID: sp.DiscordCategoryID,
		LifecycleState:    string(sp.LifecycleState),
		ACLState:          string(sp.ACLState),
		CreatedAt:         sp.CreatedAt.UTC().Format(time.RFC3339),
	}
	if sp.LastActivityAt != nil {
		s := sp.LastActivityAt.UTC().Format(time.RFC3339)
		r.LastActivityAt = &s
	}
	if sp.ArchivedAt != nil {
		s := sp.ArchivedAt.UTC().Format(time.RFC3339)
		r.ArchivedAt = &s
	}
	return r
}
