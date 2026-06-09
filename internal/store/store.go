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
// oauth_tokens upsert/get. M2+ adds spaces and jobs.
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

	// GetSpaceByID returns the space for the given id.
	// TODO(M2): implement
	GetSpaceByID(ctx context.Context, id string) (*domain.Space, error)

	// --- Jobs ---

	// CreateJob inserts a jobs mirror row.
	// TODO(M2): implement
	CreateJob(ctx context.Context, p CreateJobParams) (*domain.Job, error)

	// GetJobByID returns the job for the given id (Postgres mirror of asynq state).
	// TODO(M2): implement
	GetJobByID(ctx context.Context, id string) (*domain.Job, error)
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

// --- Sentinel errors ---

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errorString("store: not found")

// ErrConflict is returned when an insert would violate a unique constraint.
var ErrConflict = errorString("store: conflict")

type errorString string

func (e errorString) Error() string { return string(e) }
