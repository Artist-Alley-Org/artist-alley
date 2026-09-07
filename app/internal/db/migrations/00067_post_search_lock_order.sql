-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00067_post_search_lock_order.sql
--
-- #1173, #1119, ADR 0019. The post-search trigger chain was a
-- LOCK-ORDER INVERSION, and it deadlocked a batch metadata apply
-- against an ordinary single-target metadata write (SQLSTATE 40P01).
--
-- The chain nobody could see from the reported statement:
--
--   INSERT asset_field_value ... ON CONFLICT
--   +- asset_field_value_search_text  (AFTER INSERT OR UPDATE, per row)
--      +- rebuild_asset_search_text(asset_id)
--         +- UPDATE assets SET search_text ...     -> FOR NO KEY UPDATE
--            |                                       on assets[V], held
--            |                                       to COMMIT
--            +- assets_member_post_search_text     (AFTER UPDATE OF
--               |                                   search_text, ...)
--               +- asset_member_post_search_text_trigger()
--                  FOR r IN SELECT post_id FROM post_assets
--                            WHERE asset_id = NEW.id   -- NO ORDER BY
--                  +- rebuild_post_search_text(post_id)
--                     +- UPDATE posts SET search_text -> FOR NO KEY
--                                                        UPDATE on
--                                                        posts[P]
--
-- The ordinary writer therefore takes assets[V] and then posts[P]. The
-- batch, walking its targets in ascending asset id, ends up holding
-- posts[P] from an earlier target in the same post and then asking for
-- assets[V]. Two transactions, opposite order, one deadlock:
--
--   batch    holds posts[P]  -> waits assets[V]
--   ordinary holds assets[V] -> waits posts[P]
--
-- The waiting edge sits TWO trigger levels below the statement the
-- error names, which is why the wait graph read as unclosable.
--
-- This migration replaces four functions. It fixes what belongs to the
-- database; the batch's own side (a single ascending FOR UPDATE pass
-- over the whole target set, and a coalesced ascending post rebuild
-- after the writes) lives in app/internal/metadata.
--
-- 1. rebuild_post_search_text takes its post row FOR NO KEY UPDATE as
--    its FIRST action, before it reads membership, member documents or
--    tags.
--
--    FOR NO KEY UPDATE and not FOR UPDATE: it is exactly the mode the
--    function's own UPDATE posts needs, so there is no upgrade, and it
--    does not conflict with the KEY SHARE that membership foreign keys
--    take. posts_search_text fires ON posts and already holds the row
--    in that mode, so the entry lock is a no-op on that caller.
--
--    LOCK BEFORE READ, and that is the whole point. The function
--    aggregates members and tags in statements PRECEDING the UPDATE. At
--    READ COMMITTED each statement takes its own snapshot, and blocking
--    on the UPDATE's row lock re-evaluates only the target row, never
--    the local variables already computed. Without the entry lock a
--    transaction can compute a document from a stale snapshot, wait,
--    and then apply it on top of a document another transaction just
--    committed. Reachable on membership removal and on the addition of
--    a member that is not itself being edited.
--
-- 2. asset_member_post_search_text_trigger walks its containing posts
--    ORDER BY post_id, so two assets sharing two posts can no longer
--    acquire those posts in opposite orders, and honours a
--    transaction-local suppression flag so a batch can coalesce the
--    propagation it would otherwise do once per written row.
--
--    The flag is `aa.suppress_asset_post_search`, set with SET LOCAL,
--    so it can only ever be true inside the transaction that set it and
--    cannot leak through a pooled connection. A transaction that sets
--    it OWES the rebuild: the batch performs one rebuild per distinct
--    containing post, ascending, after all of its field-value writes.
--
-- 3. assets_mature_sync and 4. assets_ai_provenance_sync sort their own
--    post loops for the same reason. Neither fires during a batch
--    metadata apply, because both early-return unless `mature`,
--    `ai_provenance` or the nullness of `deleted_at` actually changed
--    and a field-value write changes none of them. But both CAN fire
--    from other paths, and an unordered multi-post loop there would
--    invert against the batch's ascending post phase.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE asset_search TEXT; post_tag_text TEXT;
BEGIN
    -- THE ENTRY LOCK. First statement, before any aggregate: the
    -- document below is computed from three reads, and a row lock taken
    -- after them would order the writes while still letting the value
    -- be built from a world that had already moved.
    PERFORM 1 FROM public.posts WHERE id = p_post_id FOR NO KEY UPDATE;

    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '') INTO asset_search
      FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id
       AND a.deleted_at IS NULL
       -- #883: only members every caller could see standalone
       -- contribute their words to the shared post document.
       AND a.sensitivity = 'public'
       AND a.status = 'active'
       AND a.processing_status = 'ready';
    SELECT COALESCE(string_agg(tag, ' '), '') INTO post_tag_text FROM post_tags WHERE post_id = p_post_id;
    UPDATE posts SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(post_tag_text, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(asset_search, '')), 'D')
     WHERE id = p_post_id;
