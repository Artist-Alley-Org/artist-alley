-- 00006_resource_request.sql
--
-- Phase 1.17.E — Resource request workflow.
--
-- Operators occasionally need to grant a temporary capability to a
-- user who lacks it — a one-off "let alice see this restricted
-- asset for 7 days" workflow. Before 1.17.E that was an out-of-
-- band Slack message + an admin manually inserting a
-- user_capability_grants row. This phase formalises it:
--
--   * The user POSTs a request via /assets/{id}/request-access.
--   * An approver (anyone holding share.grant globally or on the
--     asset's owning team) reviews via /admin/requests.
--   * The decision (grant / deny) lands as a row in resource_request
--     AND, when granted, materialises as a row in
--     user_capability_grants with expires_at set.
--
-- # Why one ledger + one grant table (not a fresh share table)
--
-- user_capability_grants already exists, already handles team-
-- scoping (team_id NULL = global; UUID = team-scoped), already
-- has expires_at (Phase 1.17.C), and the CapabilitySweeper already
-- reaps expired rows. Layering a separate share table on top would
-- duplicate the resolver, the cache, AND the sweeper.
--
-- resource_request is the lifecycle ledger (pending → granted /
-- denied / expired); user_capability_grants is the effect of
-- granting. The back-reference request_ref column lets the
-- CapabilitySweeper mark the request expired when the grant
-- reaps.
--
-- # Why typed state CHECK at the schema
--
-- ADR 0042 distributed catalogues + the Phase 1.17.A precedent:
-- typed Go constants at call sites + schema CHECK at the wire
-- boundary. A bad state value can't sneak in via raw SQL.
--
-- # Why per-sensitivity expiry defaults in system_config
--
-- Restricted / embargo are the time-bounded tiers; team / public
-- default to permanent. Operators can override via the per-
-- request expires_at param at decision time; the system_config
-- key is the "if you didn't specify, use this" fallback.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE resource_request (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_user_ref BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    target_asset_id UUID NOT NULL,
    requested_capability TEXT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'granted', 'denied', 'expired')),
    decided_at TIMESTAMPTZ,
    decided_by_user_ref BIGINT REFERENCES "user"(ref),
    decision_reason TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Hot-path indexes:
--
--   * The approver-facing /admin/requests list scans pending rows
--     ordered by requested_at ASC; partial index keeps it cheap.
--   * The requester-facing /account/requests list scans by
--     requester_user_ref ordered by requested_at DESC; covered by
--     a separate partial index keyed by requester.
--   * Asset-level inspection ("any open requests on this asset?")
--     uses the by-asset index.

CREATE INDEX idx_resource_request_pending_oldest_first
    ON resource_request(requested_at ASC)
    WHERE state = 'pending';

CREATE INDEX idx_resource_request_by_requester
    ON resource_request(requester_user_ref, requested_at DESC);

CREATE INDEX idx_resource_request_by_asset
    ON resource_request(target_asset_id);

-- Back-reference on user_capability_grants. NULL for the direct
-- admin grants from 1.17.C (`AddAdminUserGrant`); populated only
-- when the grant was the consequence of a granted resource_request.
-- ON DELETE SET NULL because deleting a request row shouldn't
-- cascade-delete the grant (the grant is the user's effective
-- capability, distinct lifecycle).

ALTER TABLE user_capability_grants
    ADD COLUMN request_ref UUID REFERENCES resource_request(id) ON DELETE SET NULL;

CREATE INDEX idx_user_capability_grants_request_ref
    ON user_capability_grants(request_ref)
    WHERE request_ref IS NOT NULL;

-- system_config defaults — operator-tunable per-sensitivity expiry.
-- Values are JSONB numbers in DAYS, or JSONB null for "permanent".
INSERT INTO system_config (key, value) VALUES
    ('requests.default_expiry_days.restricted', '7'::jsonb),
    ('requests.default_expiry_days.embargo',    '7'::jsonb),
    ('requests.default_expiry_days.team',       'null'::jsonb),
    ('requests.default_expiry_days.public',     'null'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM system_config WHERE key LIKE 'requests.default_expiry_days.%';
DROP INDEX IF EXISTS idx_user_capability_grants_request_ref;
ALTER TABLE user_capability_grants DROP COLUMN IF EXISTS request_ref;
DROP INDEX IF EXISTS idx_resource_request_by_asset;
DROP INDEX IF EXISTS idx_resource_request_by_requester;
DROP INDEX IF EXISTS idx_resource_request_pending_oldest_first;
DROP TABLE IF EXISTS resource_request;
-- +goose StatementEnd
