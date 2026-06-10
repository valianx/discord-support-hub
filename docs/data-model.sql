-- discord-support-hub — PostgreSQL data model (M0–M7)
-- Source of truth for roster, mappings, and authorization (NFR-9, NFR-13).
-- Conforms to 01-mvp-scope.md (scope-locked) and 02-architecture.md.
--
-- Design rules encoded here:
--   * Postgres is the AuthZ source of truth; Discord is a projection (three-layer truth model).
--   * Multi-tenant isolation is enforced by the schema where the DB can (NFR-5).
--   * Fail-closed ACL state is tracked, never assumed open (NFR-4).
--   * One merchant -> exactly one space (1:1), enforced by UNIQUE(merchant_id) on
--     `spaces`. Lifecycle (archive/reopen) acts on that single space.
--   * One merchant -> exactly one Discord role (1:1). The merchant role is created by
--     the API on provision; its channel allow grants collaborators access. The role id
--     lives on `spaces.merchant_role_id`.
--   * A collaborator is a global external identity invited to many merchants' spaces
--     (M:N via space_members); tenant grouping for a collaborator is DERIVED from space
--     membership, never a user->merchant foreign key. Access is role-based: a collaborator
--     acquires the merchant role natively, by joining through the merchant's stored
--     invite-with-role link (see 02-architecture.md §6).
--   * The collaborator's name and work email are OUR labels (traceability), stored here;
--     they are never a Discord primitive (Discord has no email key). Email is PII.
--   * App-level secrets (bot token, SMTP credentials) are config-by-env, never persisted.
--     There is NO per-user token store: dropping OAuth2 removed `oauth_tokens` entirely.
--
-- Conventions: snake_case, UUID primary keys (gen_random_uuid via pgcrypto),
-- timestamptz everywhere, soft-state lifecycle (no destructive deletes for spaces).

-- ---------------------------------------------------------------------------
-- Extensions
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid(), digest()
CREATE EXTENSION IF NOT EXISTS citext;     -- case-insensitive email

-- ---------------------------------------------------------------------------
-- Enumerated types
-- ---------------------------------------------------------------------------

-- A user is either an internal Agent (team) or an external Collaborator (merchant
-- guest). Admin is NOT a third type; it is the boolean `is_admin` on an agent.
CREATE TYPE user_type AS ENUM ('agent', 'collaborator');

-- Space lifecycle (FR-7). Archived/locked never deletes history; the channel is
-- hidden/locked in Discord but the row and audit trail persist.
CREATE TYPE space_lifecycle_state AS ENUM ('active', 'resolved', 'archived');

-- Result of applying the fail-closed ACL to Discord (NFR-4). `pending` = not yet
-- projected; `applied` = deny @everyone + agent allow confirmed; `degraded`/`failed`
-- = an ACL step failed -> space stays invisible, reconciler must repair. We never
-- treat a non-`applied` space as openly readable.
CREATE TYPE acl_state AS ENUM ('pending', 'applied', 'degraded', 'failed');

-- Membership role within a space. Agents are NOT enumerated per-space (they see
-- everything via the category-level role); only collaborators get space_members rows.
CREATE TYPE space_member_role AS ENUM ('collaborator');

-- Async job status mirrored from asynq into Postgres so callers can poll an
-- authoritative source (Valkey is never source of truth).
CREATE TYPE job_status AS ENUM ('pending', 'active', 'completed', 'retrying', 'archived');

-- Scope of an expulsion (FR-19). channel = revoke overwrite only (default);
-- server = also remove from guild.
CREATE TYPE expulsion_scope AS ENUM ('channel', 'server');


