// Package postgres implements the store.Store interface using pgx/pgxpool (NFR-8).
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// pgUniqueViolation is the PostgreSQL SQLSTATE code for unique_violation.
const pgUniqueViolation = "23505"

// Store is the PostgreSQL-backed implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New connects a pgxpool using the provided DSN and returns a Store.
// The caller is responsible for calling Close when the store is no longer needed.
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	// Validate connectivity immediately so the caller knows the DSN is good.
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping on connect: %w", err)
	}

	slog.InfoContext(ctx, "postgres: connected", "dsn_prefix", safeDSNPrefix(dsn))
	return &Store{pool: pool}, nil
}

// Close releases all pooled connections.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool exposes the underlying pgxpool for direct queries in later milestones.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// Ping implements store.Store. Used by the readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// ─── Merchants ────────────────────────────────────────────────────────────────

// CreateMerchant inserts a new merchant and returns the created row.
func (s *Store) CreateMerchant(ctx context.Context, p store.CreateMerchantParams) (*domain.Merchant, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO merchants (external_ref, name, help_desk_url)
		VALUES ($1, $2, $3)
		RETURNING id, external_ref, name, help_desk_url, is_active, created_at, updated_at`,
		p.ExternalRef, p.Name, p.HelpDeskURL,
	)
	return scanMerchant(row)
}

// GetMerchantByID returns the merchant for the given id.
func (s *Store) GetMerchantByID(ctx context.Context, id string) (*domain.Merchant, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, external_ref, name, help_desk_url, is_active, created_at, updated_at
		FROM merchants WHERE id = $1`, id)
	return scanMerchant(row)
}

func scanMerchant(row pgx.Row) (*domain.Merchant, error) {
	var m domain.Merchant
	err := row.Scan(
		&m.ID, &m.ExternalRef, &m.Name, &m.HelpDeskURL,
		&m.IsActive, &m.CreatedAt, &m.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan merchant: %w", err)
	}
	return &m, nil
}

// ─── Users ────────────────────────────────────────────────────────────────────

// CreateUser inserts a new user row.
// Returns store.ErrConflict when discord_user_id already exists (UNIQUE constraint).
func (s *Store) CreateUser(ctx context.Context, p store.CreateUserParams) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO users (type, is_admin, discord_user_id, email, display_name)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, type, is_admin, discord_user_id, email, display_name,
		          provisioned_at, is_active, created_at, updated_at`,
		p.Type, p.IsAdmin, p.DiscordUserID, p.Email, p.DisplayName,
	)
	u, err := scanUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, store.ErrConflict
		}
	}
	return u, err
}

// GetUserByID returns the user for the given hub id.
func (s *Store) GetUserByID(ctx context.Context, id string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, type, is_admin, discord_user_id, email, display_name,
		       provisioned_at, is_active, created_at, updated_at
		FROM users WHERE id = $1`, id)
	return scanUser(row)
}

// GetUserByDiscordID returns the user linked to a Discord user id.
func (s *Store) GetUserByDiscordID(ctx context.Context, discordUserID string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, type, is_admin, discord_user_id, email, display_name,
		       provisioned_at, is_active, created_at, updated_at
		FROM users WHERE discord_user_id = $1`, discordUserID)
	return scanUser(row)
}

// ListAgents returns all users of type=agent.
func (s *Store) ListAgents(ctx context.Context, includeInactive bool) ([]*domain.User, error) {
	query := `SELECT id, type, is_admin, discord_user_id, email, display_name,
	                 provisioned_at, is_active, created_at, updated_at
	          FROM users WHERE type = 'agent'`
	if !includeInactive {
		query += " AND is_active = TRUE"
	}
	query += " ORDER BY created_at ASC"

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres: list agents: %w", err)
	}
	defer rows.Close()

	var users []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// DeactivateUser marks a user inactive and returns the updated row.
func (s *Store) DeactivateUser(ctx context.Context, id string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE users SET is_active = FALSE, updated_at = now()
		WHERE id = $1
		RETURNING id, type, is_admin, discord_user_id, email, display_name,
		          provisioned_at, is_active, created_at, updated_at`, id)
	return scanUser(row)
}

