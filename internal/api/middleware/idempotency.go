package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

const (
	idempotencyKeyHeader = "Idempotency-Key"
	idempotencyKeyCtx    = "idempotency_key"
	idempotencyTTL       = 24 * time.Hour
)

// Idempotency is the edge-level idempotency middleware (NFR-3, §4.1).
//
// When a mutating request carries an Idempotency-Key header:
//   - New key: insert a pending idempotency_keys row, run the handler.
//   - Existing key + same body hash + stored response: replay the stored response (no re-enqueue).
//   - Existing key + different body hash: return 409 Conflict.
//
// Requests without an Idempotency-Key header pass through unchanged.
// Pass nil for s to get a no-op pass-through (test mode / read routes).
func Idempotency(s store.Store) gin.HandlerFunc {
	if s == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		key := c.GetHeader(idempotencyKeyHeader)
		if key == "" {
			c.Next()
			return
		}

		ctx := c.Request.Context()

		// Read and restore the body so downstream handlers can read it again.
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"code":    "validation_error",
				"message": "could not read request body",
			})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		reqHash := hashRequest(c.Request.Method, c.FullPath(), bodyBytes)

		existing, getErr := s.GetIdempotencyKey(ctx, key)
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "internal_error",
				"message": "idempotency check failed",
			})
			return
		}

		if existing != nil {
			// Key exists — guard against body-hash mismatch (§4.1).
			if !bytes.Equal(existing.RequestHash, reqHash) {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{
					"code":    "idempotency_conflict",
					"message": "Idempotency-Key reused with a different request body",
				})
				return
			}
			// Replay a completed response.
			if existing.ResponseCode != nil && existing.ResponseBody != nil {
				c.Header("Idempotency-Replay", "true")
				c.JSON(*existing.ResponseCode, existing.ResponseBody)
				c.Abort()
				return
			}
			// Still pending (concurrent request): pass through. asynq TaskID prevents
			// double-enqueue.
			c.Set(idempotencyKeyCtx, key)
			c.Next()
			return
		}

		// New key: insert a pending record.
		if _, insertErr := s.InsertIdempotencyKey(ctx, store.InsertIdempotencyKeyParams{
			Key:         key,
			RequestHash: reqHash,
			ExpiresAt:   time.Now().Add(idempotencyTTL),
		}); insertErr != nil && !errors.Is(insertErr, store.ErrConflict) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"code":    "internal_error",
				"message": "idempotency registration failed",
			})
			return
		}

		c.Set(idempotencyKeyCtx, key)
		c.Next()
	}
}

// GetIdempotencyKey retrieves the idempotency key set by the middleware.
// Returns "" when no key was present.
func GetIdempotencyKey(c *gin.Context) string {
	v, _ := c.Get(idempotencyKeyCtx)
	s, _ := v.(string)
	return s
}

// StoreIdempotencyResponse records the final response on the idempotency_keys row.
// Called by handlers after they determine the response. A no-op when key is empty or s nil.
func StoreIdempotencyResponse(
	ctx context.Context,
	s store.Store,
	key string,
	code int,
	body map[string]any,
	jobID *string,
) {
	if s == nil || key == "" {
		return
	}
	status := domain.JobStatusPending
	if code >= 400 {
		status = domain.JobStatusArchived
	}
	_ = s.UpdateIdempotencyKeyResponse(ctx, store.UpdateIdempotencyKeyResponseParams{
		Key:          key,
		Status:       status,
		ResponseCode: code,
		ResponseBody: body,
		JobID:        jobID,
	})
}

// hashRequest returns a SHA-256 digest of method + path + body.
func hashRequest(method, path string, body []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte(method))
	_, _ = h.Write([]byte(path))
	_, _ = h.Write(body)
	return h.Sum(nil)
}

// HashRequestForTest is the exported variant of hashRequest for use in package tests.
// It allows tests to pre-compute the hash that the middleware will generate.
func HashRequestForTest(method, path string, body []byte) []byte {
	return hashRequest(method, path, body)
}
