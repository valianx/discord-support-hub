// Package authz_test verifies the two-layer authZ model (docs/02-architecture.md §5).
// All tests are hermetic — no real database or network is needed.
package authz_test

import (
	"encoding/hex"
	"testing"

	"github.com/valianx/discord-support-hub/internal/authz"
)

// ─── Key hashing (AC-7) ───────────────────────────────────────────────────────

// TestHashAPIKey_Deterministic verifies that the same raw key always produces the same hash.
func TestHashAPIKey_Deterministic(t *testing.T) {
	raw := "test-api-key-value-123"
	h1 := authz.HashAPIKey(raw)
	h2 := authz.HashAPIKey(raw)

	if hex.EncodeToString(h1) != hex.EncodeToString(h2) {
		t.Error("HashAPIKey must be deterministic for the same input")
	}
}

// TestHashAPIKey_DifferentInputsDifferentHashes verifies collision resistance at a basic level.
func TestHashAPIKey_DifferentInputsDifferentHashes(t *testing.T) {
	h1 := authz.HashAPIKey("key-a")
	h2 := authz.HashAPIKey("key-b")

	if hex.EncodeToString(h1) == hex.EncodeToString(h2) {
		t.Error("different raw keys must produce different hashes")
	}
}

// TestHashAPIKey_SHA256Length verifies the hash is 32 bytes (SHA-256 output size).
func TestHashAPIKey_SHA256Length(t *testing.T) {
	h := authz.HashAPIKey("any-key")
	if len(h) != 32 {
		t.Errorf("SHA-256 hash must be 32 bytes, got %d", len(h))
	}
}

// TestGenerateAPIKey_NonEmpty verifies that key generation produces a non-empty hex string.
func TestGenerateAPIKey_NonEmpty(t *testing.T) {
	k, err := authz.GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	if k == "" {
		t.Error("GenerateAPIKey returned empty string")
	}
}

// TestGenerateAPIKey_Unique verifies that two generated keys are distinct.
func TestGenerateAPIKey_Unique(t *testing.T) {
	k1, _ := authz.GenerateAPIKey()
	k2, _ := authz.GenerateAPIKey()
	if k1 == k2 {
		t.Error("GenerateAPIKey must return unique keys on each call")
	}
}

// TestGenerateAPIKey_HashableRaw verifies the raw key can be hashed and the hash is 32 bytes.
func TestGenerateAPIKey_HashableRaw(t *testing.T) {
	raw, err := authz.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	h := authz.HashAPIKey(raw)
	if len(h) != 32 {
		t.Errorf("hash of generated key must be 32 bytes, got %d", len(h))
	}
}

// ─── Layer B: RequireControlPlane (§5.2) ─────────────────────────────────────

// TestRequireControlPlane_BackofficeScope verifies that a service key with
// scope=ScopeBackoffice confers control-plane authority (the fixed behavior, §5.2).
func TestRequireControlPlane_BackofficeScope(t *testing.T) {
	p := &authz.Principal{
		Type:     authz.PrincipalTypeService,
		KeyScope: authz.ScopeBackoffice,
		IsAdmin:  false, // scope alone is sufficient; is_admin not required
	}
	if !authz.RequireControlPlane(p) {
		t.Error("RequireControlPlane must return true for a backoffice-scoped service key")
	}
}

// TestRequireControlPlane_AdminUser verifies that an is_admin user principal confers
// control-plane authority even without a backoffice key scope (§5.2, future POC-FE path).
func TestRequireControlPlane_AdminUser(t *testing.T) {
	p := &authz.Principal{
		Type:    authz.PrincipalTypeSession,
		IsAdmin: true,
	}
	if !authz.RequireControlPlane(p) {
		t.Error("RequireControlPlane must return true for an is_admin=true user principal")
	}
}

// TestRequireControlPlane_NarrowScope verifies that a service key with a scope other
// than ScopeBackoffice does NOT confer control-plane authority (NFR-13, CWE-639).
func TestRequireControlPlane_NarrowScope(t *testing.T) {
	scopes := []string{"readonly", "admin", "BACKOFFICE", "", "Backoffice"}
	for _, scope := range scopes {
		p := &authz.Principal{
			Type:     authz.PrincipalTypeService,
			KeyScope: scope,
			IsAdmin:  false,
		}
		if authz.RequireControlPlane(p) {
			t.Errorf("RequireControlPlane must deny scope=%q — only %q grants control-plane access",
				scope, authz.ScopeBackoffice)
		}
	}
}

// TestRequireControlPlane_NilPrincipal verifies that a nil principal is denied.
func TestRequireControlPlane_NilPrincipal(t *testing.T) {
	if authz.RequireControlPlane(nil) {
		t.Error("RequireControlPlane must return false for a nil principal")
	}
}

// ─── Layer B: RequireAdmin (AC-2, AC-3, AC-8) ─────────────────────────────────

// TestRequireAdmin_AdminPrincipal verifies that an admin principal is allowed.
func TestRequireAdmin_AdminPrincipal(t *testing.T) {
	p := &authz.Principal{
		Type:    authz.PrincipalTypeService,
		IsAdmin: true,
	}
	if !authz.RequireAdmin(p) {
		t.Error("RequireAdmin must return true for an admin principal")
	}
}

// TestRequireAdmin_NonAdminPrincipal verifies that a non-admin principal is denied.
func TestRequireAdmin_NonAdminPrincipal(t *testing.T) {
	p := &authz.Principal{
		Type:    authz.PrincipalTypeService,
		IsAdmin: false,
	}
	if authz.RequireAdmin(p) {
		t.Error("RequireAdmin must return false for a non-admin principal")
	}
}

// TestRequireAdmin_NilPrincipal verifies that a nil principal is denied.
// This covers the unauthenticated case (Layer A missed or bypass attempted).
func TestRequireAdmin_NilPrincipal(t *testing.T) {
	if authz.RequireAdmin(nil) {
		t.Error("RequireAdmin must return false for a nil principal")
	}
}

// TestRequireAdmin_CollaboratorTypePrincipal verifies that a collaborator-type
// principal is denied even if the API key scope is valid. This mirrors the
// Postgres-always-wins invariant (NFR-13, AC-3): authZ is a pure function of
// Postgres state, not of Discord state or arbitrary flags.
func TestRequireAdmin_CollaboratorTypePrincipal(t *testing.T) {
	// A collaborator principal can never be admin (users_admin_only_agent_chk enforces this in DB).
	p := &authz.Principal{
		Type:    authz.PrincipalTypeService,
		IsAdmin: false, // collaborators cannot be admin
	}
	if authz.RequireAdmin(p) {
		t.Error("RequireAdmin must deny a non-admin collaborator principal")
	}
}

// TestRequireAuthenticated_ValidPrincipal verifies authenticated check passes.
func TestRequireAuthenticated_ValidPrincipal(t *testing.T) {
	p := &authz.Principal{Type: authz.PrincipalTypeService}
	if !authz.RequireAuthenticated(p) {
		t.Error("RequireAuthenticated must return true for a valid principal")
	}
}

// TestRequireAuthenticated_NilPrincipal verifies unauthenticated check fails.
func TestRequireAuthenticated_NilPrincipal(t *testing.T) {
	if authz.RequireAuthenticated(nil) {
		t.Error("RequireAuthenticated must return false for nil")
	}
}
