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

[Unreleased]: https://github.com/valianx/discord-support-hub/commits/main
