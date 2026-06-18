-- 00003_user_state_check.sql
--
-- Phase 1.17.A — User approval state machine.
--
-- The `user.approved` column has existed since the RS baseline as
-- BIGINT with int magic 0/1/2 scattered through call sites. 1.17.A
-- introduces:
--
--   1. A CHECK constraint pinning the legal set to {0,1,2,3} —
--      adds `archived` (= 3) as a fourth state, complementing the
--      existing pending (0) / active (1) / disabled (2).
--   2. A `pending_capabilities` row in system_config — the
--      restricted capability set granted to pending users so they
--      can authenticate + view the "waiting for approval" page
--      but nothing else.
--
-- Schema stays BIGINT — pre-MVP volatility lets us defer the TEXT
-- enum migration to a polish phase. Typed Go constants in
-- internal/users/userstate.go are the call-site barrier; this
-- CHECK is the load-bearing schema-side barrier.
--
-- # Why archived is separate from disabled
--
-- Both reject login (CanAuthenticate() returns false in both cases).
-- The split is operator-facing:
--
--   * disabled = "temporarily revoked" — appears in the default
--     admin list; an operator might restore them tomorrow.
--   * archived = "this user has left the org" — hidden from the
--     default admin list; permanent unless explicitly restored.
--
-- # Why pending_capabilities lives in system_config (not a separate table)
--
-- The set is read once on every login and effectively never written
-- (operator might edit it from /admin/system/users once a year). A
-- JSONB column on system_config with a typed key matches the
-- pattern used by site / smtp / auth / ai config blocks already.
--
-- Design references
--
--   * Phase 1.17.A brief — typed state constants, single-gate
--     assertCanAuthenticate, last-admin invariant extension.
--   * ADR 0042 — distributed catalogues (per-domain capability
--     constants, no central registry).
--   * ADR 0046 — append-only migrations.
--   * memory project_bootstrap_admin_workflow — bootstrap admin is
--     created with approved=1 (active); guard against accidentally
--     making the bootstrap admin pending.

-- +goose Up
-- +goose StatementBegin

-- 1. CHECK constraint on user.approved. NOT VALID at first to keep
--    the lock acquisition cheap on large installs, then VALIDATE
--    immediately — every existing row sits inside {0,1,2} from the
--    legacy code so the validation pass is free.
ALTER TABLE "user"
    ADD CONSTRAINT user_approved_check
    CHECK (approved IN (0, 1, 2, 3))
    NOT VALID;

ALTER TABLE "user"
    VALIDATE CONSTRAINT user_approved_check;

-- 2. Seed the pending-user capability set. The value is the
--    minimal viable set: a pending user can see their own profile
--    page (to read the "waiting for approval" copy) but cannot
--    do anything else. Operators may extend this via the
--    /admin/system/users surface (1.17.A commit 3).
--
--    ON CONFLICT keeps re-runs idempotent — re-bootstrapping a
--    fresh DB doesn't overwrite an operator's customisations.
INSERT INTO system_config (key, value)
VALUES (
    'users.pending_capabilities',
    '["profile.read_self"]'::jsonb
)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DELETE FROM system_config WHERE key = 'users.pending_capabilities';

ALTER TABLE "user" DROP CONSTRAINT IF EXISTS user_approved_check;

-- +goose StatementEnd
