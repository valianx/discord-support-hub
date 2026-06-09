package handlers

import "github.com/gin-gonic/gin"

// GetJob handles GET /jobs/{jobId} (async polling, M2).
// Status is read from the Postgres jobs table — Valkey is never the source of truth.
// TODO(M2): look up jobs row by id, return status.
func GetJob(c *gin.Context) {
	notImplemented(c)
}