-- ===========================================================================
-- merchants
-- One external customer. Owns exactly one space (1:1) and exactly one Discord role
-- (1:1). Collaborators are NOT owned by a merchant; they are global identities granted
-- access per-space (see space_members), projected via the merchant role. The merchant
-- grouping is a DB fact; the merchant ROLE is its Discord projection (one role per
-- merchant -- viable to ~200 merchants against Discord's 250-role/server cap; see
-- 02-architecture.md §5.3).
--
-- The invite-with-role link is a per-merchant fact: the operator creates it once by hand
-- in the Discord client (the REST API cannot attach a role to an invite -- see §6), and
-- it is stored here, reusable for every collaborator of this merchant and emailed by the
-- hub's SMTP sender.
-- ===========================================================================
CREATE TABLE merchants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Stable external key from the backoffice; lets the backoffice address a
    -- merchant by its own id idempotently. Unique to prevent duplicate provisioning.
    external_ref    TEXT NOT NULL,
    name            TEXT NOT NULL,
    -- Per-merchant help-desk link parameterization (FR-15 static); nullable.
    help_desk_url   TEXT,
    -- Native Discord invite-with-role link, bound to this merchant's role. Created by the
    -- operator once in the Discord client and stored via PUT /merchants/{id}/invite.
    -- NULL until the operator stores it; :send-invite is rejected while NULL.
    invite_link     TEXT,
    invite_link_set_at TIMESTAMPTZ,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT merchants_external_ref_key UNIQUE (external_ref)
);
COMMENT ON TABLE merchants IS 'External customers; one merchant -> one space and one Discord merchant role. Stores the per-merchant invite-with-role link.';
COMMENT ON COLUMN merchants.external_ref IS 'Stable id from the backoffice; enables idempotent addressing.';
COMMENT ON COLUMN merchants.invite_link IS 'Operator-created native invite-with-role link (client-only feature); stored here, emailed by the hub. NULL blocks :send-invite.';


-- ===========================================================================
-- users
-- The roster and AuthZ source of truth (FR-9, FR-23, NFR-13). `type` (agent/
-- collaborator) and `is_admin` are authoritative here; the Discord Agent role is a
-- projection the bot maintains. Authorization is ALWAYS resolved against this table,
-- never against the Discord role.
--
-- A user is just an identity: an internal agent, or an external collaborator. Neither
-- is tied to a merchant by a column here. A collaborator's merchant associations are
-- DERIVED from space membership (space_members -> spaces -> merchant), reflecting that
-- one collaborator may hold access across several merchants' spaces.
-- ===========================================================================
CREATE TABLE users (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type              user_type NOT NULL,
    -- Admin privilege is only meaningful for agents (roster management safeguard).
    is_admin          BOOLEAN NOT NULL DEFAULT FALSE,
    -- Discord identity. Nullable: for agents it is supplied by the admin; for
    -- collaborators it is unknown at registration (they are recorded by name+email and
    -- join later via the invite-with-role link). May be backfilled by the console /
    -- reconciler once observed. Access never depends on it being populated.
    discord_user_id   TEXT,
    -- Work email: OUR traceability label (PII), never a Discord lookup key. For a
    -- collaborator this is the address the invite link is emailed to.
    email             CITEXT,
    display_name      TEXT,
    -- For an agent: set once the Agent role is projected. For a collaborator: set when
    -- the merchant role / membership is first observed (optional; access is role-native).
    provisioned_at    TIMESTAMPTZ,
    is_active         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Admin only meaningful for agents.
    CONSTRAINT users_admin_only_agent_chk CHECK (is_admin = FALSE OR type = 'agent'),
    -- One Discord identity maps to at most one user row (when present).
    CONSTRAINT users_discord_user_id_key UNIQUE (discord_user_id)
);
COMMENT ON TABLE users IS 'Roster + AuthZ source of truth; a user is an identity (agent or collaborator), not merchant-bound. Discord role is a projection of type/is_admin.';
COMMENT ON COLUMN users.discord_user_id IS 'Nullable; agents supply it, collaborators are registered by name+email and join via invite-with-role. One identity = one row when present.';
COMMENT ON COLUMN users.email IS 'Our traceability label and the collaborator invite recipient (PII); never a Discord lookup key.';

CREATE INDEX users_type_idx            ON users (type);
CREATE INDEX users_is_admin_idx        ON users (is_admin) WHERE is_admin = TRUE;


