package handlers

import "github.com/gin-gonic/gin"

// ProvisionSpace handles POST /merchants/{merchantId}/channels (FR-1, M2).
// TODO(M2): validate request, persist desired state + outbox row, enqueue provision_space job, return 202.
func (h *Handlers) ProvisionSpace(c *gin.Context) {
	notImplemented(c)
}

// ListSpaces handles GET /channels (FR-10, M2/M4).
// TODO(M2): serve from Valkey cache with fall-through to Postgres.
func (h *Handlers) ListSpaces(c *gin.Context) {
	notImplemented(c)
}

// GetSpace handles GET /channels/{id} (FR-10, M2).
// TODO(M2): serve from Valkey cache with fall-through to Postgres.
func (h *Handlers) GetSpace(c *gin.Context) {
	notImplemented(c)
}

// ListSpaceMembers handles GET /channels/{id}/members (FR-17, M3).
// TODO(M3): list space_members rows for the given space.
func (h *Handlers) ListSpaceMembers(c *gin.Context) {
	notImplemented(c)
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
