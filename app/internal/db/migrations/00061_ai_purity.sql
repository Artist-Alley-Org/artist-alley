-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00061_ai_purity.sql
--
-- The SECOND derived AI fact: is this post PURELY AI? (#1242, ADR 0094
-- fourth amendment.)
--
-- ## Why a second fact, when 00060 just shipped one
--
-- Because `posts.ai_provenance` answers a DIFFERENT question and cannot
-- be made to answer this one. It is the labelling fact — "does this post
-- contain AI?" — and its positive arm propagates on ANY, so:
--
--     {generated, generated}   -> 'generated'
--     {generated, none}        -> 'generated'
--     {generated, undeclared}  -> 'generated'
--     {generated, assisted}    -> 'generated'
--
-- All four collapse to one value. A viewer-side "hide AI work" filter
-- keyed on that column would therefore exclude the three MIXED posts
-- along with the pure one — and the owner's ruling is precisely that it
-- must not:
--
--   > If someone filters out AI content it should still show a post that
--   > has mixed AI/non-AI content — only exclude posts with pure AI. AI
--   > could be used as part of an ideation phase and the final project
--   > might be pure human made.
--
-- That is not a preference. Excluding a post because ONE member carries
-- a declaration would punish the honest declaration, and the whole
-- design of ADR 0094 rests on making honest declaration cheap. So the
-- exclusion needs its own fact, and the two live side by side: one for
-- LABELLING, one for FILTERING.
--
-- ## The rule
--
--   pure = the post has at least one live contributor, and EVERY one of
--          them declares `generated`.
--
-- Three consequences, each of which is a case someone will get wrong:
--
--   * `assisted` NEVER contributes to purity. An all-`assisted` post is
--     human work made with AI help — exactly what the ruling protects.
--     This is why purity is not "the strongest value is generated" and
--     not "no member declares none".
--   * An UNDECLARED member makes the post NOT pure. We do not know what
--     it is, and not-knowing must never hide an artist's work: wrongly
--     hiding human work is a worse error than showing one more AI post
--     to someone who asked not to see them. Same direction as ADR 0094
--     §3 and the third amendment — the system never resolves an unknown
--     into a claim against the maker's interest.
--   * A post with NO live contributors is not pure. There is nothing to
--     be pure of.
--
-- The two rules look opposed (one propagates on ANY, this one requires
-- ALL) and are not. Both apply the same principle to different
-- questions: the ANY arm refuses to UNDERSTATE AI involvement in a
-- label, and the ALL arm refuses to OVERSTATE it in an exclusion.
--
-- ## Why the POPULATION is now a function of its own
--
-- 00060 spelled its `contributors` CTE inline. A second fact over the
-- same population would have meant a second copy of that CTE, and the
-- CTE is not boilerplate — it carries the #1147 correction, the arm that
-- 00052 shipped without and 00054 had to come back for: a post's
-- CONTRIBUTORS are its member assets UNION its two COVER PICTURES, which
-- are not members. `cover_asset_id` / `cover_thumbnail_asset_id` name
-- assets a post shows without holding, so a post can carry an
-- AI-generated cover over hand-made members, or hand-made members under
-- an undeclared cover.
--
-- Two copies of that union is the #1147 defect pre-installed: the next
-- person to complete the population (a future third cover pointer, a
-- membership expiry) fixes one and leaves the other, and the two derived
-- facts disagree about the same post. ADR 0063's rule — one multi-arm
-- rule, spelled once — is why `public.post_ai_contributors` exists, and
-- `post_ai_provenance` is rewritten to read from it so there is exactly
-- ONE definition of "who contributes to this post".
--
-- The `UNION ALL` is kept, and it is still safe under BOTH rules: an
-- asset that is both a member and the cover contributes twice, and the
-- unanimity arms compare two counts over the same multiset, so the
-- duplication cancels.
--
-- ## Why a COLUMN, and why it may be NOT NULL when ai_provenance is not
--
-- The column-and-trigger argument is 00060's and 00052's, unchanged: a
-- derived value recomputed per reader is a correlated subquery on a hot
-- path AND a second expression of the rule, free to disagree with the
-- first. It is maintained by the SAME triggers 00060 installs — the
-- recompute below writes both columns in one statement, so there is no
-- ordering in which one is fresh and the other stale.
--
-- ⚠️ But this column is NOT NULL DEFAULT false, where `ai_provenance` is
-- deliberately nullable, and the difference is not an inconsistency.
-- `assets.ai_provenance` is a statement about a PERSON — writing `none`
-- where nobody was asked fabricates that maker's disclaimer. `ai_pure`
-- is a statement about OUR KNOWLEDGE of a whole post: `false` means "we
-- cannot say this post is purely AI", which is TRUE of an undeclared
-- post, TRUE of an empty post and TRUE of a mixed one. There is no third
-- state to lose, and NOT NULL means the filter's SQL needs no NULL
-- handling — `NOT ai_pure` is total, which is what keeps the
-- fails-toward-showing direction from depending on three-valued logic.
--
-- ## ⚠️ This one DOES need a backfill, and 00060 did not
--
-- 00060 could skip it truthfully: the asset column was brand new, every
-- asset was NULL, so every post derived to NULL — already the column's
-- value. That is no longer true. Assets have carried declarations since
-- #1167 shipped, so a post that is ALREADY pure exists today and would
-- sit at the `false` default forever, invisible to the filter, until
-- something unrelated touched its membership. The backfill re-derives
-- every post and writes only the rows that change.
--
-- ## What this does NOT do
--
-- ⛔ It does not gate. ADR 0094 §4 is untouched: the work stays public,
-- findable and countable, nothing derived from it is withheld, and no
-- read path subtracts on this column. A viewer may exclude pure-AI posts
-- from THEIR OWN result set by asking for it; that is a filter, and it
-- is the only thing this column is for. The moment something withholds
-- on it, every derived copy inherits the obligation (search text,
-- facets, suggest, thumbhash, embeddings, counts, covers — the #1066
-- list) and the cheapness of this column is gone.
--
-- It is also not on any API projection. `posts.ai_provenance` is what
-- surfaces render, because LABELLING is the post-level fact a reader is
-- owed (fourth amendment §3). Purity is a query-grammar internal, and
-- shipping it on the wire would invite a client to re-implement the
-- exclusion locally — the second query language ADR 0093 forbids.