// SetUserProvisionedAt stamps the provisioned_at timestamp on the user row.
func (s *Store) SetUserProvisionedAt(ctx context.Context, id string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE users SET provisioned_at = now(), updated_at = now()
		WHERE id = $1
		RETURNING id, type, is_admin, discord_user_id, email, display_name,
		          provisioned_at, is_active, created_at, updated_at`, id)
	return scanUser(row)
}

// UpdateDiscordUserID links a Discord user id to a hub user (OAuth2 connect flow).
// Returns store.ErrConflict when another hub user already holds that discord_user_id
// (users.discord_user_id is UNIQUE — one Discord identity per hub user).
// Returns store.ErrNotFound when the hub user row does not exist.
func (s *Store) UpdateDiscordUserID(ctx context.Context, userID, discordUserID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE users SET discord_user_id = $1, updated_at = now()
		WHERE id = $2`, discordUserID, userID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return store.ErrConflict
		}
		return fmt.Errorf("postgres: update discord_user_id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// scanUser scans a single users row from any pgx scanner (pgx.Row or pgx.Rows).
func scanUser(row interface {
	Scan(dest ...any) error
}) (*domain.User, error) {
	var u domain.User
	err := row.Scan(
		&u.ID, &u.Type, &u.IsAdmin, &u.DiscordUserID, &u.Email, &u.DisplayName,
		&u.ProvisionedAt, &u.IsActive, &u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan user: %w", err)
	}
	return &u, nil
}

// ─── API Keys ─────────────────────────────────────────────────────────────────

// CreateAPIKey inserts an api_keys row (hash only, never raw key).
// Returns store.ErrConflict when the same key hash already exists.
func (s *Store) CreateAPIKey(ctx context.Context, p store.CreateAPIKeyParams) (*domain.APIKey, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (name, key_hash, scope)
		VALUES ($1, $2, $3)
		RETURNING id, name, key_hash, scope, created_at, last_used_at, revoked_at`,
		p.Name, p.KeyHash, p.Scope,
	)
	k, err := scanAPIKey(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, store.ErrConflict
		}
	}
	return k, err
}

// ListAPIKeys returns api_keys rows.
func (s *Store) ListAPIKeys(ctx context.Context, activeOnly bool) ([]*domain.APIKey, error) {
	query := `SELECT id, name, key_hash, scope, created_at, last_used_at, revoked_at FROM api_keys`
	if activeOnly {
		query += " WHERE revoked_at IS NULL"
	}
	query += " ORDER BY created_at ASC"

	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("postgres: list api_keys: %w", err)
	}
	defer rows.Close()

	var keys []*domain.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// LookupActiveAPIKeyByHash finds an active (non-revoked) key by its SHA-256 hash.
func (s *Store) LookupActiveAPIKeyByHash(ctx context.Context, hash []byte) (*domain.APIKey, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, key_hash, scope, created_at, last_used_at, revoked_at
		FROM api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL`, hash)
	return scanAPIKey(row)
}

// RevokeAPIKey sets revoked_at on the named key.
func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("postgres: revoke api_key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// TouchAPIKeyLastUsed bumps last_used_at to now.
func (s *Store) TouchAPIKeyLastUsed(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("postgres: touch api_key last_used: %w", err)
	}
	return nil
}

func scanAPIKey(row interface {
	Scan(dest ...any) error
}) (*domain.APIKey, error) {
	var k domain.APIKey
	err := row.Scan(
		&k.ID, &k.Name, &k.KeyHash, &k.Scope,
		&k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan api_key: %w", err)
	}
	return &k, nil
}

// ─── OAuth Tokens ─────────────────────────────────────────────────────────────

// UpsertOAuthToken stores or replaces an encrypted OAuth token (UNIQUE user_id).
func (s *Store) UpsertOAuthToken(ctx context.Context, p store.UpsertOAuthTokenParams) (*domain.OAuthToken, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO oauth_tokens (
			user_id, access_token_cipher, access_token_nonce,
			refresh_token_cipher, refresh_token_nonce,
			encryption_key_version, scopes, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (user_id) DO UPDATE SET
			access_token_cipher    = EXCLUDED.access_token_cipher,
			access_token_nonce     = EXCLUDED.access_token_nonce,
			refresh_token_cipher   = EXCLUDED.refresh_token_cipher,
			refresh_token_nonce    = EXCLUDED.refresh_token_nonce,
			encryption_key_version = EXCLUDED.encryption_key_version,
			scopes                 = EXCLUDED.scopes,
			expires_at             = EXCLUDED.expires_at,
			updated_at             = now()
		RETURNING id, user_id, access_token_cipher, access_token_nonce,
		          refresh_token_cipher, refresh_token_nonce,
		          encryption_key_version, scopes, expires_at, created_at, updated_at`,
		p.UserID,
		p.AccessTokenCipher, p.AccessTokenNonce,
		p.RefreshTokenCipher, p.RefreshTokenNonce,
		p.EncryptionKeyVersion, p.Scopes, p.ExpiresAt,
	)
	return scanOAuthToken(row)
}

// GetOAuthTokenByUserID returns the stored encrypted token for a user.
func (s *Store) GetOAuthTokenByUserID(ctx context.Context, userID string) (*domain.OAuthToken, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, user_id, access_token_cipher, access_token_nonce,
		       refresh_token_cipher, refresh_token_nonce,
		       encryption_key_version, scopes, expires_at, created_at, updated_at
		FROM oauth_tokens WHERE user_id = $1`, userID)
	return scanOAuthToken(row)
}

