-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00050_tag_follows.sql
--
-- Tags become followable (#1123), alongside users (baseline) and teams
-- (00041). One row per (user, tag) the reader has bookmarked, feeding
-- the browse rail's `#` chips and the Following feed's third source.
--
-- ## A follow is a BOOKMARK, not a grant — the same sentence 00041 wrote
--
-- Nothing reads this table to decide anything. Not
-- `visibility.ContentReadable`, not the post read rule, not any
-- capability resolution. Following #fantasy widens WHICH POSTS QUALIFY
-- for the Following feed; it does not widen WHO MAY READ ONE. In
-- `posts.ListPostsPageGated` the follow set is a NARROWING conjunct
-- ANDed beside the read rule (ADR 0063 placeholder discipline), never a
-- disjunct with it, so the worst a bookmark can do is remove rows from
-- a page the caller could already see.
--
-- That matters more here than it did for teams. A tag is written by
-- WHOEVER AUTHORED THE POST, so `tag_follows` is the one follow table
-- whose right-hand side is attacker-chosen: anybody can tag a private
-- post `fantasy`. If the third EXISTS were ever ORed into the read rule
-- rather than ANDed beside it, tagging a restricted post with a popular
-- tag would publish it to every follower of that tag. The acceptance
-- test for this issue is precisely that pair — the same restricted post
-- carrying a followed tag, visible to one caller and not the other.
--
-- ## No FK, because there is no table to point at
--
-- Tags are a CORPUS, not an entity: `post_tags (post_id, tag)` has no
-- parent row anywhere, no id, and no lifecycle. So `tag` here is the
-- string itself and the table is deliberately unconstrained against the
-- corpus. Three consequences, all wanted:
--
--   * Following a tag nobody has used yet is legal and inert. It costs
--     one row and starts matching the day someone uses it, which is what
--     a reader following an emerging tag means by the gesture.
--   * A tag that stops being used anywhere leaves the feed on its own.
--     Nothing has to garbage-collect it, and the reader's chip stays
--     until they remove it — a follow is theirs, not the corpus's.
--   * There is NO EXISTENCE PROBE on the write path, unlike
--     `FollowTeam`'s. That asymmetry is a security property, not an
--     omission: a probe answering "does this tag exist" would answer it
--     from a corpus that spans posts the caller cannot read, turning
--     follow into an oracle that enumerates the tags of private work one
--     guess at a time. Teams can afford their probe because team rows
--     are already visible to anyone holding `teams.read`; tags cannot.
--
-- The #789 vocabulary arc may later formalise tags into a real table
-- with aliases and merges. This table survives that either way: it is
-- keyed on the string, and a merge would rewrite these rows exactly as
-- it rewrites `post_tags`.
--
-- ## Matching is EXACT, because `?tag=` is exact
--
-- `posts.ListPostsPageGated` filters with `pt.tag = $5::TEXT` and
-- `dedupeTags` stores tags TRIMMED but otherwise verbatim — case and
-- all. So the stored follow is trimmed and nothing else, and it matches
-- the corpus the same way the rail chip's `?tag=` filter does. Folding
-- case HERE and not there would make the chip and the feed disagree: a
-- reader following `fantasy` would see the chip filter to nothing while
-- the Following feed showed `Fantasy` posts.
--
-- The honest limitation that leaves: `Fantasy` and `fantasy` are two
-- tags, here and everywhere else in the product today. That is the
-- corpus's existing behaviour, not something this table introduces, and
-- case-folding is #789's decision to make once for all of it.
--
-- ## No counts, no unread, no notifications
--
-- As 00041. No `follower_count`, no `last_read_at`. A follow is a
-- bookmark, not a subscription; #520's arc owns notifications and
-- should pick its own schema rather than inherit a column guessed at
-- here.
--
-- ## Cascade
--
-- One FK, on `user_ref`, cascading: a follow is meaningless without the
-- reader who made it. The other side has nothing to cascade from.

-- +goose Up

CREATE TABLE public.tag_follows (
    user_ref bigint NOT NULL,
    tag text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.tag_follows
    ADD CONSTRAINT tag_follows_pkey PRIMARY KEY (user_ref, tag);

ALTER TABLE ONLY public.tag_follows
    ADD CONSTRAINT tag_follows_user_ref_fkey FOREIGN KEY (user_ref)
        REFERENCES public."user"(ref) ON DELETE CASCADE;

-- A tag is free text with no length ceiling anywhere else in the
-- product, and an unbounded string in a PRIMARY KEY is a btree page
-- overflow waiting for the first pathological write (postgres refuses
-- index entries past ~2704 bytes, as a runtime ERROR on INSERT). The
-- ceiling is deliberately generous — two orders of magnitude past any
-- real tag — because its job is to convert a hard index failure into a
-- clean constraint violation the handler can turn into a 400, not to
-- express a product opinion about tag length. `post_tags.tag` is
-- unconstrained and stays that way; this is a property of indexing the
-- column, not of the corpus.
ALTER TABLE ONLY public.tag_follows
    ADD CONSTRAINT tag_follows_tag_length CHECK (length(tag) > 0 AND length(tag) <= 200);

-- The PK already serves "the tags I follow" (user_ref leading). This
-- index serves the other direction — "who follows this tag" — which is
-- what a follower count and any future fanout would read. Mirrors
-- team_follows_team_idx and idx_user_follows_followee.
CREATE INDEX tag_follows_tag_idx
    ON public.tag_follows USING btree (tag, created_at DESC);

-- +goose Down

DROP TABLE public.tag_follows;
