# Discord Support Hub

**Open-source, API-driven service that provisions and governs isolated private support spaces on Discord — one per merchant — with real per-channel access control.**

An internal team (*Agents*) supports external collaborators (*Collaborators*) in Discord channels that are **invisible by default** and reachable only through API-managed invitations. Each merchant gets a private space; a collaborator can only ever see the spaces they were invited to, never another merchant's.

Design philosophy: **mechanism, not policy.** The service provides the mechanisms — provisioning, ACLs, agent marking, help-desk visibility — with sane defaults and graceful degradation. Business logic stays in the hands of whoever deploys it.

## Status

**v0.1.0-rc** — all v1 milestones (M0→M5) implemented; the `v0.1.0` tag is created after this build PR merges. See [CHANGELOG.md](CHANGELOG.md).

## Why Discord

Real **per-channel ACL** via permission overwrites is exactly the isolation model this needs. Alternatives like Telegram topics share visibility across the whole supergroup, which breaks tenant isolation. Trade-off accepted: conversation data lives in Discord (SaaS); the provisioning service is self-hosted and open-source.

## Core invariants

- **Invisible by default / fail-closed** — every space is born with `deny @everyone → VIEW_CHANNEL`; an ACL failure leaves a space with *no* access, never world-readable.
- **Postgres is the source of truth** — Discord roles and permission overwrites are a projection the bot reconciles. Authorization is always resolved against the Postgres store, never against Discord.
- **No invite links** — server entry is OAuth2 `guilds.join`; `CREATE_INSTANT_INVITE` is reserved to the bot. No human can mint an invite.
- **Multi-tenant isolation is a testable security invariant**, not a convention.
- **Postgres always wins** — the reconciler revokes any Discord access not backed by a Postgres row. Operators must not grant channel access directly in Discord; the next reconcile sweep will revoke it (see *Operator caveat* below).

## Stack