func scanOAuthToken(row interface {
	Scan(dest ...any) error
}) (*domain.OAuthToken, error) {
	var t domain.OAuthToken
	err := row.Scan(
		&t.ID, &t.UserID,
		&t.AccessTokenCipher, &t.AccessTokenNonce,
		&t.RefreshTokenCipher, &t.RefreshTokenNonce,
		&t.EncryptionKeyVersion, &t.Scopes, &t.ExpiresAt,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan oauth_token: %w", err)
	}
	return &t, nil
}

// ─── Spaces ───────────────────────────────────────────────────────────────────

// CreateSpace inserts a new desired-state space row.
func (s *Store) CreateSpace(ctx context.Context, p store.CreateSpaceParams) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO spaces (merchant_id, name, discord_category_id)
		VALUES ($1, $2, $3)
		RETURNING id, merchant_id, discord_channel_id, discord_category_id, name,
		          lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		          reconciled_at, drift_count, created_at, updated_at, archived_at`,
		p.MerchantID, p.Name, p.DiscordCategoryID,
	)
	sp, err := scanSpace(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return sp, nil
}

// GetSpaceByID returns the space for the given id.
func (s *Store) GetSpaceByID(ctx context.Context, id string) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, merchant_id, discord_channel_id, discord_category_id, name,
		       lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		       reconciled_at, drift_count, created_at, updated_at, archived_at
		FROM spaces WHERE id = $1`, id)
	return scanSpace(row)
}

// GetSpaceByMerchantID returns the single space owned by the merchant.
func (s *Store) GetSpaceByMerchantID(ctx context.Context, merchantID string) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, merchant_id, discord_channel_id, discord_category_id, name,
		       lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		       reconciled_at, drift_count, created_at, updated_at, archived_at
		FROM spaces WHERE merchant_id = $1`, merchantID)
	return scanSpace(row)
}

// UpdateSpaceDiscordChannel stamps the discord_channel_id and acl_state after provisioning.
func (s *Store) UpdateSpaceDiscordChannel(ctx context.Context, p store.UpdateSpaceDiscordChannelParams) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE spaces
		SET discord_channel_id = $1, discord_category_id = COALESCE($2, discord_category_id),
		    acl_state = $3, updated_at = now()
		WHERE id = $4
		RETURNING id, merchant_id, discord_channel_id, discord_category_id, name,
		          lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		          reconciled_at, drift_count, created_at, updated_at, archived_at`,
		p.DiscordChannelID, p.DiscordCategoryID, p.ACLState, p.SpaceID,
	)
	return scanSpace(row)
}

// UpdateSpaceACLState transitions the acl_state of a space.
func (s *Store) UpdateSpaceACLState(ctx context.Context, spaceID string, state domain.ACLState) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE spaces SET acl_state = $1, updated_at = now()
		WHERE id = $2
		RETURNING id, merchant_id, discord_channel_id, discord_category_id, name,
		          lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		          reconciled_at, drift_count, created_at, updated_at, archived_at`,
		state, spaceID,
	)
	return scanSpace(row)
}

func scanSpace(row interface{ Scan(dest ...any) error }) (*domain.Space, error) {
	var sp domain.Space
	err := row.Scan(
		&sp.ID, &sp.MerchantID, &sp.DiscordChannelID, &sp.DiscordCategoryID, &sp.Name,
		&sp.LifecycleState, &sp.ACLState, &sp.WelcomeMessageID, &sp.LastActivityAt,
		&sp.ReconciledAt, &sp.DriftCount, &sp.CreatedAt, &sp.UpdatedAt, &sp.ArchivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan space: %w", err)
	}
	return &sp, nil
}

// ─── Jobs ─────────────────────────────────────────────────────────────────────

// CreateJob inserts a Postgres jobs mirror row.
func (s *Store) CreateJob(ctx context.Context, p store.CreateJobParams) (*domain.Job, error) {
	payloadJSON, err := marshalPayload(p.Payload)
	if err != nil {
		return nil, err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (task_id, kind, queue, merchant_id, space_id, user_id, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, task_id, kind, queue, status, merchant_id, space_id, user_id,
		          payload, error, retry_count, created_at, updated_at, completed_at`,
		p.TaskID, p.Kind, p.Queue, p.MerchantID, p.SpaceID, p.UserID, payloadJSON,
	)
	return scanJob(row)
}

