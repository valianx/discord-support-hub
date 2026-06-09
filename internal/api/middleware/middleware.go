// Package middleware provides Gin middleware for the API layer.
// RequestID and Recovery are real implementations.
// Auth (Layer A) and Idempotency are pass-through stubs pending M1 implementation.
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

const requestIDHeader = "X-Request-ID"
const requestIDKey = "request_id"

// RequestID generates a unique request id for each incoming request and stores it in
// the Gin context and the response header. Downstream handlers use it for log correlation.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDHeader)
		if id == "" {
			id = newRequestID()
		}
		c.Set(requestIDKey, id)
		c.Header(requestIDHeader, id)
		c.Next()
	}
}

// Recovery wraps gin.Recovery to log panics as structured slog errors before returning 500.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				reqID, _ := c.Get(requestIDKey)
				slog.Error("api: panic recovered",
					"request_id", reqID,
					"panic", r,
					"path", c.Request.URL.Path,
					"method", c.Request.Method,
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":    "internal_error",
					"message": "an unexpected error occurred",
				})
			}
		}()
		c.Next()
	}
}

// Auth is a pass-through stub for Layer A (service API key authentication).
// In M1 this will: extract the Bearer token, hash it, look up api_keys, inject Principal.
// TODO(M1): implement real API key authentication.
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// TODO(M1): extract bearer token, hash, look up api_keys, inject Principal.
		// For M0 we let all requests through so the skeleton compiles and routes respond.
		c.Next()
	}
}

// Idempotency is a pass-through stub for edge-level idempotency (NFR-3, §4.1).
// In M1/M2 this will: read Idempotency-Key header, check idempotency_keys table,
// replay stored response on a hit, or insert a pending record on a miss.
// TODO(M1/M2): implement real idempotency replay.
func Idempotency() gin.HandlerFunc {
	return func(c *gin.Context) {
		// TODO(M1/M2): read Idempotency-Key, check idempotency_keys table.
		c.Next()
	}
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
