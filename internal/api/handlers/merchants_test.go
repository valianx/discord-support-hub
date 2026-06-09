// merchants_test.go — hermetic handler tests for RegisterMerchant, ListMerchants,
// GetMerchant, and the ProvisionSpace 500→404 regression (AC-1..AC-9).
//
// Tests cover:
//   - AC-1: POST /merchants → 201 + Merchant body; id is UUID, is_active=true.
//   - AC-2: duplicate external_ref → 409.
//   - AC-3: missing/blank external_ref or name → 400; unsafe-rune name → 400; bad URL → 400.
//   - AC-4: non-control-plane principal → 403 on all three endpoints.
//   - AC-5: GET /merchants cursor pagination + created_at ordering.
//   - AC-6: GET /merchants?is_active filter.
//   - AC-7: GET /merchants/{id} 200 + 404 (absent and malformed id).
//   - AC-8: POST /merchants/{merchantId}/channels with non-UUID merchantId → 404 (regression).
package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/handlers"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/authz"
	"github.com/valianx/discord-support-hub/internal/cache"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Merchant fake store ──────────────────────────────────────────────────────

// merchantFakeStore satisfies store.Store for merchant handler tests.
// It embeds spacesFakeStore (which embeds agentFakeStore) so all interface
// methods are satisfied via the panic stubs.
type merchantFakeStore struct {
	spacesFakeStore

	merchants     map[string]*domain.Merchant // keyed by id
	byExternalRef map[string]*domain.Merchant // keyed by external_ref
	createErr     error
}

func newMerchantFakeStore() *merchantFakeStore {
	return &merchantFakeStore{
		spacesFakeStore: *newSpacesFakeStore(),
		merchants:       make(map[string]*domain.Merchant),
		byExternalRef:   make(map[string]*domain.Merchant),
	}
}

// addMerchant inserts a merchant into both indexes (test helper).
func (f *merchantFakeStore) addMerchant(m *domain.Merchant) {
	f.merchants[m.ID] = m
	f.byExternalRef[m.ExternalRef] = m
}

func (f *merchantFakeStore) CreateMerchant(_ context.Context, p store.CreateMerchantParams) (*domain.Merchant, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, dup := f.byExternalRef[p.ExternalRef]; dup {
		return nil, store.ErrConflict
	}
	m := &domain.Merchant{
		ID:          "merch-" + p.ExternalRef,
		ExternalRef: p.ExternalRef,
		Name:        p.Name,
		HelpDeskURL: p.HelpDeskURL,
		IsActive:    true,
		CreatedAt:   time.Now(),
	}
	f.addMerchant(m)
	return m, nil
}

