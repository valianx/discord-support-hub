// Package middleware_test — idempotency middleware tests (AC-4).
//
// Coverage:
//   - No Idempotency-Key header: pass-through, no store calls.
//   - New key: insert pending record, handler runs, response stored.
//   - Duplicate key + same body: stored response replayed, handler NOT called again.
//   - Duplicate key + different body: 409 Conflict.
package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/valianx/discord-support-hub/internal/api/middleware"
	"github.com/valianx/discord-support-hub/internal/domain"
	"github.com/valianx/discord-support-hub/internal/store"
)

// ─── Fake idempotency store ───────────────────────────────────────────────────

type idemFakeStore struct {
	noopStore
	keys        map[string]*domain.IdempotencyKey
	insertCalls int
}

func newIdemFakeStore() *idemFakeStore {
	return &idemFakeStore{keys: make(map[string]*domain.IdempotencyKey)}
}

func (s *idemFakeStore) InsertIdempotencyKey(_ context.Context, p store.InsertIdempotencyKeyParams) (*domain.IdempotencyKey, error) {
	s.insertCalls++
	if _, exists := s.keys[p.Key]; exists {
		return nil, store.ErrConflict
	}
	ik := &domain.IdempotencyKey{
		Key:         p.Key,
		RequestHash: p.RequestHash,
		Status:      domain.JobStatusPending,
		CreatedAt:   time.Now(),
		ExpiresAt:   p.ExpiresAt,
	}
	s.keys[p.Key] = ik
	return ik, nil
}

func (s *idemFakeStore) GetIdempotencyKey(_ context.Context, key string) (*domain.IdempotencyKey, error) {
	ik, ok := s.keys[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return ik, nil
}

func (s *idemFakeStore) UpdateIdempotencyKeyResponse(_ context.Context, p store.UpdateIdempotencyKeyResponseParams) error {
	ik, ok := s.keys[p.Key]
	if !ok {
		return store.ErrNotFound
	}
	ik.Status = p.Status
	ik.ResponseCode = &p.ResponseCode
	ik.ResponseBody = p.ResponseBody
	ik.JobID = p.JobID
	return nil
}

// ─── Helper ───────────────────────────────────────────────────────────────────

func idemEngine(s store.Store) (*gin.Engine, *bool) {
	gin.SetMode(gin.TestMode)
	handlerCalled := false
	r := gin.New()
	r.Use(middleware.Recovery())
	r.POST("/test", middleware.Idempotency(s), func(c *gin.Context) {
		handlerCalled = true
		c.JSON(http.StatusAccepted, gin.H{"job": "job-id-001"})
	})
	return r, &handlerCalled
}

// ─── No header: pass-through ──────────────────────────────────────────────────

// TestIdempotency_NoHeader_PassThrough verifies that a request without an
// Idempotency-Key header passes through without any store interaction.
func TestIdempotency_NoHeader_PassThrough(t *testing.T) {
	s := newIdemFakeStore()
	r, handlerCalled := idemEngine(s)

	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(`{"name":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	if !*handlerCalled {
		t.Error("handler must be called when no Idempotency-Key header is present")
	}
	if s.insertCalls != 0 {
		t.Errorf("no store inserts expected when header absent, got %d", s.insertCalls)
	}
}

// ─── New key: insert + handler runs ──────────────────────────────────────────

// TestIdempotency_NewKey_InsertsAndRunsHandler verifies that a new Idempotency-Key
// inserts a pending record and lets the handler run.
func TestIdempotency_NewKey_InsertsAndRunsHandler(t *testing.T) {
	s := newIdemFakeStore()
	r, handlerCalled := idemEngine(s)

	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(`{"name":"test"}`))
	req.Header.Set("Idempotency-Key", "key-001")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d", w.Code)
	}
	if !*handlerCalled {
		t.Error("handler must run for a new Idempotency-Key")
	}
	if s.insertCalls != 1 {
		t.Errorf("want 1 insert, got %d", s.insertCalls)
	}
}

// ─── Duplicate key + same body: replay ───────────────────────────────────────

// TestIdempotency_DuplicateKey_SameBody_Replays verifies that a duplicate key with
// the same body replays the stored response and does NOT call the handler again (AC-4).
func TestIdempotency_DuplicateKey_SameBody_Replays(t *testing.T) {
	s := newIdemFakeStore()
	body := `{"name":"test"}`

	// Simulate a stored completed response.
	responseCode := http.StatusAccepted
	s.keys["key-replay"] = &domain.IdempotencyKey{
		Key:          "key-replay",
		RequestHash:  middleware.HashRequestForTest(http.MethodPost, "/test", []byte(body)),
		Status:       domain.JobStatusPending,
		ResponseCode: &responseCode,
		ResponseBody: map[string]any{"job": "job-id-original"},
		CreatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}

	r, handlerCalled := idemEngine(s)

	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", "key-replay")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("want 202 replay, got %d", w.Code)
	}
	if *handlerCalled {
		t.Error("handler must NOT be called on a replay — no second enqueue (AC-4)")
	}
	if w.Header().Get("Idempotency-Replay") != "true" {
		t.Error("Idempotency-Replay header must be set on replay responses")
	}
	// Verify the stored job id is in the response.
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["job"] != "job-id-original" {
		t.Errorf("replayed response must contain the stored job id, got: %v", resp)
	}
}

// ─── Duplicate key + different body: 409 ─────────────────────────────────────

// TestIdempotency_DuplicateKey_DifferentBody_Returns409 verifies that the same
// Idempotency-Key with a different body returns 409 (AC-4).
func TestIdempotency_DuplicateKey_DifferentBody_Returns409(t *testing.T) {
	s := newIdemFakeStore()
	originalBody := `{"name":"original"}`

	// Seed the store with a record for the original body.
	s.keys["key-conflict"] = &domain.IdempotencyKey{
		Key:         "key-conflict",
		RequestHash: middleware.HashRequestForTest(http.MethodPost, "/test", []byte(originalBody)),
		Status:      domain.JobStatusPending,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}

	r, handlerCalled := idemEngine(s)

	// Send the same key but with a DIFFERENT body.
	differentBody := `{"name":"different"}`
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(differentBody))
	req.Header.Set("Idempotency-Key", "key-conflict")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("want 409 for key reuse with different body, got %d: %s", w.Code, w.Body.String())
	}
	if *handlerCalled {
		t.Error("handler must NOT be called when body hash conflicts")
	}

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if resp["code"] != "idempotency_conflict" {
		t.Errorf("want code=idempotency_conflict, got: %v", resp["code"])
	}
}
