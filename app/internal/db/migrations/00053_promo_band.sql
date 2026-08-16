-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00053_promo_band.sql
--
-- The operator promo band (#1118): a full-width strip between feed
-- pages carrying a title, a blurb, a call-to-action and an ordered row
-- of curated item cards.
--
-- ---------------------------------------------------------------
-- WHY THE CARDS REUSE featured_items RATHER THAN A NEW MEMBERSHIP
-- TABLE
-- ---------------------------------------------------------------
--
-- ADR 0065 is the whole answer: "featuring is a PLACEMENT, not a
-- property". `featured_items` is not "the rail's table" — it is the
-- ordered, polymorphic, attributed placement table, and migration
-- 00010 exists precisely because there were TWO sources of truth for
-- "what is featured" (`collections.featured` and this table) and the
-- pair had to be collapsed onto one. A `promo_band_items` table would
-- reinstate exactly that: a second ordered curation list, a second
-- polymorphic subject resolution, and — the part that actually costs —
-- a SECOND COPY OF THE VISIBILITY COMPOSITION. The rail's read splices
-- the ADR 0063 predicate five times (asset subject, collection subject,
-- collection cover, and both halves of the member count) plus the
-- per-asset sensitivity gate and the ladder check. Duplicating that for
-- the band is how the two drift, and drift in that particular query is
-- a leak, not a cosmetic bug.
--
-- So a band card IS a placement. The only thing missing from the table
-- was WHICH SURFACE a placement belongs to, and that is one nullable
-- column.
--
--   band_id IS NULL      the featured rail (every row that exists today)
--   band_id = <a band>   a card in that band
--
-- The reader is one query builder with the alias of the audience-
-- carrying row and a band filter as its only inputs; everything else —
-- every predicate splice, every sensitivity gate — is shared by
-- construction rather than by review.
--
-- ---------------------------------------------------------------
-- WHY THE BAND DEFINITION IS A TABLE AND NOT system_config
-- ---------------------------------------------------------------
--
-- v1 renders ONE band, so an operator-config blob was the cheaper
-- shape. It was rejected on one fact: a band's cards need to name
-- their band, and a config blob has no id to name. The alternative is
-- a boolean column on featured_items ("this row is in the promo
-- band"), which works for exactly one band and has to be ripped out
-- for the second — and ADR 0030's slot inventory is plural in its
-- first sentence.
--
-- The table therefore ADMITS several bands while the v1 read takes one
-- (the enabled band with the lowest `after_page`, then oldest). That is
-- stated in the reader and in the API description rather than enforced
-- with a partial unique index, because "there can be only one" is a
-- product decision for this release and not a property of the data.
--
-- ---------------------------------------------------------------
-- THE AUDIENCE LIVES ON THE BAND, NOT ON ITS CARDS
-- ---------------------------------------------------------------
--
-- `featured_items.scope` (00010) names a placement's AUDIENCE, and
-- featured.ScopeVisibleSQL turns a caller into the scopes they qualify
-- for — `public` for anonymous, `public` + `org` for a signed-in
-- reader. A band needs exactly that, once, for the whole strip: a band
-- is a single authored unit with a headline and a button, and "half
-- these cards are for logged-out visitors and half are not" is not a
-- thing an operator can hold in their head.
--
-- So `promo_bands.scope` carries the audience and the reader applies
-- ScopeVisibleSQL to the BAND row. A band card's own `scope` column
-- keeps its table default and is DELIBERATELY NOT CONSULTED — see the
-- comment on the column below. The alternative considered was copying
-- the band's scope onto every card row so the existing item-level gate
-- would agree; that is a denormalised copy of a visibility input, which
-- is the one kind of copy this codebase has paid for repeatedly (#902,
-- #1066). One row carries the audience and one predicate reads it.
--
-- A placement still grants nothing. Every card is resolved through the
-- same predicate-in-the-JOIN-condition that the rail uses, so a card
-- pointing at content this caller may not see produces no row at all —
-- and a band whose cards all resolve to nothing renders NOTHING (ADR
-- 0030's collapse rule, which governs here because a full-width band
-- between walls is a banner in the page's flow, not one of ADR 0079's
-- in-grid sized slots; §2's substitution rule is scoped to those).
--
-- ---------------------------------------------------------------
-- WHY THE UNIQUE CONSTRAINT IS WIDENED, AND WHY IT STAYS
-- `NULLS NOT DISTINCT`
-- ---------------------------------------------------------------
--
-- featured_items_placement_unique is
-- (subject_kind, subject_id, scope, team_id) NULLS NOT DISTINCT. Left
-- alone, a subject already on the rail could not also appear in a band
-- — the placement would be rejected as a duplicate, which is wrong:
-- they are two placements on two surfaces, which is the entire point
-- of the `band_id` column.
--
-- `band_id` therefore joins the key. NULLS NOT DISTINCT is preserved
-- rather than re-derived: with plain NULL semantics two rail rows for
-- the same subject would both insert (band_id NULL on each), silently
-- removing the constraint from the surface that has always had it.
-- Every existing row has band_id NULL, so the widened key is
-- byte-for-byte the same constraint on today's data.
--
-- ---------------------------------------------------------------
-- cta_url IS CONSTRAINED IN THE DATABASE, NOT ONLY IN THE HANDLER
-- ---------------------------------------------------------------
--
-- The CTA is an operator-supplied string that becomes an `href`. A
-- `javascript:` URL there is stored XSS on the browse page of every
-- reader, and Svelte does not sanitise hrefs. The handler validates it
-- and returns a clean 400; this CHECK is the backstop, in the same
-- relationship the subject_kind CHECK has with featured/http.go's
-- validation (00048): the constraint stops the row existing, the
-- handler stops the request being confusing.
--
-- The admissible shapes are an absolute http(s) URL and a site-relative
-- path. Anything else — including a scheme-relative `//host` that a
-- reader cannot tell from a local link — is refused.

-- +goose Up

CREATE TABLE public.promo_bands (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    title text DEFAULT ''::text NOT NULL,
    blurb text DEFAULT ''::text NOT NULL,
    cta_label text DEFAULT ''::text NOT NULL,
    cta_url text DEFAULT ''::text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    after_page integer DEFAULT 1 NOT NULL,
    scope text DEFAULT 'org'::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_ref bigint,
    CONSTRAINT promo_bands_pkey PRIMARY KEY (id),
    -- The feed position, counted in whole loaded pages. 1 = after the
    -- first page, which is the default the issue names. Zero would mean
    -- "above the feed", which is a different surface (the featured rail
    -- already occupies it) and is refused rather than silently rendered
    -- somewhere else.
    CONSTRAINT promo_bands_after_page_check CHECK (after_page >= 1),
    -- 'team' is admissible for featured_items because a team placement
    -- has a team_id to bind it to. A band has no such column and no
    -- surface that would render it, so the audience here is the two
    -- scopes ScopeVisibleSQL actually returns.
    CONSTRAINT promo_bands_scope_check CHECK (scope = ANY (ARRAY['public'::text, 'org'::text])),
    -- Both or neither. A label with no URL renders a button that goes
    -- nowhere; a URL with no label renders nothing the reader can press.
    CONSTRAINT promo_bands_cta_pair_check
        CHECK ((cta_label = ''::text AND cta_url = ''::text)
            OR (cta_label <> ''::text AND cta_url <> ''::text)),
    -- See the header. `~` rather than a LIKE chain so the anchors are
    -- explicit: a leading '//' is NOT a site-relative path.
    CONSTRAINT promo_bands_cta_url_check
        CHECK (cta_url = ''::text OR cta_url ~ '^(https?://[^/]|/[^/])'::text)
);

COMMENT ON TABLE public.promo_bands IS
    'Operator-authored full-width promo strips rendered BETWEEN feed pages (#1118). The cards are ordinary featured_items rows carrying this band''s id — see featured_items.band_id — so curation, ordering and visibility composition have one home (ADR 0065). `scope` is the whole band''s audience, read by featured.ScopeVisibleSQL; the cards'' own scope column is not consulted. An empty or disabled band renders nothing at all (ADR 0030''s collapse rule, which governs a full-width band; ADR 0079 §2''s substitution rule is scoped to in-grid sized slots).';

COMMENT ON COLUMN public.promo_bands.after_page IS
    'Where the band falls in the feed, counted in whole loaded pages: 1 renders it after the first page. The PAGE SIZE is the client''s (the browse feed requests 36), so this is a position in the reader''s scroll rather than a row count — deliberately, because it is what the operator can predict without knowing the API''s limit.';

ALTER TABLE public.featured_items
    ADD COLUMN band_id uuid;

COMMENT ON COLUMN public.featured_items.band_id IS
    'Which surface this placement belongs to (#1118): NULL is the featured rail — every row that existed before this column — and a band id makes the row a card in that promo band. There is no second membership table on purpose (ADR 0065; see migration 00053''s header). ⚠️ For a band row the `scope` column is NOT the audience: the BAND carries the audience, and this row''s scope keeps its table default. Reading scope on a band row would be a second, stale copy of a visibility input.';

ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_band_id_fkey
        FOREIGN KEY (band_id) REFERENCES public.promo_bands (id) ON DELETE CASCADE;

-- Constraint swap. See the header for why `band_id` joins the key and
-- why NULLS NOT DISTINCT is load-bearing rather than inherited.
ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_placement_unique;

ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_placement_unique
        UNIQUE NULLS NOT DISTINCT (subject_kind, subject_id, scope, team_id, band_id);

-- The band read is (band_id, position) exactly as the rail read is
-- (scope, position); index it the way it is queried. The existing
-- featured_items_order_idx and featured_items_scope_order_idx stay for
-- the admin list and the rail.
CREATE INDEX featured_items_band_order_idx
    ON public.featured_items USING btree (band_id, "position", created_at);

-- +goose Down

DROP INDEX IF EXISTS public.featured_items_band_order_idx;

-- Cards belonging to a band are DELETED here rather than orphaned onto
-- the rail. Leaving them would publish an operator's band content on a
-- surface they never chose — the rail is the anonymous landing page —
-- which is a worse outcome than losing curation rows on the rollback of
-- an unshipped migration. The DROP TABLE below would do this anyway via
-- the FK cascade; it is spelled out so the intent is not an accident of
-- constraint order.
DELETE FROM public.featured_items WHERE band_id IS NOT NULL;

ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_placement_unique;

ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_placement_unique
        UNIQUE NULLS NOT DISTINCT (subject_kind, subject_id, scope, team_id);

ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_band_id_fkey;

COMMENT ON COLUMN public.featured_items.band_id IS NULL;

ALTER TABLE public.featured_items
    DROP COLUMN band_id;

DROP TABLE public.promo_bands;
