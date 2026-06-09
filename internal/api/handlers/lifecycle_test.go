// lifecycle_test.go — hermetic tests for ChangeSpaceLifecycle and SyncWelcome handlers (M4).
//
// Tests cover:
//   - AC-1/AC-6: POST /channels/{id}/lifecycle → 202 for valid transition;
//     409 for illegal transitions; 409 for already-in-state;
//     400 for unknown action; 403 for non-control-plane principal.
//   - AC-4: POST /channels/{id}/welcome:sync → 202 for known space; 404 for unknown space;
//     403 for non-control-plane principal.
//   - AC-3: GET /channels response includes lifecycle_state, merchant_id, created_at fields.
package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
)

// ─── Router helpers ───────────────────────────────────────────────────────────

func buildLifecycleRouter(s *spacesFakeStore, principal *authz.Principal) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.Recovery())
	r.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("principal", principal)
		}
		c.Next()
	})
	h := handlers.NewHandlers(handlers.Config{Store: s})
	r.POST("/v1/channels/:id/lifecycle", h.ChangeSpaceLifecycle)
	r.POST("/v1/channels/:id/welcome:sync", h.SyncWelcome)
	return r
}

// makeActiveSpace builds a minimal Space in the active lifecycle state.
func makeActiveSpace(id, merchantID string) *domain.Space {
	return &domain.Space{
		ID:             id,
		MerchantID:     merchantID,
		Name:           "test-space-" + id,
		LifecycleState: domain.SpaceLifecycleActive,
		ACLState:       domain.ACLStateApplied,
		CreatedAt:      time.Now(),
	}
}

func makeArchivedSpace(id, merchantID string) *domain.Space {
	now := time.Now()
	sp := makeActiveSpace(id, merchantID)
	sp.LifecycleState = domain.SpaceLifecycleArchived
	sp.ArchivedAt = &now
	return sp
}

func makeResolvedSpace(id, merchantID string) *domain.Space {
	sp := makeActiveSpace(id, merchantID)
	sp.LifecycleState = domain.SpaceLifecycleResolved
	return sp
}

// ─── AC-1/AC-6: ChangeSpaceLifecycle ─────────────────────────────────────────

// TestChangeLifecycle_Archive_Returns202 verifies that archiving an active space
// returns 202 with a job handle (AC-1, AC-6).
func TestChangeLifecycle_Archive_Returns202(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-001"] = makeActiveSpace("sp-001", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	body := `{"action":"archive"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-001/lifecycle",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("archive active space must return 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := resp["job"]; !ok {
		t.Error("response body must contain a 'job' object")
	}
}