// GetJobByID returns the job for the given id (Postgres mirror of asynq state).
func (s *Store) GetJobByID(ctx context.Context, id string) (*domain.Job, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, task_id, kind, queue, status, merchant_id, space_id, user_id,
		       payload, error, retry_count, created_at, updated_at, completed_at
		FROM jobs WHERE id = $1`, id)
	return scanJob(row)
}

// UpdateJobStatus transitions the job row to a new status.
func (s *Store) UpdateJobStatus(ctx context.Context, p store.UpdateJobStatusParams) (*domain.Job, error) {
	var completedAt *string // let Postgres handle the timestamp via CASE
	query := `
		UPDATE jobs SET
			status      = $1,
			error       = COALESCE($2, error),
			retry_count = COALESCE($3, retry_count),
			completed_at = CASE WHEN $4 THEN now() ELSE completed_at END,
			updated_at  = now()
		WHERE id = $5
		RETURNING id, task_id, kind, queue, status, merchant_id, space_id, user_id,
		          payload, error, retry_count, created_at, updated_at, completed_at`
	_ = completedAt
	row := s.pool.QueryRow(ctx, query,
		p.Status, p.Error, p.RetryCount, p.Completed, p.JobID,
	)
	return scanJob(row)
}

func scanJob(row interface{ Scan(dest ...any) error }) (*domain.Job, error) {
	var j domain.Job
	var payloadRaw []byte
	err := row.Scan(
		&j.ID, &j.TaskID, &j.Kind, &j.Queue, &j.Status,
		&j.MerchantID, &j.SpaceID, &j.UserID,
		&payloadRaw, &j.Error, &j.RetryCount,
		&j.CreatedAt, &j.UpdatedAt, &j.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan job: %w", err)
	}
	if len(payloadRaw) > 0 {
		if err = unmarshalPayload(payloadRaw, &j.Payload); err != nil {
			return nil, err
		}
	}
	return &j, nil
}

// ─── Idempotency ──────────────────────────────────────────────────────────────

// InsertIdempotencyKey performs an atomic INSERT into idempotency_keys.
// Returns ErrConflict when the key already exists (caller must replay the stored response).
func (s *Store) InsertIdempotencyKey(ctx context.Context, p store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO idempotency_keys (key, request_hash, job_id, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING key, request_hash, status, response_code, response_body, job_id, created_at, expires_at`,
		p.Key, p.RequestHash, p.JobID, p.ExpiresAt,
	)
	ik, err := scanIdempotencyKey(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return ik, nil
}

// GetIdempotencyKey returns the stored record for the given key.
// Returns ErrNotFound when the key is absent or expired.
func (s *Store) GetIdempotencyKey(ctx context.Context, key string) (*domain.IdempotencyKey, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT key, request_hash, status, response_code, response_body, job_id, created_at, expires_at
		FROM idempotency_keys
		WHERE key = $1 AND expires_at > now()`, key)
	return scanIdempotencyKey(row)
}

// UpdateIdempotencyKeyResponse stores the final response on a pending record.
func (s *Store) UpdateIdempotencyKeyResponse(ctx context.Context, p store.UpdateIdempotencyKeyResponseParams) error {
	bodyJSON, err := marshalPayload(p.ResponseBody)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE idempotency_keys
		SET status = $1, response_code = $2, response_body = $3, job_id = COALESCE($4, job_id)
		WHERE key = $5`,
		p.Status, p.ResponseCode, bodyJSON, p.JobID, p.Key,
	)
	if err != nil {
		return fmt.Errorf("postgres: update idempotency key response: %w", err)
	}
	return nil
}

func scanIdempotencyKey(row interface{ Scan(dest ...any) error }) (*domain.IdempotencyKey, error) {
	var ik domain.IdempotencyKey
	var bodyRaw []byte
	err := row.Scan(
		&ik.Key, &ik.RequestHash, &ik.Status, &ik.ResponseCode, &bodyRaw,
		&ik.JobID, &ik.CreatedAt, &ik.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan idempotency_key: %w", err)
	}
	if len(bodyRaw) > 0 {
		if err = unmarshalPayload(bodyRaw, &ik.ResponseBody); err != nil {
			return nil, err
		}
	}
	return &ik, nil
}

// ─── Audit log ───────────────────────────────────────────────────────────────

