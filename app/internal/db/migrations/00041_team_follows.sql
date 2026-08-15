-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00041_team_follows.sql
--
-- Teams become followable (#577). One row per (user, team) the user
-- has bookmarked, feeding the teams rail on browse.
--
-- ## A follow is a BOOKMARK, not a relationship
--
-- This table is deliberately a universe away from the authorization
-- tables. Nothing reads it to decide anything: not
-- `visibility.ContentReadable`, not the post read rule, not
-- `CanAssignToTeam`, not any capability resolution. Following a studio
-- widens exactly zero rows of what you can see of its work.
--
-- That is why it is a separate table from `team_memberships` rather
-- than a column on it. A `followed BOOLEAN` on the membership row
-- would have made "I want this in my sidebar" and "I am part of this
-- team" two states of one record, and every query that joined
-- memberships for an authorization answer would then be one WHERE
-- clause away from honouring a bookmark. The two facts have different
-- lifetimes, different writers and different consequences, so they get
-- different tables.
--
-- The shape mirrors `user_follows` (baseline) for the same reason:
-- following a person and following a studio are the same gesture, and
-- the pair should stay legible side by side.
--
-- ## No counts, no unread, no notifications
--
-- There is no `follower_count` column here or on `teams`. A
-- denormalised count is a second source of truth that has to be kept
-- correct on every insert, delete, team merge and user deletion, and
-- the number is wanted in exactly one place at a scale where
-- `COUNT(*)` against the PK is free. Add it when a query is slow, with
-- the measurement that showed it.
--
-- Likewise there is no `last_read_at`. A follow is a bookmark, not a
-- subscription; the unread/digest model is #520's arc and it should
-- get to choose its own schema rather than inherit a column guessed
-- at here.
--
-- ## Cascades
--
-- Both FKs cascade because a follow is meaningless without either end
-- of it, and neither end is content anyone would want to preserve a
-- dangling reference to. Note that the `teams` cascade fires only on a
-- HARD delete — `DELETE FROM teams` — because team deletion in the API
-- is soft (`teams.deleted_at`). The FK cannot see `deleted_at`, so it
-- offers no protection against following a tombstoned team; the
-- handler runs an explicit liveness probe for that, the same discipline
-- `visibility.CanAssignToTeam` documents (#955). Read queries filter
-- `deleted_at IS NULL`, so a soft-deleted team leaves the rail without
-- the row having to go anywhere.

-- +goose Up

CREATE TABLE public.team_follows (
    user_ref bigint NOT NULL,
    team_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

ALTER TABLE ONLY public.team_follows
    ADD CONSTRAINT team_follows_pkey PRIMARY KEY (user_ref, team_id);

ALTER TABLE ONLY public.team_follows
    ADD CONSTRAINT team_follows_user_ref_fkey FOREIGN KEY (user_ref)
        REFERENCES public."user"(ref) ON DELETE CASCADE;

ALTER TABLE ONLY public.team_follows
    ADD CONSTRAINT team_follows_team_id_fkey FOREIGN KEY (team_id)
        REFERENCES public.teams(id) ON DELETE CASCADE;

-- The PK already serves "the teams I follow" (user_ref leading). This
-- index serves the other direction — "who follows this team" — which is
-- what a follower count and any future fanout read. Mirrors
-- idx_user_follows_followee.
CREATE INDEX team_follows_team_idx
    ON public.team_follows USING btree (team_id, created_at DESC);

-- +goose Down

DROP TABLE public.team_follows;
