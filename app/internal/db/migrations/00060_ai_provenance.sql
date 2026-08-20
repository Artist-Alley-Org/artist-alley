-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00060_ai_provenance.sql
--
-- The maker's AI declaration and its maintained post-level derivation
-- (#1167). ADR 0094 carries the design; this is the schema half and the
-- parts of it that are decisions.
--
-- ## Why this is not `boolean NOT NULL DEFAULT false`
--
-- That was the obvious shape — the issue asks for "one checkbox" — and
-- ADR 0094 §2 rejects it on two grounds, both of which only bite after
-- the column has data in it:
--
--   * `assisted` and `generated` are the distinction people actually
--     argue about. "I upscaled a texture" and "a model made this" are
--     different claims about authorship, and IPTC found the split
--     necessary enough to separate `compositeWithTrainedAlgorithmicMedia`
--     from `trainedAlgorithmicMedia`. A boolean collapses them
--     irreversibly: widening later cannot recover which one a `true`
--     meant.
--
--   * NULL is load-bearing and a boolean cannot hold it. Every asset
--     that exists today predates the question. `NOT NULL DEFAULT false`
--     would make every one of those rows assert "the maker declares no
--     generative AI was involved" ON THE MAKER'S BEHALF — a fabricated
--     disclosure, on the one topic where a false disclaimer is the worst
--     available error. Undeclared must stay distinguishable from
--     declared-none forever, so the column is NULLABLE and there is NO
--     backfill.
--
-- Contrast 00052's `mature`, which IS `NOT NULL DEFAULT false` and is
-- right to be: `false` there means "nobody has applied a mature rating",
-- which is a true statement about a library that predates the feature.
-- Here `false`/`none` would mean "the maker has disclaimed AI", which is
-- a statement about a PERSON, and it would not be true.
--
-- ## Why text + CHECK rather than a Postgres ENUM or a lookup table
--
-- Three values, closed, and the next thing anyone wants (which model,
-- which prompt — IPTC 2025.1 defines properties for both) is a SEPARATE
-- column rather than a fourth value. A CHECK is additive to widen
-- (DROP CONSTRAINT + re-add) where a Postgres ENUM's value order is
-- fixed at creation and its removal path is worse; a lookup table buys
-- join cost and referential ceremony for a set that is decided by an
-- ADR, not by an operator. `workflow_states` is a table because
-- operators define states; this is not that.
--
-- ## Why the post value is a COLUMN and a TRIGGER, not a subquery
--
-- Identical to 00052's reasoning and deliberately the same shape, so
-- there is one pattern here and not two: a derived value recomputed per
-- reader is a correlated subquery on the browse feed's hot path AND a
-- second expression of the rule, free to disagree with the first. The
-- maintenance point is the DATABASE because `post_assets` is written
-- from post create, from post update, from POST /posts/{id}/assets and
-- from the seeder, and a Go hook would have to be attached at each one.
--
-- ## ⚠️ The COVERS arm, and why it is here in the first migration
--
-- 00052 derived `posts.mature` from `post_assets` alone. 00054 had to
-- come back and complete it (#1147): `cover_asset_id` and
-- `cover_thumbnail_asset_id` are NOT members — the schema says so in as
-- many words — so a post could carry a labelled cover over unlabelled
-- members and compute `false`, and four surfaces then painted that cover
-- to a viewer who had opted out. It was the FIRST picture each of them
-- painted, because a cover is what a card shows.
--
-- Following 00052's SHAPE while ignoring 00054's CORRECTION would ship
-- that hole a second time, in a new column, knowing it was there. So the
-- derivation below reads members OR either cover picture from the start,
-- and the rule is spelled ONCE in a LANGUAGE SQL helper — as 00054
-- moved `post_is_mature` to, for the reason ADR 0063 gives: two copies
-- of a multi-arm rule that must agree is the defect, and the arms only
-- multiply from here.
--
-- Nothing consumes `posts.ai_provenance` as a gate today and ADR 0094 §4
-- says nothing ever should. The covers arm is not about withholding: it
-- is about the LABEL being true of what the post actually shows a
-- reader. A post whose card is an AI-generated cover is a post that
-- shows AI-generated work, whoever later reads the column and for
-- whatever purpose.
--
-- ## The derivation rule, and where it REFINES ADR 0094
--
-- ADR 0094's consequences say "derivation takes the strongest member
-- value: a post containing one `generated` member is `generated`". That
-- settles every case the ADR illustrates and leaves one it does not: a
-- post whose members are, say, {`none`, undeclared}. Read as a total
-- order NULL < none < assisted < generated, "strongest" would be `none`
-- — and the post would then assert "the maker declares no generative
-- AI" over a member whose maker was never asked. That reintroduces, at
-- the post level, the exact fabricated disclaimer the nullable column
-- exists to prevent.
--
-- So the rule implemented here is asymmetric, and the asymmetry is the
-- same one 00052 draws between ANY and ALL:
--
--   * a POSITIVE claim propagates on ANY — one `generated` contributor
--     makes the post `generated`; failing that, one `assisted`
--     contributor makes it `assisted`. Understating AI involvement is
--     the harm to avoid, and a bundle containing an AI-generated piece
--     is a bundle a reader filtering AI out does not want.
--
--   * the NEGATIVE claim requires ALL — the post reads `none` only when
--     it has at least one live contributor and EVERY one of them
--     declares `none`. One undeclared contributor and the post is
--     undeclared, because that is the honest answer.
--
--   * a post with no live contributors is NULL. There is nothing to
--     derive.
--
-- Soft-deleted assets are excluded on every arm, as in 00052/00054: a
-- deleted asset is not a member and cannot paint a cover either.
-- Restoring one re-fires the asset trigger and the value comes back.
--
-- ## Why the recompute is a full re-derivation and not an increment
--
-- As 00052: a delta is only correct if every path that can change the
-- answer fires exactly once, and the paths include a soft-delete of an
-- ASSET, which changes no `post_assets` row at all. Re-deriving is a
-- bounded query over one post's contributors and is correct under every
-- ordering, including the ones nobody has written yet.
--
-- ## Recursion
--
-- `recompute_post_ai_provenance` UPDATEs `posts`, and this migration
-- puts an AFTER UPDATE trigger on `posts`. That is not a loop, by
-- 00054's argument: the trigger returns immediately unless one of the
-- two cover columns actually changed, and the recompute's write touches
-- only `ai_provenance`, so the second firing stops at the guard.
--
-- ## No backfill, and unlike 00054 that is not an oversight
--
-- 00054 needed a backfill because operators had already labelled assets
-- before the rule was completed. Here the column is brand new: every
-- asset is NULL, so every post derives to NULL, which is already the
-- column's value. A backfill would be a no-op UPDATE over every post.
--
-- ## No index on the common value
--
-- ADR 0094 §4: this is a FILTER and never a GATE. Nothing in the read
-- path subtracts on it — the work stays public, findable and countable
-- — so there is no hot query to index for yet. The partial indexes
-- below cover the one shape a future viewer-side filter would use
-- ("exclude the declared-AI ones") and operator reporting; they are
-- small because the declared-AI rows are the rare ones. If a gate is
-- ever wanted, ADR 0094 says that is moderation and belongs in the
-- sensitivity/state machinery, not here.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE public.assets
    ADD COLUMN IF NOT EXISTS ai_provenance text;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.assets
    DROP CONSTRAINT IF EXISTS assets_ai_provenance_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.assets
    ADD CONSTRAINT assets_ai_provenance_check
    CHECK (ai_provenance IS NULL OR ai_provenance IN ('none', 'assisted', 'generated'));
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN public.assets.ai_provenance IS
    'The MAKER''S DECLARATION about generative-AI involvement in this work (#1167, ADR 0094). Three declared values — `none` (the maker declares no generative AI was involved), `assisted` (AI used in part: upscaling, inpainting, an AI-generated texture on hand-made geometry), `generated` (substantially AI-generated) — plus NULL, which means UNDECLARED: nobody was asked. ⚠️ NULL IS NOT `none`. The column is nullable and unbackfilled precisely so the rows predating the feature do not assert a disclaimer their makers never made; a reader that renders NULL as "no AI" is lying on the artist''s behalf. ⚠️ A DECLARATION IS NOT A PERMISSION (ADR 0094 §4): this is orthogonal to `sensitivity` and to `mature`; it is a FILTER a viewer may apply to their own feed and never a GATE that withholds the work from others, and nothing derived from the asset — search text, facets, suggest, thumbhash, embeddings, counts, covers — is withheld on account of it. That property is what keeps this column cheap and it holds only while nothing gates on it. Extraction may one day CORROBORATE `generated`/`assisted` from `Iptc4xmpExt:DigitalSourceType` on an UNDECLARED work, and may NEVER establish `none`: the IPTC vocabulary has no term meaning "no AI", so absence of an AI term is not evidence of absence (ADR 0094 §3). Does not federate yet — the v1 envelope rejects unknown top-level fields; the wire mapping is pre-decided in ADR 0094 §6.';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.posts
    ADD COLUMN IF NOT EXISTS ai_provenance text;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.posts
    DROP CONSTRAINT IF EXISTS posts_ai_provenance_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.posts
    ADD CONSTRAINT posts_ai_provenance_check
    CHECK (ai_provenance IS NULL OR ai_provenance IN ('none', 'assisted', 'generated'));
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN public.posts.ai_provenance IS
    'DERIVED from the post''s live CONTRIBUTORS — its member assets AND its two cover pictures (#1167, ADR 0094). Never written by a request body; maintained by `public.post_ai_provenance` via triggers on `post_assets`, `assets` and `posts`, exactly as `posts.mature` is. The rule is asymmetric: a POSITIVE claim propagates on ANY (one `generated` contributor makes the post `generated`, else one `assisted` contributor makes it `assisted`), and the NEGATIVE claim requires ALL (the post reads `none` only when it has at least one live contributor and every one of them declares `none`). One undeclared contributor makes the post undeclared, because deriving `none` over a contributor nobody asked would fabricate that maker''s disclaimer at the post level. A post with no live contributors is NULL. The covers arm is present from the first migration deliberately: `posts.mature` shipped without it and #1147 was the bill.';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS assets_ai_provenance_declared_idx
    ON public.assets (ai_provenance)
    WHERE ai_provenance IN ('assisted', 'generated');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS posts_ai_provenance_declared_idx
    ON public.posts (ai_provenance)
    WHERE ai_provenance IN ('assisted', 'generated');
-- +goose StatementEnd

-- The derivation, spelled once. `UNION ALL` rather than `UNION`: an
-- asset that is both a member and the cover contributes twice and no
-- arm cares — the positive arms test "> 0" and the unanimity arm
-- compares two counts over the same multiset, so duplicates cancel.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_ai_provenance(p_post_id uuid)
RETURNS text
LANGUAGE sql
STABLE
AS $$
    WITH contributors AS (
        SELECT a.ai_provenance
          FROM public.post_assets pa
          JOIN public.assets a ON a.id = pa.asset_id
         WHERE pa.post_id = p_post_id
           AND a.deleted_at IS NULL
        UNION ALL
        SELECT a.ai_provenance
          FROM public.posts p
          JOIN public.assets a
            ON a.id IN (p.cover_asset_id, p.cover_thumbnail_asset_id)
         WHERE p.id = p_post_id
           AND a.deleted_at IS NULL
    )
    SELECT CASE
        -- ANY, strongest first.
        WHEN count(*) FILTER (WHERE ai_provenance = 'generated') > 0 THEN 'generated'
        WHEN count(*) FILTER (WHERE ai_provenance = 'assisted')  > 0 THEN 'assisted'
        -- ALL, and only over a non-empty set.
        WHEN count(*) > 0
             AND count(*) FILTER (WHERE ai_provenance = 'none') = count(*) THEN 'none'
        -- Undeclared: no contributors, or at least one nobody asked.
        ELSE NULL
    END
      FROM contributors;
$$;
-- +goose StatementEnd

-- Write only on a real change (00052's rule: writing on no change
-- invalidates caches and bumps rows nobody asked to bump).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.recompute_post_ai_provenance(p_post_id uuid)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_post_id IS NULL THEN
        RETURN;
    END IF;
    UPDATE public.posts p
       SET ai_provenance = public.post_ai_provenance(p_post_id)
     WHERE p.id = p_post_id
       AND p.ai_provenance IS DISTINCT FROM public.post_ai_provenance(p_post_id);
END;
$$;
-- +goose StatementEnd

-- Membership changed: re-derive both sides of the edit. OLD and NEW can
-- name DIFFERENT posts on an UPDATE that moves an asset between them.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_assets_ai_provenance_sync()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        PERFORM public.recompute_post_ai_provenance(OLD.post_id);
    END IF;
    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        PERFORM public.recompute_post_ai_provenance(NEW.post_id);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS post_assets_ai_provenance_sync_trg ON public.post_assets;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER post_assets_ai_provenance_sync_trg
AFTER INSERT OR UPDATE OR DELETE ON public.post_assets
FOR EACH ROW EXECUTE FUNCTION public.post_assets_ai_provenance_sync();
-- +goose StatementEnd

-- The asset's own declaration changed, or it was soft-deleted /
-- restored: re-derive every post that HOLDS it and every post that
-- POINTS AT it as a cover — a different set, which is 00054's finding.
-- Guarded on the two columns that can change the answer so an ordinary
-- metadata edit does no work.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_ai_provenance_sync()
RETURNS trigger
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

-- +goose StatementBegin
DROP TRIGGER IF EXISTS assets_ai_provenance_sync_trg ON public.assets;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER assets_ai_provenance_sync_trg
AFTER UPDATE ON public.assets
FOR EACH ROW EXECUTE FUNCTION public.assets_ai_provenance_sync();
-- +goose StatementEnd

-- A post's own cover pointer changed (or the post was created with one).
-- Nothing else on the row can change the answer, so everything else
-- returns before doing any work — which is also what makes the
-- recompute's own UPDATE of `ai_provenance` a dead end rather than a
-- loop.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.posts_cover_ai_provenance_sync()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.cover_asset_id IS NOT DISTINCT FROM OLD.cover_asset_id
       AND NEW.cover_thumbnail_asset_id IS NOT DISTINCT FROM OLD.cover_thumbnail_asset_id THEN
        RETURN NULL;
    END IF;
    PERFORM public.recompute_post_ai_provenance(NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TRIGGER IF EXISTS posts_cover_ai_provenance_sync_trg ON public.posts;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER posts_cover_ai_provenance_sync_trg
AFTER INSERT OR UPDATE ON public.posts
FOR EACH ROW EXECUTE FUNCTION public.posts_cover_ai_provenance_sync();
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP TRIGGER IF EXISTS posts_cover_ai_provenance_sync_trg ON public.posts;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS assets_ai_provenance_sync_trg ON public.assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS post_assets_ai_provenance_sync_trg ON public.post_assets;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.posts_cover_ai_provenance_sync();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.assets_ai_provenance_sync();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.post_assets_ai_provenance_sync();
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.recompute_post_ai_provenance(uuid);
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.post_ai_provenance(uuid);
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS public.posts_ai_provenance_declared_idx;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS public.assets_ai_provenance_declared_idx;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.posts DROP CONSTRAINT IF EXISTS posts_ai_provenance_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.assets DROP CONSTRAINT IF EXISTS assets_ai_provenance_check;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.posts DROP COLUMN IF EXISTS ai_provenance;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE public.assets DROP COLUMN IF EXISTS ai_provenance;
-- +goose StatementEnd