// InsertAuditEntry appends a row to audit_log.
// Detail must never contain secrets (NFR-6, FR-14).
func (s *Store) InsertAuditEntry(ctx context.Context, p store.InsertAuditEntryParams) error {
	detailJSON, err := marshalPayload(p.Detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO audit_log (actor_api_key, actor_user_id, action, merchant_id, space_id, target_user_id, scope, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.ActorAPIKeyID, p.ActorUserID, p.Action, p.MerchantID, p.SpaceID, p.TargetUserID, p.Scope, detailJSON,
	)
	if err != nil {
		return fmt.Errorf("postgres: insert audit entry: %w", err)
	}
	return nil
}

// ListSpaces returns spaces with optional filters and cursor-based pagination.
// Results are ordered by created_at ASC.
func (s *Store) ListSpaces(ctx context.Context, p store.ListSpacesParams) ([]*domain.Space, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}

	// Build the query dynamically with argument counting.
	args := make([]any, 0, 4)
	where := " WHERE 1=1"

	if p.LifecycleState != nil {
		args = append(args, *p.LifecycleState)
		where += fmt.Sprintf(" AND lifecycle_state = $%d", len(args))
	}
	if p.MerchantID != nil {
		args = append(args, *p.MerchantID)
		where += fmt.Sprintf(" AND merchant_id = $%d", len(args))
	}
	if p.Cursor != nil {
		args = append(args, *p.Cursor)
		where += fmt.Sprintf(" AND created_at > $%d", len(args))
	}

	args = append(args, limit)
	query := `SELECT id, merchant_id, discord_channel_id, discord_category_id, name,
		       lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		       reconciled_at, drift_count, created_at, updated_at, archived_at
		  FROM spaces` + where + fmt.Sprintf(` ORDER BY created_at ASC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list spaces: %w", err)
	}
	defer rows.Close()

	var out []*domain.Space
	for rows.Next() {
		sp, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// ─── Outbox ───────────────────────────────────────────────────────────────────

// CreateSpaceWithOutbox writes the desired-state Space row AND an outbox row in
// one Postgres transaction. This is the transactional outbox pattern (NFR-3, §4):
// the relay picks up the outbox row and enqueues the asynq task, so a committed
// desired-state change is never lost before enqueue.
func (s *Store) CreateSpaceWithOutbox(
	ctx context.Context,
	sp store.CreateSpaceParams,
	ob store.CreateOutboxParams,
) (*domain.Space, *domain.OutboxRow, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Insert space row.
	spRow := tx.QueryRow(ctx, `
		INSERT INTO spaces (merchant_id, name, discord_category_id)
		VALUES ($1, $2, $3)
		RETURNING id, merchant_id, discord_channel_id, discord_category_id, name,
		          lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		          reconciled_at, drift_count, created_at, updated_at, archived_at`,
		sp.MerchantID, sp.Name, sp.DiscordCategoryID,
	)
	space, err := scanSpace(spRow)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, nil, store.ErrConflict
		}
		return nil, nil, err
	}

	// Insert outbox row.
	payloadJSON, err := marshalPayload(ob.Payload)
	if err != nil {
		return nil, nil, err
	}
	obRow := tx.QueryRow(ctx, `
		INSERT INTO outbox (aggregate, aggregate_id, kind, payload, idempotency_key)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, aggregate, aggregate_id, kind, payload, idempotency_key, enqueued_at, created_at`,
		ob.Aggregate, ob.AggregateID, ob.Kind, payloadJSON, ob.IdempotencyKey,
	)
	outbox, err := scanOutboxRow(obRow)
	if err != nil {
		return nil, nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("postgres: commit outbox tx: %w", err)
	}
	return space, outbox, nil
}

// ListPendingOutbox returns up to limit outbox rows that have not been enqueued yet.
func (s *Store) ListPendingOutbox(ctx context.Context, limit int) ([]*domain.OutboxRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, aggregate, aggregate_id, kind, payload, idempotency_key, enqueued_at, created_at
		FROM outbox
		WHERE enqueued_at IS NULL
		ORDER BY created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: list pending outbox: %w", err)
	}
	defer rows.Close()

	var out []*domain.OutboxRow
	for rows.Next() {
		ob, err := scanOutboxRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ob)
	}
	return out, rows.Err()
}

// UpdateOutboxPayload replaces the payload on an outbox row identified by its
// idempotency_key. Only updates rows that have not yet been enqueued (enqueued_at IS NULL)
// so the relay does not pick up a stale payload.
func (s *Store) UpdateOutboxPayload(
	ctx context.Context,
	idempotencyKey string,
	payload map[string]any,
) error {
	payloadJSON, err := marshalPayload(payload)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`UPDATE outbox SET payload = $1
		 WHERE idempotency_key = $2 AND enqueued_at IS NULL`,
		payloadJSON, idempotencyKey,
	)
	if err != nil {
		return fmt.Errorf("postgres: update outbox payload: %w", err)
	}
	return nil
}

// StampOutboxEnqueued sets enqueued_at = now() on the named rows.
func (s *Store) StampOutboxEnqueued(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		"UPDATE outbox SET enqueued_at = now() WHERE id = ANY($1::uuid[])",
		ids,
	)
	if err != nil {
		return fmt.Errorf("postgres: stamp outbox enqueued: %w", err)
	}
	return nil
}

func scanOutboxRow(row interface{ Scan(dest ...any) error }) (*domain.OutboxRow, error) {
	var ob domain.OutboxRow
	var payloadRaw []byte
	err := row.Scan(
		&ob.ID, &ob.Aggregate, &ob.AggregateID, &ob.Kind,
		&payloadRaw, &ob.IdempotencyKey, &ob.EnqueuedAt, &ob.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan outbox: %w", err)
	}
	if len(payloadRaw) > 0 {
		if err = unmarshalPayload(payloadRaw, &ob.Payload); err != nil {
			return nil, err
		}
	}
	return &ob, nil
}

// ─── Space members ────────────────────────────────────────────────────────────

// CreateSpaceMember inserts a desired space_member row.
// Returns store.ErrConflict on (space_id, user_id) unique violation.
func (s *Store) CreateSpaceMember(ctx context.Context, p store.CreateSpaceMemberParams) (*domain.SpaceMember, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO space_members (space_id, user_id, role, invited_by)
		VALUES ($1, $2, $3, $4)
		RETURNING id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at`,
		p.SpaceID, p.UserID, p.Role, p.InvitedBy,
	)
	sm, err := scanSpaceMember(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, store.ErrConflict
		}
		return nil, err
	}
	return sm, nil
}

