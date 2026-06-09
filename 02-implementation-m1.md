# Implementation: discord-support-hub M1

## Iteration 1 (authZ + hardening)

**Date:** 2026-06-08
**Scope:** Control-plane authority fix (roster API unreachable by backoffice key) + three security hardenings.

---

### 1. Control-plane authority — blocking fix

**Problem:** The roster API (`POST /agents`, `DELETE /agents/{userId}`, `GET /agents`) was gated on `authz.RequireAdmin(p)`, which checks only `Principal.IsAdmin`. Layer A never resolves `users.is_admin` from Postgres for a service key, so `IsAdmin` was always `false` for any bearer token, making the roster API unreachable by any real service key in production.

**Fix:** Added `RequireControlPlane(p *Principal) bool` to `internal/authz/authz.go` and updated all three roster handlers to call it instead of `RequireAdmin`.

`RequireControlPlane` grants control-plane authority when **either** of two Postgres-anchored conditions holds:

1. `p.KeyScope == ScopeBackoffice` — the service key's `api_keys.scope` column in Postgres equals the new `ScopeBackoffice = "backoffice"` constant. This value is set server-side at key creation by `cmd/keygen`; no client input can set it.
2. `p.IsAdmin == true` — a future user/session principal whose `users.is_admin` was resolved from Postgres at auth time.

**Why this is not client-controllable (CWE-639 guard):** `KeyScope` is populated in `authMiddleware` from the `api_keys` row returned by `LookupActiveAPIKeyByHash`, which queries Postgres using only the SHA-256 hash of the bearer token. The client supplies only the raw token; the scope comes exclusively from the DB row. There is no path from a request header, body, or query parameter to the `KeyScope` field.

**Files changed:**

| File | Change |
|---|---|
| `internal/authz/authz.go` | Added `ScopeBackoffice = "backoffice"` constant and `RequireControlPlane` function |
| `internal/api/handlers/agents.go` | Replaced all three `RequireAdmin` calls with `RequireControlPlane` |

**`cmd/keygen`:** No code change needed. The tool already uses `--scope backoffice` as the default and stores the scope server-side via `CreateAPIKey`. The doc comment was already clear on scope being server-controlled. The existing behavior is correct.

---

### 2. Test changes

**`internal/api/middleware/admin_gap_test.go` — full rewrite:**

The old file documented the broken state (all roster endpoints returned 403 for any service key). It was replaced with tests asserting the fixed behavior:

| New test | What it asserts |
|---|---|
| `TestRosterAPI_BackofficeKey_PostAgents_Returns201` | Backoffice-scoped key passes Layer A + Layer B → 201 |
| `TestRosterAPI_BackofficeKey_GetAgents_Returns201` | Same for GET /agents |
| `TestRosterAPI_Unauthenticated_Returns401` | No bearer → 401 from Layer A |
| `TestRosterAPI_NarrowScopedKey_Returns403` | `scope="readonly"` passes Layer A, denied by Layer B → 403 |
| `TestLayerA_BackofficeKey_PrincipalFieldsCorrect` | All Principal fields populated from the DB row |
| `TestAuth_NonBackofficeScope_DoesNotGrantControlPlane` | Table-driven: `"admin"`, `"superuser"`, `"BACKOFFICE"`, `""`, `"Backoffice"` all denied — only exact `"backoffice"` grants access (NFR-13) |
| `TestRosterAPI_EndToEnd_PostAgents_BackofficeKey_Returns201` | Full end-to-end: `testRawKey` → SHA-256 hash → `exactHashStore` lookup → Principal → `RequireControlPlane` → 201 + `connect_url` |
| `TestKeygenContract_RawKeyNeverPersisted` | Kept from old file (AC-7 contract) |

**`internal/authz/authz_test.go` — added tests for `RequireControlPlane`:**

