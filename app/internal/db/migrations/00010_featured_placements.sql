-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00010_featured_placements.sql
--
-- Featuring is a PLACEMENT, not a property of the thing featured
-- (ADR 0065, #417, closes #382).
--
-- Two mechanisms were live: `collections.featured` (a boolean on the
-- row, baseline 00001) and `featured_items` (a polymorphic, ordered,
-- attributed placement table, 00002). Two sources of truth for "what
-- is featured" is the same defect class as a visibility rule expressed
-- in two places — the one that cost #210, #212, #432 and #449 in a
-- single week. This collapses them onto featured_items and removes the
-- boolean.
--
-- Three changes, in dependency order:
--
--   1. featured_items gains `scope` (public | org | team) and
--      `team_id`, so a placement names its AUDIENCE.
--   2. The uniqueness constraint is replaced (see below) — without
--      this the public rail is not buildable as intended.
--   3. collections.featured migrates into featured_items at
--      scope='org' and the column is dropped.
--
-- ---------------------------------------------------------------
-- WHY team_id IS ITS OWN COLUMN AND NOT DERIVED FROM THE SUBJECT
-- ---------------------------------------------------------------
--
-- Two independent reasons, either sufficient:
--
--   * It is not derivable. The subject is polymorphic, and
--     `collections` HAS NO team_id column at all (only owner_user_ref).
--     Assets have one; collections do not. So "key off the subject's
--     team" cannot be expressed for half the subject kinds.
--   * It would be wrong even if it were derivable. A placement's
--     audience is not the subject's owner: team A may feature team B's
--     public asset on team A's dashboard. Deriving the audience from
--     ownership makes that unrepresentable and quietly conflates "who
--     made it" with "who is being shown it".
--
-- ---------------------------------------------------------------
-- WHY `NULLS NOT DISTINCT` — THE LOAD-BEARING DETAIL
-- ---------------------------------------------------------------
--
-- The old constraint, UNIQUE (subject_kind, subject_id), means a
-- subject can be featured exactly ONCE, GLOBALLY: featuring something
-- publicly AND internally is currently impossible. It becomes
-- UNIQUE (subject_kind, subject_id, scope, team_id).
--
-- But team_id is NULL for public and org placements, and PostgreSQL's
-- DEFAULT unique semantics treat NULLs as DISTINCT — so a plain
-- UNIQUE(...) would let the SAME subject be inserted at scope='public'
-- without limit, enforcing nothing on exactly the two scopes the rail
-- uses. Verified on this server (PG 16) rather than assumed: two rows
-- of ('x', NULL) both insert under a plain UNIQUE, and the second is
-- rejected under NULLS NOT DISTINCT.
--
-- NULLS NOT DISTINCT requires PostgreSQL 15+. The compose stack and CI
-- both run 16.
--
-- ---------------------------------------------------------------
-- WHAT THIS MIGRATION DOES NOT DO
-- ---------------------------------------------------------------
--
-- It adds no FK from featured_items to assets/collections, matching
-- 00002's deliberate choice: a subject can be hard-deleted out from
-- under a placement and the read tolerates the dangling reference. It
-- also grants no new access — scope decides which AUDIENCE a placement
-- is for, never whether the subject is visible. Visibility remains the
-- ADR 0063 predicate's job, applied by the reader. A placement whose
-- subject fails the predicate renders nothing.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.featured_items
    ADD COLUMN scope text NOT NULL DEFAULT 'org',
    ADD COLUMN team_id uuid;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_scope_check
        CHECK (scope = ANY (ARRAY['public'::text, 'org'::text, 'team'::text]));
-- +goose StatementEnd

-- team_id is required for team scope and forbidden otherwise. Without
-- this, a 'team' placement with a NULL team is an audience of nobody
-- that still occupies the uniqueness slot for every other team.
-- +goose StatementBegin
ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_team_scope_check
        CHECK ((scope = 'team' AND team_id IS NOT NULL)
            OR (scope <> 'team' AND team_id IS NULL));
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_team_id_fkey
        FOREIGN KEY (team_id) REFERENCES public.teams (id) ON DELETE CASCADE;
-- +goose StatementEnd

-- Constraint swap. CASCADE on the drop is unnecessary here (nothing
-- depends on it) and deliberately not used, so an unexpected
-- dependency fails loudly rather than being silently removed.
-- +goose StatementBegin
ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_subject_unique;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_placement_unique
        UNIQUE NULLS NOT DISTINCT (subject_kind, subject_id, scope, team_id);
-- +goose StatementEnd

-- The rail reads (scope, position) — index it the way it is queried.
-- The existing featured_items_order_idx stays for the unscoped admin
-- list.
-- +goose StatementBegin
CREATE INDEX featured_items_scope_order_idx
    ON public.featured_items USING btree (scope, position, created_at);
-- +goose StatementEnd

-- Migrate the boolean into placements at scope='org', its de-facto
-- current meaning: `collections.featured` was an internal browse
-- filter, never a public surface, so promoting it to 'public' would
-- publish content nobody consented to publish.
--
-- ON CONFLICT DO NOTHING because a collection may already have an
-- unscoped placement from the admin surface, which the DEFAULT 'org'
-- above has just made an org placement.
-- +goose StatementBegin
INSERT INTO public.featured_items (subject_kind, subject_id, position, scope, created_by_user_ref)
SELECT 'collection', c.id,
       COALESCE((SELECT MAX(position) FROM public.featured_items), -1)
           + ROW_NUMBER() OVER (ORDER BY c.created_at, c.id),
       'org',
       NULL
FROM public.collections c
WHERE c.featured = TRUE
  AND c.deleted_at IS NULL
ON CONFLICT DO NOTHING;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.collections DROP COLUMN featured;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE public.collections
    ADD COLUMN featured boolean DEFAULT false NOT NULL;
-- +goose StatementEnd

-- Restore the boolean from org-scoped collection placements. This is
-- lossy in the direction that cannot be helped: a collection featured
-- ONLY publicly comes back as featured=true, because the boolean has
-- no way to say "public". Down is for backing out an unshipped
-- migration, not for round-tripping curation state.
-- +goose StatementBegin
UPDATE public.collections c
   SET featured = TRUE
  FROM public.featured_items f
 WHERE f.subject_kind = 'collection'
   AND f.subject_id = c.id
   AND f.scope IN ('org', 'public');
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS public.featured_items_scope_order_idx;
-- +goose StatementEnd

-- Collapse back to one placement per subject before restoring the old
-- constraint, which cannot represent multiple audiences. Keeps the
-- lowest-position row per subject and drops the rest.
-- +goose StatementBegin
DELETE FROM public.featured_items f
 WHERE f.id NOT IN (
     SELECT DISTINCT ON (subject_kind, subject_id) id
       FROM public.featured_items
      ORDER BY subject_kind, subject_id, position ASC, created_at ASC
 );
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_placement_unique;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_subject_unique UNIQUE (subject_kind, subject_id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_team_id_fkey,
    DROP CONSTRAINT featured_items_team_scope_check,
    DROP CONSTRAINT featured_items_scope_check;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.featured_items
    DROP COLUMN team_id,
    DROP COLUMN scope;
-- +goose StatementEnd
