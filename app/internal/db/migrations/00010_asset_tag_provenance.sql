-- 00010_asset_tag_provenance.sql
--
-- Phase 1.14.A-bridge — asset_tag gains provenance columns so the
-- bridge layer can distinguish operator-set tags from AI-generated
-- tags and preserve manual data across AI re-runs.
--
-- # Audit-first deltas vs brief
--
--   - Existing asset_tag column is named `tag`, NOT `value`. Brief
--     assumed `value`; corrected here + in the queries.sql additions.
--   - Caption columns on the assets table are deferred to a
--     follow-up. The bridge ships a CaptionWriter interface that
--     returns ErrNotImplementedYet until that schema lands; the
--     ai.caption job handler also returns terminal failure cleanly.
--     Skipping the caption schema in this PR keeps the change focused
--     on what's load-bearing for the AI tag pipeline.

-- +goose Up
-- +goose StatementBegin

-- 1. asset_tag provenance columns.
--    Existing rows backfill to source='manual' via the DEFAULT (no
--    UPDATE needed). The CHECK constraint enforces the closed enum
--    matching ai package's TagSource constants.
ALTER TABLE asset_tag
    ADD COLUMN source              TEXT NOT NULL DEFAULT 'manual'
        CHECK (source IN ('manual', 'ai', 'import')),
    ADD COLUMN confidence          REAL
        CHECK (confidence IS NULL OR (confidence >= 0.0 AND confidence <= 1.0)),
    ADD COLUMN created_by_provider TEXT,
    ADD COLUMN created_by_model    TEXT;

-- 2. Indexes.
--
--    idx_asset_tag_ai_provenance — operator analytics: "find all
--    AI-generated tags from a specific stale model" (selective
--    re-generation by provider/model). Partial index keeps it small
--    when the catalogue is mostly manual tags.
CREATE INDEX idx_asset_tag_ai_provenance
    ON asset_tag (created_by_model, added_at DESC)
    WHERE source = 'ai';

--    idx_asset_tag_asset_source — backs the merge-by-source query
--    `DELETE FROM asset_tag WHERE asset_id = $1 AND source = 'ai'`
--    that SetAITagsForAsset runs inside its transaction.
CREATE INDEX idx_asset_tag_asset_source
    ON asset_tag (asset_id, source);

-- 3. Default config keys the bridge layer reads.
--
--    ai.tag.confidence_threshold — UI threshold; tags below this
--    confidence don't render in the default asset detail view.
--    Operator can drop to 0 to see every AI suggestion.
--
--    ai.tag.merge_semantics — sentinel for the merge mode. Only
--    "preserve_manual" is implemented in 1.14.A-bridge. A future
--    "replace_all" or "additive_only" mode would land here as a
--    string enum extension.
INSERT INTO system_config (key, value) VALUES
    ('ai.tag.confidence_threshold', '0.5'::jsonb),
    ('ai.tag.merge_semantics',      '"preserve_manual"'::jsonb)
ON CONFLICT (key) DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM system_config WHERE key IN ('ai.tag.confidence_threshold', 'ai.tag.merge_semantics');
DROP INDEX IF EXISTS idx_asset_tag_asset_source;
DROP INDEX IF EXISTS idx_asset_tag_ai_provenance;
ALTER TABLE asset_tag
    DROP COLUMN IF EXISTS created_by_model,
    DROP COLUMN IF EXISTS created_by_provider,
    DROP COLUMN IF EXISTS confidence,
    DROP COLUMN IF EXISTS source;
-- +goose StatementEnd
