# Discord Support Hub — MVP Scope & Roadmap

> Status: **Draft v1 (scope-locked)** · Source: [Servicio de Espacios de Soporte en Discord](https://docs.google.com/document/d/1ZTt9v8gaIYGbHiWWwdXTIAbI00ttfm0_w_jlkQBFGc4/edit)
> Working project name: `discord-support-hub` (final name TBD — §11 of the spec)

This document resolves the open decisions in §12 of the spec and draws a firm **v1 cut line**. It is the authoritative roadmap: code, OpenAPI contracts, and DDL follow this, not the other way around.

---

## 1. Guiding principle for the cut

The spec is a B2B multi-tenant service whose entire reason to exist is **hard isolation between customers**. That makes the cut asymmetric:

- **Security and correctness invariants are NOT MVP-negotiable.** Fail-closed ACLs, multi-tenant isolation, two-layer authZ, no-invites, idempotency/reconciliation, and secret handling ship in v1 *even though* they are "non-functional." A support hub that leaks one merchant's channel to another merchant is not a smaller product — it is a broken one.
- **Convenience and scale features ARE deferrable.** Message mirroring, agent routing, sticky-message choreography, thread-mode scaling, and role-icon polish add value but do not change the security posture. They are fast-follows.

The MVP is the **smallest slice that proves the isolation invariant end-to-end through the real async/rate-limited architecture** — not the smallest slice that compiles.

---

## 2. v1 (MVP) — IN scope

### Functional requirements in v1

| FR | Requirement | v1 cut |
| :-- | :-- | :-- |
| FR-1 | Provision a private space per merchant via API | **Full** |
| FR-2 | Channel mode **and** thread mode | **Channel mode only** (thread mode → v1.1) — see §4 |
| FR-3 | Per-space ACL (deny @everyone, allow Agent at category, per-user allow for Collaborators) | **Full** |
| FR-4 | Collaborator membership (add/remove from a space) | **Full** |
| FR-5 | Invisible by default; undiscoverable without an Agent-executed invite | **Full** (the core invariant) |
| FR-6 | Agents read/write all spaces via category-level role | **Full** |
| FR-7 | Lifecycle: active → resolved → archived, with reopen | **Full** |
| FR-9 | Identity mapping as single source of truth (merchant ↔ users ↔ spaces) | **Full** |
| FR-10 | List all spaces with state, owner, created, last activity | **Full** |
| FR-11 | Control surface (admin API) to provision/invite/expel/list/close/reopen | **API only** (slash-commands → later) |
| FR-13 | Declarative config (guild ID, agent role, naming, mode, archive policy) with sane defaults | **Full** |
| FR-14 | Audit log (who/what/when) of provisioning, membership, lifecycle | **Full** |
| FR-15 | Help-desk visibility | **Static presence only** (topic + pin); sticky/nudge → v1.1 — see §4 |
| FR-16 | Two-role model (Agent/Collaborator) + Admin layer, names configurable | **Full** |
| FR-17 | List members of a space with role + merchant | **Full** |
| FR-18 | Global directory (spaces × users × role), bidirectional search | **Full** |
| FR-19 | Expulsion by an Agent, scope configurable (channel vs server), audited | **Full** |
| FR-20 | Invite restricted to Agents; Collaborators cannot invite | **Full** (invariant) |
| FR-21 | "Channels by collaborator" endpoint | **Full** |
| FR-22 | Provisioning only by API: OAuth2 `guilds.join`, overwrites, no invite links | **Full** (invariant) |
| FR-23 | Agent roster management (Admin layer); `type`/`is_admin` in store; bot projects + reconciles role | **Full** |
| FR-24 | Visual agent marking with graceful degradation | **emoji + color + hoist** default; role-icon (Boost L2) → v1.1 — see §4 |

### Non-functional requirements in v1 (the floor)

These are **mandatory** — they are why Discord was chosen over Telegram in the first place:

| NFR | Requirement | Why it can't wait |
| :-- | :-- | :-- |
| NFR-2 | Respect Discord rate limits (global + per-route) with queue, backoff, retries | The architecture *is* the rate-limit handling; without it the demo falls over |
| NFR-3 | Idempotency + reconciliation (desired DB state vs real Discord, drift auto-repair) | Retries must not double-provision; drift is inevitable on a SaaS backend |
| NFR-4 | Fail-closed: ACL apply failure → no access, never world-readable | Direct security invariant |
| NFR-5 | Multi-tenant isolation as a verifiable, testable invariant | The product's entire value proposition |
| NFR-6 | Secret handling (bot token + collaborator OAuth2 tokens encrypted; redacted logs) | Storing customer access tokens; non-negotiable |
| NFR-9 | Persistent store of merchant↔space↔user mapping; survives restart; backups | Lose the mapping → lose isolation and the audit trail |
| NFR-13 | Two-layer authZ resolved against the store; `MANAGE_ROLES` reserved to the bot | Agent role must not be self-assignable |
| NFR-14 | No-invites invariant; `CREATE_INSTANT_INVITE` reserved to the bot | All access auditable; bypass = isolation break |

These ship in a **lighter but real** form in v1 (full rigor continues through v1.x):

| NFR | v1 form |
| :-- | :-- |
| NFR-1 | **Design for** thread/shard; **implement** channel mode within the 500-channel budget. Capacity target must be set (§6, open). |
| NFR-7 | Structured logging + health checks + a minimal metric set (provisioning latency, active spaces, rate-limit hits, errors). Full OTel tracing → v1.1 |
| NFR-8 | Storage backend behind an interface (pluggable later); webhooks/hooks → v2 |
| NFR-10 | Single Go binary + Docker image + env/file config from day one |
| NFR-16 | License chosen (Apache-2.0, see §5), README + semver + CHANGELOG from the first tag; integration tests against a throwaway test guild |

### Deferred NFRs

- **NFR-11** (latency SLO): measure in v1, set a target in v1.1 once we have real numbers.
- **NFR-12** (retention/compliance, customer data deletion): policy stub in v1; full deletion-on-offboard flow in v2 (couples to FR-8).
- **NFR-15** (anti-noise throttling): scoped to the deferred FR-15 dynamic pieces → v1.1.

---

## 3. Explicitly OUT of v1

| Item | Where | Rationale |
| :-- | :-- | :-- |
| **FR-8** — Message mirroring to an external store | **v2** | Biggest weight on the DB. Keeping v1 to *access management only* makes the MVP dramatically smaller. The conversation lives in Discord. |
| **FR-12** — New-message notification / routing / auto-assign | **v2** | Pure convenience; no isolation impact. |
| **FR-2** thread mode | **v1.1** | Channel mode proves isolation; thread mode is a scale lever (NFR-1) that only matters past ~hundreds of merchants. |
| **FR-15** sticky message + activity nudges | **v1.1** | Static topic+pin already satisfies "always available." Sticky/nudge brings the NFR-15 throttling machinery — defer together. |
| **FR-24** role-icon | **v1.1** | Requires Boost L2; emoji+color+hoist is the free, always-available default and the degradation target anyway. |
| **FR-11** slash commands | **v1.1** | Admin API covers the control surface; commands are a second front-end. |
| Webhooks / event hooks (NFR-8) | **v2** | Userland extensibility, not core. |

---

## 4. Resolved open decisions (§12)

| Decision | Resolution | Note |
| :-- | :-- | :-- |
| **MVP FR set** | FR-1,3,4,5,6,7,9,10,11,13,14,16,17,18,19,20,21,22,23 + reduced FR-2/15/24 | This document |
| **FR-8 persistence** | **Deferred to v2.** v1 manages access only. | Keeps the DB schema small |
| **Agent identity** | **v1 = manual allowlist** via the Admin layer (FR-23). SSO/Workspace binding → v2. | Matches spec |
| **Expulsion cascade default** | **`remove-from-channel` is the default** (revoke the overwrite, keep the person in the guild). `remove-from-server` is explicit opt-in via `?scope=server`. | Least-destructive default; reversible |
| **Visual marking default** | **emoji prefix + distinct color + hoist.** Default emoji/color need an operator pick (§6). | Free, always available, is the degradation floor |
| **Persistence backend** | PostgreSQL = source of truth; Valkey = cache/coordination only, **never** source of truth | Per §10 |
| **License** | **Apache-2.0** (recommend) | Patent grant matters for a B2B/payments-adjacent OSS tool; avoids AGPL ambiguity (mirrors the Valkey-over-Redis reasoning in §10) |

---

## 5. Milestones

**M0 — Skeleton** *(enabler)*
Go module, Gin API, asynq+Valkey wiring, discordgo client, Postgres + migrations, config loader, Docker, CI, health checks. Empty but running.

**M1 — Identity & authZ core** *(FR-9, FR-16, FR-23, NFR-13, NFR-6)*
Postgres as source of truth: merchants, users, spaces, `type`/`is_admin`. Two-layer authZ middleware resolving against the store. Bot projects + reconciles the Agent role. Secret encryption + log redaction.

**M2 — Provisioning vertical slice** *(FR-1, FR-3, FR-5, FR-13, NFR-2, NFR-3, NFR-4)*
`POST /merchants/{id}/channels` → enqueue → worker creates a **channel** with fail-closed ACL (deny @everyone, allow Agent at category) → persist. The riskiest path first: async + rate limiter + idempotency + per-space locks. **This milestone proves the architecture.**

**M3 — Membership & isolation** *(FR-4, FR-6, FR-17, FR-18, FR-19, FR-20, FR-21, FR-22, NFR-5, NFR-14)*
Collaborator add/remove via per-user overwrite; OAuth2 `guilds.join` "Connect with Discord"; no-invites lockdown; directory + per-space members + per-collaborator channels. **Multi-tenant isolation test suite** as a gate.

**M4 — Lifecycle, audit, visibility, marking** *(FR-7, FR-10, FR-11, FR-14, FR-15-static, FR-24-default)*
Space lifecycle (active/resolved/archived/reopen); audit log; list-all; static help-desk presence (topic+pin); emoji+color+hoist agent marking with degradation.

**M5 — OSS hardening** *(NFR-7, NFR-10, NFR-16)*
Integration tests against a test guild, structured logs + metrics + health, Docker image, README/CHANGELOG/license, first tagged release `v0.1.0`.

> **v1 = M0 → M5.** Then v1.1 picks up thread mode, sticky/nudge, role-icon, slash commands, full OTel tracing.

---

## 6. Still needs an operator decision

These don't block M0–M1 but must be answered before the milestone that consumes them:

1. **Capacity target (NFR-1)** — expected number of merchants/spaces? Decides how soon thread mode (v1.1) becomes mandatory vs optional. *Needed before M2 design is finalized.*
2. **Default agent emoji + color (FR-24)** — concrete default for the marking. *Needed before M4.*
3. **Test guild** — a throwaway Discord server + bot application (token) for integration tests. *Needed before M2 can run for real.*
4. **Final project name (§11)** — Bastion/Castellan/etc. pending npm/GitHub/domain availability check. *Needed before the `v0.1.0` tag in M5; `discord-support-hub` is the working name until then.*

---

## 7. Next step

With scope locked, the natural follow-on is the **M0/M1 technical design**: Postgres DDL, the OpenAPI contract for the v1 API surface (§8), and the reconciliation/idempotency model. Say the word and I'll route that through the design pipeline.
