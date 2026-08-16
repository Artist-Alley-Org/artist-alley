-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00048_featured_team_subject.sql
--
-- A TEAM becomes a featurable subject (#1084).
--
-- # This is a database problem, not a code one
--
-- #982's scope sketch says "a featured-team slot fed by featured_items
-- (new subject kind `team`)" as though it were an if-statement. It is
-- not: `subject_kind` is constrained in the database, and the two-value
-- CHECK below is the actual blocker. The enumeration is restated in SIX
-- coordinated places, and missing any one fails ASYMMETRICALLY:
--
--   1. this CHECK                    miss it → a valid request 500s on a 23514
--   2. featured/http.go's validation miss it → the DATABASE refuses it,
--                                    as a 500, instead of the handler
--                                    refusing it as a clean 400
--   3. that check's error string     miss it → the refusal names the
--                                    wrong set of kinds
--   4. FeaturedItemInput enum        miss it → the generated client will
--                                    not send the value at all
--   5. FeaturedItem enum (the        miss it → the server serialises the
--      RESPONSE schema, a separate            kind while the client's
--      object)                                type narrows it away
--   6. ListFeaturedItems' title      miss it → the operator's own
--      resolution + the /admin/               curation list shows the new
--      content/featured page                  kind as an untitled
--                                             Collection with a dead link
--
-- Places 5 and 6 were not in this change's original scope; they were
-- found by featuring a team and then looking at the admin page. All six
-- move together here.
--
-- # Only the SUBJECT was missing — the audience model was already right
--
-- featured_items already carries `scope` (public/org/team) with
-- featured_items_team_scope_check binding team_id to scope='team', and
-- featured_items_placement_unique keyed on
-- (subject_kind, subject_id, scope, team_id) NULLS NOT DISTINCT. So a
-- team can be featured to one audience without inventing a second
-- mechanism, and the same team may be featured publicly AND internally.
-- Nothing about ADR 0065's placement model needed changing; it was
-- always polymorphic and the enumeration simply had not caught up.
--
-- # Featuring a team is a PLACEMENT, not a grant
--
-- Widening this CHECK grants nothing. A placement row says "render this
-- team first in the strip"; who may see the team, and whose hero picture
-- renders, are decided by the read path exactly as they were before —
-- the team must still be live, the caller must still hold teams.read,
-- and the hero still goes through the TeamHeroes render-time re-check
-- from 00047. ADR 0009's rule that membership never widens visibility is
-- the shape being preserved: curation is a SELECTION over what is
-- already visible.
--
-- # The Down REFUSES rather than deleting rows, and it does so by itself
--
-- Restoring the two-value constraint on a table that holds a `team` row
-- is not possible, and the interesting question is what should happen.
-- Deleting the operator's curation rows to make a rollback succeed is
-- destroying user data to satisfy bookkeeping, so it is not done here.
--
-- Instead the refusal is STRUCTURAL: `ALTER TABLE ... ADD CONSTRAINT
-- CHECK` validates existing rows, so if any `team` placement survives,
-- Postgres raises 23514 and goose — which runs each migration inside a
-- transaction — rolls the whole Down back. The DROP above it is undone
-- with it, so the database is left exactly as it was: widened, intact,
-- and consistent with its goose version. No plpgsql guard is needed to
-- produce that behaviour, and none is written; the constraint IS the
-- guard.
--
-- An operator who genuinely wants the rollback removes the placements
-- first, which is a deliberate act with a visible command:
--
--     SELECT id, subject_id, scope, team_id FROM featured_items
--      WHERE subject_kind = 'team';
--     DELETE FROM featured_items WHERE subject_kind = 'team';
--
-- Existing 'asset' and 'collection' rows are untouched in both
-- directions — the constraint only ever gained a third admissible value.
--
-- Plain DDL, so no StatementBegin/End markers — those exist to stop
-- goose splitting a body that contains its own semicolons (plpgsql), and
-- wrapping plain statements in them would instead fuse them into one
-- exec. Same note as 00046 and 00047.

-- +goose Up

ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_subject_kind_check;

ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_subject_kind_check
        CHECK (subject_kind = ANY (ARRAY['asset'::text, 'collection'::text, 'team'::text]));

COMMENT ON COLUMN public.featured_items.subject_kind IS
    'What kind of thing this placement points at: ''asset'', ''collection'' or ''team'' (#1084). There is deliberately no foreign key — the subject is polymorphic — so the read path resolves the subject by joining the matching table and DROPS the placement when that join finds nothing the caller may see. Adding a kind here is never sufficient on its own: the same enumeration is restated in SIX places (enumerated in featured/http.go''s AddFeaturedItem) — this CHECK, that handler''s validation, its error string, the OpenAPI FeaturedItemInput enum, the FeaturedItem RESPONSE enum, and the admin curation list''s title resolution plus the page that renders it. Miss any one and the failure is asymmetric: a 500 instead of a 400, a client that refuses to send the value, or an operator staring at an untitled row with a dead link.';

-- +goose Down

-- Refuses if any 'team' placement still exists — see the header. The
-- ADD below validates the surviving rows and raises 23514, which aborts
-- goose's transaction and undoes this DROP with it.
ALTER TABLE public.featured_items
    DROP CONSTRAINT featured_items_subject_kind_check;

ALTER TABLE public.featured_items
    ADD CONSTRAINT featured_items_subject_kind_check
        CHECK (subject_kind = ANY (ARRAY['asset'::text, 'collection'::text]));

COMMENT ON COLUMN public.featured_items.subject_kind IS NULL;
