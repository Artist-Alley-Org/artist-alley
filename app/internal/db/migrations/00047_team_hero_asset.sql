-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00047_team_hero_asset.sql
--
-- A team gets a picture (#982). Until now a team's only visual identity
-- anywhere in the product was two initials in a grey circle.
--
-- # A POINTER at an asset, not a bespoke upload
--
-- ADR 0088: a representative image is a nullable pointer at an ordinary
-- asset. Same argument as the collection cover in 00046 — pointing at an
-- asset inherits storage, the `col` rendition, permissions, federation
-- and GC for free, and "upload a dedicated banner" still works: upload it
-- as an ordinary asset, then pick it.
--
-- # ADR 0088 is NARROWED here, and the narrowing is the point
--
-- 0088 gates a representative image PER VIEWER, with a derived fallback
-- for a viewer who may not picture the chosen asset. That clause does not
-- transfer to a team, because a team hero renders in the followed-teams
-- RAIL — a navigation strip. A strip that shows some teams' pictures and
-- not others depending on who is looking is noise, not security: the
-- reader cannot tell a team with no hero from a team whose hero is
-- withheld from them, and the strip stops being a stable thing to
-- navigate by.
--
-- So a team hero is not viewer-dependent at all. It is admissible only if
-- it is already visible to EVERYONE:
--
--     the asset's sensitivity is 'public'  AND  its team_id is this team
--
-- Both halves are needed. `public` alone would let a team pin any public
-- asset in the install onto itself, which is an attribution claim rather
-- than a permission problem. `team_id` alone would let a team hero paint
-- a 'team'- or 'restricted'-sensitivity asset into a strip that anonymous
-- readers see, which is the leak.
--
-- # The rule is checked TWICE, and the second time is the one that matters
--
-- Validated at SELECTION (the write endpoint refuses an asset that fails
-- it) and re-checked at RENDER (the read path re-derives it every time).
-- An asset that is public today can be set to 'restricted' tomorrow, and
-- the write-time check cannot know that. Without the render-time re-check
-- the hero would linger, and a strip on an anonymous browse page would go
-- on painting a picture that is no longer public. The re-check is why the
-- hero falls back to initials the moment the asset stops qualifying.
--
-- The read-side re-check additionally requires a stored object and a
-- `col` rendition; the write side deliberately does NOT. Renditions are
-- produced asynchronously, so refusing a just-uploaded asset would hand
-- the admin an error they cannot act on. The read path's fallback to
-- initials carries that slack — same asymmetry, and the same reason, as
-- collections.CallerMayPictureAsset vs ComposeCovers.
--
-- # ON DELETE SET NULL is the behaviour, not the cheap option
--
-- If the asset is hard-deleted the team REVERTS to its initials tile.
-- RESTRICT would make one team's branding choice block an unrelated
-- asset's deletion; CASCADE would delete the team, which is absurd. SET
-- NULL says "the chosen picture is gone, fall back to the derived one",
-- which is the same fallback every other unrenderable-hero path takes.
--
-- Soft-delete needs no help here: the render-time re-check requires
-- `assets.deleted_at IS NULL`, so a soft-deleted hero falls back through
-- the same door while the pointer stays put and returns if the asset is
-- restored.
--
-- # One pointer, not two
--
-- #982 also floats an `avatar_asset_id` "for small contexts". Not built:
-- there is one consumer and it wants one image. A second column would be
-- speculative schema, and the `col` rendition already scales to a 28px
-- chip and a card alike.
--
-- # Federation: the pointer does NOT travel
--
-- A local `assets.id` names something that exists only on the sender, so
-- an exported pointer would either dangle or resolve to an unrelated
-- local asset. Same exclusion criterion 00046 recorded for the collection
-- cover (ADR 0083, applied by analogy). Nothing is exported by this
-- migration.
--
-- Plain DDL, so no StatementBegin/End markers — those exist to stop
-- goose splitting a body that contains its own semicolons (plpgsql), and
-- wrapping plain statements in them would instead fuse three statements
-- into one exec.

-- +goose Up

ALTER TABLE public.teams
    ADD COLUMN hero_asset_id UUID NULL
        REFERENCES public.assets(id) ON DELETE SET NULL;

COMMENT ON COLUMN public.teams.hero_asset_id IS
    'The team''s chosen hero picture (#982): a pointer at an ordinary asset, NULL means fall back to the derived initials tile. Admissible only if the asset is sensitivity=''public'' AND its team_id is this team — validated at SELECTION by the write endpoint and RE-CHECKED AT RENDER, because an asset that qualifies today can be set to ''restricted'' tomorrow and must then drop out of the rail rather than linger. This narrows ADR 0088: a team hero is NOT gated per viewer, because a navigation strip that shows some teams'' pictures and not others depending on who is looking is noise rather than security. ON DELETE SET NULL so a hard-deleted asset reverts the team to its initials rather than dangling. Does NOT federate: a local asset id names something that exists only on this server (ADR 0083''s exclusion criterion, applied by analogy).';

-- The read path asks "does THIS team have a hero, and does it still
-- qualify" for a page of teams at a time; it joins from teams to assets
-- by this column and never searches for the teams pointing AT a given
-- asset. So the index that earns its keep is a partial one over the
-- non-NULL pointers — a small minority of rows — which keeps that join
-- off a sequential scan. Same shape as collections_cover_asset_id_idx.
CREATE INDEX teams_hero_asset_id_idx
    ON public.teams (hero_asset_id)
    WHERE hero_asset_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS public.teams_hero_asset_id_idx;
ALTER TABLE public.teams DROP COLUMN hero_asset_id;
