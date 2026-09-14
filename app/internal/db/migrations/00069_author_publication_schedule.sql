-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00069_author_publication_schedule.sql
--
-- #1119 (sprint 21e), ADR 0020. An author can schedule their OWN post
-- for future publication through the existing scheduled-action engine.
-- The row that carries the instruction is an ordinary
-- `change_state` / `post` action, fired by the same reaper through the
-- same publication core as #1238's operator-scheduled arm. Two things
-- the generic table could not say are added here.
--
-- 1. WHICH SURFACE MADE THE ROW: `origin`.
--
--    The author surface (PUT/GET/DELETE /posts/{id}/publication-schedule)
--    owns exactly the rows it made. Its status read must not show an
--    author an operator's takedown of their post, and its cancel must
--    never cancel anything but the author's own instruction. `created_by`
--    alone cannot tell those apart: an operator who is also the author,
--    scheduling through the generic surface, would be indistinguishable
--    from the same person acting as an author. So the surface is
--    recorded on the row. 'generic' is every row the engine's own
--    Store.Schedule writes (operator or system); 'author' is only what
--    the author seam writes.
--
-- 2. ONE PENDING AUTHOR PUBLICATION SCHEDULE PER POST, enforced by the
--    database and not by a check-then-insert.
--
--    Two requests that both read "no pending schedule" and both insert
--    would leave two standing instructions for one post. The engine
--    would fire both (the second lands on #1238's idempotent no-op
--    cell), so nothing breaks, but the author's status surface would
--    have to pick one to show, and the one it did not show could not be
--    cancelled from it. A partial unique index on the pending author
--    rows closes that at the transaction boundary: the second inserter
--    blocks on the first's uncommitted row and receives 23505 when it
--    commits. The predicate is deliberately narrow so that:
--
--      * the generic engine keeps its multiplicity. Two operator
--        schedules for one post, a scheduled unpublish beside a
--        scheduled publish, a notify beside either: all still allowed,
--        because none of them carries origin = 'author'.
--      * terminal rows never collide. A done, failed or cancelled
--        author schedule is outside the predicate, so a later valid
--        schedule for the same post inserts cleanly.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.scheduled_actions
    ADD COLUMN origin text NOT NULL DEFAULT 'generic'
        CONSTRAINT scheduled_actions_origin_check
        CHECK (origin = ANY (ARRAY['generic'::text, 'author'::text]));
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX scheduled_actions_author_pending_post_idx
    ON public.scheduled_actions (target_id)
    WHERE state = 'pending'
      AND origin = 'author'
      AND action = 'change_state'
      AND target_kind = 'post';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX public.scheduled_actions_author_pending_post_idx;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE public.scheduled_actions DROP COLUMN origin;
-- +goose StatementEnd
