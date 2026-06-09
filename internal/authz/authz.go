// Package authz provides the two-layer authorization model (docs/02-architecture.md §5).
//
// Layer A authenticates service API keys: extract bearer → hash → look up api_keys.
// Layer B resolves per-request authorization decisions against Postgres state.
//
// AuthZ is always a pure function of Postgres state, never of Discord state (NFR-13).
package authz

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// PrincipalType identifies how a request was authenticated.
type PrincipalType string

const (
	// PrincipalTypeService is an opaque bearer service API key (backoffice, Layer A).
	PrincipalTypeService PrincipalType = "service"

	// PrincipalTypeSession is a short-lived session token for the future POC frontend.
	// The seam is reserved here; the session issuer is built in POC-FE (§5.3).
	// TODO(POC-FE): implement session issuance and validation.
	PrincipalTypeSession PrincipalType = "session"
)

// Principal represents an authenticated caller after Layer A passes.
// It is injected into the Gin context by the auth middleware and consumed
// by Layer B checks. All fields are resolved from Postgres at auth time.
type Principal struct {
	// Type is how the caller was authenticated.
	Type PrincipalType

	// KeyID is the api_keys.id of the authenticating service key.
	// Empty for session principals (reserved for POC-FE).
	KeyID string

	// UserID is the users.id of the acting user when the key has an explicit user binding.
	// May be empty for pure-service principals acting on behalf of the backoffice.
	UserID string

	// IsAdmin reflects users.is_admin resolved from Postgres at auth time.
	// A service key that is not bound to a user is NOT admin by default.
	// Only an explicit is_admin=true agent row grants this.
	IsAdmin bool

	// KeyScope is the api_keys.scope, e.g. "backoffice".
	KeyScope string
}

// Decision constants for Layer B checks.
const (
	// Allow means the principal is authorized to perform the action.
	Allow = true
	// Deny means the principal is not authorized.
	Deny = false
)

// ScopeBackoffice is the api_keys.scope value that confers control-plane authority.
// It is a server-side DB value set at key creation (cmd/keygen), never client-supplied.
// Only this exact string grants control-plane access for service keys (§5.2, CWE-639).
const ScopeBackoffice = "backoffice"

// RequireControlPlane returns Allow when the principal has control-plane authority,
// Deny otherwise. Control-plane authority is conferred by either of two Postgres-anchored
// facts (docs/02-architecture.md §5.2):
//   - a service API key whose api_keys.scope == ScopeBackoffice (server-side DB value,
//     set at key creation by cmd/keygen, never derivable from any client-supplied input); or
//   - a future user/session principal with users.is_admin == true.
//
// Both conditions are resolved from Postgres state at auth time — never from a request
// header, body, or self-asserted value — so there is no privilege-escalation path (CWE-639).
// Used for roster management endpoints: POST /agents, DELETE /agents/{id}, GET /agents.
func RequireControlPlane(p *Principal) bool {
	if p == nil {
		return Deny
	}
	// Service key path: scope must be exactly the server-side "backoffice" value.
	if p.KeyScope == ScopeBackoffice {
		return Allow
	}
	// User/session path (future POC-FE): is_admin flag from users.is_admin in Postgres.
	return p.IsAdmin
}

// RequireAdmin returns Allow when the principal has is_admin=true, Deny otherwise.
// Used for Admin-gated endpoints where only a human admin principal is sufficient.
// The decision is a pure function of Postgres state injected into the Principal.
func RequireAdmin(p *Principal) bool {
	if p == nil {
		return Deny
	}
	return p.IsAdmin
}

// RequireAuthenticated returns Allow when a valid principal was injected, Deny otherwise.
// Used as a base check — Layer A already rejects unauthenticated requests before
// handlers run, but this allows handlers to double-check defensively.
func RequireAuthenticated(p *Principal) bool {
	return p != nil
}

// ─── Key hashing ─────────────────────────────────────────────────────────────

// HashAPIKey computes the SHA-256 hash of a raw API key (opaque bearer token).
// The hash is stored in api_keys.key_hash; the raw key is never persisted (§5.1).
// SHA-256 is sufficient here because the raw keys are high-entropy random values
// (see GenerateAPIKey), making pre-image attacks infeasible without the raw material.
func HashAPIKey(rawKey string) []byte {
	h := sha256.Sum256([]byte(rawKey))
	return h[:]
}

// GenerateAPIKey generates a cryptographically random 32-byte (256-bit) opaque API key
// encoded as a hex string. The key is shown once at issuance and never persisted.
// The caller stores only HashAPIKey(rawKey).
func GenerateAPIKey() (rawKey string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", fmt.Errorf("authz: generate api key: %w", err)
	}
	return hex.EncodeToString(b), nil
}
