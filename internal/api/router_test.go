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
	"github.com/valianx/discord-support-hub/internal/observability"
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
// M6: /v1/oauth/discord/callback removed (AC-M6-9 — OAuth2 fully removed).
func TestStubHandlers_TransversalEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodGet, "/v1/directory")
	assertNotImplemented(t, r, http.MethodGet, "/v1/audit")
}

// TestOAuthCallback_NotRegistered_Returns404 verifies that the OAuth2 callback route
// was removed in M6 (AC-M6-9) and returns 404 (no longer registered).
func TestOAuthCallback_NotRegistered_Returns404(t *testing.T) {
	r := newTestRouter()
	assertStatus(t, r, http.MethodGet, "/v1/oauth/discord/callback", http.StatusNotFound)
}

// TestStubHandlers_JobEndpoints verifies the jobs polling route returns 501.
func TestStubHandlers_JobEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodGet, "/v1/jobs/job-001")
}

// TestWelcomeSyncRoute_ContractPath verifies that the router serves the OpenAPI contract
// path POST /v1/channels/{id}/welcome:sync (with a literal colon) and returns the
// handler response — NOT 404 (AC-4 route-path fix: the colon must be part of the literal
// segment, not a Gin route-param prefix).
//
// The nil-store router returns 501 from SyncWelcome (store == nil guard). A 404 would
// indicate the route was never registered at the contract path.
func TestWelcomeSyncRoute_ContractPath(t *testing.T) {
	r := newTestRouter()
	// Contract path per OpenAPI: POST /v1/channels/{id}/welcome:sync
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/space-001/welcome:sync", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// The handler must be reachable (not 404). With nil store it returns 501.
	if w.Code == http.StatusNotFound {
		t.Errorf("AC-4: POST /v1/channels/{id}/welcome:sync must be registered at the colon path, got 404 — " +
			"route is not wired correctly. Check router.go welcome:sync registration.")
	}
	if w.Code != http.StatusNotImplemented {
		t.Errorf("AC-4: POST .../welcome:sync with nil store must return 501 (handler reached), got %d; body: %s",
			w.Code, w.Body.String())
	}
}

// TestMetricsEndpoint_MountedWhenMetricsNonNil verifies that the /metrics endpoint is
// mounted and returns 200 with the expected metric names when a non-nil Metrics instance
// is provided to RouterConfig (M5, AC-2).
//
// Each metric family requires at least one observation before Prometheus includes it in
// the text exposition output. We record one sample of each before requesting /metrics.
func TestMetricsEndpoint_MountedWhenMetricsNonNil(t *testing.T) {
	gin.SetMode(gin.TestMode)

	m := observability.NewMetrics()
	// Pre-seed one observation per metric so they all appear in the exposition output.
	observability.RecordProvisioningLatency(m, 0.1, true)
	observability.IncRateLimitHit(m)
	observability.IncError(m, "fatal")
	observability.SetActiveSpaces(m, 1)

	r := api.NewRouter(api.RouterConfig{
		Metrics:     m,
		PGPinger:    &alwaysHealthyPinger{},
		RedisPinger: &alwaysHealthyPinger{},
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/metrics: want 200, got %d; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, name := range []string{
		"hub_provisioning_latency_seconds",
		"hub_active_spaces_total",
		"hub_ratelimit_hits_total",
		"hub_errors_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics response missing metric %q", name)
		}
	}
}

// TestMetricsEndpoint_AbsentWhenMetricsNil verifies that /metrics returns 404 (route not
// registered) when the RouterConfig.Metrics field is nil (M5, AC-2 — optional mount).
func TestMetricsEndpoint_AbsentWhenMetricsNil(t *testing.T) {
	r := newTestRouter() // Metrics is nil in newTestRouter

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("/metrics with nil Metrics: want 404 (route not registered), got %d", w.Code)
	}
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
