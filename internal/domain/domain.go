// Package domain contains pure business entities matching the data model.
// This package MUST NOT import any infrastructure packages (store, queue, discord, etc.).
// The import-boundary test enforces this constraint (AC-3).
package domain

import "time"

// UserType classifies a user as either an internal agent or an external collaborator.
type UserType string

const (
	UserTypeAgent        UserType = "agent"
	UserTypeCollaborator UserType = "collaborator"
)

// SpaceLifecycleState represents the lifecycle of a merchant's support space (FR-7).
type SpaceLifecycleState string

const (
	SpaceLifecycleActive   SpaceLifecycleState = "active"
	SpaceLifecycleResolved SpaceLifecycleState = "resolved"
	SpaceLifecycleArchived SpaceLifecycleState = "archived"
)

// ACLState tracks the fail-closed ACL projection status (NFR-4).
// A space is only treated as accessible when acl_state = ACLStateApplied.
type ACLState string

const (
	ACLStatePending  ACLState = "pending"
	ACLStateApplied  ACLState = "applied"
	ACLStateDegraded ACLState = "degraded"
	ACLStateFailed   ACLState = "failed"
)

// SpaceMemberRole is the only role a collaborator can hold within a space.
// Agents are not listed in space_members; they access all spaces via the category-level role.
type SpaceMemberRole string

const SpaceMemberRoleCollaborator SpaceMemberRole = "collaborator"

// JobStatus mirrors asynq task state in Postgres so callers can poll authoritatively.
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusActive    JobStatus = "active"
	JobStatusCompleted JobStatus = "completed"
	JobStatusRetrying  JobStatus = "retrying"
	JobStatusArchived  JobStatus = "archived"
)

// ExpulsionScope controls how far a collaborator removal reaches (FR-19).
type ExpulsionScope string

const (
	ExpulsionScopeChannel ExpulsionScope = "channel" // revoke overwrite only (default)
	ExpulsionScopeServer  ExpulsionScope = "server"  // also remove from guild
)

// Merchant is a customer that owns exactly one support space (1:1, enforced by UNIQUE).
// Collaborators are NOT owned by a merchant; their tenant associations derive from space_members.
// The per-merchant invite-with-role link is stored here (operator-created, reusable for all
// collaborators of this merchant; nil until the operator stores it via PUT /merchants/{id}/invite).
type Merchant struct {
	ID               string
	ExternalRef      string
	Name             string
	HelpDeskURL      *string
	InviteLink       *string    // native Discord invite-with-role URL; nil blocks :send-invite
	InviteLinkSetAt  *time.Time // timestamp of last PUT /merchants/{id}/invite
	IsActive         bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// User is an identity in the roster — either an internal agent or an external collaborator.
// A user is NOT bound to a merchant by column; a collaborator's merchant associations derive
// from space membership (space_members -> spaces -> merchant), reflecting that one collaborator
// may hold access across several merchants' spaces. Authorization is always resolved against
// this table, never against the Discord role (NFR-13).
type User struct {
	ID            string
	Type          UserType
	IsAdmin       bool // meaningful only for agents; enforced by DB CHECK constraint
	DiscordUserID *string
	Email         *string
	DisplayName   *string
	ProvisionedAt *time.Time
	IsActive      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Space is the private support channel per merchant (channel mode only in v1, FR-2).
// The 1:1 merchant↔space constraint is enforced by UNIQUE(merchant_id) on the spaces table.
// MerchantRoleID is the Discord role auto-created on provision; the channel allow grants
// it VIEW+SEND. Collaborators acquire it via the merchant's stored invite-with-role link.
type Space struct {
	ID                string
	MerchantID        string
	DiscordChannelID  *string // nil until the worker provisions the channel
	DiscordCategoryID *string
	MerchantRoleID    *string // nil until the provision worker creates the merchant role
	Name              string
	LifecycleState    SpaceLifecycleState
	ACLState          ACLState // fail-closed: only "applied" is accessible
	WelcomeMessageID  *string
	LastActivityAt    *time.Time
	ReconciledAt      *time.Time
	DriftCount        int
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ArchivedAt        *time.Time
}

// SpaceMember records a collaborator's desired access to one space (FR-3, FR-4).
// Access is role-based: the collaborator acquires the merchant role via the stored
// invite-with-role link. The reconciler diffs role membership against these rows (NFR-5).
type SpaceMember struct {
	ID              string
	SpaceID         string
	UserID          string
	Role            SpaceMemberRole
	InviteSentAt    *time.Time // stamped by the notify worker on successful SMTP send
	RoleObservedAt  *time.Time // optional: set when the reconciler/console observes the role
	InvitedBy       *string
	CreatedAt       time.Time
	RevokedAt       *time.Time
}

// AuditEntry is an append-only record of provisioning, membership, lifecycle, and expulsion
// actions (FR-14). Stores references (user id, action), never raw tokens.
type AuditEntry struct {
	ID            int64
	ActorAPIKeyID *string
	ActorUserID   *string
	Action        string
	MerchantID    *string
	SpaceID       *string
	TargetUserID  *string
	Scope         *ExpulsionScope
	Detail        map[string]any
	CreatedAt     time.Time
}

// APIKey is a service bearer credential (Layer A). Only the hash is persisted;
// the raw key is shown once at creation and never stored (§5.1).
type APIKey struct {
	ID         string
	Name       string
	KeyHash    []byte // SHA-256 of the raw key
	Scope      string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// IsActive reports whether the key has not been revoked.
func (k *APIKey) IsActive() bool {
	return k.RevokedAt == nil
}

// Job mirrors asynq task state in Postgres so callers can poll an authoritative source
// without reaching into Valkey (which is never source of truth).
type Job struct {
	ID          string
	TaskID      string
	Kind        string
	Queue       string
	Status      JobStatus
	MerchantID  *string
	SpaceID     *string
	UserID      *string
	Payload     map[string]any
	Error       *string
	RetryCount  int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// IdempotencyKey is an edge-level idempotency record for mutating requests (NFR-3).
// When a key already exists with a stored response, the API replays that response
// without re-enqueueing a second job. If the key exists with a different body hash,
// the API returns 409.
type IdempotencyKey struct {
	Key          string
	RequestHash  []byte
	Status       JobStatus
	ResponseCode *int
	ResponseBody map[string]any
	JobID        *string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// OutboxRow is a single entry in the transactional outbox table. The API writes the
// desired-state change AND an outbox row in one Postgres transaction. The relay picks
// up pending rows, enqueues the asynq task, and stamps enqueued_at.
type OutboxRow struct {
	ID             string
	Aggregate      string
	AggregateID    string
	Kind           string
	Payload        map[string]any
	IdempotencyKey string
	EnqueuedAt     *time.Time
	CreatedAt      time.Time
}