// GetSpaceMemberBySpaceAndUser returns the space_member for (space_id, user_id).
func (s *Store) GetSpaceMemberBySpaceAndUser(ctx context.Context, spaceID, userID string) (*domain.SpaceMember, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at
		FROM space_members WHERE space_id = $1 AND user_id = $2`, spaceID, userID)
	return scanSpaceMember(row)
}

// SetSpaceMemberOverwriteApplied marks the Discord overwrite as projected.
func (s *Store) SetSpaceMemberOverwriteApplied(ctx context.Context, id string) (*domain.SpaceMember, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE space_members SET overwrite_applied = TRUE
		WHERE id = $1
		RETURNING id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at`, id)
	return scanSpaceMember(row)
}

// RevokeSpaceMember sets revoked_at on the row (channel-scope expulsion, row kept for audit).
func (s *Store) RevokeSpaceMember(ctx context.Context, id string) (*domain.SpaceMember, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE space_members SET revoked_at = now()
		WHERE id = $1
		RETURNING id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at`, id)
	return scanSpaceMember(row)
}

// ListSpaceMembers returns active (revoked_at IS NULL) space_member rows for a space.
func (s *Store) ListSpaceMembers(ctx context.Context, spaceID string) ([]*domain.SpaceMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at
		FROM space_members
		WHERE space_id = $1 AND revoked_at IS NULL
		ORDER BY created_at ASC`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list space_members: %w", err)
	}
	defer rows.Close()
	return collectSpaceMembers(rows)
}

// ListCollaboratorChannels returns active space_member rows for a user.
func (s *Store) ListCollaboratorChannels(ctx context.Context, userID string) ([]*domain.SpaceMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at
		FROM space_members
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list collaborator channels: %w", err)
	}
	defer rows.Close()
	return collectSpaceMembers(rows)
}