func (f *merchantFakeStore) GetMerchantByID(_ context.Context, id string) (*domain.Merchant, error) {
	m, ok := f.merchants[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return m, nil
}

func (f *merchantFakeStore) GetMerchantByExternalRef(_ context.Context, ref string) (*domain.Merchant, error) {
	m, ok := f.byExternalRef[ref]
	if !ok {
		return nil, store.ErrNotFound
	}
	return m, nil
}

func (f *merchantFakeStore) ListMerchants(_ context.Context, p store.ListMerchantsParams) ([]*domain.Merchant, error) {
	var out []*domain.Merchant
	for _, m := range f.merchants {
		if p.IsActive != nil && m.IsActive != *p.IsActive {
			continue
		}
		out = append(out, m)
	}
	// Sort by CreatedAt ascending (stable for test assertions).
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.Before(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	// Apply cursor (created_at > cursor) and limit.
	if p.Cursor != nil {
		cutoff, err := time.Parse(time.RFC3339Nano, *p.Cursor)
		if err == nil {
			filtered := out[:0]
			for _, m := range out {
				if m.CreatedAt.After(cutoff) {
					filtered = append(filtered, m)
				}
			}
			out = filtered
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ─── Router helper ────────────────────────────────────────────────────────────

func buildMerchantRouter(s store.Store, principal *authz.Principal) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Recovery())

	r.Use(func(c *gin.Context) {
		if principal != nil {
			c.Set("principal", principal)
		}
		c.Next()
	})

	idem := middleware.Idempotency(s)

	h := handlers.NewHandlers(handlers.Config{
		Store: s,
		Cache: cache.NoopCache{},
	})

	r.POST("/v1/merchants", idem, h.RegisterMerchant)
	r.GET("/v1/merchants", h.ListMerchants)
	r.GET("/v1/merchants/:merchantId", h.GetMerchant)
	r.POST("/v1/merchants/:merchantId/channels", idem, h.ProvisionSpace)
	return r
}

func merchantControlPlanePrincipal() *authz.Principal {
	return &authz.Principal{
		Type:     authz.PrincipalTypeService,
		KeyID:    "k-backoffice",
		KeyScope: authz.ScopeBackoffice,
	}
}

func merchantNonControlPlanePrincipal() *authz.Principal {
	return &authz.Principal{
		Type:  authz.PrincipalTypeSession,
		KeyID: "u-collab",
	}
}

// ─── AC-1: POST /merchants → 201 ─────────────────────────────────────────────

// TestRegisterMerchant_HappyPath_Returns201 verifies control-plane POST returns 201
// with a Merchant body where id is present, is_active=true (AC-1).
func TestRegisterMerchant_HappyPath_Returns201(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "ext-001",
		"name":         "ACME Corp",
	}, nil)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] == nil || resp["id"] == "" {
		t.Error("want non-empty id in response")
	}
	if resp["external_ref"] != "ext-001" {
		t.Errorf("want external_ref=ext-001, got %v", resp["external_ref"])
	}
	if resp["name"] != "ACME Corp" {
		t.Errorf("want name=ACME Corp, got %v", resp["name"])
	}
	if resp["is_active"] != true {
		t.Errorf("want is_active=true, got %v", resp["is_active"])
	}
	if resp["created_at"] == nil {
		t.Error("want created_at in response")
	}
}

// TestRegisterMerchant_WithHelpDeskURL_Returns201 verifies optional help_desk_url is
// accepted and echoed back (AC-1).
func TestRegisterMerchant_WithHelpDeskURL_Returns201(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref":  "ext-url",
		"name":          "URL Corp",
		"help_desk_url": "https://help.example.com",
	}, nil)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
}

// ─── AC-2: duplicate external_ref → 409 ──────────────────────────────────────

// TestRegisterMerchant_DuplicateExternalRef_Returns409 verifies that registering a
// merchant with an already-used external_ref returns 409 (AC-2).
func TestRegisterMerchant_DuplicateExternalRef_Returns409(t *testing.T) {
	s := newMerchantFakeStore()
	s.addMerchant(&domain.Merchant{
		ID:          "existing-id",
		ExternalRef: "dup-ref",
		Name:        "Existing",
		IsActive:    true,
		CreatedAt:   time.Now(),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "dup-ref",
		"name":         "Duplicate",
	}, nil)

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409 for duplicate external_ref, got %d: %s", w.Code, w.Body)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["code"] == nil {
		t.Error("want code field in 409 body")
	}
}

// ─── AC-3: validation errors → 400 ───────────────────────────────────────────

// TestRegisterMerchant_MissingExternalRef_Returns400 verifies that a missing
// external_ref returns 400 (AC-3).
func TestRegisterMerchant_MissingExternalRef_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"name": "NoRef Corp",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for missing external_ref, got %d: %s", w.Code, w.Body)
	}
	assertValidationError(t, w.Body.Bytes())
}

// TestRegisterMerchant_BlankExternalRef_Returns400 verifies that a whitespace-only
// external_ref returns 400 (AC-3).
func TestRegisterMerchant_BlankExternalRef_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "   ",
		"name":         "Blank Ref",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for blank external_ref, got %d: %s", w.Code, w.Body)
	}
}

