-- Phase 1.16.B-1 — search foundation.
--
-- Adds collections.search_text so the unified GET /search endpoint
-- can rank collections alongside assets and posts (both of which
-- already carry search_text columns from the 00001 baseline). The
-- shape mirrors the existing rebuild_asset_search_text / rebuild_
-- post_search_text pattern: a plain to_tsvector('english', ...)
-- over the concatenated text (single weight class), maintained by
-- a per-row AFTER-INSERT-OR-UPDATE trigger that calls a rebuild
-- function.
--
-- Divergence from the phase brief's "field-weighted A/B/C/D" design:
--
--   Pre-audit Q1 revealed that assets + posts do NOT currently use
--   setweight() — they compute a single-class tsvector. To keep the
--   three entities consistent (so ts_rank_cd behaves the same way
--   across them) and to avoid a large one-shot re-vectorisation of
--   the existing assets + posts populations (assets pulls custom-
--   field-value text through a join), collections joins the same
--   unweighted pattern. ts_rank_cd remains meaningful (density,
--   proximity, length-normalisation) without setweight. A weighted
--   retrofit of all three rebuild functions is a clean seam left
--   for a later sub-phase.
--
-- Backfill is inline + batched (1000 rows/batch) with a fast-exit
-- for large corpora: if the pre-migration row count exceeds 10 000
-- we seed the column + trigger + index only and defer the backfill
-- to the admin reindex tooling shipping in 1.16.B-5. In a small
-- dev / MVP install this path never fires; on a large corpus the
-- migration still runs to completion in seconds and the /search
-- endpoint's collection results start populating as the reindex
-- job walks the tail.

-- +goose Up

-- +goose StatementBegin

-- 1. Column.
ALTER TABLE collections
    ADD COLUMN search_text TSVECTOR;

COMMENT ON COLUMN collections.search_text IS
    'Postgres TSVECTOR maintained by the collections_search_text trigger over (name, description). Unweighted to match the assets + posts pattern (Phase 1.16.B-1). Backed by the collections_search_text_gin index. Consumed by the unified /search endpoint.';

-- 2. Rebuild function. Kept as a callable proc so admin tooling +
--    tests can force-recompute a single row without triggering the
--    write path.
CREATE FUNCTION public.rebuild_collection_search_text(p_collection_id UUID) RETURNS void
    LANGUAGE plpgsql
    AS $fn$
BEGIN
    UPDATE collections
       SET search_text = to_tsvector('english',
                            COALESCE(name, '') || ' ' ||
                            COALESCE(description, ''))
     WHERE id = p_collection_id;
END;
$fn$;

COMMENT ON FUNCTION public.rebuild_collection_search_text(UUID) IS
    'Recompute collections.search_text for one row. Called by the AFTER-INSERT/UPDATE trigger + by the future admin reindex tooling.';

-- 3. Trigger — mirrors the posts_search_text_trigger shape from
--    the 00001 baseline (fires AFTER INSERT OR UPDATE OF the source
--    columns; delegates to the rebuild function).
CREATE FUNCTION public.collections_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $fn$
BEGIN
    PERFORM rebuild_collection_search_text(NEW.id);
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER collections_search_text
    AFTER INSERT OR UPDATE OF name, description ON collections
    FOR EACH ROW EXECUTE FUNCTION collections_search_text_trigger();

-- 4. GIN index. No WHERE clause — collections has no deleted_at
--    column today (expires_at is the soft-lifecycle signal, but the
--    row is meaningful right up to expiry). The GIN entry set stays
--    small even at scale because search_text is naturally sparse.
CREATE INDEX collections_search_text_gin
    ON collections USING gin (search_text);

-- 5. Inline batched backfill for existing rows. Guarded on a fast
--    count: if the corpus is > 10 000 rows we skip and leave the
--    tail for admin reindex tooling.
DO $$
DECLARE
    batch_size CONSTANT INT := 1000;
    row_est BIGINT;
    updated_count INT;
BEGIN
    SELECT COUNT(*) INTO row_est FROM collections;
    IF row_est > 10000 THEN
        RAISE NOTICE 'search_foundation: collections corpus is % rows — deferring inline backfill to admin reindex tooling (Phase 1.16.B-5).', row_est;
        RETURN;
    END IF;
    LOOP
        UPDATE collections
           SET search_text = to_tsvector('english',
                                COALESCE(name, '') || ' ' ||
                                COALESCE(description, ''))
         WHERE id IN (
             SELECT id FROM collections WHERE search_text IS NULL LIMIT batch_size
         );
        GET DIAGNOSTICS updated_count = ROW_COUNT;
        RAISE NOTICE 'search_foundation: collections backfill batch: % rows', updated_count;
        EXIT WHEN updated_count = 0;
    END LOOP;
END $$;

-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP TRIGGER IF EXISTS collections_search_text ON collections;
DROP FUNCTION IF EXISTS public.collections_search_text_trigger();
DROP FUNCTION IF EXISTS public.rebuild_collection_search_text(UUID);
DROP INDEX IF EXISTS collections_search_text_gin;
ALTER TABLE collections DROP COLUMN IF EXISTS search_text;
-- +goose StatementEnd