// TestChangeLifecycle_Resolve_Returns202 verifies that resolving an active space
// returns 202 (AC-1, AC-6).
func TestChangeLifecycle_Resolve_Returns202(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-002"] = makeActiveSpace("sp-002", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-002/lifecycle",
		bytes.NewBufferString(`{"action":"resolve"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("resolve active space must return 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChangeLifecycle_Reopen_Returns202 verifies that reopening an archived space
// returns 202 (AC-1, AC-6).
func TestChangeLifecycle_Reopen_Returns202(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-003"] = makeArchivedSpace("sp-003", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-003/lifecycle",
		bytes.NewBufferString(`{"action":"reopen"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("reopen archived space must return 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChangeLifecycle_IllegalTransition_Returns409 verifies that an illegal transition
// (archived → resolve) is rejected with 409 conflict (AC-6).
func TestChangeLifecycle_IllegalTransition_Returns409(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-004"] = makeArchivedSpace("sp-004", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-004/lifecycle",
		bytes.NewBufferString(`{"action":"resolve"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("archived→resolve must be rejected with 409, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["code"] != "invalid_transition" {
		t.Errorf("error code must be 'invalid_transition', got %v", resp["code"])
	}
}

// TestChangeLifecycle_SameStateTransition_Returns409 verifies that transitioning to
// the same state (active → open, where open = active) is rejected with 409 (AC-6).
// The handler rejects this as an invalid_transition because the state machine does not
// define an active→active edge.
func TestChangeLifecycle_SameStateTransition_Returns409(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-005"] = makeActiveSpace("sp-005", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-005/lifecycle",
		bytes.NewBufferString(`{"action":"open"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// active→open (active) is an invalid_transition since active→active is not a defined edge.
	if w.Code != http.StatusConflict {
		t.Fatalf("active→open on active space must return 409, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChangeLifecycle_UnknownAction_Returns400 verifies that an unsupported action
// is rejected with 400 (AC-6 validation guard).
func TestChangeLifecycle_UnknownAction_Returns400(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-006"] = makeActiveSpace("sp-006", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-006/lifecycle",
		bytes.NewBufferString(`{"action":"delete"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown action must return 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChangeLifecycle_SpaceNotFound_Returns404 verifies that requesting a lifecycle
// transition on a non-existent space returns 404.
func TestChangeLifecycle_SpaceNotFound_Returns404(t *testing.T) {
	s := newSpacesFakeStore() // empty store — no spaces

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/no-such-space/lifecycle",
		bytes.NewBufferString(`{"action":"archive"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown space must return 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestChangeLifecycle_NonControlPlane_Returns403 verifies that a non-control-plane
// principal is rejected with 403 (Layer B, AC-6).
func TestChangeLifecycle_NonControlPlane_Returns403(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-007"] = makeActiveSpace("sp-007", "m-001")

	r := buildLifecycleRouter(s, spacesCollaboratorPrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-007/lifecycle",
		bytes.NewBufferString(`{"action":"archive"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-control-plane must get 403, got %d", w.Code)
	}
}

// TestChangeLifecycle_ResponseBody verifies that the 202 body contains the expected
// job shape (id, kind, status, space_id) (AC-6 response contract).
func TestChangeLifecycle_ResponseBody(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-008"] = makeResolvedSpace("sp-008", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-008/lifecycle",
		bytes.NewBufferString(`{"action":"archive"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("resolved→archive must return 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	jobObj, ok := resp["job"].(map[string]any)
	if !ok {
		t.Fatalf("response 'job' must be an object, got %T", resp["job"])
	}
	if jobObj["kind"] != "space:change_lifecycle" {
		t.Errorf("job.kind must be 'space:change_lifecycle', got %v", jobObj["kind"])
	}
	if jobObj["status"] != "pending" {
		t.Errorf("job.status must be 'pending', got %v", jobObj["status"])
	}
	if jobObj["space_id"] != "sp-008" {
		t.Errorf("job.space_id must be 'sp-008', got %v", jobObj["space_id"])
	}
}

// TestChangeLifecycle_LocationHeader verifies that the 202 response includes a
// Location header pointing to the job (AC-6 async path).
func TestChangeLifecycle_LocationHeader(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-009"] = makeActiveSpace("sp-009", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-009/lifecycle",
		bytes.NewBufferString(`{"action":"archive"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if loc == "" {
		t.Error("202 response must include a Location header")
	}
	if len(loc) < 10 || loc[:9] != "/v1/jobs/" {
		t.Errorf("Location must start with /v1/jobs/, got %q", loc)
	}
}

// ─── AC-4: SyncWelcome ────────────────────────────────────────────────────────

// TestSyncWelcome_Returns202 verifies that POST .../welcome:sync returns 202
// for an existing space (AC-4).
func TestSyncWelcome_Returns202(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-010"] = makeActiveSpace("sp-010", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-010/welcome:sync",
		bytes.NewBufferString(`{"message":"Hello!"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("welcome:sync for existing space must return 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	jobObj, ok := resp["job"].(map[string]any)
	if !ok {
		t.Fatalf("response 'job' must be an object, got %T", resp["job"])
	}
	if jobObj["kind"] != "space:sync_welcome" {
		t.Errorf("job.kind must be 'space:sync_welcome', got %v", jobObj["kind"])
	}
}

// TestSyncWelcome_EmptyBody_Returns202 verifies that POST .../welcome:sync with no
// body (optional message) still returns 202 (AC-4 optional body).
func TestSyncWelcome_EmptyBody_Returns202(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-011"] = makeActiveSpace("sp-011", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-011/welcome:sync", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("welcome:sync with no body must return 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSyncWelcome_SpaceNotFound_Returns404 verifies that POST .../welcome:sync on a
// non-existent space returns 404 (AC-4 guard).
func TestSyncWelcome_SpaceNotFound_Returns404(t *testing.T) {
	s := newSpacesFakeStore() // empty

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/no-such-space/welcome:sync",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("welcome:sync on missing space must return 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSyncWelcome_NonControlPlane_Returns403 verifies that a non-control-plane
// principal is rejected with 403 (AC-4 auth guard).
func TestSyncWelcome_NonControlPlane_Returns403(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-012"] = makeActiveSpace("sp-012", "m-001")

	r := buildLifecycleRouter(s, spacesCollaboratorPrincipal())
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-012/welcome:sync", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-control-plane must get 403, got %d", w.Code)
	}
}

// ─── AC-3: GET /channels — lifecycle visibility ───────────────────────────────

// ─── SEC-M4-001: welcome:sync message length cap ─────────────────────────────

// TestSyncWelcome_MessageTooLong_Returns400 verifies that a welcome message exceeding
// Discord's 2000-character limit is rejected with 400 validation_error (SEC-M4-001).
func TestSyncWelcome_MessageTooLong_Returns400(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-sec-001"] = makeActiveSpace("sp-sec-001", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())

	longMsg := strings.Repeat("a", 2001) // 2001 chars — one over Discord's 2000-char limit
	body, _ := json.Marshal(map[string]string{"message": longMsg})
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-sec-001/welcome:sync",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("message > 2000 chars must return 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["code"] != "validation_error" {
		t.Errorf("want code=validation_error, got %q", resp["code"])
	}
}

// TestSyncWelcome_MessageExactLimit_Returns202 verifies that a message of exactly
// 2000 characters is accepted (boundary: at-limit is valid).
func TestSyncWelcome_MessageExactLimit_Returns202(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-sec-002"] = makeActiveSpace("sp-sec-002", "m-001")

	r := buildLifecycleRouter(s, spacesControlPlanePrincipal())

	exactMsg := strings.Repeat("a", 2000) // exactly 2000 chars — at the limit
	body, _ := json.Marshal(map[string]string{"message": exactMsg})
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/sp-sec-002/welcome:sync",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("message of exactly 2000 chars must return 202, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListSpaces_IncludesLifecycleFields verifies that each item in the GET /channels
// response includes lifecycle_state, merchant_id, and created_at (AC-3, FR-10).
func TestListSpaces_IncludesLifecycleFields(t *testing.T) {
	s := newSpacesFakeStore()
	s.spaces["sp-013"] = makeArchivedSpace("sp-013", "m-001")

	// Use a real miniredis-backed cache so the test exercises the cache-miss path.
	redisCache, _ := newMiniredisCache(t)
	r := buildSpaceRouter(s, spacesControlPlanePrincipal(), redisCache)
	req := httptest.NewRequest(http.MethodGet, "/v1/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /channels must return 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items, ok := resp["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("response must have at least one item in 'items'")
	}
	item := items[0].(map[string]any)

	// Verify the fields required by AC-3/FR-10 are present.
	if _, ok := item["lifecycle_state"]; !ok {
		t.Error("GET /channels item must include 'lifecycle_state'")
	}
	if _, ok := item["merchant_id"]; !ok {
		t.Error("GET /channels item must include 'merchant_id'")
	}
	if _, ok := item["created_at"]; !ok {
		t.Error("GET /channels item must include 'created_at'")
	}
	if item["lifecycle_state"] != "archived" {
		t.Errorf("lifecycle_state must be 'archived', got %v", item["lifecycle_state"])
	}
}