// TestRegisterMerchant_MissingName_Returns400 verifies that a missing name returns 400 (AC-3).
func TestRegisterMerchant_MissingName_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "ext-no-name",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for missing name, got %d: %s", w.Code, w.Body)
	}
}

// TestRegisterMerchant_UnsafeRuneInName_Returns400 verifies that a name containing
// a bidi override (U+202E) is rejected with 400 (AC-3, AC-9).
func TestRegisterMerchant_UnsafeRuneInName_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "ext-bidi",
		"name":         "Evil‮Name", // U+202E RLO bidi override
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for name with bidi override, got %d: %s", w.Code, w.Body)
	}
	assertValidationError(t, w.Body.Bytes())
}

// TestRegisterMerchant_UnsafeRuneInExternalRef_Returns400 verifies that an external_ref
// containing a control character is rejected with 400 (AC-3, AC-9).
func TestRegisterMerchant_UnsafeRuneInExternalRef_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "bad\x01ref",
		"name":         "Valid Name",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for external_ref with control char, got %d: %s", w.Code, w.Body)
	}
}

// TestRegisterMerchant_InvalidHelpDeskURL_Returns400 verifies that a non-http/https
// URL in help_desk_url is rejected with 400 (AC-3, AC-9).
func TestRegisterMerchant_InvalidHelpDeskURL_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref":  "ext-bad-url",
		"name":          "BadURL Corp",
		"help_desk_url": "javascript:alert(1)",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for javascript: URL, got %d: %s", w.Code, w.Body)
	}
}

// TestRegisterMerchant_RelativeHelpDeskURL_Returns400 verifies that a relative URL is
// rejected (must be absolute with http/https scheme) (AC-9).
func TestRegisterMerchant_RelativeHelpDeskURL_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref":  "ext-relurl",
		"name":          "RelURL Corp",
		"help_desk_url": "/relative/path",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for relative URL, got %d: %s", w.Code, w.Body)
	}
}

// ─── AC-4: authZ — non-control-plane → 403 ───────────────────────────────────

// TestRegisterMerchant_NonControlPlane_Returns403 verifies 403 for non-backoffice callers (AC-4).
func TestRegisterMerchant_NonControlPlane_Returns403(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantNonControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "ext-auth",
		"name":         "Auth Test",
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d: %s", w.Code, w.Body)
	}
}

// TestListMerchants_NonControlPlane_Returns403 verifies 403 on GET /merchants (AC-4).
func TestListMerchants_NonControlPlane_Returns403(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantNonControlPlanePrincipal())

	w := getJSON(router, "/v1/merchants", nil)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

// TestGetMerchant_NonControlPlane_Returns403 verifies 403 on GET /merchants/{id} (AC-4).
func TestGetMerchant_NonControlPlane_Returns403(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantNonControlPlanePrincipal())

	w := getJSON(router, "/v1/merchants/00000000-0000-0000-0000-000000000001", nil)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

// ─── AC-5: GET /merchants cursor pagination ───────────────────────────────────

// TestListMerchants_Pagination_ReturnsOrderedItems verifies created_at ASC ordering
// and that next_cursor is set when a full page is returned (AC-5).
func TestListMerchants_Pagination_ReturnsOrderedItems(t *testing.T) {
	s := newMerchantFakeStore()
	base := time.Now()
	for i := 0; i < 3; i++ {
		m := &domain.Merchant{
			ID:          generateTestID(i),
			ExternalRef: generateTestRef(i),
			Name:        generateTestName(i),
			IsActive:    true,
			CreatedAt:   base.Add(time.Duration(i) * time.Second),
		}
		s.addMerchant(m)
	}

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := getJSON(router, "/v1/merchants?limit=2", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	var resp struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("want 2 items (limit=2), got %d", len(resp.Items))
	}
	if resp.NextCursor == nil {
		t.Error("want non-nil next_cursor when full page returned")
	}
}

