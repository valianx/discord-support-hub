// Package store defines the storage interface and its adapters (NFR-8 pluggable storage).
// All database access goes through the Store interface so implementations can be swapped.
package store

import (
	"context"
	"time"

	"github.com/valianx/discord-support-hub/internal/domain"
)

// Store is the primary storage abstraction.
// M1 adds: merchants create/get, users create/get/list/deactivate, api_keys lifecycle,
// oauth_tokens upsert/get. M2 adds spaces, jobs, idempotency, and outbox.
type Store interface {
	// Ping checks whether the database is reachable. Used by the readiness probe.
	Ping(ctx context.Context) error

	// --- Merchants ---

	// CreateMerchant inserts a new merchant and returns the created row.
	CreateMerchant(ctx context.Context, p CreateMerchantParams) (*domain.Merchant, error)

	// GetMerchantByID returns the merchant for the given id.
	GetMerchantByID(ctx context.Context, id string) (*domain.Merchant, error)

	// --- Users ---

	// CreateUser inserts a new user (agent or collaborator).
	// Returns ErrConflict when a user with the same discord_user_id already exists.
	CreateUser(ctx context.Context, p CreateUserParams) (*domain.User, error)

	// GetUserByID returns the user for the given hub id.
	GetUserByID(ctx context.Context, id string) (*domain.User, error)

	// GetUserByDiscordID returns the user linked to a given Discord user id.
	// Returns ErrNotFound when no row matches.
	GetUserByDiscordID(ctx context.Context, discordUserID string) (*domain.User, error)

	// ListAgents returns all users of type=agent, active or not depending on includeInactive.
	ListAgents(ctx context.Context, includeInactive bool) ([]*domain.User, error)

	// DeactivateUser marks a user inactive (is_active = false) and returns the updated row.
	// Returns ErrNotFound when the user does not exist.
	DeactivateUser(ctx context.Context, id string) (*domain.User, error)

	// SetUserProvisionedAt records the timestamp at which the user was added to the guild
	// and the Agent role was projected (or overwrite applied for a collaborator).
	SetUserProvisionedAt(ctx context.Context, id string) (*domain.User, error)

	// --- API Keys ---

	// CreateAPIKey inserts an api_keys row (stores only the hash, never the raw key).
	CreateAPIKey(ctx context.Context, p CreateAPIKeyParams) (*domain.APIKey, error)

	// ListAPIKeys returns all api_keys rows for auditing. Active-only can be filtered with activeOnly.
	ListAPIKeys(ctx context.Context, activeOnly bool) ([]*domain.APIKey, error)

	// LookupActiveAPIKeyByHash returns the api_keys row whose key_hash matches the provided
	// SHA-256 hash and has not been revoked. Returns ErrNotFound on a miss.
	LookupActiveAPIKeyByHash(ctx context.Context, hash []byte) (*domain.APIKey, error)

	// RevokeAPIKey sets revoked_at on the named key. Returns ErrNotFound when absent.
	RevokeAPIKey(ctx context.Context, id string) error

	// TouchAPIKeyLastUsed updates last_used_at to now for the given key id.
	TouchAPIKeyLastUsed(ctx context.Context, id string) error

	// --- OAuth Tokens ---

	// UpsertOAuthToken stores or replaces an encrypted OAuth token for a user.
	// Replaces an existing row on conflict (UNIQUE user_id).
	UpsertOAuthToken(ctx context.Context, p UpsertOAuthTokenParams) (*domain.OAuthToken, error)

	// GetOAuthTokenByUserID returns the current encrypted token for a user.
	// Returns ErrNotFound when no token has been stored yet.
	GetOAuthTokenByUserID(ctx context.Context, userID string) (*domain.OAuthToken, error)

	// --- Spaces ---

	// CreateSpace inserts a new spaces row (desired state before worker provisions it).
	// Returns ErrConflict when a space for the merchant already exists (1:1 invariant).
	CreateSpace(ctx context.Context, p CreateSpaceParams) (*domain.Space, error)

	// GetSpaceByID returns the space for the given id.
	GetSpaceByID(ctx context.Context, id string) (*domain.Space, error)

	// GetSpaceByMerchantID returns the single space owned by a merchant.
	// Returns ErrNotFound when no space has been provisioned yet.
	GetSpaceByMerchantID(ctx context.Context, merchantID string) (*domain.Space, error)

	// UpdateSpaceDiscordChannel stamps discord_channel_id and acl_state on the space
	// after the worker has provisioned the Discord channel.
	UpdateSpaceDiscordChannel(ctx context.Context, p UpdateSpaceDiscordChannelParams) (*domain.Space, error)

	// UpdateSpaceACLState updates the acl_state of a space (e.g. applied → degraded).
	UpdateSpaceACLState(ctx context.Context, spaceID string, state domain.ACLState) (*domain.Space, error)

	// --- Jobs ---

	// CreateJob inserts a jobs mirror row and returns the created row.
	CreateJob(ctx context.Context, p CreateJobParams) (*domain.Job, error)

	// GetJobByID returns the job for the given id (Postgres mirror of asynq state).
	// Returns ErrNotFound when no row exists for that id.
	GetJobByID(ctx context.Context, id string) (*domain.Job, error)

	// UpdateJobStatus transitions the jobs row status and optionally sets error/completed_at.
	UpdateJobStatus(ctx context.Context, p UpdateJobStatusParams) (*domain.Job, error)

	// --- Idempotency ---

	// InsertIdempotencyKey attempts an atomic insert of an idempotency_keys row.
	// Returns ErrConflict when a row with that key already exists (caller should replay).
	InsertIdempotencyKey(ctx context.Context, p InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error)

	// GetIdempotencyKey returns the stored record for the given key.
	// Returns ErrNotFound when the key is not present (or has expired).
	GetIdempotencyKey(ctx context.Context, key string) (*domain.IdempotencyKey, error)

	// UpdateIdempotencyKeyResponse stores the final response on a pending record.
	UpdateIdempotencyKeyResponse(ctx context.Context, p UpdateIdempotencyKeyResponseParams) error

	// --- Outbox ---

	// CreateSpaceWithOutbox writes the desired-state Space row AND an outbox row in
	// one Postgres transaction. This guarantees the committed change is never lost
	// before the relay enqueues the job (NFR-3 transactional outbox, §4).
	CreateSpaceWithOutbox(ctx context.Context, sp CreateSpaceParams, ob CreateOutboxParams) (*domain.Space, *domain.OutboxRow, error)

	// ListPendingOutbox returns up to limit outbox rows not yet enqueued (enqueued_at IS NULL).
	ListPendingOutbox(ctx context.Context, limit int) ([]*domain.OutboxRow, error)

	// StampOutboxEnqueued marks outbox rows as enqueued by setting enqueued_at.
	StampOutboxEnqueued(ctx context.Context, ids []string) error

	// UpdateOutboxPayload replaces the payload on an outbox row identified by its
	// idempotency_key. Called immediately after CreateSpaceWithOutbox to inject the
	// space_id (which is only known after the transaction commits) into the relay payload
	// so the worker's GetSpaceByID call resolves to the correct space (fix DEFECT-2).
	// Only updates rows that have not yet been enqueued (enqueued_at IS NULL).
	UpdateOutboxPayload(ctx context.Context, idempotencyKey string, payload map[string]any) error

	// --- Audit log ---

	// InsertAuditEntry appends an audit_log row.
	// No secrets may appear in entry.Detail (NFR-6, FR-14).
	InsertAuditEntry(ctx context.Context, entry InsertAuditEntryParams) error

	// --- Spaces list ---

	// ListSpaces returns spaces ordered by created_at asc with optional filters and
	// cursor-based pagination. Limit of 0 uses the default page size (50).
	ListSpaces(ctx context.Context, p ListSpacesParams) ([]*domain.Space, error)
}

