-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00052_mature_content.sql
--
-- The mature-content flag and its maintained post-level derivation
-- (#1115, epic #1114). ADR 0090 carries the design; this is the schema
-- half and the parts of it that are decisions.
--
-- ## Why a column beside `sensitivity` and not a fifth tier
--
-- ADR 0090 §1: a RATING is not a CLEARANCE. `sensitivity` answers "who
-- is allowed"; this answers "who has opted in". They are independent in
-- both directions — a PUBLIC artwork can be mature, and a restricted
-- one need not be — and `sensitivity` is an ORDERED LADDER that
-- `ContentReadable` reads as one. A product of two independent values
-- cannot be expressed as a point on a ladder, and the attempt would
-- silently make every mature piece non-public: a different access
-- decision, taken on the artist's behalf, by a labelling control.
--
-- ## Why the post value is a COLUMN and a TRIGGER, not a subquery
--
-- A post is mature iff ANY member asset is. That is derivable at read
-- time with an EXISTS, and deriving it there is the mistake #902 is
-- about: a derived copy recomputed per reader is (a) a correlated
-- subquery on the browse feed's hot path and (b) a SECOND expression of
-- the rule, free to disagree with the first.
--
-- So it is stored on `posts` and maintained by triggers. The
-- maintenance point is the DATABASE and not a Go write-path hook, and
-- the reason is enumerable rather than stylistic — `post_assets` is
-- written from post create, from post update, from
-- `POST /posts/{id}/assets` and from the seeder, and `assets.mature`
-- will be written from upload, from the asset edit form and from the
-- operator override. A Go hook would have to be attached at each of
-- those, and the failure mode of missing one is a post whose flag is
-- stale — i.e. a mature asset handed to a viewer who opted out. A
-- trigger cannot be forgotten by a call site that has not been written
-- yet.
--
-- ANY rather than ALL: a bundle containing one mature piece is a bundle
-- a disqualified viewer must not be handed.
--
-- ## Why the recompute is a full re-derivation and not an increment
--
-- `recompute_post_mature` re-reads the membership rather than toggling
-- the flag by delta. A delta is only correct if every path that can
-- change the answer fires exactly once, and the paths here include a
-- soft-delete of an ASSET (which changes no `post_assets` row at all).
-- Re-deriving is a bounded query over one post's members and it is
-- correct under every ordering, including the ones nobody has written
-- yet.
--
-- Soft-deleted members are EXCLUDED from the derivation, which follows
-- from the same rule the contents listings use: a deleted member is not
-- a member. Restoring it re-fires the asset trigger and the flag comes
-- back.
--
-- ## Both defaults are FALSE, and no backfill is needed
--
-- Nothing in an existing install has been labelled, so `false` is not a
-- guess — it is the only true statement about a library that predates
-- the feature. An operator who turns the instance toggle on gets a
-- library where nothing is marked yet, which is the honest starting
-- point and the one they can act on.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE public.assets
    ADD COLUMN IF NOT EXISTS mature boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.posts
    ADD COLUMN IF NOT EXISTS mature boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- The reader's own answer. A NEW BAG rather than a key inside
-- `feed_filters`, per 00049's rule ("a new concern gets a new bag; a new
-- key inside an existing concern gets none") — and mature IS a new
-- concern by that rule's own test. `feed_filters` documents itself as
-- what the browse FEED subtracts with, and `show_restricted` is
-- feed-only by design; this preference is consulted on the PICTURE plane
-- too (ADR 0090 §3 — the blur on a deep-linked item), so filing it there
-- would make the bag's own doc false.
--
-- JSONB rather than a boolean column, matching the package's stated
-- schema philosophy and leaving room for the granularity #1116 will
-- want. `{}` is the zero value and means NOT opted in, which is the
-- default the owner specified and the safe reading of an empty blob.
-- +goose StatementBegin
ALTER TABLE public.user_preferences
    ADD COLUMN IF NOT EXISTS mature_content jsonb NOT NULL DEFAULT '{}'::jsonb;
-- +goose StatementEnd

-- Partial indexes: the overwhelmingly common row is `false`, and the
-- queries that consult this column are all of the form "exclude the
-- true ones". A full index over a near-uniform boolean earns nothing;
-- a partial index on the rare value is small and is the one a
-- disqualified viewer's feed can use.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS assets_mature_idx
    ON public.assets (id) WHERE mature;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS posts_mature_idx
    ON public.posts (id) WHERE mature;
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
       -- Write only on a real change. Without this every membership
       -- edit touches the post row, which invalidates caches and
       -- bumps nothing anybody asked to bump.
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

-- Membership changed: re-derive the post on both sides of the edit.
-- OLD and NEW can name DIFFERENT posts on an UPDATE that moves an
-- asset between them, so both are recomputed rather than assuming the
-- post id is stable.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_assets_mature_sync()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM public.recompute_post_mature(OLD.post_id);
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        PERFORM public.recompute_post_mature(NEW.post_id);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS post_assets_mature_sync_trg ON public.post_assets;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER post_assets_mature_sync_trg
AFTER INSERT OR UPDATE OR DELETE ON public.post_assets
FOR EACH ROW EXECUTE FUNCTION public.post_assets_mature_sync();
-- +goose StatementEnd

-- The asset's own flag changed, or it was soft-deleted / restored:
-- every post that holds it has to be re-derived. Guarded on the two
-- columns that can change the answer so an ordinary metadata edit does
-- no work.
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
DROP TRIGGER IF EXISTS assets_mature_sync_trg ON public.assets;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER assets_mature_sync_trg
AFTER UPDATE ON public.assets
FOR EACH ROW EXECUTE FUNCTION public.assets_mature_sync();
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP TRIGGER IF EXISTS assets_mature_sync_trg ON public.assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS post_assets_mature_sync_trg ON public.post_assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.assets_mature_sync();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.post_assets_mature_sync();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.recompute_post_mature(uuid);
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS public.posts_mature_idx;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS public.assets_mature_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.user_preferences DROP COLUMN IF EXISTS mature_content;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.posts DROP COLUMN IF EXISTS mature;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.assets DROP COLUMN IF EXISTS mature;
-- +goose StatementEnd
