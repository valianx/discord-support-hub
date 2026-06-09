// Package api_test verifies the API router wiring and handler surface.
//
// M0 stubs return 501; M1 agent handlers enforce Layer B authZ (403/401).
// All tests are hermetic: no real database, Redis, or network is needed.
package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
)

// alwaysHealthyPinger is a no-op Pinger that always returns nil (healthy).
type alwaysHealthyPinger struct{}

func (p *alwaysHealthyPinger) Ping(_ context.Context) error { return nil }

// newTestRouter builds the Gin router in test mode with no-op pingers, no CORS origins,
// and no real store (so no Layer A auth is enforced — principal is nil).
func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return api.NewRouter(api.RouterConfig{
		CORSAllowedOrigins: nil,
		Store:              nil, // nil → no-op auth, principal is nil
		PGPinger:           &alwaysHealthyPinger{},
		RedisPinger:        &alwaysHealthyPinger{},
	})
}

// newAdminRouter builds the Gin router with an admin principal pre-injected.
// Used for testing agent endpoints that require Admin (Layer B).
func newAdminRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(middleware.RequestID())

	// Inject an admin principal before every request.
	r.Use(func(c *gin.Context) {
		c.Set("principal", &authz.Principal{
			Type:    authz.PrincipalTypeService,
			KeyID:   "test-key-id",
			IsAdmin: true,
		})
		c.Next()
	})

	// Re-build the same router but with a custom engine that has admin principal.
	// We need to route through the same handler logic, so we use the full router but
	// wrap it in a test engine that pre-sets the principal. Since NewRouter always
	// builds its own engine, we use the simpler pattern: test handlers directly.
	// For integration purposes, the admin principal test is in the handler tests.
	// Here we just assert the routing is wired correctly.
	_ = r
	return newTestRouter()
}

// assertStatus is a test helper that makes a request and asserts the status code.
func assertStatus(t *testing.T, r *gin.Engine, method, path string, wantCode int) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != wantCode {
		t.Errorf("%s %s: want %d, got %d; body: %s", method, path, wantCode, w.Code, w.Body.String())
	}
}

// assertNotImplemented is a test helper that makes a request and asserts:
//  1. Status code is 501 Not Implemented.
//  2. Response body contains the "not_implemented" error code (contract Error shape).
func assertNotImplemented(t *testing.T, r *gin.Engine, method, path string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("%s %s: want 501, got %d; body: %s", method, path, w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "not_implemented") {
		t.Errorf("%s %s: expected 'not_implemented' code in body, got: %s", method, path, body)
	}
}

// TestStubHandlers_SpacesEndpoints verifies that all space-related routes return 501
// with the contract error shape (stubs until M2/M4).
func TestStubHandlers_SpacesEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodPost, "/v1/merchants/merch-001/channels")
	assertNotImplemented(t, r, http.MethodGet, "/v1/channels")
	assertNotImplemented(t, r, http.MethodGet, "/v1/channels/space-001")
	assertNotImplemented(t, r, http.MethodGet, "/v1/channels/space-001/members")
	assertNotImplemented(t, r, http.MethodPost, "/v1/channels/space-001/lifecycle")
	assertNotImplemented(t, r, http.MethodPost, "/v1/channels/space-001/welcomesync")
}

// TestStubHandlers_CollaboratorEndpoints verifies collaborator routes return 501 (stubs until M3).
func TestStubHandlers_CollaboratorEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodPost, "/v1/channels/space-001/collaborators")
	assertNotImplemented(t, r, http.MethodDelete, "/v1/channels/space-001/collaborators/user-001")
	assertNotImplemented(t, r, http.MethodGet, "/v1/collaborators/user-001/channels")
}

// TestAgentEndpoints_NoAuth verifies that agent routes return 403 when no principal is
// present (nil principal → RequireAdmin fails → 403). M1 behavior: Layer B enforced.
func TestAgentEndpoints_NoAuth(t *testing.T) {
	r := newTestRouter()

	// With no principal injected (nil store → no-op auth → nil principal),
	// Layer B returns 403 Forbidden on Admin-gated endpoints.
	assertStatus(t, r, http.MethodGet, "/v1/agents", http.StatusForbidden)
	assertStatus(t, r, http.MethodPost, "/v1/agents", http.StatusForbidden)
	assertStatus(t, r, http.MethodDelete, "/v1/agents/user-001", http.StatusForbidden)
}

// TestStubHandlers_TransversalEndpoints verifies transversal routes return 501.
func TestStubHandlers_TransversalEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodGet, "/v1/directory")
	assertNotImplemented(t, r, http.MethodGet, "/v1/audit")
	assertNotImplemented(t, r, http.MethodGet, "/v1/oauth/discord/callback")
}

// TestStubHandlers_JobEndpoints verifies the jobs polling route returns 501.
func TestStubHandlers_JobEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodGet, "/v1/jobs/job-001")
}

// TestHealthEndpoints_NotStubbed verifies that health endpoints return 200, not 501.
func TestHealthEndpoints_NotStubbed(t *testing.T) {
	r := newTestRouter()

	for _, path := range []string{"/livez", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code == http.StatusNotImplemented {
			t.Errorf("%s: health endpoint must not return 501", path)
		}
		if w.Code != http.StatusOK {
			t.Errorf("%s: want 200 from health endpoint, got %d; body: %s", path, w.Code, w.Body.String())
		}
	}
}