-- ===========================================================================
-- spaces
-- The private support conversation per merchant, materialized as a Discord CHANNEL
-- (channel mode only in v1). One merchant -> exactly one space (1:1), enforced by
-- UNIQUE(merchant_id). Tracks the Discord projection (channel id, category) and the
-- fail-closed ACL state.
-- ===========================================================================
CREATE TABLE spaces (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id         UUID NOT NULL REFERENCES merchants(id) ON DELETE RESTRICT,
    -- Discord channel id. NULL between desired-row creation and worker provisioning.
    -- UNIQUE (when present) so two spaces can never project onto the same channel.
    discord_channel_id  TEXT,
    -- The Discord category the channel lives under; the Agent-role VIEW_CHANNEL allow
    -- is applied at THIS category level so agents see all spaces with one overwrite.
    discord_category_id TEXT,
    -- The merchant's Discord role id (created by the API on provision, GuildRoleCreate).
    -- The channel grants this role VIEW_CHANNEL+SEND; collaborators acquire it by joining
    -- through the merchant's invite-with-role link. NULL until provisioned. UNIQUE so two
    -- spaces can never share a merchant role (mirrors the 1:1 merchant<->role invariant).
    merchant_role_id    TEXT,
    name                TEXT NOT NULL,
    lifecycle_state     space_lifecycle_state NOT NULL DEFAULT 'active',
    -- Fail-closed ACL tracking (NFR-4). A space is only treated as accessible to its
    -- members when acl_state='applied'. pending/degraded/failed => invisible.
    acl_state           acl_state NOT NULL DEFAULT 'pending',
    -- Static help-desk presence (FR-15 static, M4): topic/pin content reference.
    welcome_message_id  TEXT,
    last_activity_at    TIMESTAMPTZ,
    reconciled_at       TIMESTAMPTZ,
    drift_count         INTEGER NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at         TIMESTAMPTZ,

    CONSTRAINT spaces_discord_channel_id_key UNIQUE (discord_channel_id),
    -- One merchant role per space (and per merchant, since merchant<->space is 1:1).
    CONSTRAINT spaces_merchant_role_id_key UNIQUE (merchant_role_id),
    -- 1:1 merchant<->space: a merchant has exactly one space (hard invariant).
    CONSTRAINT spaces_merchant_id_key UNIQUE (merchant_id)
);
COMMENT ON TABLE spaces IS 'Per-merchant private channel; one merchant -> exactly one space (1:1, UNIQUE merchant_id) and one merchant role. Tracks fail-closed ACL state.';
COMMENT ON COLUMN spaces.acl_state IS 'Fail-closed: space treated accessible only when applied; else invisible (NFR-4).';
COMMENT ON COLUMN spaces.discord_channel_id IS 'NULL until worker provisions the channel; UNIQUE prevents double-projection.';
COMMENT ON COLUMN spaces.merchant_role_id IS 'Merchant Discord role id (GuildRoleCreate on provision); channel grants it VIEW+SEND. Collaborators acquire it via invite-with-role.';

-- merchant_id is already indexed by the UNIQUE(merchant_id) constraint above.
CREATE INDEX spaces_lifecycle_state_idx    ON spaces (lifecycle_state);
CREATE INDEX spaces_acl_state_idx          ON spaces (acl_state) WHERE acl_state <> 'applied'; -- reconciler targets
CREATE INDEX spaces_last_activity_idx      ON spaces (last_activity_at);


-- ===========================================================================
-- space_members
-- The per-collaborator access mapping (FR-3, FR-4). One row = one collaborator's
-- DESIRED access to one space. Access is ROLE-BASED: the collaborator acquires the
-- space's merchant role natively by joining through the merchant invite-with-role link
-- (no per-user channel overwrite). Agents are NOT listed here; they get access via the
-- category-level Agent role.
--
-- This table is DESIRED state. The reconciler diffs which members carry each merchant
-- role against these rows and removes a merchant role from any member the source of
-- truth does not list here (isolation teeth, NFR-5).
--
-- The invite-send lifecycle (name+email captured at registration; the stored merchant
-- link emailed by the hub) is tracked per row: invite_sent_at marks delivery; the
-- merchant link itself lives on `merchants.invite_link` (reusable, not per collaborator).
-- ===========================================================================
CREATE TABLE space_members (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id          UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role              space_member_role NOT NULL DEFAULT 'collaborator',
    -- Has the merchant invite link been emailed to this collaborator? Set by the notify
    -- worker on successful SMTP send. NULL = registered but invite not yet sent.
    invite_sent_at    TIMESTAMPTZ,
    -- Has the collaborator been observed carrying the merchant role in Discord? Optional
    -- liveness signal (console/reconciler); access does not depend on it being set.
    role_observed_at  TIMESTAMPTZ,
    invited_by        UUID REFERENCES users(id),  -- the Agent who registered (FR-19/FR-20 audit)
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at        TIMESTAMPTZ,                 -- set on channel-scope expulsion; row kept for audit

    -- A user appears at most once per space (active membership).
    CONSTRAINT space_members_space_user_key UNIQUE (space_id, user_id)
);
COMMENT ON TABLE space_members IS 'DESIRED per-collaborator access (role-based). Reconciler strips merchant roles from members not backed by a row here (NFR-5).';
COMMENT ON COLUMN space_members.role IS 'Only collaborators are listed; agents access via category-level role.';
COMMENT ON COLUMN space_members.invite_sent_at IS 'Set by the notify worker when the merchant invite link is emailed to this collaborator.';

