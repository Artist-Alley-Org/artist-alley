-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00054_post_mature_covers.sql
--
-- `posts.mature` learns about the post's own COVER PICTURES (#1147,
-- epic #1114, ADR 0090).
--
-- ## The hole
--
-- 00052 derived the post flag from `post_assets` alone, on the stated
-- rule "a post is mature iff ANY member asset is" and the stated reason
-- "a bundle containing one mature piece is a bundle a disqualified
-- viewer must not be handed".
--
-- `posts.cover_asset_id` and `posts.cover_thumbnail_asset_id` are NOT
-- members. The schema says so in as many words — the thumbnail column's
-- own comment reads "an optional standalone thumbnail (not a post
-- member) used by the upload modal's 'use a different image as the
-- cover' UX" — and `posts.attachablesOf` gates the two separately from
-- `members` for exactly that reason.
--
-- So a post could carry a MATURE cover over entirely non-mature members
-- and still compute `mature = false`. Every surface downstream trusts
-- that column: the browse feed, /search's post arm, the featured rail's
-- cover lateral, and the collection cover mosaic. All four then painted
-- the mature picture to a viewer who had opted out — and it was the
-- FIRST picture each of them paints, because a cover is what a card
-- shows.
--
-- ## Why the fix is here and not at the four surfaces
--
-- One place instead of six, which is 00052's own argument for putting
-- the derivation in the database at all: "a trigger cannot be forgotten
-- by a call site that has not been written yet". Gating the cover ids
-- per-surface would be four more transcriptions of one rule, and #1147
-- exists because a fifth construction had never heard of it.
--
-- This does not WIDEN the rule 00052 wrote down; it completes it. "Any
-- mature piece in the bundle" already covered the cover in spirit — the
-- cover is the piece a reader is handed first — and the derivation
-- simply did not read the two columns that hold it.
--
-- ## Recursion
--
-- `recompute_post_mature` UPDATEs `posts`, and this migration puts an
-- AFTER UPDATE trigger on `posts`. That is not a loop: the trigger
-- returns immediately unless one of the two cover columns actually
-- changed, and the recompute's write touches only `mature`. The second
-- firing therefore stops at the guard, which is the same shape
-- `assets_mature_sync` already uses on its own table.
--
-- ## The backfill is REAL here, unlike 00052's
--
-- 00052 needed none because nothing had been labelled yet. By now an
-- operator may have labelled assets and pointed post covers at them, so
-- the Up path re-derives every post whose stored flag disagrees with the
-- completed rule. It is bounded by the two partial cover indexes that
-- already exist (posts_cover_asset_id_idx, posts_cover_thumbnail_idx).

-- +goose Up

-- The completed derivation: members, OR either cover picture.
--
-- Soft-deleted assets are excluded on every arm, following 00052 and the
-- contents listings: a deleted asset is not a member and cannot paint a
-- cover either. The write guard is kept verbatim — writing only on a
-- real change is what keeps a membership edit from invalidating caches
-- and bumping rows nobody asked to bump.
--
-- The derivation is spelled ONCE, in a LANGUAGE SQL helper, rather than
-- pasted into the SET and the guard as 00052 did. Two copies of a
-- three-arm rule that must agree is the defect ADR 0063 exists to
-- prevent, and the arms only multiply from here.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_is_mature(p_post_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
          FROM public.post_assets pa
          JOIN public.assets a ON a.id = pa.asset_id
         WHERE pa.post_id = p_post_id
           AND a.deleted_at IS NULL
           AND a.mature
    ) OR EXISTS (
        SELECT 1
          FROM public.posts p
          JOIN public.assets a
            ON a.id IN (p.cover_asset_id, p.cover_thumbnail_asset_id)
         WHERE p.id = p_post_id
           AND a.deleted_at IS NULL
           AND a.mature
    );
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.recompute_post_mature(p_post_id uuid)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_post_id IS NULL THEN
        RETURN;
    END IF;
    UPDATE public.posts p
       SET mature = public.post_is_mature(p_post_id)
     WHERE p.id = p_post_id
       AND p.mature IS DISTINCT FROM public.post_is_mature(p_post_id);
