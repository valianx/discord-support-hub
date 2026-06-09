# Changelog

All notable changes to this project are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **M0 — Skeleton.** Empty-but-running scaffold for the service (step 1 of the v1 work plan):
  - Go module `github.com/valianx/discord-support-hub` (Go 1.26) with the package layout from `docs/02-architecture.md §8`.
  - Entrypoints: `cmd/api` (Gin HTTP server, graceful shutdown), `cmd/worker` (asynq server over Valkey with the `provision`/`membership`/`reconcile`/`marking` queue topology), `cmd/migrate` (idempotent SQL migration runner), `cmd/reconciler` (stub).
  - `internal/` adapters behind interfaces: config, observability (structured slog + secret redaction + `/livez` and `/readyz` health checks), store (Postgres via pgx), discord (discordgo session), queue (asynq client + task kinds), and stubs for authz, ratelimit, lock, cache, oauth, reconcile, secrets, worker. Business API handlers return `501 Not Implemented`.
  - `internal/secrets`: AES-256-GCM encrypt/decrypt with key versioning, and log redaction of secret fields.
  - `migrations/0001_init.sql`: the full PostgreSQL schema (`docs/data-model.sql`).
  - `deploy/Dockerfile` (multi-stage, CGO-off static binary, distroless, non-root) and `deploy/docker-compose.yml` (api + worker + postgres + valkey).
  - CI (`.github/workflows/ci.yml`): build, vet, gofmt check, and race-enabled tests on push/PR.
  - `docs/test-guild-setup.md`: guide to create a test Discord server, bot application, token, and OAuth2 redirect.
  - Hermetic test suite (37 tests): config defaults, AES-GCM round-trip, log redaction, `domain` import-boundary, asynq queue topology/priorities, and a miniredis-backed enqueue→consume round-trip across all four queues.
- **M1 — Identity & authZ core** (step 2 of the v1 work plan): Postgres as the authorization source of truth.
  - Postgres store implementations (pgx) for `merchants`, `users`, and `api_keys`, honoring the schema constraints.
  - **Two-layer authZ:** Layer A authenticates a service API key (opaque bearer, SHA-256 hashed, looked up in `api_keys`, `Principal` injected, 401 before any handler, fail-closed on store error); Layer B authorizes against Postgres only (never the Discord role, NFR-13).
  - **Control-plane authority:** roster operations require a `backoffice`-scoped service key (a server-side `api_keys.scope` value, not client-controllable) or a future `is_admin` user — reconciling the §5.1/§5.2 design (`docs/02-architecture.md §5.2`).
  - **Roster API** (`POST /agents` → 201 + one-time `connect_url`; `DELETE /agents/{userId}` → 202; `GET /agents`), all control-plane-gated.
  - **Agent role projection + reconcile** worker (`project_agent_role`): assigns the Agent role once an agent has joined; re-asserts a missing role and removes the role from a non-agent; `MANAGE_ROLES` reserved to the bot.
  - `cmd/keygen`: mint a backoffice service key — prints the raw key once, stores only its hash.
  - Hardening: DB DSN credentials never logged; `ENCRYPTION_KEY` validated at boot; `secrets.Decrypt` guards short ciphertext.
- **M2a — Async provisioning foundation** (first half of step 3): the rate-limit / idempotency / locking / job machinery the provisioning vertical sits on.
  - Distributed token-bucket rate limiter over Valkey (atomic Lua), global + per-route, seeded and penalized from Discord rate-limit headers, with clamping against hostile header values.
  - Distributed locks (Valkey `SET NX` + fencing-token compare-and-delete release) keyed per space / per merchant.
  - Three-layer idempotency: edge-replay middleware (request-hash guard, 409 on body mismatch) + asynq `TaskID`/`Unique` + a transactional outbox committed atomically with the desired-state change.
  - Outbox **relay** with exactly-once enqueue (an `asynq` task-ID conflict is treated as already-enqueued, not a failure loop).
  - asynq retry/backoff: `RetryDelayFunc` honors `Retry-After`; rate-limit retries are excluded from the failure counter; `SkipRetry` for terminal/fail-closed errors.
  - Jobs mirror: `GET /jobs/{id}` reads authoritative status from Postgres (never Valkey), gated on control-plane authority.
  - Read-through Valkey cache helper (TTL + invalidation).
- **M2b — Provisioning vertical** (completes step 3 / M2): a merchant channel is provisioned end-to-end, fail-closed, through the M2a machinery.
  - `POST /merchants/{id}/channels` — validates (name length/charset, category snowflake), writes the desired `spaces` row + `outbox` row in one transaction, returns `202` + `Location` + job, stores the `202` for idempotent replay. Control-plane gated (FR-1).
  - **Fail-closed provisioning worker** — acquires the per-merchant lock + a rate-limit token, creates the channel **already denied to `@everyone`** (the deny-`VIEW_CHANNEL` overwrite is in the *initial* create — no visible window, NFR-4), applies the category-level allow to the **configured Agent role** (never `@everyone`), persists `discord_channel_id` + `acl_state='applied'`. Any ACL failure → `SkipRetry` + `degraded`/`failed` + audit, never world-readable (FR-3, FR-5).
  - **Isolation guard** — the handler refuses to grant category visibility when the Agent role is unset or equals the guild id (`@everyone`), protecting the multi-tenant isolation invariant (NFR-5).
  - `GET /channels` and `GET /channels/{id}` served from cache (generation-token invalidation), control-plane gated (FR-10).
- **M3 — Membership, OAuth2 entry & isolation** (step 4 of the v1 build): external collaborators join via OAuth2 and are isolated per space.
  - Collaborator invite (`POST /channels/{id}/collaborators`) → OAuth2 `guilds.join` add-if-needed + a per-user permission overwrite as the **only** access grant; expel (`DELETE …?scope=channel|server`); both audited. Control-plane gated; collaborators cannot invite (FR-4, FR-19, FR-20).
  - **OAuth2 "Connect with Discord"** callback (`GET /oauth/discord/callback`): HMAC-signed single-use `state` (CSRF), server-side code exchange, AES-GCM-encrypted token at rest, identity bound to the verified Discord user (one Discord id ↔ one hub user; 409 on conflict), pending invites applied on connect (FR-22, NFR-6).
  - **No-invites lockdown** — `CREATE_INSTANT_INVITE` reserved to the bot (NFR-14).
  - Directory (`GET /directory`, bidirectional), space members (`GET /channels/{id}/members`), channels-by-collaborator (`GET /collaborators/{userId}/channels`) (FR-17, FR-18, FR-21).
  - **Reconcile teeth** — any Discord overwrite not backed by a `space_members` row is revoked (Postgres wins, NFR-5).
  - **Multi-tenant isolation test suite** (`test/integration/`) wired as a merge gate (NFR-5).
  - Hardening closed: `secrets.Decrypt` nonce-length guard, `RequireAgentRoleID()` at boot, Unicode control-char rejection in channel names.

[Unreleased]: https://github.com/valianx/discord-support-hub/commits/main
