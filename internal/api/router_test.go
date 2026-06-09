// Package api_test verifies that all M0 stub handlers return 501 Not Implemented
// with the documented Error shape. This is the legitimate M0 surface — every
// handler is a stub in this milestone; the test confirms the shape is correct
// so downstream clients get a well-formed response.
//
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
)

// alwaysHealthyPinger is a no-op Pinger that always returns nil (healthy).
type alwaysHealthyPinger struct{}

func (p *alwaysHealthyPinger) Ping(_ context.Context) error { return nil }

// newTestRouter builds the Gin router in test mode with no-op pingers and no CORS origins.
func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return api.NewRouter(api.RouterConfig{
		CORSAllowedOrigins: nil,
		PGPinger:           &alwaysHealthyPinger{},
		RedisPinger:        &alwaysHealthyPinger{},
	})
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
// with the contract error shape (M0 stub surface).
func TestStubHandlers_SpacesEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodPost, "/v1/merchants/merch-001/channels")
	assertNotImplemented(t, r, http.MethodGet, "/v1/channels")
	assertNotImplemented(t, r, http.MethodGet, "/v1/channels/space-001")
	assertNotImplemented(t, r, http.MethodGet, "/v1/channels/space-001/members")
	assertNotImplemented(t, r, http.MethodPost, "/v1/channels/space-001/lifecycle")
	assertNotImplemented(t, r, http.MethodPost, "/v1/channels/space-001/welcomesync")
}

// TestStubHandlers_CollaboratorEndpoints verifies collaborator routes return 501.
func TestStubHandlers_CollaboratorEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodPost, "/v1/channels/space-001/collaborators")
	assertNotImplemented(t, r, http.MethodDelete, "/v1/channels/space-001/collaborators/user-001")
	assertNotImplemented(t, r, http.MethodGet, "/v1/collaborators/user-001/channels")
}

// TestStubHandlers_AgentEndpoints verifies agent routes return 501.
func TestStubHandlers_AgentEndpoints(t *testing.T) {
	r := newTestRouter()

	assertNotImplemented(t, r, http.MethodGet, "/v1/agents")
	assertNotImplemented(t, r, http.MethodPost, "/v1/agents")
	assertNotImplemented(t, r, http.MethodDelete, "/v1/agents/user-001")
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

// TestHealthEndpoints_NotStubbed verifies that the health endpoints (livez/readyz) are
// NOT stubs — they must return 200, not 501. This guards against a regression where
// health checks are accidentally registered as stub handlers.
func TestHealthEndpoints_NotStubbed(t *testing.T) {
	r := newTestRouter()

	for _, path := range []string{"/livez", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code == http.StatusNotImplemented {
			t.Errorf("%s: health endpoint must not return 501 (it is a real handler, not a stub)", path)
		}
		if w.Code != http.StatusOK {
			t.Errorf("%s: want 200 from health endpoint, got %d; body: %s", path, w.Code, w.Body.String())
		}
	}
}