-- +goose Up

-- +goose StatementBegin
ALTER TABLE public.posts
    ADD COLUMN IF NOT EXISTS ai_pure boolean NOT NULL DEFAULT false;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON COLUMN public.posts.ai_pure IS
    'DERIVED — TRUE when this post has at least one live CONTRIBUTOR and EVERY one of them declares `generated` (#1242, ADR 0094 fourth amendment). Contributors are the member assets UNION the two cover pictures, exactly as `ai_provenance` counts them. ⚠️ THIS IS THE FILTERING FACT AND `ai_provenance` IS THE LABELLING FACT; they are not interchangeable. `ai_provenance` propagates a positive claim on ANY member, so `{generated, none}`, `{generated, undeclared}` and `{generated, assisted}` all read `generated` — a "hide AI work" filter keyed on it would exclude exactly the MIXED posts the owner''s ruling protects, because excluding a post for one member''s declaration punishes the honest declaration the design depends on. `assisted` NEVER contributes to purity: an all-`assisted` post is human work made with AI help. An UNDECLARED contributor makes the post NOT pure, because not-knowing must never hide an artist''s work. A post with no live contributors is not pure. NOT NULL is correct here where `assets.ai_provenance` is nullable: `false` is a statement about OUR KNOWLEDGE ("we cannot say this post is purely AI"), not a disclaimer written on a maker''s behalf. ⛔ A FILTER, NEVER A GATE (ADR 0094 §4): nothing withholds on this column, nothing is subtracted from counts, facets or suggest, and it carries no derived-copies obligation for exactly that reason.';
-- +goose StatementEnd

-- The POPULATION, spelled once for both facts. Extracted from 00060's
-- inline CTE so a second rule cannot come to disagree with the first
-- about who contributes to a post — see the header.
--
-- The output column is named `declaration` rather than `ai_provenance`
-- so a caller's `WHERE declaration = …` cannot be ambiguous with the
-- assets column of the same name.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_ai_contributors(p_post_id uuid)
RETURNS TABLE (declaration text)
LANGUAGE sql
STABLE
AS $$
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
       AND a.deleted_at IS NULL;
$$;
-- +goose StatementEnd

-- 00060's rule, unchanged, over the shared population. The CASE is
-- byte-for-byte the one that shipped; only the FROM moved.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_ai_provenance(p_post_id uuid)
RETURNS text
LANGUAGE sql
STABLE
AS $$
    SELECT CASE
        -- ANY, strongest first.
        WHEN count(*) FILTER (WHERE declaration = 'generated') > 0 THEN 'generated'
        WHEN count(*) FILTER (WHERE declaration = 'assisted')  > 0 THEN 'assisted'
        -- ALL, and only over a non-empty set.
        WHEN count(*) > 0
             AND count(*) FILTER (WHERE declaration = 'none') = count(*) THEN 'none'
        -- Undeclared: no contributors, or at least one nobody asked.
        ELSE NULL
    END
      FROM public.post_ai_contributors(p_post_id);
