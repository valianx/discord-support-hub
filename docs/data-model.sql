-- discord-support-hub — PostgreSQL data model (M0–M3)
-- Source of truth for roster, mappings, and authorization (NFR-9, NFR-13).
-- Conforms to 01-mvp-scope.md (scope-locked) and 02-architecture.md.
--
-- Design rules encoded here:
--   * Postgres is the AuthZ source of truth; Discord is a projection (three-layer truth model).
--   * Multi-tenant isolation is enforced by the schema where the DB can (NFR-5).
--   * Fail-closed ACL state is tracked, never assumed open (NFR-4).
--   * One merchant -> N spaces, NO hard cap (typically one active). A one-at-a-time
--     rule, if ever wanted, is a userland concern; the optional partial-unique seam
--     is shown (commented) on the `spaces` table.
--   * Secrets (OAuth2 tokens) stored encrypted at rest; only ciphertext + nonce + key
--     version persisted (NFR-6).
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
-- One external customer. Owns N spaces and N collaborators. The merchant grouping
-- lives HERE (in Postgres), never as a Discord role (Discord caps at 250 roles;
-- a role-per-merchant does not scale -- see scope §4.3).
-- ===========================================================================
CREATE TABLE merchants (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Stable external key from the Zippy backoffice; lets the backoffice address a
    -- merchant by its own id idempotently. Unique to prevent duplicate provisioning.
    external_ref    TEXT NOT NULL,
    name            TEXT NOT NULL,
    -- Per-merchant help-desk link parameterization (FR-15 static); nullable.
    help_desk_url   TEXT,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT merchants_external_ref_key UNIQUE (external_ref)
);
COMMENT ON TABLE merchants IS 'External customers; merchant grouping is a DB fact, never a Discord role.';
COMMENT ON COLUMN merchants.external_ref IS 'Stable id from the Zippy backoffice; enables idempotent addressing.';


-- ===========================================================================
-- users
-- The roster and AuthZ source of truth (FR-9, FR-23, NFR-13). `type` (agent/
-- collaborator) and `is_admin` are authoritative here; the Discord Agent role is a
-- projection the bot maintains. Authorization is ALWAYS resolved against this table,
-- never against the Discord role.
--
-- A collaborator belongs to exactly one merchant; an agent belongs to none
-- (merchant_id NULL). A CHECK enforces that invariant at the row level.
-- ===========================================================================
CREATE TABLE users (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type              user_type NOT NULL,
    -- Admin privilege is only meaningful for agents (roster management safeguard).
    is_admin          BOOLEAN NOT NULL DEFAULT FALSE,
    -- Collaborators are owned by a merchant; agents are not. Enforced by chk below.
    merchant_id       UUID REFERENCES merchants(id) ON DELETE RESTRICT,
    -- Discord identity. Nullable until the user completes "Connect with Discord"
    -- (OAuth2 identify) -- a roster row can exist before the user has joined.
    discord_user_id   TEXT,
    email             CITEXT,
    display_name      TEXT,
    -- Set once the bot has added the user to the guild and projected role/overwrite.
    provisioned_at    TIMESTAMPTZ,
    is_active         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Isolation/consistency invariants the DB CAN enforce:
    -- agents have no merchant; collaborators must have one.
    CONSTRAINT users_type_merchant_chk CHECK (
        (type = 'agent'        AND merchant_id IS NULL) OR
        (type = 'collaborator' AND merchant_id IS NOT NULL)
    ),
    -- Admin only meaningful for agents.
    CONSTRAINT users_admin_only_agent_chk CHECK (is_admin = FALSE OR type = 'agent'),
    -- One Discord identity maps to at most one user row (when present).
    CONSTRAINT users_discord_user_id_key UNIQUE (discord_user_id)
);
COMMENT ON TABLE users IS 'Roster + AuthZ source of truth; Discord role is a projection of type/is_admin.';
COMMENT ON COLUMN users.discord_user_id IS 'NULL until the user completes OAuth2 identify; one identity = one row.';
COMMENT ON CONSTRAINT users_type_merchant_chk ON users IS 'Isolation invariant: collaborators belong to exactly one merchant; agents to none.';

CREATE INDEX users_merchant_id_idx     ON users (merchant_id) WHERE merchant_id IS NOT NULL;
CREATE INDEX users_type_idx            ON users (type);
CREATE INDEX users_is_admin_idx        ON users (is_admin) WHERE is_admin = TRUE;


-- ===========================================================================
-- spaces
-- The private support conversation per merchant, materialized as a Discord CHANNEL
-- (channel mode only in v1). One merchant -> N spaces, NO hard cap. Tracks the
-- Discord projection (channel id, category) and the fail-closed ACL state.
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

    CONSTRAINT spaces_discord_channel_id_key UNIQUE (discord_channel_id)
);
COMMENT ON TABLE spaces IS 'Per-merchant private channel; one merchant -> N spaces, no hard cap. Tracks fail-closed ACL state.';
COMMENT ON COLUMN spaces.acl_state IS 'Fail-closed: space treated accessible only when applied; else invisible (NFR-4).';
COMMENT ON COLUMN spaces.discord_channel_id IS 'NULL until worker provisions the channel; UNIQUE prevents double-projection.';

