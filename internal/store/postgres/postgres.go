// Package postgres implements the store.Store interface using pgx/pgxpool (NFR-8).
package postgres

import (
	"context"
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

// ─── Spaces (TODO M2) ─────────────────────────────────────────────────────────

// GetSpaceByID implements store.Store.
// TODO(M2): implement the real query.
func (s *Store) GetSpaceByID(_ context.Context, _ string) (*domain.Space, error) {
	return nil, fmt.Errorf("GetSpaceByID: not implemented") // TODO(M2)
}

// ─── Jobs (TODO M2) ───────────────────────────────────────────────────────────

// CreateJob implements store.Store.
// TODO(M2): implement the real insert.
func (s *Store) CreateJob(_ context.Context, _ store.CreateJobParams) (*domain.Job, error) {
	return nil, fmt.Errorf("CreateJob: not implemented") // TODO(M2)
}

// GetJobByID implements store.Store.
// TODO(M2): implement the real query.
func (s *Store) GetJobByID(_ context.Context, _ string) (*domain.Job, error) {
	return nil, fmt.Errorf("GetJobByID: not implemented") // TODO(M2)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

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