// ListActiveSpaceMembers returns space_member rows for a space with overwrite_applied=true.
// Used by the reconciler to compare the Postgres-blessed set against Discord's overwrites.
func (s *Store) ListActiveSpaceMembers(ctx context.Context, spaceID string) ([]*domain.SpaceMember, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, space_id, user_id, role, overwrite_applied, invited_by, created_at, revoked_at
		FROM space_members
		WHERE space_id = $1 AND revoked_at IS NULL`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list active space_members: %w", err)
	}
	defer rows.Close()
	return collectSpaceMembers(rows)
}

func collectSpaceMembers(rows pgx.Rows) ([]*domain.SpaceMember, error) {
	var out []*domain.SpaceMember
	for rows.Next() {
		sm, err := scanSpaceMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

func scanSpaceMember(row interface{ Scan(dest ...any) error }) (*domain.SpaceMember, error) {
	var sm domain.SpaceMember
	err := row.Scan(
		&sm.ID, &sm.SpaceID, &sm.UserID, &sm.Role,
		&sm.OverwriteApplied, &sm.InvitedBy, &sm.CreatedAt, &sm.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan space_member: %w", err)
	}
	return &sm, nil
}

// ─── Directory ────────────────────────────────────────────────────────────────

// ListDirectory returns directory entries (space x user x role) with optional filters (FR-18).
func (s *Store) ListDirectory(ctx context.Context, p store.ListDirectoryParams) ([]*store.DirectoryEntry, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}

	args := make([]any, 0, 5)
	where := " WHERE sm.revoked_at IS NULL"

	if p.UserID != nil {
		args = append(args, *p.UserID)
		where += fmt.Sprintf(" AND sm.user_id = $%d", len(args))
	}
	if p.SpaceID != nil {
		args = append(args, *p.SpaceID)
		where += fmt.Sprintf(" AND sm.space_id = $%d", len(args))
	}
	if p.MerchantID != nil {
		args = append(args, *p.MerchantID)
		where += fmt.Sprintf(" AND sp.merchant_id = $%d", len(args))
	}
	if p.Cursor != nil {
		args = append(args, *p.Cursor)
		where += fmt.Sprintf(" AND sm.created_at > $%d", len(args))
	}

	args = append(args, limit)
	query := `
		SELECT sm.space_id, sp.name, sp.merchant_id, m.name,
		       sm.user_id, u.display_name, u.type
		FROM space_members sm
		JOIN spaces sp ON sp.id = sm.space_id
		JOIN merchants m ON m.id = sp.merchant_id
		JOIN users u ON u.id = sm.user_id` +
		where +
		fmt.Sprintf(` ORDER BY sm.created_at ASC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list directory: %w", err)
	}
	defer rows.Close()

	var out []*store.DirectoryEntry
	for rows.Next() {
		var e store.DirectoryEntry
		if err := rows.Scan(
			&e.SpaceID, &e.SpaceName, &e.MerchantID, &e.MerchantName,
			&e.UserID, &e.UserDisplayName, &e.Role,
		); err != nil {
			return nil, fmt.Errorf("postgres: scan directory entry: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// UpdateSpaceReconciledAt stamps reconciled_at = now() on a space.
func (s *Store) UpdateSpaceReconciledAt(ctx context.Context, spaceID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE spaces SET reconciled_at = now(), updated_at = now() WHERE id = $1`, spaceID)
	if err != nil {
		return fmt.Errorf("postgres: update space reconciled_at: %w", err)
	}
	return nil
}

// ─── M4: Lifecycle ────────────────────────────────────────────────────────────

// UpdateSpaceLifecycle transitions a space's lifecycle_state.
// When the new state is "archived", archived_at is set to now().
// When the new state is anything else (active, resolved), archived_at is cleared.
// Returns ErrNotFound when the space does not exist.
func (s *Store) UpdateSpaceLifecycle(ctx context.Context, p store.UpdateSpaceLifecycleParams) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE spaces
		SET lifecycle_state = $1,
		    archived_at = CASE WHEN $1::text = 'archived' THEN now() ELSE NULL END,
		    updated_at = now()
		WHERE id = $2
		RETURNING id, merchant_id, discord_channel_id, discord_category_id, name,
		          lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		          reconciled_at, drift_count, created_at, updated_at, archived_at`,
		p.LifecycleState, p.SpaceID,
	)
	sp, err := scanSpace(row)
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// UpdateSpaceWelcomeMessageID records the pinned message id from sync_welcome.
func (s *Store) UpdateSpaceWelcomeMessageID(ctx context.Context, spaceID, messageID string) (*domain.Space, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE spaces SET welcome_message_id = $1, updated_at = now()
		WHERE id = $2
		RETURNING id, merchant_id, discord_channel_id, discord_category_id, name,
		          lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		          reconciled_at, drift_count, created_at, updated_at, archived_at`,
		messageID, spaceID,
	)
	return scanSpace(row)
}

// ─── M4: Audit entries ────────────────────────────────────────────────────────

// ListAuditEntries returns audit_log rows newest-first with optional filters (FR-14).
func (s *Store) ListAuditEntries(ctx context.Context, p store.ListAuditEntriesParams) ([]*domain.AuditEntry, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}

	args := make([]any, 0, 6)
	where := " WHERE 1=1"

	if p.MerchantID != nil {
		args = append(args, *p.MerchantID)
		where += fmt.Sprintf(" AND merchant_id = $%d", len(args))
	}
	if p.SpaceID != nil {
		args = append(args, *p.SpaceID)
		where += fmt.Sprintf(" AND space_id = $%d", len(args))
	}
	if p.Action != nil {
		args = append(args, *p.Action)
		where += fmt.Sprintf(" AND action = $%d", len(args))
	}
	if p.Since != nil {
		args = append(args, *p.Since)
		where += fmt.Sprintf(" AND created_at > $%d::timestamptz", len(args))
	}
	// Cursor is the last seen id (newest-first: cursor means "id < cursor").
	if p.Cursor != nil {
		args = append(args, *p.Cursor)
		where += fmt.Sprintf(" AND id < $%d", len(args))
	}

	args = append(args, limit)
	query := `SELECT id, actor_api_key, actor_user_id, action, merchant_id, space_id,
		             target_user_id, scope, detail, created_at
		      FROM audit_log` + where +
		fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list audit entries: %w", err)
	}
	defer rows.Close()

	var out []*domain.AuditEntry
	for rows.Next() {
		e, err := scanAuditEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanAuditEntry(row interface{ Scan(dest ...any) error }) (*domain.AuditEntry, error) {
	var e domain.AuditEntry
	var detailRaw []byte
	var scopeStr *string
	err := row.Scan(
		&e.ID, &e.ActorAPIKeyID, &e.ActorUserID, &e.Action,
		&e.MerchantID, &e.SpaceID, &e.TargetUserID,
		&scopeStr, &detailRaw, &e.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: scan audit_entry: %w", err)
	}
	if scopeStr != nil {
		s := domain.ExpulsionScope(*scopeStr)
		e.Scope = &s
	}
	if len(detailRaw) > 0 {
		if err = unmarshalPayload(detailRaw, &e.Detail); err != nil {
			return nil, err
		}
	}
	return &e, nil
}

// ─── M4: Job lookup by space ──────────────────────────────────────────────────

// GetJobBySpaceIDAndKind returns the most-recent jobs row for a given space and task kind.
// Returns ErrNotFound when no row exists. Used by workers to advance job status without
// needing the job_id in the asynq task payload (fix for lookupJobBySpaceID stub).
func (s *Store) GetJobBySpaceIDAndKind(ctx context.Context, spaceID, kind string) (*domain.Job, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, task_id, kind, queue, status, merchant_id, space_id, user_id,
		       payload, error, retry_count, created_at, updated_at, completed_at
		FROM jobs
		WHERE space_id = $1 AND kind = $2
		ORDER BY created_at DESC
		LIMIT 1`, spaceID, kind)
	return scanJob(row)
}

// ListActiveProvisionedSpaces returns all spaces in lifecycle_state=active with a
// discord_channel_id set. Used by the M5 scheduled full-guild reconcile sweep (AC-5).
func (s *Store) ListActiveProvisionedSpaces(ctx context.Context) ([]*domain.Space, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, merchant_id, discord_channel_id, discord_category_id, name,
		       lifecycle_state, acl_state, welcome_message_id, last_activity_at,
		       reconciled_at, drift_count, created_at, updated_at, archived_at
		FROM spaces
		WHERE lifecycle_state = 'active'
		  AND discord_channel_id IS NOT NULL
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("postgres: list active provisioned spaces: %w", err)
	}
	defer rows.Close()

	var out []*domain.Space
	for rows.Next() {
		sp, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// marshalPayload converts a map to a JSON byte slice suitable for JSONB columns.
// Returns nil when the map is nil.
func marshalPayload(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal payload: %w", err)
	}
	return b, nil
}

// unmarshalPayload parses raw JSONB bytes into a map.
func unmarshalPayload(b []byte, dst *map[string]any) error {
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("postgres: unmarshal payload: %w", err)
	}
	return nil
}

// safeDSNPrefix returns a credential-free representation of the DSN for log output.
// It extracts only the host and database name from either URL-style
// ("postgres://user:pass@host:port/db") or key=value style DSNs, so that passwords
// embedded in the string are never emitted to structured logs (NFR-6, §7).
func safeDSNPrefix(dsn string) string {
	// URL-style DSN: postgres://user:pass@host:port/db?...
	// Strip everything before "@" to drop the user:pass segment.
	if idx := strings.Index(dsn, "://"); idx != -1 {
		rest := dsn[idx+3:] // user:pass@host:port/db?...
		if at := strings.Index(rest, "@"); at != -1 {
			rest = rest[at+1:] // host:port/db?...
		}
		// Drop query string.
		if q := strings.IndexAny(rest, "?"); q != -1 {
			rest = rest[:q]
		}
		return rest
	}
	// Key=value style: "host=... user=... password=... dbname=..."
	// Extract only host and dbname fields; skip everything else.
	var host, dbname string
	for _, field := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "host":
			host = v
		case "dbname":
			dbname = v
		}
	}
	if host != "" || dbname != "" {
		return "host=" + host + " dbname=" + dbname
	}
	// Unrecognised format — emit nothing rather than risk leaking a password.
	return "[dsn-format-unknown]"
}
