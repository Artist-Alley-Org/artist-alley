-- 00016_per_user_asset_dedup.sql
--
-- Phase 1.18.A-2 follow-up A — per-user asset-row deduplication.
--
-- Storage already deduplicates BYTES globally
-- (storage_objects.hash unique); this migration adds row-level
-- dedup so the same user can't accidentally create two asset
-- rows pointing at the same hash. Two different users still get
-- two asset rows (deliberate — they own different copies; the
-- bytes plane is the only shared layer).
--
-- # The constraint
--
-- Partial unique index on (owner_user_ref, file_hash) WHERE
-- file_hash IS NOT NULL AND deleted_at IS NULL. Excluding
-- soft-deleted rows means an operator can DELETE then re-upload
-- the same bytes (e.g., to fix a corrupted earlier import). The
-- partial-WHERE on file_hash NOT NULL handles the bootstrap
-- case where an asset row exists without a content-addressed
-- file.
--
-- # Per-team / global scope (follow-up)
--
-- The system_config upload.dedup_scope seeded by migration
-- 00015 supports per_user / per_team / global / off. This
-- migration only adds the per_user database guarantee — per_team
-- and global are application-level checks (no DB constraint
-- possible since the scope can change at runtime). Per_user is
-- the default; widening to per_team requires the operator
-- explicitly setting the sysconfig + the upload handler running
-- the pre-insert visibility-aware query.

-- +goose Up
-- +goose StatementBegin

CREATE UNIQUE INDEX idx_assets_owner_hash_unique
    ON assets(owner_user_ref, file_hash)
    WHERE file_hash IS NOT NULL AND deleted_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_assets_owner_hash_unique;

-- +goose StatementEnd
