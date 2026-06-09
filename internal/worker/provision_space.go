// provision_space.go implements the KindProvisionSpace worker handler (M2b, §4.4).
//
// Fail-closed invariant (NFR-4, §4.4):
//  1. Acquire the per-merchant distributed lock (§3.3).
//  2. Take a global + per-route rate-limit token before every Discord call (§3.1).
//  3. Call CreateChannelDenied — the channel is born with @everyone deny-VIEW_CHANNEL
//     so there is NO window in which it is world-readable (AC-2).
//  4. Apply the category-level Agent-role allow.
//  5. Persist discord_channel_id + acl_state='applied'.
//
// If ANY ACL step fails, the handler returns SkipRetry (terminal), marks the space
// acl_state='degraded'/'failed', writes an audit entry, and leaves the channel invisible
// (AC-3). We never retry into a half-open ACL.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/hibiken/asynq"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/discord"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/lock"
	"github.com/valianx/discord-support-hub/internal/observability"
	"github.com/valianx/discord-support-hub/internal/queue"
	"github.com/valianx/discord-support-hub/internal/ratelimit"
	"github.com/valianx/discord-support-hub/internal/store"
)

// cacheKeySpaces is the Valkey key prefix for the spaces list cache.
const cacheKeySpaces = "spaces:list"

// cacheKeySpace is the Valkey key prefix for individual space cache entries.
const cacheKeySpace = "spaces:id:"

// cacheKeySpacesGen is the generation token key for the spaces list cache.
// Deleting this key causes all filtered list variants (spaces:list:lc=...:m=...) to
// miss on their next read because the handler includes the generation in the cache key.
// Bumping (deleting) this key is cheaper than scanning for filtered variants (fix DEFECT-1).
const cacheKeySpacesGen = "spaces:list:gen"

// provisionSpaceConfig carries all dependencies for the provision handler.
type provisionSpaceConfig struct {
	store           store.Store
	discord         discord.Client
	limiter         ratelimit.Limiter
	locker          lock.Locker
	cache           cache.Cache
	metrics         *observability.Metrics // nil → no-op (AC-2 wire-up, nil-safe)
	guildID         string
	everyoneRoleID  string // Discord @everyone role id (equals guildID in Discord)
	agentRoleID     string // Discord Agent role id — MUST NOT equal guildID (NFR-5)
	defaultCategory string // default Discord category id when request omits category_id
}

type provisionSpaceHandler struct {
	cfg provisionSpaceConfig
}

func newProvisionSpaceHandler(cfg provisionSpaceConfig) asynq.HandlerFunc {
	if cfg.store == nil || cfg.discord == nil {
		return stubHandler(queue.KindProvisionSpace)
	}
	// Default noop fallbacks so the handler is safe even when optional deps are absent.
	if cfg.limiter == nil {
		cfg.limiter = ratelimit.NoopLimiter{}
	}
	if cfg.locker == nil {
		cfg.locker = lock.NoopLocker{}
	}
	if cfg.cache == nil {
		cfg.cache = cache.NoopCache{}
	}
	// When everyoneRoleID is absent, fall back to guildID (Discord's @everyone role
	// always equals the guild id in their permission model).
	if cfg.everyoneRoleID == "" {
		cfg.everyoneRoleID = cfg.guildID
	}
	// fix(NFR-5): agentRoleID MUST be a real, distinct role — not guildID (@everyone).
	// The category-level allow would make every channel world-readable if guildID is used.
	if cfg.agentRoleID == "" || cfg.agentRoleID == cfg.guildID {
		slog.Error("provision_space: CRITICAL misconfiguration — AgentRoleID is absent or equals GuildID (@everyone); " +
			"refusing to start handler to protect multi-tenant isolation (NFR-5)")
		return stubHandler(queue.KindProvisionSpace)
	}
	h := &provisionSpaceHandler{cfg: cfg}
	return h.handle
}