CREATE INDEX space_members_space_id_idx ON space_members (space_id);
CREATE INDEX space_members_user_id_idx  ON space_members (user_id);
-- Supports "channels by collaborator" (FR-21) and directory (FR-18) fast paths.
CREATE INDEX space_members_active_idx   ON space_members (user_id, space_id) WHERE revoked_at IS NULL;


-- ===========================================================================
-- (removed) oauth_tokens
-- The OAuth2 `guilds.join` onboarding model was dropped in favour of native Discord
-- invite-with-role links (see 02-architecture.md §6). There are no per-user Discord
-- tokens to store: onboarding carries no Discord credential. The merchant invite link
-- lives on `merchants.invite_link`; the collaborator's email is a traceability label on
-- `users.email`. App-level secrets (bot token, SMTP credentials) are config-by-env.
-- ===========================================================================


-- ===========================================================================
-- api_keys
-- Backoffice -> hub authentication (AuthZ Layer A). Opaque service bearer tokens,
-- stored HASHED only (never the raw key). Each key carries a scope and an optional
-- merchant binding for audit attribution. Multiple active keys per principal enable
-- zero-downtime rotation. Revocation = set revoked_at (instant, no JWT blocklist).
-- ===========================================================================
CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,                 -- human label, e.g. 'backoffice-prod'
    -- SHA-256 (or argon2id) of the raw key. The raw key is shown once at creation
    -- and never stored or logged.
    key_hash     BYTEA NOT NULL,
    -- Coarse scope: 'backoffice' (full control plane) or narrower future scopes.
    scope        TEXT NOT NULL DEFAULT 'backoffice',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,                   -- instant revocation

    CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash)
);
COMMENT ON TABLE api_keys IS 'Backoffice->hub service auth (Layer A). Opaque keys hashed at rest; instantly revocable.';

CREATE INDEX api_keys_active_idx ON api_keys (key_hash) WHERE revoked_at IS NULL;


-- ===========================================================================
-- jobs
-- Postgres mirror of asynq task state so callers poll an authoritative source
-- (Valkey is never source of truth). One row per enqueued mutating operation.
-- The API returns the job id in the 202 response; GET /jobs/{id} reads here.
-- Defined before idempotency_keys because idempotency_keys.job_id references it.
-- ===========================================================================
CREATE TABLE jobs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- asynq task id == idempotency key, for cross-referencing.
    task_id         TEXT NOT NULL,
    kind            TEXT NOT NULL,              -- 'provision_space','invite_collaborator','expel','project_agent_role',...
    queue           TEXT NOT NULL DEFAULT 'provision',
    status          job_status NOT NULL DEFAULT 'pending',
    -- Loose references to the entities the job acts on (nullable; depends on kind).
    merchant_id     UUID REFERENCES merchants(id),
    space_id        UUID REFERENCES spaces(id),
    user_id         UUID REFERENCES users(id),
    payload         JSONB,                      -- redacted snapshot of job input (no secrets)
    error           TEXT,                       -- last error (redacted) if retrying/archived
    retry_count     INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ,

    CONSTRAINT jobs_task_id_key UNIQUE (task_id)
);
COMMENT ON TABLE jobs IS 'Authoritative mirror of asynq task state for polling (Valkey is not source of truth).';

CREATE INDEX jobs_status_idx   ON jobs (status) WHERE status IN ('pending','active','retrying');
CREATE INDEX jobs_space_id_idx ON jobs (space_id);


-- ===========================================================================
-- idempotency_keys
-- Edge-level idempotency for mutating requests (NFR-3). An atomic insert on `key`
-- collapses client retries: a key already present with a stored response is replayed
-- instead of enqueueing a second job. Combined with asynq Unique(ttl)+TaskID and
-- worker-side upserts, retries cannot double-provision.
-- ===========================================================================
CREATE TABLE idempotency_keys (
    key            TEXT PRIMARY KEY,            -- caller Idempotency-Key or derived deterministic key
    request_hash   BYTEA NOT NULL,              -- hash of method+path+body; detects key reuse with different body
    status         job_status NOT NULL DEFAULT 'pending',
    response_code  INTEGER,                     -- stored 202 (or final) to replay
    response_body  JSONB,                       -- stored response to replay verbatim
    job_id         UUID REFERENCES jobs(id),    -- the enqueued job, for polling
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Keys expire so the table does not grow unbounded; matches asynq Unique TTL.
    expires_at     TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '24 hours')
);
COMMENT ON TABLE idempotency_keys IS 'Edge idempotency: replay stored response on duplicate key instead of re-enqueueing (NFR-3).';
COMMENT ON COLUMN idempotency_keys.request_hash IS 'Detects an Idempotency-Key reused with a different request body (409).';

CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);


-- ===========================================================================
-- outbox
-- Transactional outbox: the API writes the desired-state change AND an outbox row in
-- ONE Postgres transaction, then a relay enqueues the asynq job. Guarantees that a
-- committed desired-state change is never lost before enqueue (NFR-3/NFR-9), even if
-- the process dies between the DB commit and the enqueue call.
-- ===========================================================================
CREATE TABLE outbox (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate     TEXT NOT NULL,                -- 'space','space_member','user',...
    aggregate_id  UUID NOT NULL,
    kind          TEXT NOT NULL,                -- job kind to enqueue
    payload       JSONB NOT NULL,               -- redacted job payload
    idempotency_key TEXT NOT NULL,              -- becomes asynq TaskID
    enqueued_at   TIMESTAMPTZ,                  -- NULL until the relay has enqueued it
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_idempotency_key_key UNIQUE (idempotency_key)
);
COMMENT ON TABLE outbox IS 'Transactional outbox: desired-state change + job intent committed atomically; relay enqueues (NFR-3).';

CREATE INDEX outbox_unenqueued_idx ON outbox (created_at) WHERE enqueued_at IS NULL;


-- ===========================================================================
-- audit_log
-- Append-only record of provisioning, membership, lifecycle, and expulsion actions
-- (FR-14): who, what, when. Backs GET /audit. Stores references, never secrets.
-- ===========================================================================
CREATE TABLE audit_log (
    id            BIGSERIAL PRIMARY KEY,
    -- Who acted: the API key principal and/or the acting agent user.
    actor_api_key UUID REFERENCES api_keys(id),
    actor_user_id UUID REFERENCES users(id),
    action        TEXT NOT NULL,                -- 'space.provision','collaborator.invite','collaborator.expel','agent.add','space.archive','reconcile.repair',...
    -- What was acted upon (nullable subset depending on action).
    merchant_id   UUID REFERENCES merchants(id),
    space_id      UUID REFERENCES spaces(id),
    target_user_id UUID REFERENCES users(id),
    scope         expulsion_scope,              -- set for expulsion actions (FR-19)
    -- Structured detail (redacted; never raw tokens).
    detail        JSONB,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
COMMENT ON TABLE audit_log IS 'Append-only who/what/when for provisioning, membership, lifecycle, expulsion (FR-14). No secrets.';

CREATE INDEX audit_log_created_idx       ON audit_log (created_at DESC);
CREATE INDEX audit_log_space_id_idx      ON audit_log (space_id);
CREATE INDEX audit_log_merchant_id_idx   ON audit_log (merchant_id);
CREATE INDEX audit_log_target_user_idx   ON audit_log (target_user_id);
CREATE INDEX audit_log_action_idx        ON audit_log (action);


-- ===========================================================================
-- reconciliation_runs (optional observability, M3+)
-- Records each reconcile sweep and what drift it repaired. Feeds NFR-7 metrics and
-- gives operators visibility into Postgres-wins corrections.
-- ===========================================================================
CREATE TABLE reconciliation_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope           TEXT NOT NULL,              -- 'guild' | 'space'
    space_id        UUID REFERENCES spaces(id),
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    drift_found     INTEGER NOT NULL DEFAULT 0,
    drift_repaired  INTEGER NOT NULL DEFAULT 0,
    error           TEXT
);
COMMENT ON TABLE reconciliation_runs IS 'One row per reconcile sweep; tracks drift found/repaired (NFR-3/NFR-7).';

CREATE INDEX reconciliation_runs_space_idx   ON reconciliation_runs (space_id);
CREATE INDEX reconciliation_runs_started_idx ON reconciliation_runs (started_at DESC);


-- ---------------------------------------------------------------------------
-- updated_at trigger (applied to mutable tables)
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER merchants_set_updated_at    BEFORE UPDATE ON merchants    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER users_set_updated_at        BEFORE UPDATE ON users        FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER spaces_set_updated_at       BEFORE UPDATE ON spaces       FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER jobs_set_updated_at         BEFORE UPDATE ON jobs         FOR EACH ROW EXECUTE FUNCTION set_updated_at();
