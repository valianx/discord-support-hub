# discord-support-hub

> Working name — final name pending (see [docs/00-requirements.md §11](docs/00-requirements.md#11-nombre-del-proyecto-candidatos)).

**Open-source, API-driven service that provisions and governs isolated private support spaces on Discord — one per merchant — with real per-channel access control.**

An internal team (*Agents*) supports external collaborators (*Collaborators*) in Discord channels that are **invisible by default** and reachable only through API-managed invitations. Each merchant gets a private space; a collaborator can only ever see the spaces they were invited to, never another merchant's.

Design philosophy: **mechanism, not policy.** The service provides the mechanisms — provisioning, ACLs, agent marking, help-desk visibility — with sane defaults and graceful degradation. Business logic stays in the hands of whoever deploys it.

## Why Discord

Real **per-channel ACL** via permission overwrites is exactly the isolation model this needs. Alternatives like Telegram topics share visibility across the whole supergroup, which breaks tenant isolation. Trade-off accepted: conversation data lives in Discord (SaaS); the provisioning service is self-hosted and open-source.

## Core invariants

- **Invisible by default / fail-closed** — every space is born with `deny @everyone → VIEW_CHANNEL`; an ACL failure leaves a space with *no* access, never world-readable.
- **Postgres is the source of truth** — Discord roles are a projection the bot reconciles. Authorization is always resolved against the store.
- **No invite links** — server entry is OAuth2 `guilds.join`; `CREATE_INSTANT_INVITE` is reserved to the bot. No human can mint an invite.
- **Multi-tenant isolation is a testable security invariant**, not a convention.

## Stack

Go + Gin (API) · asynq over [Valkey](https://valkey.io) (async queue, distributed rate limiter, idempotency, locks) · [discordgo](https://github.com/bwmarrin/discordgo) · PostgreSQL (source of truth) · OpenTelemetry.

The synchronous-API / asynchronous-worker split is what makes Discord's rate limits (global + per-route) tractable.

## Status

🚧 **Pre-development.** This repository currently holds the functional/architectural specification and the MVP scope. No application code yet.

| Document | Contents |
| :-- | :-- |
| [docs/00-requirements.md](docs/00-requirements.md) | Full functional & architectural spec (24 FRs, 16 NFRs, API surface, stack) |
| [docs/01-mvp-scope.md](docs/01-mvp-scope.md) | v1 cut line, milestones, resolved decisions, open questions |

## License

[Apache-2.0](LICENSE).