END;
$$;
-- +goose StatementEnd

-- The asset's own flag changed, or it was soft-deleted / restored. 00052
-- re-derived every post that HOLDS it; it must also re-derive every post
-- that POINTS AT it as a cover, which is a different set — a cover need
-- not be a member, which is the whole point of this migration.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_mature_sync()
RETURNS trigger
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

-- A post's own cover pointer changed (or the post was created with one).
-- Nothing else on the row can change the answer, so everything else
-- returns before doing any work — which is also what makes the
-- recompute's own UPDATE of `mature` a dead end rather than a loop.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.posts_cover_mature_sync()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.cover_asset_id IS NOT DISTINCT FROM OLD.cover_asset_id
       AND NEW.cover_thumbnail_asset_id IS NOT DISTINCT FROM OLD.cover_thumbnail_asset_id THEN
        RETURN NULL;
    END IF;
    PERFORM public.recompute_post_mature(NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS posts_cover_mature_sync_trg ON public.posts;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER posts_cover_mature_sync_trg
AFTER INSERT OR UPDATE ON public.posts
FOR EACH ROW EXECUTE FUNCTION public.posts_cover_mature_sync();
-- +goose StatementEnd

-- Backfill. Every post whose stored flag disagrees with the completed
-- rule — in practice, the posts whose covers are mature and whose
-- members are not.
-- +goose StatementBegin
UPDATE public.posts p
   SET mature = public.post_is_mature(p.id)
 WHERE p.mature IS DISTINCT FROM public.post_is_mature(p.id);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP TRIGGER IF EXISTS posts_cover_mature_sync_trg ON public.posts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.posts_cover_mature_sync();
-- +goose StatementEnd

-- Back to 00052's members-only derivation, spelled out rather than
-- calling post_is_mature, because the helper is dropped below.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_mature_sync()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    r record;
BEGIN
    IF NEW.mature IS NOT DISTINCT FROM OLD.mature
       AND (NEW.deleted_at IS NULL) IS NOT DISTINCT FROM (OLD.deleted_at IS NULL) THEN
        RETURN NULL;
    END IF;
    FOR r IN SELECT DISTINCT post_id FROM public.post_assets WHERE asset_id = NEW.id LOOP
        PERFORM public.recompute_post_mature(r.post_id);
    END LOOP;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.recompute_post_mature(p_post_id uuid)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_post_id IS NULL THEN
        RETURN;
    END IF;
    UPDATE public.posts p
       SET mature = EXISTS (
               SELECT 1
                 FROM public.post_assets pa
                 JOIN public.assets a ON a.id = pa.asset_id
                WHERE pa.post_id = p_post_id
                  AND a.deleted_at IS NULL
                  AND a.mature
           )
     WHERE p.id = p_post_id
       AND p.mature IS DISTINCT FROM EXISTS (
               SELECT 1
                 FROM public.post_assets pa
                 JOIN public.assets a ON a.id = pa.asset_id
                WHERE pa.post_id = p_post_id
                  AND a.deleted_at IS NULL
                  AND a.mature
           );
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.post_is_mature(uuid);
-- +goose StatementEnd

-- Re-derive under the reverted rule, so a down-migration leaves no post
-- carrying a flag the restored derivation would not produce.
-- +goose StatementBegin
UPDATE public.posts p
   SET mature = EXISTS (
           SELECT 1
             FROM public.post_assets pa
             JOIN public.assets a ON a.id = pa.asset_id
            WHERE pa.post_id = p.id
              AND a.deleted_at IS NULL
              AND a.mature
       )
 WHERE p.mature IS DISTINCT FROM EXISTS (
           SELECT 1
             FROM public.post_assets pa
             JOIN public.assets a ON a.id = pa.asset_id
            WHERE pa.post_id = p.id
              AND a.deleted_at IS NULL
              AND a.mature
       );
-- +goose StatementEnd
