// nonce_store.go provides a Valkey-backed nonceStore for OAuth2 CSRF state tokens.
// Each nonce is stored as a Valkey key with a short TTL (stateTTL = 15 min).
// ConsumeNonce uses a Lua GETDEL so the nonce is deleted atomically on first read,
// preventing replay attacks even under concurrent requests.
package oauth

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const nonceKeyPrefix = "oauth:nonce:"

// ValkeyNonceStore is the production nonceStore backed by Valkey (Redis-compatible).
type ValkeyNonceStore struct {
	rdb *redis.Client
}

// NewValkeyNonceStore creates a ValkeyNonceStore backed by the given redis client.
func NewValkeyNonceStore(rdb *redis.Client) *ValkeyNonceStore {
	return &ValkeyNonceStore{rdb: rdb}
}

// SetNonce stores a nonce → userID binding with the given TTL.
func (v *ValkeyNonceStore) SetNonce(ctx context.Context, nonce, userID string, ttl time.Duration) error {
	key := nonceKeyPrefix + nonce
	if err := v.rdb.Set(ctx, key, userID, ttl).Err(); err != nil {
		return fmt.Errorf("oauth: set nonce: %w", err)
	}
	return nil
}

// getdelScript atomically GETs and DELs a key (single-use consumption).
// Equivalent to Redis 6.2+ GETDEL, but works on older versions via Lua.
var getdelScript = redis.NewScript(`
local v = redis.call("GET", KEYS[1])
if v then redis.call("DEL", KEYS[1]) end
return v
`)

// ConsumeNonce retrieves and deletes the nonce from Valkey (single-use).
// Returns ("", false, nil) when the nonce does not exist or has expired.
func (v *ValkeyNonceStore) ConsumeNonce(ctx context.Context, nonce string) (string, bool, error) {
	key := nonceKeyPrefix + nonce
	result, err := getdelScript.Run(ctx, v.rdb, []string{key}).Text()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("oauth: consume nonce: %w", err)
	}
	return result, true, nil
}

// ─── In-memory nonceStore for tests ─────────────────────────────────────────

// MemNonceStore is a simple in-memory nonceStore for use in unit tests.
// Not safe for concurrent use in production; tests run single-threaded.
type MemNonceStore struct {
	nonces map[string]nonceEntry
}

type nonceEntry struct {
	userID    string
	expiresAt time.Time
}

// NewMemNonceStore returns an empty MemNonceStore.
func NewMemNonceStore() *MemNonceStore {
	return &MemNonceStore{nonces: make(map[string]nonceEntry)}
}

func (m *MemNonceStore) SetNonce(_ context.Context, nonce, userID string, ttl time.Duration) error {
	m.nonces[nonce] = nonceEntry{userID: userID, expiresAt: time.Now().Add(ttl)}
	return nil
}

func (m *MemNonceStore) ConsumeNonce(_ context.Context, nonce string) (string, bool, error) {
	e, ok := m.nonces[nonce]
	if !ok || time.Now().After(e.expiresAt) {
		delete(m.nonces, nonce)
		return "", false, nil
	}
	delete(m.nonces, nonce)
	return e.userID, true, nil
}
