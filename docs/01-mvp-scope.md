# Discord Support Hub — MVP Scope & Roadmap

> Status: **Draft v1 (scope-locked)** · Source: [Servicio de Espacios de Soporte en Discord](https://docs.google.com/document/d/1ZTt9v8gaIYGbHiWWwdXTIAbI00ttfm0_w_jlkQBFGc4/edit)
> Project name: `discord-support-hub` (locked).

This document resolves the open decisions in §12 of the spec and draws a firm **v1 cut line**. It is the authoritative roadmap: code, OpenAPI contracts, and DDL follow this, not the other way around.

---

## 1. Guiding principle for the cut

The spec is a B2B multi-tenant service whose entire reason to exist is **hard isolation between customers**. That makes the cut asymmetric:

- **Security and correctness invariants are NOT MVP-negotiable.** Fail-closed ACLs, multi-tenant isolation, two-layer authZ, no-invites, idempotency/reconciliation, and secret handling ship in v1 *even though* they are "non-functional." A support hub that leaks one merchant's channel to another merchant is not a smaller product — it is a broken one.
- **Convenience and scale features ARE deferrable.** Message mirroring, agent routing, sticky-message choreography, thread-mode scaling, and role-icon polish add value but do not change the security posture. They are fast-follows (or, at this scale, dropped).

The MVP is the **smallest slice that proves the isolation invariant end-to-end through the real async/rate-limited architecture** — not the smallest slice that compiles.

---

## 1.5 System context — the backoffice

`discord-support-hub` is the **mechanism**; the **backoffice** is the **policy/business layer** that drives it (*mechanism, not policy* — §1 of the spec).

- The backoffice is where a staffer performs the human action — "add this agent", "open a space for this merchant", "invite this collaborator". The hub ships **no human UI** in v1; its control surface is the **API** (FR-11), and the backoffice is its consumer. After the pivot the backoffice also serves as a **read/admin console** (view users per space, expel, assign agent roles).
- **Three layers of truth, cleanly separated:**
  - **backoffice** — origin of the operational action (who to onboard, when).
  - **discord-support-hub / Postgres** — authorization source of truth (roster, merchant↔space↔user mappings, the collaborator's name+email traceability labels, the per-merchant invite link). The backoffice's action propagates here via the hub API.
  - **Discord** — projection (the Agent role, one role per merchant, channel allows) the bot reconciles from Postgres.
- **Access is role-based.** Each merchant owns one Discord role, created automatically by the API on space provision; the merchant's channel allows that role VIEW+SEND. A collaborator acquires the role by joining through the merchant's **native invite-with-role link** — created once by the operator in the Discord client (the REST API cannot attach a role to an invite) and stored against the merchant. The hub emails that link itself. There are no per-user channel overwrites and no OAuth2 onboarding.

**Agent onboarding-by-API, end to end:**
1. Staffer adds an agent in the **backoffice**.
2. Backoffice calls the hub API (`POST /agents` with the agent's Discord id) → Postgres records `type=agent` (+ `is_admin` if applicable). This is the authZ source of truth (FR-23).
3. The bot **assigns the Agent role** once the agent is present in the guild → category-level allow grants every space at once (FR-6).

**Collaborator onboarding-by-API, end to end:**
1. Operator creates the merchant's invite-with-role link once in the Discord client and stores it (`PUT /merchants/{id}/invite`).
2. Agent registers the collaborator by **name + work email** (`POST /channels/{id}/collaborators`) → Postgres records the desired membership and the traceability labels.
3. Agent triggers `:send-invite` → the hub emails the stored link (our SMTP) with the configurable welcome message.
4. Collaborator clicks the link → joins the guild → Discord's native invite-with-role assigns the **merchant role** → the merchant's channel appears (and `#bienvenida`, visible to `@everyone`, greets them meanwhile).

An agent's entire access surface is the single **Agent role**; a collaborator's is the **merchant role** they acquire on join.

---

## 2. v1 (MVP) — IN scope

### Functional requirements in v1

| FR | Requirement | v1 cut |
| :-- | :-- | :-- |
| FR-1 | Provision a private space per merchant via API | **Full** |
| FR-2 | Channel mode **and** thread mode | **Channel mode only** — thread mode dropped (unnecessary at ~50 merchants; revisit only on a real scale change) |
| FR-3 | Per-space ACL (deny @everyone, allow Agent at category, **allow the merchant role** for Collaborators) | **Full** — role-based, replacing per-user overwrites |
| FR-4 | Collaborator membership (add/remove from a space) | **Full** |
| FR-5 | Invisible by default; undiscoverable without an Agent-executed invite | **Full** (the core invariant) |
| FR-6 | Agents read/write all spaces via category-level role | **Full** |
| FR-7 | Lifecycle: active → resolved → archived, with reopen | **Full** |
| FR-9 | Identity mapping as single source of truth (merchant ↔ users ↔ spaces) | **Full** |
| FR-10 | List all spaces with state, owner, created, last activity | **Full** |
| FR-11 | Control surface (admin API) to provision/invite/expel/list/close/reopen | **API only** — consumed by the backoffice (no hub UI); slash-commands → v1.1 |
| FR-13 | Declarative config (guild ID, agent role, naming, mode, archive policy) with sane defaults | **Full** |
| FR-14 | Audit log (who/what/when) of provisioning, membership, lifecycle | **Full** |
| FR-15 | Help-desk visibility | **Static presence only** (topic + pin); sticky/nudge → v1.1 — see §4 |
| FR-16 | Two-role model (Agent/Collaborator) + Admin layer, names configurable | **Full** |
| FR-17 | List members of a space with role + merchant | **Full** |
| FR-18 | Global directory (spaces × users × role), bidirectional search | **Full** |
| FR-19 | Expulsion by an Agent, scope configurable (channel vs server), audited | **Full** |
| FR-20 | Invite restricted to Agents; Collaborators cannot invite | **Full** (invariant) |
| FR-21 | "Channels by collaborator" endpoint | **Full** |
| FR-22 | Provisioning only by API: merchant role + role-based channel allow created by the API; collaborators onboarded via a stored native invite-with-role link the hub emails | **Full** — see §1.5 (supersedes the OAuth2/no-invite-link model) |
| FR-23 | Agent roster management (Admin layer); `type`/`is_admin` in store; bot projects + reconciles role | **Full** — driven by the backoffice |
| FR-24 | Visual agent marking with graceful degradation | **Optional** — configurable nickname suffix applied by the bot; **off by default**. Emoji/color/hoist/role-icon dropped — see §4 |

### Non-functional requirements in v1 (the floor)

These are **mandatory** — they are why Discord was chosen over Telegram in the first place:

| NFR | Requirement | Why it can't wait |
| :-- | :-- | :-- |
| NFR-2 | Respect Discord rate limits (global + per-route) with queue, backoff, retries | The architecture *is* the rate-limit handling; without it the demo falls over |
| NFR-3 | Idempotency + reconciliation (desired DB state vs real Discord, drift auto-repair) | Retries must not double-provision; drift is inevitable on a SaaS backend |
| NFR-4 | Fail-closed: ACL apply failure → no access, never world-readable | Direct security invariant |
| NFR-5 | Multi-tenant isolation as a verifiable, testable invariant | The product's entire value proposition |
| NFR-6 | Secret handling (bot token + SMTP credentials config-by-env; redacted logs) | App-level secrets only — no per-user token store after the OAuth2 drop |
| NFR-9 | Persistent store of merchant↔space↔user mapping; survives restart; backups | Lose the mapping → lose isolation and the audit trail |
| NFR-13 | Two-layer authZ resolved against the store; `MANAGE_ROLES` reserved to the bot | Agent and merchant roles must not be self-assignable; reconciler strips unrecorded roles |
| NFR-14 | Controlled-entry invariant: the merchant invite-with-role link is the only collaborator entry path; `MANAGE_ROLES`/`CREATE_INSTANT_INVITE` reserved to the bot | All access auditable and role-gated; manual role grants are treated as drift |

These ship in a **lighter but real** form in v1 (full rigor continues through v1.x):

| NFR | v1 form |
| :-- | :-- |
| NFR-1 | **Channel mode** within the 500-channel budget. At ~50 merchants this is comfortable; thread mode and multi-guild sharding are **out of scope** until scale changes. |
| NFR-7 | Structured logging + health checks + a minimal metric set (provisioning latency, active spaces, rate-limit hits, errors). Full OTel tracing → v1.1 |
| NFR-8 | Storage backend behind an interface (pluggable later); webhooks/hooks → v2 |
| NFR-10 | Single Go binary + Docker image + env/file config from day one |
| NFR-16 | License chosen (Apache-2.0, see §4); README + semver + CHANGELOG from the first tag; integration tests against a throwaway test guild |

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
| **FR-2** thread mode | **Dropped** | Unnecessary at the ~50-merchant target (≈50 channels « the 500 budget). Revisit only on a real scale change. |
| **FR-15** sticky message + activity nudges | **v1.1** | Static topic+pin already satisfies "always available." Sticky/nudge brings the NFR-15 throttling machinery — defer together. |
| **FR-24** role-icon / color / hoist | **Dropped** | Marking reduced to an optional configurable nickname suffix; the richer visual treatments aren't wanted. |
| **FR-11** slash commands | **v1.1** | The admin API (consumed by the backoffice) covers the control surface; commands are a second front-end. |
| Webhooks / event hooks (NFR-8) | **v2** | Userland extensibility, not core. |

---

## 4. Resolved open decisions (§12)

| Decision | Resolution | Note |
| :-- | :-- | :-- |
| **MVP FR set** | FR-1,3,4,5,6,7,9,10,11,13,14,16,17,18,19,20,21,22,23 + reduced FR-15; FR-24 reduced to optional suffix | This document |
| **Capacity target (NFR-1)** | **~50 merchants → channel mode.** Thread mode and multi-guild sharding dropped from scope. | 50 « the 500-channel budget |
| **Cardinality** | **merchant ↔ space = 1:1** (each merchant has exactly one space; `UNIQUE(merchant_id)`) **and merchant ↔ role = 1:1** (one Discord role per merchant). **collaborator ↔ space = M:N** — a collaborator is a global external user who may be invited to several merchants' spaces; tenant grouping derives from space membership, not a user→merchant FK. | Isolation unchanged: a collaborator holds only the merchant roles they were invited to |
| **Access / onboarding model** | **Role per merchant + native invite-with-role.** The API auto-creates the merchant role + role-based channel allow on provision; collaborators join via the merchant's stored invite-with-role link, which the hub emails (our SMTP). **This reverses the original "no per-merchant roles" decision** (rejected for the 250-role cap) and **drops OAuth2 `guilds.join`** — OAuth2 needed a public callback + per-user browser authorize the operator will not host. | Viable to ~200 merchants vs the 250-role cap; one manual operator step per merchant (the invite link). See §1.5 and 02-architecture §5.3/§6 |
| **FR-8 persistence** | **Deferred to v2** (confirmed). v1 manages access only. The M7 console reads metadata (users, audit, directory) and links out to the conversation via an "Open in Discord" deep link; reading message content lands in the v2 mirror. | Keeps the DB schema small; no Message Content Intent in v1 |
| **Agent identity / onboarding origin** | **backoffice** is the upstream admin surface (manual roster, FR-23). Backoffice → hub API (`POST /agents` with Discord id) → bot assigns Agent role once the agent is in the guild. SSO/Workspace binding → v2. | See §1.5 |
| **Expulsion cascade default** | **`remove-from-channel` is the default** (remove the merchant role, keep the person in the guild). `remove-from-server` is explicit opt-in via `?scope=server`. | Least-destructive default; reversible |
| **Visual marking** | **Optional configurable nickname suffix** applied by the bot; **off by default**. No emoji/color/hoist/role-icon. | Minimal, opt-in |
| **Persistence backend** | PostgreSQL = source of truth; Valkey = cache/coordination only, **never** source of truth | Per §10 |
| **License** | **Apache-2.0** | Patent grant matters for a B2B/payments-adjacent OSS tool; avoids AGPL ambiguity (mirrors the Valkey-over-Redis reasoning in §10) |
| **Project name** | **`discord-support-hub`** (locked, not a placeholder) | §11 caveats dropped |

---

## 5. Milestones

**M0 — Skeleton** *(enabler)*
Go module, Gin API, asynq+Valkey wiring, discordgo client, Postgres + migrations, config loader, Docker, CI, health checks. Empty but running. Includes a **test-guild setup guide** (create server, bot application, token, bot permissions, and the per-merchant invite-with-role dialog walkthrough) and SMTP relay configuration.

**M1 — Identity & authZ core** *(FR-9, FR-16, FR-23, NFR-13, NFR-6)*
Postgres as source of truth: merchants, users, spaces, `type`/`is_admin`. Two-layer authZ middleware resolving against the store. **Backoffice-facing roster API** (`POST/DELETE/GET /agents`). Bot projects + reconciles the Agent role. Secret encryption + log redaction.

**M2 — Provisioning vertical slice** *(FR-1, FR-3, FR-5, FR-13, NFR-2, NFR-3, NFR-4)*
`POST /merchants/{id}/channels` → enqueue → worker creates a **channel** with fail-closed ACL (deny @everyone, allow Agent at category) → persist. The riskiest path first: async + rate limiter + idempotency + per-space locks. **This milestone proves the architecture.**

**M3 — Membership & isolation** *(FR-4, FR-6, FR-17, FR-18, FR-19, FR-20, FR-21, FR-22, NFR-5, NFR-14)*
Collaborator membership model; directory + per-space members + per-collaborator channels; controlled-entry lockdown. **Multi-tenant isolation test suite** as a gate.

**M4 — Lifecycle, audit, visibility, marking** *(FR-7, FR-10, FR-11, FR-14, FR-15-static, FR-24-optional)*
Space lifecycle (active/resolved/archived/reopen); audit log; list-all; static help-desk presence (topic+pin) + the `#bienvenida` welcome channel; optional configurable nickname-suffix marking.

**M5 — OSS hardening** *(NFR-7, NFR-10, NFR-16)*
Integration tests against a test guild, structured logs + metrics + health, Docker image, README/CHANGELOG/license, first tagged release `v0.1.0`.

**M6 — Role-per-merchant provisioning + invite storage + email send** *(FR-3, FR-4, FR-22, NFR-5, NFR-6, NFR-14)*
The pivot core. Provision auto-creates the merchant role (`GuildRoleCreate`) + role-based channel allow; store the per-merchant invite-with-role link (`PUT /merchants/{id}/invite`); register a collaborator by name+email (`POST /channels/{id}/collaborators`); send the stored link via the SMTP `notify` queue (`:send-invite`); reconciler diffs role membership. Removes the OAuth2 callback, `oauth_tokens`, AES-GCM token store, and per-user collaborator overwrites.

**M7 — Read/admin console** *(FR-10, FR-17, FR-18, FR-19, FR-23)*
Backoffice-facing read surface: view users per space (directory/members — exists), an **"Open in Discord" deep link** per space/channel (`https://discord.com/channels/{guildId}/{channelId}`) to jump to the conversation, expel (`scope=server` — exists), assign agent roles (roster + projection — exists). **Reading message content in the console is deferred to the v2 FR-8 mirror** — metadata + deep-link only, so no privileged Message Content Intent is needed.

> **v1 = M0 → M7.** M6 supersedes the OAuth2 collaborator-onboarding parts of M3; the rest of M3 (isolation suite, directory) stands. Then v1.1 picks up sticky-message/nudge visibility (FR-15 dynamic), slash commands, and full OTel tracing. Thread mode and role-icon are **dropped** at current scale, not merely deferred.

---

## 6. Still needs an operator decision

Resolved above: capacity (~50 → channel mode), cardinality (merchant↔space 1:1, merchant↔role 1:1, collaborator↔space M:N), access/onboarding model (role-per-merchant + native invite-with-role, OAuth2 dropped), agent marking (optional configurable suffix), agent onboarding origin (backoffice → hub API), **console message-read (resolved: deferred to the v2 FR-8 mirror — the M7 console is metadata + an "Open in Discord" deep link, no Message Content Intent)**, license, and project name. Remaining:

1. **Test guild + bot application** — a throwaway Discord server plus a bot application/token, needed to run integration tests (NFR-16) and real provisioning. This is an **operational prerequisite, not a design decision**; the M0 setup guide covers creation, including the per-merchant invite-with-role dialog. *Needed before M2/M6 runs for real.*
2. **SMTP relay** — host/port/credentials/from-address for sending invite emails (config-by-env). An **operational prerequisite** for M6's `:send-invite`. *Needed before M6 runs for real.*
3. **POC frontend session mechanism** — hub-minted (`/auth/session`) vs delegated to the backoffice's auth. The API reserves the `session` principal seam either way; decide before the POC frontend phase.

---

## 7. Next step

With scope locked, the natural follow-on is the **M0/M1 technical design**: Postgres DDL, the OpenAPI contract for the v1 API surface (§8 of the spec, including the backoffice-facing roster endpoints), and the reconciliation/idempotency model. Say the word and I'll route that through the design pipeline.