END; $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_member_post_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE r RECORD;
BEGIN
    -- SUPPRESSION, transaction-local. A caller that sets this flag has
    -- taken responsibility for rebuilding every affected post itself,
    -- once each and in ascending post id order. `true` as the second
    -- argument makes an unset flag return NULL rather than raise, which
    -- is every caller that never opts in.
    IF COALESCE(current_setting('aa.suppress_asset_post_search', true), '') = 'on' THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT post_id FROM public.post_assets WHERE asset_id = NEW.id
         ORDER BY post_id
    LOOP
        PERFORM public.rebuild_post_search_text(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_mature_sync() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    IF NEW.mature IS NOT DISTINCT FROM OLD.mature
       AND (NEW.deleted_at IS NULL) IS NOT DISTINCT FROM (OLD.deleted_at IS NULL) THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT post_id FROM public.post_assets WHERE asset_id = NEW.id
        UNION
        SELECT id AS post_id FROM public.posts
         WHERE cover_asset_id = NEW.id
            OR cover_thumbnail_asset_id = NEW.id
        ORDER BY post_id
    LOOP
        PERFORM public.recompute_post_mature(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_ai_provenance_sync() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    IF NEW.ai_provenance IS NOT DISTINCT FROM OLD.ai_provenance
       AND (NEW.deleted_at IS NULL) IS NOT DISTINCT FROM (OLD.deleted_at IS NULL) THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT post_id FROM public.post_assets WHERE asset_id = NEW.id
        UNION
        SELECT id AS post_id FROM public.posts
         WHERE cover_asset_id = NEW.id
            OR cover_thumbnail_asset_id = NEW.id
        ORDER BY post_id
    LOOP
        PERFORM public.recompute_post_ai_provenance(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE asset_search TEXT; post_tag_text TEXT;
BEGIN
    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '') INTO asset_search
      FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id
       AND a.deleted_at IS NULL
       AND a.sensitivity = 'public'
       AND a.status = 'active'
       AND a.processing_status = 'ready';
    SELECT COALESCE(string_agg(tag, ' '), '') INTO post_tag_text FROM post_tags WHERE post_id = p_post_id;
    UPDATE posts SET search_text =
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(description, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(post_tag_text, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(asset_search, '')), 'D')
     WHERE id = p_post_id;
END; $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_member_post_search_text_trigger() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT post_id FROM public.post_assets WHERE asset_id = NEW.id LOOP
        PERFORM public.rebuild_post_search_text(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_mature_sync() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    IF NEW.mature IS NOT DISTINCT FROM OLD.mature
       AND (NEW.deleted_at IS NULL) IS NOT DISTINCT FROM (OLD.deleted_at IS NULL) THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT post_id FROM public.post_assets WHERE asset_id = NEW.id
        UNION
        SELECT id AS post_id FROM public.posts
         WHERE cover_asset_id = NEW.id
            OR cover_thumbnail_asset_id = NEW.id
    LOOP
        PERFORM public.recompute_post_mature(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_ai_provenance_sync() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    IF NEW.ai_provenance IS NOT DISTINCT FROM OLD.ai_provenance
       AND (NEW.deleted_at IS NULL) IS NOT DISTINCT FROM (OLD.deleted_at IS NULL) THEN
        RETURN NULL;
    END IF;
    FOR r IN
        SELECT post_id FROM public.post_assets WHERE asset_id = NEW.id
        UNION
        SELECT id AS post_id FROM public.posts
         WHERE cover_asset_id = NEW.id
            OR cover_thumbnail_asset_id = NEW.id
    LOOP
        PERFORM public.recompute_post_ai_provenance(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
