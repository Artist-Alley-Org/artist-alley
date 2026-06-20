-- 00008_collection_metadata.sql
--
-- Phase 1.9.B — Per-collection custom fields.
--
-- # What this migration adds
--
--   1. `field_definition.subject_kind` discriminator (asset|collection)
--      backfilling every existing row to 'asset' via DEFAULT.
--   2. `collection_field_value` — the typed-value table, mirroring
--      the asset_field_value shape one-for-one EXCEPT the set_by
--      enum (collections aren't files, so exif/iptc/xmp don't apply).
--   3. `collection_field_value_history` — append-only audit, same
--      shape as `asset_field_value_history` (old_value/new_value
--      JSONB pair, NOT per-typed columns). Population is by handler
--      INSERT inside the same tx as the upsert/delete; no DB trigger
--      — matches the asset path so cross-domain debuggers see one
--      pattern, not two.
--
-- # What this migration deliberately does NOT do
--
--   - Touch `asset_field_value` or its history. The asset metadata
--     pipeline supports federation v1.0-rc1 (soak window through
--     2026-06-22); changing its storage shape would re-open the
--     soak. The discriminator is additive only.
--   - Add a TSVECTOR on collection values. Collection search is its
--     own scope; deferring keeps the trigger surface clean.
--   - Federate. Collections are local-instance per ADR 0043; values
--     and definitions follow the same scope.

-- +goose Up
-- +goose StatementBegin

-- 1. Discriminator on field_definition. DEFAULT backfills existing
--    rows to 'asset' implicitly; new collection-side rows must set
--    the column explicitly.
ALTER TABLE field_definition
    ADD COLUMN subject_kind TEXT NOT NULL DEFAULT 'asset'
        CHECK (subject_kind IN ('asset', 'collection'));

-- Index for the common admin list/filter path:
--   "active collection fields ordered for display"
CREATE INDEX idx_field_definition_subject_kind
    ON field_definition (subject_kind, status, display_order);

-- 2. collection_field_value — typed-value table.
--
--    Primary key is (collection_id, field_id): one value per
--    collection/field pair, identical to the asset side. Cascade
--    delete on the collection drops every value with it; cascade
--    delete on the field definition drops every value of that
--    field across all collections (matches asset_field_value).
CREATE TABLE collection_field_value (
    collection_id     UUID NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    field_id          UUID NOT NULL REFERENCES field_definition(id) ON DELETE CASCADE,
    value_text        TEXT,
    value_num         DOUBLE PRECISION,
    value_date        TIMESTAMPTZ,
    value_options     TEXT[],
    value_ref         UUID,
    set_by            TEXT NOT NULL DEFAULT 'manual'
        CHECK (set_by IN ('manual', 'api', 'import', 'computed')),
    set_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    set_by_user_ref   BIGINT,
    PRIMARY KEY (collection_id, field_id)
);

CREATE INDEX idx_collection_field_value_collection_id
    ON collection_field_value (collection_id);

CREATE INDEX idx_collection_field_value_field_id
    ON collection_field_value (field_id);

-- 3. collection_field_value_history — append-only audit.
--    Mirrors the asset history shape: a JSONB pair for old/new
--    rather than per-typed columns. The handler is responsible for
--    serialising the value-of-the-day into JSON before writing.
CREATE TABLE collection_field_value_history (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id        UUID NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    field_id             UUID NOT NULL REFERENCES field_definition(id) ON DELETE CASCADE,
    old_value            JSONB,
    new_value            JSONB,
    set_by               TEXT NOT NULL DEFAULT 'manual',
    changed_by_user_ref  BIGINT,
    changed_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX cfvh_collection_idx
    ON collection_field_value_history (collection_id, changed_at DESC);

CREATE INDEX cfvh_field_idx
    ON collection_field_value_history (field_id, changed_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS collection_field_value_history;
DROP TABLE IF EXISTS collection_field_value;
DROP INDEX IF EXISTS idx_field_definition_subject_kind;
ALTER TABLE field_definition DROP COLUMN IF EXISTS subject_kind;
-- +goose StatementEnd