// handle is the asynq task handler for KindProvisionSpace.
// Named return (retErr) is used so the deferred metrics recorder can observe the final
// outcome without introducing extra control-flow indirection (AC-2).
func (h *provisionSpaceHandler) handle(ctx context.Context, task *asynq.Task) (retErr error) { //nolint:nonamedreturns
	var payload queue.ProvisionSpacePayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		// Malformed payload — archive immediately, do not retry.
		observability.IncError(h.cfg.metrics, "fatal")
		return fmt.Errorf("%w: decode provision payload: %v", asynq.SkipRetry, err)
	}

	// fix(DEFECT-2): guard against an empty SpaceID — this indicates the outbox payload
	// was written without the space_id (old bug). Retrying 10× would only produce
	// GetSpaceByID("") → ErrNotFound on every attempt. Fail terminal instead.
	if payload.SpaceID == "" {
		observability.IncError(h.cfg.metrics, "fatal")
		return fmt.Errorf("%w: provision_space: payload missing space_id — task cannot be processed", asynq.SkipRetry)
	}

	// fix(AC-2): record provisioning latency from job start on both success and fail-closed
	// paths. Rate-limit retries and lock-held retries are excluded (they are not terminal
	// provisioning outcomes).
	jobStart := time.Now()
	defer func() {
		if retErr == nil || errors.Is(retErr, asynq.SkipRetry) {
			success := retErr == nil
			observability.RecordProvisioningLatency(h.cfg.metrics, time.Since(jobStart).Seconds(), success)
		}
	}()

	slog.InfoContext(ctx, "provision_space: starting",
		"space_id", payload.SpaceID, "merchant_id", payload.MerchantID)

	// Advance the job to active status.
	h.transitionJob(ctx, payload.SpaceID, domain.JobStatusActive, nil)

	// --- Acquire per-merchant lock (§3.3) ---
	token, ok, err := h.cfg.locker.AcquireMerchant(ctx, payload.MerchantID)
	if err != nil {
		return fmt.Errorf("provision_space: acquire merchant lock: %w", err)
	}
	if !ok {
		// Lock held by another worker — re-enqueue with backoff.
		slog.InfoContext(ctx, "provision_space: merchant lock held, re-enqueuing",
			"merchant_id", payload.MerchantID)
		return fmt.Errorf("provision_space: merchant %s lock held; retry later", payload.MerchantID)
	}
	defer func() { _ = h.cfg.locker.ReleaseMerchant(ctx, payload.MerchantID, token) }()

	// --- Idempotency: skip if already provisioned ---
	sp, err := h.cfg.store.GetSpaceByID(ctx, payload.SpaceID)
	if err != nil {
		return fmt.Errorf("provision_space: load space: %w", err)
	}
	if sp.DiscordChannelID != nil && sp.ACLState == domain.ACLStateApplied {
		slog.InfoContext(ctx, "provision_space: already provisioned, skipping",
			"space_id", payload.SpaceID, "discord_channel_id", *sp.DiscordChannelID)
		return nil
	}

	categoryID := payload.CategoryID
	if categoryID == "" && h.cfg.defaultCategory != "" {
		// Fall back to the operator-configured default so Agent allow is always applied
		// and the space is never silently marked 'applied' without agent visibility (fix #3).
		categoryID = h.cfg.defaultCategory
		slog.InfoContext(ctx, "provision_space: no category in payload, using configured default",
			"space_id", payload.SpaceID, "default_category", categoryID)
	}
	if categoryID == "" {
		// No category in payload and no configured default.
		// Treat as a precondition error — we cannot apply the Agent allow without a category,
		// so we must not mark the space 'applied' (fix #3 guard).
		return h.failClosed(ctx, payload.SpaceID, payload.MerchantID, "", "no_category_for_agent_allow",
			fmt.Errorf("provision_space: space %s has no category_id and DISCORD_CATEGORY_ID is not configured; "+
				"Agent allow cannot be applied (NFR-5)", payload.SpaceID))
	}

	// --- Step 1: Create channel already denied (fail-closed, §4.4) ---
	// Rate-limit guard before the Discord call.
	if err := h.takeTokens(ctx, "POST/channels"); err != nil {
		return err // *RateLimitError triggers RetryDelayFunc with Retry-After
	}

	channelID, err := h.cfg.discord.CreateChannelDenied(
		ctx, h.cfg.guildID, payload.SpaceName, categoryID, h.cfg.everyoneRoleID,
	)
	if err != nil {
		if isDiscord429(err) {
			return h.handle429(ctx, "POST/channels", err)
		}
		// fix(DEFECT-2): non-429 channel-create errors are terminal — the channel was never
		// created so there is no Discord object to repair. Mark acl_state='failed' via
		// failClosed (empty channelID triggers the ACLStateFailed branch).
		return h.failClosed(ctx, payload.SpaceID, payload.MerchantID, "", "channel_create_failed", err)
	}

	slog.InfoContext(ctx, "provision_space: channel created (born denied to @everyone)",
		"space_id", payload.SpaceID, "discord_channel_id", channelID)

	// --- Step 2: Apply category-level Agent allow (fail-closed: any error = terminal) ---
	// The channel already exists at this point. If the Agent allow fails, we leave the
	// channel invisible (@everyone deny from creation is still in effect). We mark the
	// space degraded and archive the task so we never retry into a half-open ACL (AC-3).
	// categoryID is guaranteed non-empty here (checked above).
	// fix(NFR-5): use agentRoleID — NOT guildID — to avoid granting @everyone view access.
	if rErr := h.takeTokens(ctx, "PUT/channels/"+categoryID+"/permissions"); rErr != nil {
		// Rate limit on the ACL step — still retryable (channel is still invisible).
		return rErr
	}
	if aclErr := h.cfg.discord.ApplyCategoryAgentAllow(ctx, categoryID, h.cfg.agentRoleID); aclErr != nil {
		if isDiscord429(aclErr) {
			return h.handle429(ctx, "PUT/channels/"+categoryID+"/permissions", aclErr)
		}
		return h.failClosed(ctx, payload.SpaceID, payload.MerchantID, channelID, "acl_apply_failed", aclErr)
	}

	// --- Step 3: Persist discord_channel_id + acl_state='applied' ---
	var catPtr *string
	if categoryID != "" {
		catPtr = &categoryID
	}
	if _, err := h.cfg.store.UpdateSpaceDiscordChannel(ctx, store.UpdateSpaceDiscordChannelParams{
		SpaceID:           payload.SpaceID,
		DiscordChannelID:  channelID,
		DiscordCategoryID: catPtr,
		ACLState:          domain.ACLStateApplied,
	}); err != nil {
		// Persist failure: channel exists in Discord but Postgres not updated.
		// Retryable — the worker upsert will set acl_state on retry (idempotent).
		return fmt.Errorf("provision_space: persist discord channel: %w", err)
	}

	// --- Step 4: Advance job to completed, write audit entry, invalidate cache ---
	h.transitionJob(ctx, payload.SpaceID, domain.JobStatusCompleted, nil)

	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:     "space.provision",
		MerchantID: &payload.MerchantID,
		SpaceID:    &payload.SpaceID,
		Detail:     map[string]any{"discord_channel_id": channelID, "category_id": categoryID},
	})

	h.invalidateSpaceCache(ctx, payload.SpaceID)

	slog.InfoContext(ctx, "provision_space: completed",
		"space_id", payload.SpaceID, "discord_channel_id", channelID,
		"acl_state", domain.ACLStateApplied)
	return nil
}