CREATE INDEX spaces_merchant_id_idx        ON spaces (merchant_id);
CREATE INDEX spaces_lifecycle_state_idx    ON spaces (lifecycle_state);
CREATE INDEX spaces_acl_state_idx          ON spaces (acl_state) WHERE acl_state <> 'applied'; -- reconciler targets
CREATE INDEX spaces_last_activity_idx      ON spaces (last_activity_at);

-- OPTIONAL SEAM (operator decision, scope §6.2): enforce exactly-one-active space
-- per merchant at the DB level. Left COMMENTED -- default is "no cap, userland rule".
-- Uncomment to make one-active-at-a-time a hard invariant:
-- CREATE UNIQUE INDEX spaces_one_active_per_merchant_uq
--     ON spaces (merchant_id) WHERE lifecycle_state = 'active';


-- ===========================================================================
-- space_members
-- The per-collaborator overwrite mapping (FR-3, FR-4). One row = one collaborator's
-- access to one space, projected as a Discord per-user permission overwrite
-- (ChannelPermissionSet, PermissionOverwriteTypeMember). Agents are NOT listed here;
-- they get access via the category-level Agent role, not per-space overwrites.
--
-- This table is DESIRED state. The worker projects it onto Discord; the reconciler
-- revokes any Discord overwrite NOT backed by a row here (isolation teeth, NFR-5).
-- ===========================================================================
CREATE TABLE space_members (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    space_id          UUID NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    role              space_member_role NOT NULL DEFAULT 'collaborator',
    -- Projection state: has the per-user overwrite been applied in Discord?
    overwrite_applied BOOLEAN NOT NULL DEFAULT FALSE,
    invited_by        UUID REFERENCES users(id),  -- the Agent who invited (FR-19/FR-20 audit)
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at        TIMESTAMPTZ,                 -- set on channel-scope expulsion; row kept for audit

    -- A user appears at most once per space (active membership).
    CONSTRAINT space_members_space_user_key UNIQUE (space_id, user_id)
);
COMMENT ON TABLE space_members IS 'DESIRED per-collaborator overwrites. Reconciler revokes Discord overwrites not backed here (NFR-5).';
COMMENT ON COLUMN space_members.role IS 'Only collaborators are listed; agents access via category-level role.';

CREATE INDEX space_members_space_id_idx ON space_members (space_id);
CREATE INDEX space_members_user_id_idx  ON space_members (user_id);
-- Supports "channels by collaborator" (FR-21) and directory (FR-18) fast paths.
CREATE INDEX space_members_active_idx   ON space_members (user_id, space_id) WHERE revoked_at IS NULL;


-- ===========================================================================
-- oauth_tokens
-- Encrypted-at-rest OAuth2 tokens captured at /oauth/discord/callback (FR-22, NFR-6).
-- The `guilds.join` access token lets the bot add the user to the guild
-- (GuildMemberAdd uses GuildMemberAddParams.AccessToken). We store ONLY ciphertext
-- + nonce + key version -- never plaintext, never logged.
-- ===========================================================================
CREATE TABLE oauth_tokens (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- AES-256-GCM ciphertext of the access/refresh tokens.
    access_token_cipher    BYTEA NOT NULL,
    access_token_nonce     BYTEA NOT NULL,
    refresh_token_cipher   BYTEA,
    refresh_token_nonce    BYTEA,
    -- Key version enables rotation without downtime (re-encrypt under new key).
    encryption_key_version INTEGER NOT NULL DEFAULT 1,
    scopes                 TEXT NOT NULL,          -- e.g. 'identify guilds.join'
    expires_at             TIMESTAMPTZ,            -- access token expiry; refresh before use
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One current token record per user (rotated in place on refresh/reconnect).
    CONSTRAINT oauth_tokens_user_id_key UNIQUE (user_id)
);
COMMENT ON TABLE oauth_tokens IS 'Encrypted-at-rest OAuth2 tokens (guilds.join). Only ciphertext+nonce+key_version stored (NFR-6).';
COMMENT ON COLUMN oauth_tokens.encryption_key_version IS 'Supports key rotation: re-encrypt rows under a new key version.';


-- ===========================================================================
-- api_keys
-- Backoffice -> hub authentication (AuthZ Layer A). Opaque service bearer tokens,
-- stored HASHED only (never the raw key). Each key carries a scope and an optional
-- merchant binding for audit attribution. Multiple active keys per principal enable
-- zero-downtime rotation. Revocation = set revoked_at (instant, no JWT blocklist).
-- ===========================================================================
CREATE TABLE api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,                 -- human label, e.g. 'zippy-backoffice-prod'
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
CREATE TRIGGER oauth_tokens_set_updated_at BEFORE UPDATE ON oauth_tokens FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER jobs_set_updated_at         BEFORE UPDATE ON jobs         FOR EACH ROW EXECUTE FUNCTION set_updated_at();
