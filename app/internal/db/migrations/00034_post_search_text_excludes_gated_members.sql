-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00034_post_search_text_excludes_gated_members.sql
--
-- #883 — membership must never widen an item, and the post search
-- document was widening one.
--
-- rebuild_post_search_text folds every member asset's own search_text
-- (title weight A, description weight B, field text weight D) into the
-- POST's search_text at weight D, filtered on nothing but
-- `a.deleted_at IS NULL`. The post row then carries its members' words
-- and is matched by the POST predicate, which for a public post admits
-- everyone including anonymous.
--
-- So a public post containing a restricted asset titled "SECRET boss
-- concept" is returned to an anonymous caller who searches that phrase.
-- Nothing in the response names the asset, which is what makes it worse
-- rather than better: the result COUNT is the oracle. Query a phrase
-- only the restricted title contains and the total goes 0 -> 1. That
-- confirms the title, and a stranger can walk it token by token without
-- ever being shown a field they are not entitled to.
--
-- The fix is to fold in only members that are readable by EVERYONE, i.e.
-- the anonymous branch of the EntityAsset predicate (status='active' AND
-- processing_status='ready') conjoined with the public sensitivity tier
-- (ADR 0064). search_text is ONE stored column shared by every caller,
-- so it can only ever be baked at the most restrictive reading — a
-- per-caller document would mean a document per caller.
--
-- What that costs: an author searching for the title of their OWN
-- restricted member no longer finds the containing post by that word.
-- They still find the ASSET, whose own search_text is unchanged and
-- whose row predicate admits its owner. Losing one indirect route to a
-- post you own is the correct trade against handing its members'
-- titles to the internet, and it fails closed.
--
-- The backfill re-runs the function for every existing post: posts
-- indexed under the old rule keep leaking until their document is
-- rebuilt, and nothing else would rebuild them until the next edit.

-- +goose Up
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

-- A filter is only worth as much as its refresh. NOTHING rebuilt a
-- post's document when a MEMBER asset changed — the baseline triggers
-- fire on post_assets, post_tags and the post's own title/description,
-- never on the assets row — so an asset flipped public -> restricted
-- would leave its words sitting in every containing post's search_text
-- indefinitely, and the filter above would never be applied to it.
--
-- (That gap predates #883 and also made the document go stale on a plain
-- title edit: renaming an asset updated assets.search_text and left the
-- post matching the OLD name. Same trigger fixes both.)
--
-- UPDATE OF is deliberately narrow. search_text is in the list because
-- it is what carries the member's title and description; sensitivity /
-- status / processing_status / deleted_at because they are the gating
-- columns the new filter reads.
--
-- No recursion: this updates `posts`, and posts_search_text fires only
-- on UPDATE OF title, description.
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
DROP TRIGGER IF EXISTS assets_member_post_search_text ON public.assets;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER assets_member_post_search_text
    AFTER UPDATE OF search_text, sensitivity, status, processing_status, deleted_at
    ON public.assets
    FOR EACH ROW EXECUTE FUNCTION public.asset_member_post_search_text_trigger();
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT id FROM public.posts LOOP
        PERFORM public.rebuild_post_search_text(r.id);
    END LOOP;
END $$;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS assets_member_post_search_text ON public.assets;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.asset_member_post_search_text_trigger();
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.rebuild_post_search_text(p_post_id uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE asset_search TEXT; post_tag_text TEXT;
BEGIN
    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '') INTO asset_search
      FROM post_assets pa JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id AND a.deleted_at IS NULL;
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
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT id FROM public.posts LOOP
        PERFORM public.rebuild_post_search_text(r.id);
    END LOOP;
END $$;
-- +goose StatementEnd
