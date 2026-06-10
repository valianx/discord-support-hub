// Package queue wraps the asynq client and defines task kind constants, payload structs,
// and helper functions for building tasks with idempotency keys (NFR-3).
package queue

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
)

// Queue names match the topology defined in docs/02-architecture.md §3.4.
const (
	QueueProvision  = "provision"  // high priority: space creation, ACL apply
	QueueMembership = "membership" // high priority: role assign/remove
	QueueReconcile  = "reconcile"  // low  priority: drift detection + repair
	QueueMarking    = "marking"    // low  priority: optional nickname suffix (M4)
	QueueNotify     = "notify"     // default priority: invite emails (AC-M6-5, AC-M6-6)
)

// Task kind constants used as asynq task type strings.
// Each kind maps to exactly one handler registered on the worker.
const (
	KindProvisionSpace      = "space:provision"
	KindInviteCollaborator  = "membership:invite_collaborator"
	KindExpelCollaborator   = "membership:expel_collaborator"
	KindProjectAgentRole    = "membership:project_agent_role"
	KindChangeLifecycle     = "space:change_lifecycle"
	KindReconcileGuild      = "reconcile:guild"
	KindReconcileSpace      = "reconcile:space"
	KindSyncWelcome         = "space:sync_welcome"
	KindApplyNicknameSuffix = "marking:apply_nickname_suffix"
	KindSendInvite          = "notify:send_invite" // AC-M6-5: send invite email via notify queue
)

// ProvisionSpacePayload is the task payload for KindProvisionSpace.
type ProvisionSpacePayload struct {
	MerchantID     string `json:"merchant_id"`
	SpaceID        string `json:"space_id"`
	SpaceName      string `json:"space_name"`
	CategoryID     string `json:"category_id,omitempty"`
	WelcomeMessage string `json:"welcome_message,omitempty"`
}

// InviteCollaboratorPayload is the task payload for KindInviteCollaborator.
type InviteCollaboratorPayload struct {
	SpaceID   string `json:"space_id"`
	UserID    string `json:"user_id"`
	InvitedBy string `json:"invited_by"`
}

// ExpelCollaboratorPayload is the task payload for KindExpelCollaborator.
type ExpelCollaboratorPayload struct {
	SpaceID string `json:"space_id"`
	UserID  string `json:"user_id"`
	Scope   string `json:"scope"` // "channel" or "server"
}

// ProjectAgentRolePayload is the task payload for KindProjectAgentRole.
type ProjectAgentRolePayload struct {
	UserID string `json:"user_id"`
	Add    bool   `json:"add"` // true = assign role, false = remove role
}

// ChangeLifecyclePayload is the task payload for KindChangeLifecycle.
type ChangeLifecyclePayload struct {
	SpaceID string `json:"space_id"`
	Action  string `json:"action"` // "open", "resolve", "archive", "reopen"
}

// ReconcileSpacePayload is the task payload for KindReconcileSpace.
type ReconcileSpacePayload struct {
	SpaceID string `json:"space_id"`
}

// SyncWelcomePayload is the task payload for KindSyncWelcome.
type SyncWelcomePayload struct {
	SpaceID string `json:"space_id"`
	Message string `json:"message,omitempty"` // override message; empty = use default template
}

// ApplyNicknameSuffixPayload is the task payload for KindApplyNicknameSuffix.
type ApplyNicknameSuffixPayload struct {
	UserID  string `json:"user_id"`
	GuildID string `json:"guild_id"`
	Suffix  string `json:"suffix"`
}

// SendInvitePayload is the task payload for KindSendInvite (AC-M6-5).
// The worker loads the merchant invite link, fetches the user email, sends the SMTP email,
// and stamps space_members.invite_sent_at on success.
type SendInvitePayload struct {
	SpaceMemberID string `json:"space_member_id"` // space_members.id (for StampSpaceMemberInviteSent)
	SpaceID       string `json:"space_id"`
	UserID        string `json:"user_id"`
	MerchantID    string `json:"merchant_id"`
}

// Client wraps the asynq client and provides type-safe task builders.
type Client struct {
	inner *asynq.Client
}

// NewClient creates a Client connected to the Valkey instance at addr.
func NewClient(addr, password string, db int) *Client {
	return &Client{
		inner: asynq.NewClient(asynq.RedisClientOpt{
			Addr:     addr,
			Password: password,
			DB:       db,
		}),
	}
}

// Close closes the underlying asynq client.
func (c *Client) Close() error {
	return c.inner.Close()
}

// Enqueue serializes payload and enqueues a task with the given options.
// Use TaskID + Unique for idempotency (NFR-3, asynq §4.1).
func (c *Client) Enqueue(taskType, queue string, payload any, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("queue: marshal payload: %w", err)
	}

	task := asynq.NewTask(taskType, b)
	opts = append(opts, asynq.Queue(queue))

	info, err := c.inner.Enqueue(task, opts...)
	if err != nil {
		return nil, fmt.Errorf("queue: enqueue %s: %w", taskType, err)
	}
	return info, nil
}

// TaskIDOpt returns an asynq.TaskID option for idempotency-key-based deduplication (NFR-3).
func TaskIDOpt(idempotencyKey string) asynq.Option {
	return asynq.TaskID(idempotencyKey)
}

// UniqueOpt returns an asynq.Unique option that prevents duplicate tasks within ttl.
func UniqueOpt(ttl time.Duration) asynq.Option {
	return asynq.Unique(ttl)
}