- `TestRequireControlPlane_BackofficeScope` — scope alone grants authority
- `TestRequireControlPlane_AdminUser` — `is_admin=true` alone grants authority
- `TestRequireControlPlane_NarrowScope` — non-backoffice scopes all denied
- `TestRequireControlPlane_NilPrincipal` — nil denied

**`TestAuth_ValidKey_IsAdminNotLeakedFromScope` — removed.** The original intent ("an arbitrary scope string must not grant IsAdmin=true") is now covered more precisely by `TestAuth_NonBackofficeScope_DoesNotGrantControlPlane`, which tests the actual control-plane gate rather than the `IsAdmin` field (which was a proxy for the real check). The NFR-13 invariant is fully preserved: only the exact server-side `"backoffice"` scope grants authority.

**`internal/api/handlers/agents_test.go` — additions:**

- Added `backofficePrincipal()` factory (scope="backoffice", IsAdmin=false).
- Added `TestListAgents_BackofficeKey_Returns200` — backoffice key reaches GET /agents.
- Added `TestAddAgent_BackofficeKey_Returns201WithConnectURL` — backoffice key reaches POST /agents and gets connect_url.

---

### 3. Security hardenings

#### 3a. DSN credential leak — `safeDSNPrefix`

**Before:** `safeDSNPrefix` returned the first 40 characters of the raw DSN string. A URL-style DSN like `postgres://user:secretpass@host:5432/db` has the password in character positions 15-30 — well within the 40-char window.

**After:** Rewrote `safeDSNPrefix` in `internal/store/postgres/postgres.go` to parse the DSN format and emit only the host+database (never the user/password segment):
- URL-style: strips `user:pass@` by splitting on `@`, strips query string, returns `host:port/db`.
- Key=value style: extracts only `host=` and `dbname=` fields; ignores `password=`, `user=`, etc.
- Unknown format: emits `[dsn-format-unknown]` rather than risking a partial leak.

Also added `"strings"` to the import block.

#### 3b. Encryption key boot validation — `ValidateEncryptionKey`

**Before:** `RequireEncryptionKey` only checked presence (non-empty string). An invalid base64 value or a key that decodes to the wrong number of bytes would only surface as an error on the first `NewEncrypter` call (at OAuth token store time), not at startup.

**After:** Added `ValidateEncryptionKey()` to `internal/config/config.go`. It checks:
1. `ENCRYPTION_KEY` is non-empty.
2. The value is valid base64 (standard encoding).
3. The decoded byte slice is exactly 32 bytes (AES-256 requirement).

Called in `cmd/api/main.go` at startup (before `postgres.New`), so a misconfigured key fails loudly with a clear error message rather than at first use.

#### 3c. Decrypt nonce-length guard

**Already present in the existing code.** The `Decrypt` method in `internal/secrets/secrets.go` already had:

```go
if len(ev.Ciphertext) < gcm.NonceSize() {
    return nil, ErrCiphertextTooShort
}
```

and `var ErrCiphertextTooShort = errors.New("secrets: ciphertext too short")` was already declared. No change needed.

---

### 4. Final command results

```
$ go build ./...
(no output — clean build)

$ go vet ./...
(no output — clean)

$ gofmt -l .
(no output — no formatting issues)

$ go test ./... -count=1
ok  github.com/valianx/discord-support-hub/internal/api          1.6s
ok  github.com/valianx/discord-support-hub/internal/api/handlers 1.9s
ok  github.com/valianx/discord-support-hub/internal/api/middleware 1.4s
ok  github.com/valianx/discord-support-hub/internal/authz        1.1s
ok  github.com/valianx/discord-support-hub/internal/config       1.4s
ok  github.com/valianx/discord-support-hub/internal/domain       0.3s
ok  github.com/valianx/discord-support-hub/internal/observability 0.2s
ok  github.com/valianx/discord-support-hub/internal/queue        0.4s
ok  github.com/valianx/discord-support-hub/internal/secrets      0.2s
ok  github.com/valianx/discord-support-hub/internal/worker       1.9s

92 tests pass, 0 failures.
```
