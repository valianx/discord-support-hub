// Package handlers_test — GetJob handler tests (AC-7).
//
// Verifies that GET /jobs/{jobId} reads authoritative status from the Postgres
// jobs table (not Valkey) and returns the correct Job shape.
// Coverage:
//   - 200 with the correct job fields for an existing job (control-plane principal).
//   - 403 when a non-control-plane principal attempts to poll a job (SEC-002).
//   - 404 when no jobs row exists for the given id.
//   - 501 when no store is wired (test/stub mode).
package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Fake job store ───────────────────────────────────────────────────────────

type jobFakeStore struct {
	agentFakeStore
	jobs map[string]*domain.Job
}

func newJobFakeStore() *jobFakeStore {
	return &jobFakeStore{
		agentFakeStore: agentFakeStore{users: make(map[string]*domain.User)},
		jobs:           make(map[string]*domain.Job),
	}
}

func (s *jobFakeStore) GetJobByID(_ context.Context, id string) (*domain.Job, error) {
	j, ok := s.jobs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return j, nil
}

// ─── Helper ───────────────────────────────────────────────────────────────────

// jobRouter builds a minimal Gin engine for GetJob tests.
// The principal is injected directly (bypassing Layer A) so tests focus on Layer B.
// Pass nil to simulate a missing/unauthenticated principal.
func jobRouter(s store.Store, principal *authz.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("principal", principal)
		}
		c.Next()
	})
	h := handlers.NewHandlers(handlers.Config{Store: s})
	r.GET("/v1/jobs/:jobId", h.GetJob)
	return r
}

// controlPlanePrincipal returns a principal representing a backoffice-scoped service key.
func controlPlanePrincipal() *authz.Principal {
	return &authz.Principal{
		Type:     authz.PrincipalTypeService,
		KeyID:    "cp-key-1",
		KeyScope: authz.ScopeBackoffice,
	}
}

// nonControlPlanePrincipal returns a principal that lacks control-plane authority.
func nonControlPlanePrincipal() *authz.Principal {
	return &authz.Principal{
		Type:    authz.PrincipalTypeService,
		KeyID:   "limited-key-1",
		IsAdmin: false,
	}
}

// ─── 200: existing job ────────────────────────────────────────────────────────

// TestGetJob_ExistingJob_Returns200 verifies that GET /jobs/{id} returns 200 with
// the correct job fields when the jobs row exists (AC-7).
func TestGetJob_ExistingJob_Returns200(t *testing.T) {
	s := newJobFakeStore()
	merchantID := "merchant-abc"
	spaceID := "space-xyz"
	completedAt := time.Now().UTC()
	s.jobs["job-001"] = &domain.Job{
		ID:          "job-001",
		TaskID:      "idem-key-001",
		Kind:        "space:provision",
		Queue:       "provision",
		Status:      domain.JobStatusCompleted,
		MerchantID:  &merchantID,
		SpaceID:     &spaceID,
		RetryCount:  0,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
		CompletedAt: &completedAt,
	}

	r := jobRouter(s, controlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["id"] != "job-001" {
		t.Errorf("id: want job-001, got %v", resp["id"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status: want completed, got %v", resp["status"])
	}
	if resp["kind"] != "space:provision" {
		t.Errorf("kind: want space:provision, got %v", resp["kind"])
	}
	if resp["space_id"] != spaceID {
		t.Errorf("space_id: want %v, got %v", spaceID, resp["space_id"])
	}
	if resp["merchant_id"] != merchantID {
		t.Errorf("merchant_id: want %v, got %v", merchantID, resp["merchant_id"])
	}
	if resp["completed_at"] == nil {
		t.Error("completed_at must be set for a completed job")
	}
}

// ─── 404: job not found ───────────────────────────────────────────────────────

// TestGetJob_NotFound_Returns404 verifies that 404 is returned when no jobs row
// exists for the given id (AC-7 — Postgres is authoritative, not Valkey).
func TestGetJob_NotFound_Returns404(t *testing.T) {
	s := newJobFakeStore() // empty — no jobs
	r := jobRouter(s, controlPlanePrincipal())

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/nonexistent-job", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode 404 body: %v", err)
	}
	if resp["code"] != "not_found" {
		t.Errorf("want code=not_found, got %v", resp["code"])
	}
}

// ─── 501: no store wired ──────────────────────────────────────────────────────

// TestGetJob_NilStore_Returns501 verifies that the handler falls back to the
// not-implemented stub when no store is configured (test router mode).
func TestGetJob_NilStore_Returns501(t *testing.T) {
	// nil store check occurs before the authz gate, so no principal is needed.
	r := jobRouter(nil, controlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/any-id", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Errorf("want 501, got %d", w.Code)
	}
}

// ─── SEC-002: control-plane gate ─────────────────────────────────────────────

// TestGetJob_ControlPlane_Returns200 verifies that a control-plane principal (backoffice-
// scoped key) receives 200 for an existing job (SEC-002 — gate allows control plane).
func TestGetJob_ControlPlane_Returns200(t *testing.T) {
	s := newJobFakeStore()
	s.jobs["job-cp-001"] = &domain.Job{
		ID:        "job-cp-001",
		TaskID:    "idem-cp-001",
		Kind:      "space:provision",
		Queue:     "provision",
		Status:    domain.JobStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	r := jobRouter(s, controlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-cp-001", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("SEC-002: control-plane key must reach GET /jobs/{id} (want 200), got %d; body: %s",
			w.Code, w.Body.String())
	}
}

// TestGetJob_NonControlPlane_Returns403 verifies that a non-control-plane principal
// is rejected with 403, preventing IDOR access to other tenants' job data (SEC-002).
func TestGetJob_NonControlPlane_Returns403(t *testing.T) {
	s := newJobFakeStore()
	s.jobs["job-other-tenant"] = &domain.Job{
		ID:        "job-other-tenant",
		TaskID:    "idem-other",
		Kind:      "space:provision",
		Queue:     "provision",
		Status:    domain.JobStatusPending,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	r := jobRouter(s, nonControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/job-other-tenant", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("SEC-002: non-control-plane principal must be rejected with 403, got %d; body: %s",
			w.Code, w.Body.String())
	}
}
