-- Phase 1.16.B-2 — field-weighted tsvector retrofit + collection.smart_query.
--
-- Two things:
--
--   1. Rewrite the three rebuild_* functions from 00001 (assets, posts)
--      + 00021 (collections) so search_text is a WEIGHTED tsvector
--      (setweight per source column). Every source column that fed the
--      unweighted version continues to feed the weighted one — the retrofit
--      is input-preserving; only the WEIGHT class per input changes.
--
--        Assets:      A=title, B=description, C=(tag list; empty for now,
--                     asset-level tags aren't stored on a joined table today
--                     — reserved for a later tag phase), D=custom-field
--                     values (via the same asset_field_value + field_
--                     definition join the 00001 body used verbatim).
--        Collections: A=name, B=description, C=(tag list; empty in B-2
--                     since collections don't carry tags today).
--        Posts:       A=title, B=description (body), C=tag list from
--                     post_tags, D=aggregated asset.search_text via
--                     post_assets (the 00001 body's asset-recursion input).
--
--      Weights matter for ts_rank_cd: with setweight, a match in the
--      title (A) scores strictly higher than the same match in the
--      description (B), which scores higher than tags (C), which score
--      higher than custom-field values (D). Facet + suggestion + ranking
--      all benefit — see the B-2 brief's decision 1.
--
--   2. Add collections.smart_query TEXT column. The save-as-collection
--      endpoint stores the DSL string here so a future sub-phase (B-4)
--      can re-run the query as a saved-search. B-2 writes it but doesn't
--      re-execute; the column is nullable + defaults NULL so every
--      existing row is unaffected.
--
-- pg_trgm is already installed (00001_baseline_v1.sql:43) — no
-- extension work. Trigram indexes on the four hot text columns
-- (asset.title, collection.name, post.title, tag.tag) speed the
-- autocomplete suggestion queries B-2 ships; nightly bloat is
-- bounded because these columns are short (<= a few hundred bytes).
--
-- Backfill is inline + batched (1000 rows/batch) with a
-- fast-exit for large corpora: rows_est > 10 000 → seed the trigger
-- update + emit RAISE NOTICE, leave existing search_text values
-- in place. The stored values are already valid tsvectors
-- (unweighted) — searches keep working; the WEIGHT vector merely
-- doesn't distinguish A/B/C/D matches until an admin reindex
-- (Phase 1.16.B-5) walks the tail. This matches the 00021
-- migration's fast-exit pattern.

-- +goose Up

-- +goose StatementBegin

-- 1. Rewrite assets rebuild to WEIGHTED.
CREATE OR REPLACE FUNCTION public.rebuild_asset_search_text(p_asset_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $fn$
DECLARE
    field_text TEXT;
BEGIN
    SELECT COALESCE(
        STRING_AGG(
            CASE
                WHEN v.value_text     IS NOT NULL THEN v.value_text
                WHEN v.value_options  IS NOT NULL THEN array_to_string(v.value_options, ' ')
                ELSE NULL
            END,
            ' '
        ),
        ''
    )
    INTO field_text
    FROM asset_field_value v
    JOIN field_definition f ON f.id = v.field_id
    WHERE v.asset_id = p_asset_id
      AND f.searchable = TRUE
      AND f.status = 'active';

    UPDATE assets
       SET search_text =
              setweight(to_tsvector('english', COALESCE(title, '')),       'A') ||
              setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
              -- C is reserved for asset-level tags. Empty in B-2; a
              -- later phase that lands per-asset tags will feed a
              -- string_agg here without changing the weight scheme.
              setweight(to_tsvector('english', ''),                        'C') ||
              setweight(to_tsvector('english', COALESCE(field_text, '')),  'D')
     WHERE id = p_asset_id;
END;
$fn$;

-- 2. Rewrite posts rebuild to WEIGHTED.
CREATE OR REPLACE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $fn$
DECLARE
    asset_search  TEXT;
    post_tag_text TEXT;
BEGIN
    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '')
      INTO asset_search
      FROM post_assets pa
      JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id AND a.deleted_at IS NULL;

    SELECT COALESCE(string_agg(tag, ' '), '')
      INTO post_tag_text
      FROM post_tags
     WHERE post_id = p_post_id;

    UPDATE posts
       SET search_text =
              setweight(to_tsvector('english', COALESCE(title, '')),          'A') ||
              setweight(to_tsvector('english', COALESCE(description, '')),    'B') ||
              setweight(to_tsvector('english', COALESCE(post_tag_text, '')),  'C') ||
              setweight(to_tsvector('english', COALESCE(asset_search, '')),   'D')
     WHERE id = p_post_id;
END;
$fn$;

-- 3. Rewrite collections rebuild to WEIGHTED.
CREATE OR REPLACE FUNCTION public.rebuild_collection_search_text(p_collection_id UUID) RETURNS void
    LANGUAGE plpgsql
    AS $fn$
BEGIN
    UPDATE collections
       SET search_text =
              setweight(to_tsvector('english', COALESCE(name, '')),        'A') ||
              setweight(to_tsvector('english', COALESCE(description, '')), 'B')
              -- C reserved for collection-level tags (not yet stored).
     WHERE id = p_collection_id;
END;
$fn$;

-- 4. Backfill existing rows so the weighted vectors go live. Each
--    entity's rebuild function has been updated; calling it forces a
--    fresh compute. Batched to keep locks short on any single
--    row-set; fast-exit above 10k rows.
DO $$
DECLARE
    batch_size CONSTANT INT := 1000;
    row_est BIGINT;
    updated_count INT;
BEGIN
    SELECT COUNT(*) INTO row_est FROM assets WHERE deleted_at IS NULL;
    IF row_est > 10000 THEN
        RAISE NOTICE 'weighted_tsvector: assets corpus is % rows — inline backfill deferred to admin reindex (Phase 1.16.B-5). Existing vectors remain queryable.', row_est;
    ELSE
        LOOP
            WITH targets AS (
                SELECT id FROM assets WHERE deleted_at IS NULL LIMIT batch_size
            )
            UPDATE assets a
               SET search_text = (
                  SELECT setweight(to_tsvector('english', COALESCE(a.title, '')),       'A') ||
                         setweight(to_tsvector('english', COALESCE(a.description, '')), 'B') ||
                         setweight(to_tsvector('english', ''),                          'C') ||
                         setweight(to_tsvector('english', COALESCE(
                             (SELECT STRING_AGG(
                                 CASE
                                     WHEN v.value_text    IS NOT NULL THEN v.value_text
                                     WHEN v.value_options IS NOT NULL THEN array_to_string(v.value_options, ' ')
                                     ELSE NULL
                                 END, ' ')
                              FROM asset_field_value v
                              JOIN field_definition f ON f.id = v.field_id
                              WHERE v.asset_id = a.id
                                AND f.searchable = TRUE
                                AND f.status = 'active'), '')),         'D')
                 )
             FROM targets
             WHERE a.id = targets.id
               AND (a.search_text IS NULL
                    OR NOT strpos(a.search_text::text, ':A') > 0);
            GET DIAGNOSTICS updated_count = ROW_COUNT;
            RAISE NOTICE 'weighted_tsvector: assets backfill batch: % rows', updated_count;
            EXIT WHEN updated_count = 0;
        END LOOP;
    END IF;

    SELECT COUNT(*) INTO row_est FROM collections;
    IF row_est > 10000 THEN
        RAISE NOTICE 'weighted_tsvector: collections corpus is % rows — inline backfill deferred.', row_est;
    ELSE
        LOOP
            WITH targets AS (
                SELECT id FROM collections LIMIT batch_size
            )
            UPDATE collections c
               SET search_text =
                    setweight(to_tsvector('english', COALESCE(c.name, '')),        'A') ||
                    setweight(to_tsvector('english', COALESCE(c.description, '')), 'B')
             FROM targets
             WHERE c.id = targets.id
               AND (c.search_text IS NULL
                    OR NOT strpos(c.search_text::text, ':A') > 0);
            GET DIAGNOSTICS updated_count = ROW_COUNT;
            RAISE NOTICE 'weighted_tsvector: collections backfill batch: % rows', updated_count;
            EXIT WHEN updated_count = 0;
        END LOOP;
    END IF;

    SELECT COUNT(*) INTO row_est FROM posts WHERE deleted_at IS NULL;
    IF row_est > 10000 THEN
        RAISE NOTICE 'weighted_tsvector: posts corpus is % rows — inline backfill deferred.', row_est;
    ELSE
        LOOP
            -- Posts backfill delegates to the rebuild function because
            -- its inputs (asset recursion + tag aggregation) are more
            -- complex than the assets/collections inline expressions.
            UPDATE posts p
               SET search_text = search_text  -- no-op write
             WHERE id IN (SELECT id FROM posts WHERE deleted_at IS NULL LIMIT batch_size);
            -- Force actual rebuild via the function on the same batch.
            PERFORM rebuild_post_search_text(p2.id) FROM (
                SELECT id FROM posts
                 WHERE deleted_at IS NULL
                   AND (search_text IS NULL
                        OR NOT strpos(search_text::text, ':A') > 0)
                 LIMIT batch_size
            ) p2;
            GET DIAGNOSTICS updated_count = ROW_COUNT;
            RAISE NOTICE 'weighted_tsvector: posts backfill batch: % rows', updated_count;
            EXIT WHEN updated_count = 0;
        END LOOP;
    END IF;
END $$;

-- 5. Add collection.smart_query for save-as-collection (B-2 write,
--    B-4 re-run).
ALTER TABLE collections
    ADD COLUMN smart_query TEXT;

COMMENT ON COLUMN collections.smart_query IS
    'DSL query string that was executed to populate this collection. Written by /search/save-as-collection (Phase 1.16.B-2); re-executed by the saved-search notifier (Phase 1.16.B-4). NULL for hand-curated collections.';

-- 6. Trigram indexes on the four hot text columns the suggestion
--    endpoint queries. gin_trgm_ops is the operator class pg_trgm
--    ships that lets similarity() and % operator use the index.
--
--    Post title index skipped for now since posts.title is NOT
--    NULL default '' and the suggestion path filters WHERE title
--    <> ''; the base search_text GIN suffices for larger-corpus
--    searches. Add if suggestions get slow.
CREATE INDEX IF NOT EXISTS assets_title_trgm ON assets USING gin (title gin_trgm_ops)
    WHERE deleted_at IS NULL AND title <> '';
CREATE INDEX IF NOT EXISTS collections_name_trgm ON collections USING gin (name gin_trgm_ops)
    WHERE name <> '';
CREATE INDEX IF NOT EXISTS posts_title_trgm ON posts USING gin (title gin_trgm_ops)
    WHERE deleted_at IS NULL AND title <> '';
-- post_tags.tag is short + high-cardinality — trigram helps prefix +
-- fuzzy matches for autocomplete of tags. No WHERE clause because
-- every row is a live tag application.
CREATE INDEX IF NOT EXISTS post_tags_tag_trgm ON post_tags USING gin (tag gin_trgm_ops);

-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin

DROP INDEX IF EXISTS post_tags_tag_trgm;
DROP INDEX IF EXISTS posts_title_trgm;
DROP INDEX IF EXISTS collections_name_trgm;
DROP INDEX IF EXISTS assets_title_trgm;

ALTER TABLE collections DROP COLUMN IF EXISTS smart_query;

-- The rewritten rebuild functions stay — reverting to unweighted
-- would need re-running rebuild on every row. Ops teams that
-- genuinely need to reverse this can DROP FUNCTION + reinstall the
-- old bodies from the 00001 / 00021 migrations.

-- +goose StatementEnd
