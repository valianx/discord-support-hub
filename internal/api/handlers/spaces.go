// Package handlers contains one Gin handler per OpenAPI operationId.
// All handlers in M0 return 501 Not Implemented with the contract Error shape.
// Real implementations land in M2 (provisioning), M3 (membership), M4 (lifecycle).
package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// notImplemented returns the contract Error shape with HTTP 501.
func notImplemented(c *gin.Context) {
	c.JSON(http.StatusNotImplemented, gin.H{
		"code":    "not_implemented",
		"message": "this endpoint is not yet implemented",
	})
}

// ProvisionSpace handles POST /merchants/{merchantId}/channels (FR-1, M2).
// TODO(M2): validate request, persist desired state + outbox row, enqueue provision_space job, return 202.
func ProvisionSpace(c *gin.Context) {
	notImplemented(c)
}

// ListSpaces handles GET /channels (FR-10, M2/M4).
// TODO(M2): serve from Valkey cache with fall-through to Postgres.
func ListSpaces(c *gin.Context) {
	notImplemented(c)
}

// GetSpace handles GET /channels/{id} (FR-10, M2).
// TODO(M2): serve from Valkey cache with fall-through to Postgres.
func GetSpace(c *gin.Context) {
	notImplemented(c)
}

// ListSpaceMembers handles GET /channels/{id}/members (FR-17, M3).
// TODO(M3): list space_members rows for the given space.
func ListSpaceMembers(c *gin.Context) {
	notImplemented(c)
}

// ChangeSpaceLifecycle handles POST /channels/{id}/lifecycle (FR-7, M4).
// TODO(M4): validate transition, enqueue change_lifecycle job, return 202.
func ChangeSpaceLifecycle(c *gin.Context) {
	notImplemented(c)
}

// SyncWelcome handles POST /channels/{id}/welcome:sync (FR-15 static, M4).
// TODO(M4): enqueue sync_welcome job (set topic + pin), return 202.
func SyncWelcome(c *gin.Context) {
	notImplemented(c)
}