// takeTokens acquires one global token and one per-route token before a Discord call.
// Returns a *RateLimitError when a bucket is empty (asynq RetryDelayFunc handles it).
// fix(AC-2): increments the rate-limit hit counter whenever a bucket is empty.
func (h *provisionSpaceHandler) takeTokens(ctx context.Context, routeKey string) error {
	if err := h.cfg.limiter.TakeGlobal(ctx); err != nil {
		observability.IncRateLimitHit(h.cfg.metrics)
		return err
	}
	if err := h.cfg.limiter.TakeRoute(ctx, routeKey); err != nil {
		observability.IncRateLimitHit(h.cfg.metrics)
		return err
	}
	return nil
}

// handle429 penalizes the rate-limit bucket and returns a retryable *RateLimitError.
func (h *provisionSpaceHandler) handle429(ctx context.Context, routeKey string, discordErr error) error {
	retryAfter := extractDiscord429RetryAfter(discordErr)
	if retryAfter <= 0 {
		retryAfter = 5 * time.Second // safe default
	}
	_ = h.cfg.limiter.PenalizeUntil(ctx, routeKey, retryAfter)
	return &ratelimit.RateLimitError{RetryAfter: retryAfter, Bucket: routeKey}
}

// failClosed marks the space degraded, writes an audit entry, and returns a terminal
// SkipRetry error so asynq archives the task without further retries (AC-3, NFR-4).
//
// The channel (if created) is left with the @everyone deny from the initial creation
// so it remains invisible — we never apply an @everyone allow on failure.
//
// fix(AC-2): records provisioning latency (failure label) and increments the fatal-error
// counter so /metrics reflects real outcomes.
func (h *provisionSpaceHandler) failClosed(
	ctx context.Context,
	spaceID, merchantID, channelID, reason string,
	cause error,
) error {
	// fix(AC-2): record failure outcome in metrics.
	observability.IncError(h.cfg.metrics, "fatal")

	slog.ErrorContext(ctx, "provision_space: fail-closed — ACL apply failed",
		"space_id", spaceID, "reason", reason, "error", cause)

	// Mark the space as failed/degraded so the reconciler and operators know.
	newState := domain.ACLStateDegraded
	if channelID == "" {
		// Channel was never created — mark failed (no Discord object to repair).
		newState = domain.ACLStateFailed
	}
	_, storeErr := h.cfg.store.UpdateSpaceACLState(ctx, spaceID, newState)
	if storeErr != nil {
		slog.ErrorContext(ctx, "provision_space: could not update acl_state on fail-closed",
			"space_id", spaceID, "error", storeErr)
	}

	// Advance the job to archived status.
	h.transitionJob(ctx, spaceID, domain.JobStatusArchived, &cause)

	// Write an audit entry for the failure (no secrets in detail).
	_ = h.cfg.store.InsertAuditEntry(ctx, store.InsertAuditEntryParams{
		Action:     "space.provision.failed",
		MerchantID: &merchantID,
		SpaceID:    &spaceID,
		Detail:     map[string]any{"reason": reason, "acl_state": string(newState)},
	})

	return SkipRetryError(fmt.Errorf("provision_space fail-closed: %s: %w", reason, cause))
}

