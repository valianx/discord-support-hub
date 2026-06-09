// Package oauth handles the Discord OAuth2 "Connect with Discord" flow (FR-22).
// In M0 only the interface seam is defined; the real implementation (code exchange,
// encrypted token storage, state validation) lands in M3.
package oauth

import "context"

// TokenStore persists and retrieves encrypted OAuth2 tokens (NFR-6).
type TokenStore interface {
	// Store encrypts the access/refresh tokens and saves them for the given user.
	// TODO(M3): implement using secrets.Encrypter + postgres store.
	Store(ctx context.Context, userID, accessToken, refreshToken, scopes string) error

	// LoadAccessToken decrypts and returns the access token for the given user.
	// TODO(M3): implement.
	LoadAccessToken(ctx context.Context, userID string) (string, error)
}

// StateManager issues and validates single-use CSRF state tokens (NFR-14, FR-22).
type StateManager interface {
	// Issue returns a signed state token for the given user and redirect target.
	// TODO(M3): implement with HMAC-SHA256 over a server secret.
	Issue(ctx context.Context, userID, redirectURL string) (string, error)

	// Validate verifies and consumes a state token. Returns the original userID on success.
	// TODO(M3): implement.
	Validate(ctx context.Context, state string) (userID string, err error)
}
