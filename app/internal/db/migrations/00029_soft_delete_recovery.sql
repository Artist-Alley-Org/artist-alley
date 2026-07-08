-- Phase 1.55.C-1 — Soft-delete recovery window (GDPR-shaped).
--
-- Ships §4.6 of docs/v0_1_readiness.md. Adds an operator-visible
-- delete reason + brings collections onto the soft-delete pattern
-- assets + posts already use.
--
-- Pre-audit discovery: only 2 of 4 target entities had deleted_at
-- pre-existing (assets + posts). Collections used HARD DELETE.
-- User uses a 4-state enum (UserStateArchived) instead of
-- deleted_at — GC-past-retention hooks THAT state machine,
-- so no user-table schema change here.
--
-- Design decisions:
--
--   - deleted_reason is a nullable TEXT with no length constraint
--     at the DB layer (handlers cap at 500 chars). Nullable so
--     existing soft-deleted rows on assets + posts don't require
--     backfill.
--   - Never federates. Per-instance operator metadata explaining
--     why the row went away; peers don't share our reason strings.
--     No origin_server_id column added here.
--   - collections.deleted_at follows the same pattern assets + posts
--     already use: NULL = live, non-NULL = soft-deleted. Partial
--     indexes match the existing per-owner + name lookup shape.
--   - Weighted-tsvector GIN index gets a WHERE deleted_at IS NULL
--     predicate — matches assets/posts convention so future search
--     paths naturally exclude soft-deleted collections.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE assets      ADD COLUMN deleted_reason TEXT NULL;
ALTER TABLE posts       ADD COLUMN deleted_reason TEXT NULL;
ALTER TABLE collections ADD COLUMN deleted_at     TIMESTAMPTZ NULL;
ALTER TABLE collections ADD COLUMN deleted_reason TEXT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- Match the assets/posts partial-index convention: active-row
-- indexes exclude soft-deleted rows so per-owner lookups stay
-- fast on the common path.
CREATE INDEX collections_owner_active_idx
    ON collections (owner_user_ref)
    WHERE deleted_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
-- Time-window scan for the nightly GC coordinator:
-- WHERE deleted_at IS NOT NULL AND deleted_at < NOW() - INTERVAL '<n> days'.
-- Same pattern as would be needed on assets + posts if their
-- GC path ever grew a hot loop.
CREATE INDEX collections_deleted_at_idx
    ON collections (deleted_at)
    WHERE deleted_at IS NOT NULL;

CREATE INDEX assets_deleted_at_idx
    ON assets (deleted_at)
    WHERE deleted_at IS NOT NULL;

CREATE INDEX posts_deleted_at_idx
    ON posts (deleted_at)
    WHERE deleted_at IS NOT NULL;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS posts_deleted_at_idx;
DROP INDEX IF EXISTS assets_deleted_at_idx;
DROP INDEX IF EXISTS collections_deleted_at_idx;
DROP INDEX IF EXISTS collections_owner_active_idx;
ALTER TABLE collections DROP COLUMN IF EXISTS deleted_reason;
ALTER TABLE collections DROP COLUMN IF EXISTS deleted_at;
ALTER TABLE posts       DROP COLUMN IF EXISTS deleted_reason;
ALTER TABLE assets      DROP COLUMN IF EXISTS deleted_reason;
-- +goose StatementEnd
