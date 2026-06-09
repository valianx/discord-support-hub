package handlers

import "github.com/gin-gonic/gin"

// InviteCollaborator handles POST /channels/{id}/collaborators (FR-4, M3).
// Only Agents/Admins may invite (FR-20, Layer B). No invite links (NFR-14).
// TODO(M3): persist space_member, enqueue invite_collaborator job, return 202 + connect_url.
func (h *Handlers) InviteCollaborator(c *gin.Context) {
	notImplemented(c)
}

// ExpelCollaborator handles DELETE /channels/{id}/collaborators/{userId} (FR-19, M3).
// scope=channel (default) revokes overwrite; scope=server also removes from guild.
// TODO(M3): persist expulsion, enqueue expel_collaborator job, return 202.
func (h *Handlers) ExpelCollaborator(c *gin.Context) {
	notImplemented(c)
}

// ListCollaboratorChannels handles GET /collaborators/{userId}/channels (FR-21, M3).
// TODO(M3): query space_members joined to spaces for the given user.
func (h *Handlers) ListCollaboratorChannels(c *gin.Context) {
	notImplemented(c)
}