// --- Parameter types ---

// CreateMerchantParams carries validated fields for creating a merchant.
type CreateMerchantParams struct {
	ExternalRef string
	Name        string
	HelpDeskURL *string
}

// CreateUserParams carries validated fields for creating a user.
type CreateUserParams struct {
	Type          domain.UserType
	IsAdmin       bool
	DiscordUserID *string
	Email         *string
	DisplayName   *string
}

// CreateAPIKeyParams carries the name, hash, and scope for a new api_keys row.
type CreateAPIKeyParams struct {
	Name    string
	KeyHash []byte
	Scope   string
}

// UpsertOAuthTokenParams carries the encrypted token fields for upserting.
type UpsertOAuthTokenParams struct {
	UserID               string
	AccessTokenCipher    []byte
	AccessTokenNonce     []byte
	RefreshTokenCipher   []byte // nil if not present
	RefreshTokenNonce    []byte // nil if not present
	EncryptionKeyVersion int
	Scopes               string
	ExpiresAt            *time.Time
}

// CreateSpaceParams carries fields for creating a new desired-state space row.
type CreateSpaceParams struct {
	MerchantID        string
	Name              string
	DiscordCategoryID *string
	WelcomeMessage    *string
}

// UpdateSpaceDiscordChannelParams carries the result of the worker's provisioning call.
type UpdateSpaceDiscordChannelParams struct {
	SpaceID           string
	DiscordChannelID  string
	DiscordCategoryID *string
	ACLState          domain.ACLState
}