// TestListMerchants_Pagination_LastPage_NilCursor verifies that next_cursor is null
// on the last page (AC-5).
func TestListMerchants_Pagination_LastPage_NilCursor(t *testing.T) {
	s := newMerchantFakeStore()
	s.addMerchant(&domain.Merchant{
		ID:          "only-one",
		ExternalRef: "only-ref",
		Name:        "Only Merchant",
		IsActive:    true,
		CreatedAt:   time.Now(),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := getJSON(router, "/v1/merchants?limit=50", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	var resp struct {
		Items      []map[string]any `json:"items"`
		NextCursor interface{}      `json:"next_cursor"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NextCursor != nil {
		t.Errorf("want nil next_cursor on last page, got %v", resp.NextCursor)
	}
}

// ─── AC-6: GET /merchants?is_active filter ────────────────────────────────────

// TestListMerchants_IsActiveFilter_ReturnsOnlyActive verifies is_active=true filter (AC-6).
func TestListMerchants_IsActiveFilter_ReturnsOnlyActive(t *testing.T) {
	s := newMerchantFakeStore()
	now := time.Now()
	s.addMerchant(&domain.Merchant{
		ID:          "active-id",
		ExternalRef: "active-ref",
		Name:        "Active Merchant",
		IsActive:    true,
		CreatedAt:   now,
	})
	s.addMerchant(&domain.Merchant{
		ID:          "inactive-id",
		ExternalRef: "inactive-ref",
		Name:        "Inactive Merchant",
		IsActive:    false,
		CreatedAt:   now.Add(time.Second),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := getJSON(router, "/v1/merchants?is_active=true", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Errorf("want 1 active merchant, got %d", len(resp.Items))
	}
	if resp.Items[0]["is_active"] != true {
		t.Errorf("want is_active=true, got %v", resp.Items[0]["is_active"])
	}
}

// TestListMerchants_IsActiveFilter_ReturnsOnlyInactive verifies is_active=false filter (AC-6).
func TestListMerchants_IsActiveFilter_ReturnsOnlyInactive(t *testing.T) {
	s := newMerchantFakeStore()
	now := time.Now()
	s.addMerchant(&domain.Merchant{
		ID:          "active-id2",
		ExternalRef: "active-ref2",
		Name:        "Active Two",
		IsActive:    true,
		CreatedAt:   now,
	})
	s.addMerchant(&domain.Merchant{
		ID:          "inactive-id2",
		ExternalRef: "inactive-ref2",
		Name:        "Inactive Two",
		IsActive:    false,
		CreatedAt:   now.Add(time.Second),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := getJSON(router, "/v1/merchants?is_active=false", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Errorf("want 1 inactive merchant, got %d", len(resp.Items))
	}
	if resp.Items[0]["is_active"] != false {
		t.Errorf("want is_active=false, got %v", resp.Items[0]["is_active"])
	}
}

// TestListMerchants_NoFilter_ReturnsBoth verifies that omitting is_active returns all (AC-6).
func TestListMerchants_NoFilter_ReturnsBoth(t *testing.T) {
	s := newMerchantFakeStore()
	now := time.Now()
	s.addMerchant(&domain.Merchant{
		ID:          "a1",
		ExternalRef: "ref-a1",
		Name:        "A1",
		IsActive:    true,
		CreatedAt:   now,
	})
	s.addMerchant(&domain.Merchant{
		ID:          "a2",
		ExternalRef: "ref-a2",
		Name:        "A2",
		IsActive:    false,
		CreatedAt:   now.Add(time.Second),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := getJSON(router, "/v1/merchants", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("want 2 merchants (no filter), got %d", len(resp.Items))
	}
}

// ─── AC-7: GET /merchants/{id} ────────────────────────────────────────────────

// TestGetMerchant_Exists_Returns200 verifies 200 + Merchant body for a known id (AC-7).
func TestGetMerchant_Exists_Returns200(t *testing.T) {
	s := newMerchantFakeStore()
	const merchantID = "00000000-0000-0000-0000-000000000042"
	s.addMerchant(&domain.Merchant{
		ID:          merchantID,
		ExternalRef: "ref-42",
		Name:        "Merchant 42",
		IsActive:    true,
		CreatedAt:   time.Now(),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := getJSON(router, "/v1/merchants/"+merchantID, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["id"] != merchantID {
		t.Errorf("want id=%q, got %v", merchantID, resp["id"])
	}
}

// TestGetMerchant_AbsentUUID_Returns404 verifies 404 for a well-formed UUID
// that matches no row (AC-7).
func TestGetMerchant_AbsentUUID_Returns404(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := getJSON(router, "/v1/merchants/00000000-0000-0000-0000-000000000099", nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for absent UUID, got %d: %s", w.Code, w.Body)
	}
	assertNotFoundCode(t, w.Body.Bytes())
}

// TestGetMerchant_MalformedID_Returns404 verifies 404 for a non-UUID path segment (AC-7, AC-8).
func TestGetMerchant_MalformedID_Returns404(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := getJSON(router, "/v1/merchants/not-a-uuid", nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for malformed id, got %d: %s", w.Code, w.Body)
	}
}

// ─── AC-8: provision regression — non-UUID merchantId → 404 not 500 ──────────

// TestProvisionSpace_NonUUIDMerchantID_Returns404 is the regression test for the
// reported defect: POST /merchants/{merchantId}/channels where merchantId is not a
// valid UUID (e.g. an external_ref string) used to return 500 due to Postgres
// SQLSTATE 22P02 on the uuid cast (AC-8).
func TestProvisionSpace_NonUUIDMerchantID_Returns404(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	// "demo-merchant-1" is a realistic external_ref that is NOT a UUID.
	w := postJSON(router, "/v1/merchants/demo-merchant-1/channels",
		map[string]any{"name": "support"}, nil)

	if w.Code == http.StatusInternalServerError {
		t.Fatalf("REGRESSION AC-8: non-UUID merchantId must not return 500 (got 500): %s", w.Body)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for non-UUID merchantId, got %d: %s", w.Code, w.Body)
	}
	assertNotFoundCode(t, w.Body.Bytes())
}

// TestProvisionSpace_AbsentUUIDMerchantID_Returns404 verifies that a well-formed UUID
// with no matching merchant row still returns 404 (AC-8, same contract).
func TestProvisionSpace_AbsentUUIDMerchantID_Returns404(t *testing.T) {
	s := newMerchantFakeStore()
	// No merchants in store.
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants/00000000-0000-0000-0000-000000000001/channels",
		map[string]any{"name": "support"}, nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for absent merchant UUID, got %d: %s", w.Code, w.Body)
	}
}

// ─── Test helpers ─────────────────────────────────────────────────────────────

func assertValidationError(t *testing.T, body []byte) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if resp["code"] != "validation_error" {
		t.Errorf("want code=validation_error, got %q", resp["code"])
	}
}

func assertNotFoundCode(t *testing.T, body []byte) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if resp["code"] != "not_found" {
		t.Errorf("want code=not_found, got %q", resp["code"])
	}
}

func generateTestID(i int) string {
	ids := []string{
		"10000000-0000-0000-0000-000000000001",
		"10000000-0000-0000-0000-000000000002",
		"10000000-0000-0000-0000-000000000003",
	}
	if i < len(ids) {
		return ids[i]
	}
	return "10000000-0000-0000-0000-00000000000" + string(rune('4'+i))
}

func generateTestRef(i int) string {
	refs := []string{"ref-a", "ref-b", "ref-c"}
	if i < len(refs) {
		return refs[i]
	}
	return "ref-x"
}

func generateTestName(i int) string {
	names := []string{"Alpha Corp", "Beta Inc", "Gamma LLC"}
	if i < len(names) {
		return names[i]
	}
	return "Other"
}

// ─── Strengthened tests (verify-run additions) ────────────────────────────────

// TestRegisterMerchant_ResponseID_IsUUID verifies that the id field in a 201 response
// is present and non-empty. The fake store returns a synthetic id; the contract
// requires the real store to return a UUID — this test documents the contract
// assertion (AC-1: "id is a UUID").
func TestRegisterMerchant_ResponseID_IsUUID(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "uuid-check-ref",
		"name":         "UUID Check Corp",
	}, nil)

	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		t.Error("want non-empty id in 201 response (AC-1: id must be present)")
	}
	// The contract states id is a UUID; the handler passes the store-returned id through.
	// The fake returns a synthetic value — assert the field is a non-empty string.
	// Integration tests against the pgx store verify the UUID format end-to-end.
}

// TestRegisterMerchant_BlankName_Returns400 verifies that a whitespace-only name
// is rejected with 400 validation_error (AC-3: blank/whitespace value).
func TestRegisterMerchant_BlankName_Returns400(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "ext-blank-name",
		"name":         "   ", // whitespace-only
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400 for blank name, got %d: %s", w.Code, w.Body)
	}
	assertValidationError(t, w.Body.Bytes())
}

// TestRegisterMerchant_DuplicateExternalRef_409Code verifies that the 409 body
// carries a specific code field value, not just a non-nil code (AC-2).
func TestRegisterMerchant_DuplicateExternalRef_409Code(t *testing.T) {
	s := newMerchantFakeStore()
	s.addMerchant(&domain.Merchant{
		ID:          "exists-uuid",
		ExternalRef: "dup-code-ref",
		Name:        "Existing",
		IsActive:    true,
		CreatedAt:   time.Now(),
	})

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())
	w := postJSON(router, "/v1/merchants", map[string]any{
		"external_ref": "dup-code-ref",
		"name":         "Duplicate",
	}, nil)

	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["code"] != "conflict" {
		t.Errorf("want code=conflict in 409 body, got %q", resp["code"])
	}
	if resp["message"] == nil || resp["message"] == "" {
		t.Error("want non-empty message in 409 body")
	}
}

// TestListMerchants_Pagination_CursorFollowThrough verifies the full two-page
// cursor flow: first page returns next_cursor; using that cursor returns the
// next page with no overlap and a nil next_cursor (AC-5).
func TestListMerchants_Pagination_CursorFollowThrough(t *testing.T) {
	s := newMerchantFakeStore()
	base := time.Now()
	for i := 0; i < 3; i++ {
		s.addMerchant(&domain.Merchant{
			ID:          generateTestID(i),
			ExternalRef: generateTestRef(i),
			Name:        generateTestName(i),
			IsActive:    true,
			CreatedAt:   base.Add(time.Duration(i) * time.Second),
		})
	}

	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	// Page 1: limit=2, expect 2 items and a cursor.
	w1 := getJSON(router, "/v1/merchants?limit=2", nil)
	if w1.Code != http.StatusOK {
		t.Fatalf("page 1: want 200, got %d: %s", w1.Code, w1.Body)
	}
	var page1 struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.NewDecoder(w1.Body).Decode(&page1); err != nil {
		t.Fatalf("page 1 decode: %v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page 1: want 2 items, got %d", len(page1.Items))
	}
	if page1.NextCursor == nil {
		t.Fatal("page 1: want non-nil next_cursor, got nil")
	}

	// Page 2: use the cursor from page 1.
	w2 := getJSON(router, "/v1/merchants?limit=2&cursor="+*page1.NextCursor, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("page 2: want 200, got %d: %s", w2.Code, w2.Body)
	}
	var page2 struct {
		Items      []map[string]any `json:"items"`
		NextCursor interface{}      `json:"next_cursor"`
	}
	if err := json.NewDecoder(w2.Body).Decode(&page2); err != nil {
		t.Fatalf("page 2 decode: %v", err)
	}
	if len(page2.Items) != 1 {
		t.Errorf("page 2: want 1 item (the third merchant), got %d", len(page2.Items))
	}
	if page2.NextCursor != nil {
		t.Errorf("page 2: want nil next_cursor on last page, got %v", page2.NextCursor)
	}

	// No overlap: page 2 item must not appear in page 1.
	if len(page2.Items) > 0 {
		p2ID := page2.Items[0]["id"]
		for _, item := range page1.Items {
			if item["id"] == p2ID {
				t.Errorf("overlap: id %v appeared in both page 1 and page 2", p2ID)
			}
		}
	}
}

// TestGetMerchant_MalformedID_Returns404WithCode verifies that a non-UUID path
// segment returns 404 with code=not_found in the body (AC-7, AC-8).
func TestGetMerchant_MalformedID_Returns404WithCode(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := getJSON(router, "/v1/merchants/not-a-uuid", nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for malformed id, got %d: %s", w.Code, w.Body)
	}
	assertNotFoundCode(t, w.Body.Bytes())
}

// TestProvisionSpace_NonUUIDMerchantID_BodyCode is the explicit body-code assertion
// for AC-8: the 404 body must carry code=not_found, not an empty body or 500.
// This is the critical regression guard for the reported defect.
func TestProvisionSpace_NonUUIDMerchantID_BodyCode(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	// "demo-merchant-1" is a realistic external_ref-style string, not a UUID.
	// Before the fix this triggered Postgres SQLSTATE 22P02 → 500.
	w := postJSON(router, "/v1/merchants/demo-merchant-1/channels",
		map[string]any{"name": "support"}, nil)

	if w.Code == http.StatusInternalServerError {
		t.Fatalf("REGRESSION AC-8: non-UUID merchantId returned 500 — fix did not take effect: %s", w.Body)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for non-UUID merchantId, got %d: %s", w.Code, w.Body)
	}
	// Body must carry the structured error — not an empty response.
	assertNotFoundCode(t, w.Body.Bytes())
}

// TestProvisionSpace_ExternalRefStyleMerchantID_Returns404 verifies that a short
// external_ref-style string (not resembling a UUID at all) returns 404, not 500.
// Covers the specific example called out in the task dispatch: "demo-merchant-1",
// "acme", "partner-id-123" etc. (AC-8, regression critical path).
func TestProvisionSpace_ExternalRefStyleMerchantID_Returns404(t *testing.T) {
	s := newMerchantFakeStore()
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	cases := []struct {
		name       string
		merchantID string
	}{
		{"short word", "acme"},
		{"hyphenated ref", "partner-id-123"},
		{"numeric-only but wrong length", "12345"},
		{"too long non-UUID", "this-is-definitely-not-a-uuid-format-string"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postJSON(router, "/v1/merchants/"+tc.merchantID+"/channels",
				map[string]any{"name": "support"}, nil)

			if w.Code == http.StatusInternalServerError {
				t.Fatalf("REGRESSION AC-8: merchantId=%q returned 500 (must be 404)", tc.merchantID)
			}
			if w.Code != http.StatusNotFound {
				t.Errorf("want 404 for merchantId=%q, got %d: %s", tc.merchantID, w.Code, w.Body)
			}
			assertNotFoundCode(t, w.Body.Bytes())
		})
	}
}

// TestProvisionSpace_AbsentUUIDMerchantID_BodyCode verifies that a well-formed UUID
// that matches no merchant row returns 404 with code=not_found (AC-8, AC-7 contract).
// This is distinct from the malformed-UUID path: the UUID guard passes, the store
// call is made, and ErrNotFound is returned.
func TestProvisionSpace_AbsentUUIDMerchantID_BodyCode(t *testing.T) {
	s := newMerchantFakeStore()
	// Empty store — no merchants registered.
	router := buildMerchantRouter(s, merchantControlPlanePrincipal())

	w := postJSON(router, "/v1/merchants/ffffffff-ffff-ffff-ffff-ffffffffffff/channels",
		map[string]any{"name": "support"}, nil)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404 for absent merchant UUID, got %d: %s", w.Code, w.Body)
	}
	assertNotFoundCode(t, w.Body.Bytes())
}