$$;
-- +goose StatementEnd

-- The purity rule. Unanimity over `generated`, over a non-empty set.
--
-- Never NULL: count(*) is never NULL, so `count(*) > 0 AND …` is FALSE
-- for the empty set rather than unknown — which is what lets the column
-- be NOT NULL and the filter's SQL be two-valued.
--
-- Note what is NOT written here. "No contributor declares none" would
-- admit an all-undeclared post; "the strongest value is generated" is
-- `ai_provenance = 'generated'`, which is the ANY rule and admits every
-- mixed post. Both are the shapes this fact exists to refuse.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.post_ai_pure(p_post_id uuid)
RETURNS boolean
LANGUAGE sql
STABLE
AS $$
    SELECT count(*) > 0
       AND count(*) FILTER (WHERE declaration = 'generated') = count(*)
      FROM public.post_ai_contributors(p_post_id);
$$;
-- +goose StatementEnd

-- Both facts, one write. 00060's guard is kept — write only on a real
-- change, because writing on no change invalidates caches and bumps rows
-- nobody asked to bump — and it now covers both columns, so a post whose
-- label is unchanged but whose purity flipped still gets written.
--
-- Each function is evaluated ONCE into a local rather than twice in the
-- SET and the WHERE, which is what keeps a two-column guard from
-- doubling 00060's four evaluations into eight.
--
-- The name does not change. It is the maintenance entry point three
-- triggers already call, and renaming it would mean re-pointing all
-- three for no gain; what it maintains is "the post's derived AI facts",
-- which is what it always did.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.recompute_post_ai_provenance(p_post_id uuid)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    v_provenance text;
    v_pure       boolean;
BEGIN
    IF p_post_id IS NULL THEN
        RETURN;
    END IF;
    SELECT public.post_ai_provenance(p_post_id), public.post_ai_pure(p_post_id)
      INTO v_provenance, v_pure;
    UPDATE public.posts p
       SET ai_provenance = v_provenance,
           ai_pure       = v_pure
     WHERE p.id = p_post_id
       AND (p.ai_provenance IS DISTINCT FROM v_provenance
            OR p.ai_pure IS DISTINCT FROM v_pure);
END;
$$;
-- +goose StatementEnd

-- Partial, on the RARE value. `ai:pure` ("show me only the pure-AI
-- work") is the selective query and this serves it; `NOT ai_pure` is
-- nearly the whole table and no index helps it, which is fine because
-- the filter that matters runs beside a text predicate or a visibility
-- gate that has already narrowed the population.
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS posts_ai_pure_idx
    ON public.posts (ai_pure)
    WHERE ai_pure;
-- +goose StatementEnd

-- The backfill 00060 could truthfully skip. Re-derives every post and
-- writes only the rows whose value actually changes, so a re-run (goose
-- redo, a fresh CI database that already has the column) is a no-op.
--
-- Safe against the cover trigger: `posts_cover_ai_provenance_sync_trg`
-- returns immediately unless one of the two cover columns changed, and
-- this statement touches neither.
-- +goose StatementBegin
UPDATE public.posts p
   SET ai_pure = public.post_ai_pure(p.id)
 WHERE p.ai_pure IS DISTINCT FROM public.post_ai_pure(p.id);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS public.posts_ai_pure_idx;
-- +goose StatementEnd

-- Back to 00060's two-evaluation, single-column form.
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

-- Back to 00060's inline `contributors` CTE, so dropping
-- post_ai_contributors below cannot leave a dangling dependency.
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
        WHEN count(*) FILTER (WHERE ai_provenance = 'generated') > 0 THEN 'generated'
        WHEN count(*) FILTER (WHERE ai_provenance = 'assisted')  > 0 THEN 'assisted'
        WHEN count(*) > 0
             AND count(*) FILTER (WHERE ai_provenance = 'none') = count(*) THEN 'none'
        ELSE NULL
    END
      FROM contributors;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.post_ai_pure(uuid);
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS public.post_ai_contributors(uuid);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.posts DROP COLUMN IF EXISTS ai_pure;
-- +goose StatementEnd
