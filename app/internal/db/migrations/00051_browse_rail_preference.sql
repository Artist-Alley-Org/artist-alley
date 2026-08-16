-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00051_browse_rail_preference.sql
--
-- Rename `user_preferences.team_rail` to `browse_rail` and let it carry
-- the tag chips' curation beside the team chips' (#1123).
--
-- ## Why the bag is WIDENED and not joined by a sibling
--
-- 00049 wrote the rule this migration is obeying: "a NEW CONCERN gets a
-- new bag and a new key inside an existing concern gets none." The
-- question is therefore only whether a followed tag's chip is a new
-- concern, and it is not. It is the SAME PIECE OF FURNITURE: one strip,
-- one component (TeamsRail), one manage panel, one client-side
-- application point, one save. The chips differ in what they filter by
-- — `?team=` or `?tag=` — which is a difference between two rows of the
-- same list, not between two concerns.
--
-- Every test 00049 applied to `feed_filters` still separates this bag
-- from that one, and separates it identically for both chip kinds:
-- `feed_filters` is SERVER-APPLIED and SUBTRACTS CONTENT, this is
-- CLIENT-APPLIED and curates NAVIGATION. Hiding #fantasy from your rail
-- must not hide fantasy posts from your feed, exactly as hiding a team
-- must not hide its posts, and the guarantee is the same one: no
-- server-side query has this column available to consult.
--
-- A sibling `tag_rail` column would have split one save across two bags
-- and left them to be kept consistent by convention. It would also have
-- foreclosed the obvious next request — one rail order INTERLEAVING
-- teams and tags — which a single bag can grow a `rail_order` key for
-- and two bags cannot without a merge migration.
--
-- ## Why RENAME rather than keep the old name and add tag keys
--
-- Because `team_rail.hidden_tags` is a lie in the schema, and the
-- schema is where the next reader looks first. The column is TWO DAYS
-- OLD (00049 landed with sprint 24), predates any release that names
-- it, and has no external consumer: the whole cost of the rename is
-- mechanical and it will never be lower than it is now. Pre-release
-- practice here is to fix the name rather than ship a compat shim
-- around it.
--
-- RENAME COLUMN preserves the data, so every reader who curated a rail
-- in the last two days keeps their curation. The old key names inside
-- the bag are NOT renamed — `hidden_team_ids` and `team_order` are
-- team-specific keys and correctly say so; they are joined by, not
-- replaced with, `hidden_tags` and `tag_order`.
--
-- ## The key names carry the default (00036's rule, as restated by 00049)
--
-- For a list the zero value is EMPTY, and an absent key means "this
-- build's default":
--
--   hidden_tags  — empty = nothing hidden. (`shown_tags` would invert
--                  it: empty would then have to mean "show everything",
--                  which is not what an empty allow-list says.)
--   tag_order    — empty = the server's order (followed_at, then the
--                  tag). A partial list is legal and means "these
--                  first, in this order, then everything else as
--                  before", so moving one chip never requires writing
--                  an entry per tag.
--
-- Neither list is validated against `tag_follows` on write, for 00049's
-- reason unchanged: a preference write that 400s because the reader's
-- own list holds a tag they have since unfollowed would strand them.
-- Unknown entries are inert — the rail intersects both lists with the
-- follows the server actually returned, so a dead tag is dropped at
-- render and costs nothing.

-- +goose Up

ALTER TABLE public.user_preferences
    RENAME COLUMN team_rail TO browse_rail;

-- +goose Down

ALTER TABLE public.user_preferences
    RENAME COLUMN browse_rail TO team_rail;
