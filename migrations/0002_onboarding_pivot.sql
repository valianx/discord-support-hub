-- discord-support-hub — M6 onboarding pivot migration
-- Applies cleanly on a fresh DB (after 0001_init.sql) and on a DB at 0001 version.
--
-- Changes:
--   1. DROP oauth_tokens table (+ its updated_at trigger) — OAuth2 removed (AC-M6-9, AC-M6-10).
--   2. ADD spaces.merchant_role_id TEXT UNIQUE — the Discord role id created on provision (AC-M6-1).
--   3. ADD merchants.invite_link TEXT — the operator-created native invite-with-role URL (AC-M6-3).
--   4. ADD merchants.invite_link_set_at TIMESTAMPTZ — timestamp of last PUT /merchants/{id}/invite.
--   5. space_members: ADD invite_sent_at TIMESTAMPTZ, ADD role_observed_at TIMESTAMPTZ,
--      DROP overwrite_applied (role-based access replaces per-user overwrites) (AC-M6-4, AC-M6-10).

-- ---------------------------------------------------------------------------
-- 1. Drop oauth_tokens (+ its trigger) — OAuth2 onboarding fully removed
-- ---------------------------------------------------------------------------
DROP TRIGGER IF EXISTS oauth_tokens_set_updated_at ON oauth_tokens;
DROP TABLE IF EXISTS oauth_tokens;

-- ---------------------------------------------------------------------------
-- 2. Add spaces.merchant_role_id — Discord role auto-created on provision
-- ---------------------------------------------------------------------------
ALTER TABLE spaces
    ADD COLUMN IF NOT EXISTS merchant_role_id TEXT;

-- Enforce the 1:1 merchant<->role invariant: two spaces must never share a role.
-- The constraint name matches docs/data-model.sql so comparisons remain clean.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.table_constraints
        WHERE table_name = 'spaces'
          AND constraint_name = 'spaces_merchant_role_id_key'
    ) THEN
        ALTER TABLE spaces ADD CONSTRAINT spaces_merchant_role_id_key UNIQUE (merchant_role_id);
    END IF;
END$$;

COMMENT ON COLUMN spaces.merchant_role_id IS
    'Merchant Discord role id (GuildRoleCreate on provision); channel grants it VIEW+SEND. '
    'Collaborators acquire it via the merchant invite-with-role link. NULL until provisioned. '
    'UNIQUE so two spaces can never share a merchant role (mirrors the 1:1 merchant<->role invariant).';

-- ---------------------------------------------------------------------------
-- 3 & 4. Add merchants.invite_link and merchants.invite_link_set_at
-- ---------------------------------------------------------------------------
ALTER TABLE merchants
    ADD COLUMN IF NOT EXISTS invite_link TEXT,
    ADD COLUMN IF NOT EXISTS invite_link_set_at TIMESTAMPTZ;

COMMENT ON COLUMN merchants.invite_link IS
    'Operator-created native invite-with-role link (client-only feature); stored here, '
    'emailed by the hub. NULL blocks :send-invite.';
COMMENT ON COLUMN merchants.invite_link_set_at IS
    'Timestamp of the last PUT /merchants/{id}/invite that stored the current link.';

-- ---------------------------------------------------------------------------
-- 5. Evolve space_members to role-based membership
--    ADD invite_sent_at  — stamped by the notify worker on SMTP send
--    ADD role_observed_at — optional liveness signal (console/reconciler)
--    DROP overwrite_applied — per-user overwrites are replaced by the merchant role
-- ---------------------------------------------------------------------------
ALTER TABLE space_members
    ADD COLUMN IF NOT EXISTS invite_sent_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS role_observed_at TIMESTAMPTZ;

ALTER TABLE space_members
    DROP COLUMN IF EXISTS overwrite_applied;

COMMENT ON COLUMN space_members.invite_sent_at IS
    'Set by the notify worker when the merchant invite link is emailed to this collaborator.';
COMMENT ON COLUMN space_members.role_observed_at IS
    'Has the collaborator been observed carrying the merchant role in Discord? '
    'Optional liveness signal; access does not depend on it being set.';
