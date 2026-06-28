-- Phase 1.18.A-3.B — paginated-asset page count.
--
-- One column: assets.page_count (INTEGER, nullable).
-- Populated by the PDF metadata extractor (pdfcpu) — and in a future
-- phase by the comic / ebook extractors. NULL = "not paginated" OR
-- "extractor hasn't run yet"; both are read the same way by clients
-- (just don't show a "page X of Y" widget).
--
-- Why not store this in metadata JSONB?
--   - The PDF preview handler already writes pdf.num_pages there via
--     pdfinfo (subprocess). Phase 1.18.A-3.B replaces that path with
--     the canonical extractor pipeline; routing page_count through
--     the field-value system would require a new ValueKind for
--     integers PLUS a per-asset field-definition map.
--   - page_count is asset-intrinsic (like pixel dimensions and
--     thumbhash), not user-managed metadata. Sits next to its peers.
--   - Federation: when an asset arrives via inbox, page_count is a
--     property of THE OBJECT, not an editorial decision; co-locating
--     it with pixel_w / pixel_h keeps the projection consistent.
--
-- No index needed: page_count is only ever read alongside the asset
-- row itself, never as a search predicate.

-- +goose Up

ALTER TABLE assets
    ADD COLUMN page_count INTEGER;

COMMENT ON COLUMN assets.page_count IS 'For paginated assets (PDF today; comics + ebooks later), the total page count extracted by the metadata pipeline. NULL = not paginated OR extractor has not run yet; both are read the same way by clients.';

-- +goose Down

ALTER TABLE assets DROP COLUMN IF EXISTS page_count;
