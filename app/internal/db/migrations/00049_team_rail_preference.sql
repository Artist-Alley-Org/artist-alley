-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00049_team_rail_preference.sql
--
-- Give #1113's rail curation somewhere to live: a per-user bag holding
-- WHICH TEAM CHIPS THE BROWSE RAIL DRAWS AND IN WHAT ORDER.
--
-- ## Why a fifth jsonb column and not two keys in feed_filters
--
-- `user_preferences` is one jsonb bag PER CONCERN — notification_channels
-- (which channels for which event), default_views (how a set is arranged),
-- email_cadence (when email fires), feed_filters (which rows reach the
-- client). The rule 00036 wrote down is that a NEW CONCERN gets a new bag
-- and a new key inside an existing concern gets none.
--
-- This is a new concern, and the line is sharper here than it looks.
-- `feed_filters` is SERVER-APPLIED and SUBTRACTS CONTENT: `GET /posts`
-- reads it and the rows never reach the client. The team rail's curation
-- is the opposite on both counts — it is CLIENT-APPLIED and it curates a
-- piece of NAVIGATION FURNITURE. Hiding a team from your rail must not
-- hide that team's posts from your feed (#1113 states this explicitly),
-- so putting these keys in the bag the posts predicate consults is
-- exactly the mistake that would make it happen the first time someone
-- wires "read the user's feed_filters" one level too broadly.
--
-- `default_views` was the other candidate and is wrong for a smaller
-- reason: it holds SCALAR selections from closed enums that seed a
-- device's local choice. These are unbounded lists of team ids that are
-- authoritative rather than a seed.
--
-- ## Why NOT NULL DEFAULT '{}'
--
-- Same as its four siblings. An absent key inside the blob means "the
-- build's default for this knob", so an existing row and a brand-new one
-- both read as THE DEFAULT RAIL — every visible team, followed-first,
-- then name order.
--
-- ## The key names carry the default (the 00036 rule, restated for lists)
--
-- 00036's rule is "name each key so that the zero value is what this
-- build does by default". For a list the zero value is EMPTY, so:
--
--   hidden_team_ids  — empty = nothing hidden. (`shown_team_ids` would
--                      have inverted it: empty would then have to mean
--                      "show everything", which is not what an empty
--                      allow-list says.)
--   team_order       — empty = the server's order. A partial list is
--                      legal and means "these first, in this order, then
--                      everything else as before"; the client never has
--                      to write an entry per team to move one.
--
-- Neither list is validated against `teams` on write, deliberately. A
-- team can be deleted or leave the caller's visibility between the save
-- and the next read, and a preference write that 400s because a stale id
-- is in the reader's own list would strand them. Unknown ids are inert:
-- the rail intersects both lists with the teams the server actually
-- returned, so a dead id is dropped at render and costs nothing.

-- +goose Up

ALTER TABLE public.user_preferences
    ADD COLUMN team_rail jsonb DEFAULT '{}'::jsonb NOT NULL;

-- +goose Down

ALTER TABLE public.user_preferences
    DROP COLUMN team_rail;
