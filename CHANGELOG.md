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

[Unreleased]: https://github.com/valianx/discord-support-hub/commits/main