Go + Gin (API) · asynq over [Valkey](https://valkey.io) (async queue, distributed rate limiter, idempotency, locks) · [discordgo](https://github.com/bwmarrin/discordgo) · PostgreSQL (source of truth) · Prometheus metrics.

The synchronous-API / asynchronous-worker split is what makes Discord's rate limits (global + per-route) tractable.

---

## Quickstart

### Prerequisites

- Go 1.26+
- Docker and Docker Compose (for the full local stack)
- A Discord server + bot application (see [docs/test-guild-setup.md](docs/test-guild-setup.md))

### 1. Clone and configure

```bash
git clone https://github.com/valianx/discord-support-hub.git
cd discord-support-hub
cp .env.example .env
# Edit .env and fill in all required values (see Configuration below)
```

### 2. Run with Docker Compose

**Test/demo stack — one command, no `.env` required:**

```bash
docker compose -f deploy/docker-compose.test.yml up --build
# Starts: api + worker + postgres + valkey + frontend (nginx SPA)
# Opens on http://localhost:3000
```

Mint a key and try the UI: see [web/poc/README.md](web/poc/README.md) § "Run the whole stack (test/demo)".

**Production-ish stack:**

```bash
make up
# Starts: api + worker + postgres + valkey
# Applies migrations automatically via cmd/migrate
# Requires a .env file with real credentials
```

Verify the stack is healthy:

```bash
curl http://localhost:8080/readyz
# Expected: {"postgres":"ok","valkey":"ok"}
```

Prometheus metrics are available at:

```bash
curl http://localhost:8080/metrics
```

### 3. Run without Docker (development)

```bash
# Apply migrations (requires POSTGRES_DSN in env)
make migrate

# Start API
make run-api

# Start worker (separate terminal)
make run-worker
```

### 4. Run tests

```bash
make test
# go test -v -race -count=1 ./...
# Hermetic tests only — no real Discord/Postgres required.
```

Run the live integration suite (requires a provisioned test guild):

```bash
export DISCORD_BOT_TOKEN=<bot-token>
export TEST_GUILD_ID=<guild-id>
export DISCORD_AGENT_ROLE_ID=<agent-role-id>
export POSTGRES_DSN=<dsn>
export ENCRYPTION_KEY=<base64-32-bytes>
go test -v -tags integration ./test/integration/...
```

---

## Configuration

All configuration is via environment variables. Copy `.env.example` to `.env` and fill in the values. Required fields must be set; the service fails loudly at startup if they are missing.

| Variable | Required | Default | Description |
|---|---|---|---|
| `POSTGRES_DSN` | Yes | — | PostgreSQL connection string |
| `DISCORD_BOT_TOKEN` | Yes | — | Discord bot token (never logged) |
| `DISCORD_GUILD_ID` | Yes | — | Discord server (guild) ID |
| `DISCORD_AGENT_ROLE_ID` | Yes | — | Role ID that all agents receive |
| `DISCORD_CATEGORY_ID` | No | — | Default category for new spaces |
| `DISCORD_OAUTH_CLIENT_ID` | No | — | OAuth2 client ID (required for OAuth2 flows) |
| `DISCORD_OAUTH_CLIENT_SECRET` | No | — | OAuth2 client secret (never logged) |
| `DISCORD_OAUTH_REDIRECT_URL` | No | — | OAuth2 redirect URL registered in Discord |
| `OAUTH_HMAC_SECRET` | No | — | Hex-encoded 32-byte HMAC secret for CSRF state tokens |
| `ENCRYPTION_KEY` | Yes | — | Base64-encoded 32-byte AES-256-GCM key for token storage |
| `CORS_ALLOWED_ORIGINS` | No | `""` | Comma-separated allowed origins; empty = no CORS |
| `VALKEY_ADDR` | No | `localhost:6379` | Valkey/Redis address |
| `VALKEY_PASSWORD` | No | `""` | Valkey/Redis password |
| `VALKEY_DB` | No | `0` | Valkey/Redis database index |
| `HTTP_ADDR` | No | `:8080` | HTTP listen address |
| `WORKER_CONCURRENCY` | No | `10` | asynq worker concurrency |
| `AGENT_NICKNAME_SUFFIX` | No | `""` | Nickname suffix for agents (empty = disabled) |
| `RECONCILE_SWEEP_CRON` | No | `*/5 * * * *` | Cron expression for the scheduled reconcile sweep; empty = disabled |
| `LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error` |

Generate the encryption key:

```bash
openssl rand -base64 32
```

---

## API overview

The API follows an **async-first design**: mutating endpoints return `202 Accepted` with a `job_id`. Poll `GET /v1/jobs/{id}` for completion.

| Endpoint | Description |
|---|---|
| `POST /v1/merchants/{id}/channels` | Provision a private space |
| `GET /v1/channels` | List all spaces |
| `GET /v1/channels/{id}` | Get a space |
| `POST /v1/channels/{id}/lifecycle` | Change space lifecycle (archive/reopen/resolve) |
| `POST /v1/channels/{id}/collaborators` | Invite a collaborator |
| `DELETE /v1/channels/{id}/collaborators/{userId}` | Expel a collaborator |
| `GET /v1/channels/{id}/members` | List space members |
| `POST /v1/channels/{id}/welcome:sync` | Sync help-desk topic + pin |
| `POST /v1/agents` | Register an agent |
| `DELETE /v1/agents/{userId}` | Deactivate an agent |
| `GET /v1/agents` | List agents (Admin only) |
| `GET /v1/directory` | Bidirectional search (space × user) |
| `GET /v1/collaborators/{userId}/channels` | Channels accessible to a collaborator |
| `GET /v1/audit` | Audit log |
| `GET /v1/jobs/{id}` | Job status |
| `GET /v1/oauth/discord/callback` | OAuth2 callback (no auth required) |
| `GET /livez` | Liveness probe |
| `GET /readyz` | Readiness probe (pings Postgres + Valkey) |
| `GET /metrics` | Prometheus metrics |

Authentication: `Authorization: Bearer <service-api-key>`. Generate a key with:

```bash
go run ./cmd/keygen
```

Full API contract: [api/openapi.yaml](api/openapi.yaml).

---

## Test guild setup

See [docs/test-guild-setup.md](docs/test-guild-setup.md) for step-by-step instructions to:

1. Create a throwaway Discord server.
2. Create a bot application and invite it.
3. Create the Agent role and Support category.
4. Configure OAuth2 redirect URLs.
5. Populate `.env` and verify connectivity.

---

## Operator caveat — reconciler is Postgres-authoritative

**The reconciler revokes any Discord permission overwrite or role assignment that is not backed by a Postgres row.** This is the mechanism behind the multi-tenant isolation invariant.

Concretely:

- If you manually grant a collaborator access to a channel in Discord, the next reconcile sweep (every 5 minutes by default) will **revoke it**.
- If you manually remove the Agent role from an agent in Discord, the next reconcile sweep will **re-assign it**.
- Any Discord state that disagrees with Postgres is corrected toward what Postgres says.

This is intentional behavior, not a bug. Manage access exclusively through the API.

---

## Architecture

See [docs/02-architecture.md](docs/02-architecture.md) for the full technical design:

- Three-layer truth model (backoffice → Postgres → Discord)
- Async/rate-limit architecture (synchronous API, asynq workers)
- Two-layer authZ (service API key + Postgres-resolved authorization)
- Fail-closed ACL behavior
- Idempotency (three layers: edge replay, asynq unique task, worker upsert)
- Reconciliation loop

---

## Observability

Structured JSON logs (slog) with automatic secret redaction (bot token, OAuth2 tokens, API keys are never logged).

Prometheus metrics at `/metrics`:

| Metric | Type | Description |
|---|---|---|
| `hub_provisioning_latency_seconds{status}` | Histogram | Provisioning job latency by outcome |
| `hub_active_spaces_total` | Gauge | Spaces in `lifecycle=active` |
| `hub_ratelimit_hits_total` | Counter | Token-bucket denials |
| `hub_errors_total{kind}` | Counter | Worker errors (fatal / transient) |

**Security note (SEC-M5-004):** `/metrics` is unauthenticated and bound to the same port as the API. It should be network-restricted to trusted scrape interfaces only (e.g. internal VPC, a Prometheus pod network policy, or a reverse proxy that blocks external access to this path). Do not expose it to the public internet.

---

## Versioning

This project follows [Semantic Versioning](https://semver.org/). The current version is **v0.1.0**.

See [CHANGELOG.md](CHANGELOG.md) for the full history.

---

## License

[Apache-2.0](LICENSE).
