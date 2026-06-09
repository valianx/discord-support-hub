// audit_test.go — hermetic tests for GET /audit handler (M4 AC-2, FR-14).
//
// Tests cover:
//   - Control-plane gated: 403 for non-control-plane principal.
//   - Returns 200 with items array when audit entries exist.
//   - Response shape: each entry has id, action, created_at (no secrets in output).
//   - Filters: merchant_id, space_id, action query params are forwarded to the store.
//   - Cursor pagination: next_cursor is set when the page is full.
//   - Empty result: 200 with empty items array when no entries match.
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
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Fake store for audit tests ───────────────────────────────────────────────

// auditFakeStore extends agentFakeStore with a configurable ListAuditEntries response.
type auditFakeStore struct {
	agentFakeStore
	entries []*domain.AuditEntry
	// lastParams captures the params passed to the last ListAuditEntries call.
	lastParams store.ListAuditEntriesParams
}

func newAuditFakeStore() *auditFakeStore {
	return &auditFakeStore{
		agentFakeStore: agentFakeStore{users: make(map[string]*domain.User)},
	}
}

// ListAuditEntries overrides the panic stub in agentFakeStore.
func (f *auditFakeStore) ListAuditEntries(
	_ context.Context,
	p store.ListAuditEntriesParams,
) ([]*domain.AuditEntry, error) {
	f.lastParams = p
	return f.entries, nil
}

// ─── Router helper ────────────────────────────────────────────────────────────

func buildAuditRouter(s store.Store, principal *authz.Principal) *gin.Engine {
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
	r.GET("/v1/audit", h.GetAudit)
	return r
}

// sampleAuditEntry returns a minimal valid AuditEntry for use in assertions.
func sampleAuditEntry(id int64, action string) *domain.AuditEntry {
	spaceID := "sp-001"
	return &domain.AuditEntry{
		ID:        id,
		Action:    action,
		SpaceID:   &spaceID,
		CreatedAt: time.Now().UTC(),
	}
}

// ─── AC-2: GetAudit ───────────────────────────────────────────────────────────

// TestGetAudit_Returns200_WithItems verifies that the audit endpoint returns 200
// with a non-empty items array when entries exist (AC-2).
func TestGetAudit_Returns200_WithItems(t *testing.T) {
	s := newAuditFakeStore()
	s.entries = []*domain.AuditEntry{
		sampleAuditEntry(1, "space.lifecycle.archive"),
		sampleAuditEntry(2, "space.lifecycle.reopen"),
	}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit with entries must return 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items, ok := resp["items"].([]any)
	if !ok {
		t.Fatalf("response must have an 'items' array, got %T", resp["items"])
	}
	if len(items) != 2 {
		t.Errorf("expected 2 items, got %d", len(items))
	}
}

// TestGetAudit_ResponseShape verifies that each entry in the response has the
// required fields (id, action, created_at) and no secret fields (AC-2, NFR-6, FR-14).
func TestGetAudit_ResponseShape(t *testing.T) {
	s := newAuditFakeStore()
	s.entries = []*domain.AuditEntry{sampleAuditEntry(42, "space.lifecycle.archive")}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items := resp["items"].([]any)
	entry := items[0].(map[string]any)

	if entry["id"] == nil {
		t.Error("audit entry must have 'id' field")
	}
	if entry["action"] != "space.lifecycle.archive" {
		t.Errorf("entry.action must be 'space.lifecycle.archive', got %v", entry["action"])
	}
	if entry["created_at"] == nil {
		t.Error("audit entry must have 'created_at' field")
	}
	// Verify no raw secret fields appear.
	forbiddenFields := []string{"access_token", "bot_token", "password", "secret"}
	body := w.Body.String()
	for _, field := range forbiddenFields {
		if contains(body, field) {
			t.Errorf("response must not contain secret field %q", field)
		}
	}
}