// transitionJob updates the Postgres jobs mirror row.
// Errors are logged but not propagated — job-row staleness does not affect the
// provisioning invariant (the space's acl_state is the authoritative record).
func (h *provisionSpaceHandler) transitionJob(
	ctx context.Context,
	spaceID string,
	status domain.JobStatus,
	cause *error,
) {
	job, err := h.lookupJobBySpaceID(ctx, spaceID)
	if err != nil || job == nil {
		return
	}
	params := store.UpdateJobStatusParams{
		JobID:     job.ID,
		Status:    status,
		Completed: status == domain.JobStatusCompleted,
	}
	if cause != nil {
		msg := (*cause).Error()
		params.Error = &msg
	}
	if _, err := h.cfg.store.UpdateJobStatus(ctx, params); err != nil {
		slog.WarnContext(ctx, "provision_space: could not update job status",
			"job_id", job.ID, "status", status, "error", err)
	}
}

// lookupJobBySpaceID returns the most recent provision job for a space.
// Returns (nil, nil) when no job is found (store.ErrNotFound is treated as nil).
// fix(M4): uses GetJobBySpaceIDAndKind so the job mirror row is reliably found and
// updated — resolves the "GET /jobs/{id} stuck at pending" issue (M4 job-mirror fix).
func (h *provisionSpaceHandler) lookupJobBySpaceID(ctx context.Context, spaceID string) (*domain.Job, error) {
	job, err := h.cfg.store.GetJobBySpaceIDAndKind(ctx, spaceID, queue.KindProvisionSpace)
	if err != nil {
		// ErrNotFound is expected when the API did not create a jobs row (test paths);
		// treat it as a graceful skip rather than a hard error.
		return nil, nil //nolint:nilerr
	}
	return job, nil
}

// invalidateSpaceCache drops the cached spaces list, per-space entry, and bumps the
// list generation token so all filtered list variants (spaces:list:lc=...:m=...) become
// stale on their next read (fix DEFECT-1/SEC-M2b-003 cache invalidation).
func (h *provisionSpaceHandler) invalidateSpaceCache(ctx context.Context, spaceID string) {
	keysToDelete := []string{
		cacheKeySpaces,
		cacheKeySpace + spaceID,
		cacheKeySpacesGen, // bump generation token so filtered list keys all miss
	}
	if err := h.cfg.cache.Del(ctx, keysToDelete...); err != nil {
		slog.WarnContext(ctx, "provision_space: cache invalidation failed",
			"space_id", spaceID, "error", err)
	}
}

// isDiscord429 reports whether an error from discordgo is a 429 rate limit response.
func isDiscord429(err error) bool {
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) {
		return restErr.Response.StatusCode == 429
	}
	return false
}

// extractDiscord429RetryAfter reads the Retry-After value (seconds) from a discordgo
// RESTError response. Falls back to 0 when the value is absent or malformed.
func extractDiscord429RetryAfter(err error) time.Duration {
	var restErr *discordgo.RESTError
	if !errors.As(err, &restErr) {
		return 0
	}
	ra := restErr.Response.Header.Get("Retry-After")
	if ra == "" {
		return 0
	}
	var secs float64
	if _, scanErr := fmt.Sscanf(ra, "%f", &secs); scanErr != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}