// CreateJobParams carries the fields for creating a Postgres jobs mirror row.
type CreateJobParams struct {
	TaskID     string
	Kind       string
	Queue      string
	MerchantID *string
	SpaceID    *string
	UserID     *string
	Payload    map[string]any
}

// UpdateJobStatusParams carries the transition fields for a job row.
type UpdateJobStatusParams struct {
	JobID      string
	Status     domain.JobStatus
	Error      *string
	RetryCount *int
	Completed  bool // when true, sets completed_at = now()
}

// InsertIdempotencyKeyParams carries the fields for inserting an idempotency_keys row.
type InsertIdempotencyKeyParams struct {
	Key         string
	RequestHash []byte
	JobID       *string
	ExpiresAt   time.Time
}

// UpdateIdempotencyKeyResponseParams carries the stored response fields.
type UpdateIdempotencyKeyResponseParams struct {
	Key          string
	Status       domain.JobStatus
	ResponseCode int
	ResponseBody map[string]any
	JobID        *string
}

// InsertAuditEntryParams carries the fields for an audit_log row.
// Never include raw tokens or secret values in Detail (NFR-6).
type InsertAuditEntryParams struct {
	ActorAPIKeyID *string
	ActorUserID   *string
	Action        string
	MerchantID    *string
	SpaceID       *string
	TargetUserID  *string
	Detail        map[string]any
}

// ListSpacesParams carries filter and pagination parameters for ListSpaces.
type ListSpacesParams struct {
	// LifecycleState filters to a specific lifecycle state when non-empty.
	LifecycleState *domain.SpaceLifecycleState
	// MerchantID filters to a specific merchant when non-empty.
	MerchantID *string
	// Cursor is the last seen created_at value (ISO-8601) for page continuation.
	Cursor *string
	// Limit is the maximum number of rows to return. 0 = default (50).
	Limit int
}

// CreateOutboxParams carries the fields for inserting an outbox row.
type CreateOutboxParams struct {
	Aggregate      string
	AggregateID    string
	Kind           string
	Payload        map[string]any
	IdempotencyKey string
}

// --- Sentinel errors ---

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errorString("store: not found")

// ErrConflict is returned when an insert would violate a unique constraint.
var ErrConflict = errorString("store: conflict")

type errorString string

func (e errorString) Error() string { return string(e) }