// TestGetAudit_EmptyResult_Returns200 verifies that GET /audit returns 200 with
// an empty items array when no entries match (AC-2).
func TestGetAudit_EmptyResult_Returns200(t *testing.T) {
	s := newAuditFakeStore()
	// entries slice is nil — store returns empty list.

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("empty audit log must still return 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items := resp["items"].([]any)
	if len(items) != 0 {
		t.Errorf("empty result must return empty items array, got %d items", len(items))
	}
}

// TestGetAudit_NonControlPlane_Returns403 verifies that a non-control-plane principal
// cannot read the audit log (AC-2 auth guard).
func TestGetAudit_NonControlPlane_Returns403(t *testing.T) {
	s := newAuditFakeStore()

	r := buildAuditRouter(s, spacesCollaboratorPrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-control-plane must get 403, got %d", w.Code)
	}
}

// TestGetAudit_Filters_PassedToStore verifies that query params (merchant_id, space_id,
// action) are forwarded to the store as filter parameters (AC-2, FR-14 filter contract).
func TestGetAudit_Filters_PassedToStore(t *testing.T) {
	s := newAuditFakeStore()
	s.entries = []*domain.AuditEntry{sampleAuditEntry(1, "space.lifecycle.archive")}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet,
		"/v1/audit?merchant_id=m-001&space_id=sp-001&action=space.lifecycle.archive", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	if s.lastParams.MerchantID == nil || *s.lastParams.MerchantID != "m-001" {
		t.Errorf("merchant_id filter must be forwarded, got %v", s.lastParams.MerchantID)
	}
	if s.lastParams.SpaceID == nil || *s.lastParams.SpaceID != "sp-001" {
		t.Errorf("space_id filter must be forwarded, got %v", s.lastParams.SpaceID)
	}
	if s.lastParams.Action == nil || *s.lastParams.Action != "space.lifecycle.archive" {
		t.Errorf("action filter must be forwarded, got %v", s.lastParams.Action)
	}
}

// TestGetAudit_Pagination_NextCursorSet verifies that next_cursor is set when the
// store returns a full page (AC-2 cursor pagination).
func TestGetAudit_Pagination_NextCursorSet(t *testing.T) {
	s := newAuditFakeStore()
	// Fill exactly the page limit (50) so next_cursor should be set.
	for i := int64(1); i <= 50; i++ {
		s.entries = append(s.entries, sampleAuditEntry(i, "space.lifecycle.archive"))
	}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["next_cursor"] == nil {
		t.Error("next_cursor must be set when the store returns a full page")
	}
}

// TestGetAudit_Pagination_NextCursorNilForPartialPage verifies that next_cursor is
// nil when the store returns fewer entries than the page limit (last page).
func TestGetAudit_Pagination_NextCursorNilForPartialPage(t *testing.T) {
	s := newAuditFakeStore()
	// Only 3 entries — clearly below the 50-entry page limit.
	for i := int64(1); i <= 3; i++ {
		s.entries = append(s.entries, sampleAuditEntry(i, "space.lifecycle.archive"))
	}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["next_cursor"] != nil {
		t.Errorf("next_cursor must be nil for partial page, got %v", resp["next_cursor"])
	}
}

// TestGetAudit_SinceFilter_ForwardedToStore verifies that the `since` query param
// is forwarded to the store as the Since filter (AC-2, FR-14 filter contract).
func TestGetAudit_SinceFilter_ForwardedToStore(t *testing.T) {
	s := newAuditFakeStore()
	s.entries = []*domain.AuditEntry{sampleAuditEntry(1, "space.lifecycle.archive")}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	since := "2026-06-01T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, "/v1/audit?since="+since, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if s.lastParams.Since == nil || *s.lastParams.Since != since {
		t.Errorf("since filter must be forwarded to store, got %v", s.lastParams.Since)
	}
}

// TestGetAudit_NewestFirstOrdering verifies that the response items are ordered
// newest-first (highest id first) as returned by the store (AC-2, FR-14).
//
// The handler preserves the store's ordering — the store is the authority for
// newest-first. We seed entries with descending IDs to simulate the expected
// store output and confirm the handler does not re-sort or reverse the list.
func TestGetAudit_NewestFirstOrdering(t *testing.T) {
	s := newAuditFakeStore()
	// Three entries in newest-first order (as the store would return them).
	s.entries = []*domain.AuditEntry{
		sampleAuditEntry(30, "space.lifecycle.archive"),  // newest
		sampleAuditEntry(20, "space.lifecycle.reopen"),   // middle
		sampleAuditEntry(10, "space.provision.complete"), // oldest
	}

	r := buildAuditRouter(s, spacesControlPlanePrincipal())
	req := httptest.NewRequest(http.MethodGet, "/v1/audit", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	items, ok := resp["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("expected 3 items, got %v", resp["items"])
	}

	// The handler must not re-sort — entries must appear in store order (newest first).
	ids := make([]float64, len(items))
	for i, it := range items {
		entry := it.(map[string]any)
		ids[i] = entry["id"].(float64)
	}
	if ids[0] < ids[1] || ids[1] < ids[2] {
		t.Errorf("items must be newest-first (ids descending), got %v", ids)
	}
}

// ─── Helper ───────────────────────────────────────────────────────────────────

// contains reports whether substr appears in s (case-sensitive).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && len(s) > 0 && len(substr) > 0 &&
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}()
}
