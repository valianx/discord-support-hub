package handlers

import "github.com/gin-gonic/gin"

// ListAgents handles GET /agents (FR-23, M1). Admin only.
// TODO(M1): query users where type='agent'; enforce Layer B Admin check.
func ListAgents(c *gin.Context) {
	notImplemented(c)
}

// AddAgent handles POST /agents (FR-23, M1). Admin only. Synchronous 201.
// TODO(M1): persist type=agent user, return 201 + connect_url.
func AddAgent(c *gin.Context) {
	notImplemented(c)
}

// RemoveAgent handles DELETE /agents/{userId} (FR-23, M1). Admin only. Async 202.
// TODO(M1): mark agent inactive, enqueue project_agent_role(add=false) job, return 202.
func RemoveAgent(c *gin.Context) {
	notImplemented(c)
}
