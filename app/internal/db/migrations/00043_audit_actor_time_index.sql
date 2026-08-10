-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00043_audit_actor_time_index.sql
--
-- An (actor_user_ref, occurred_at DESC) index on audit_events, for
-- GET /account/activity (#600).
--
-- WHY. The baseline shipped `audit_events__subject_time_idx` on
-- (subject_user_ref, occurred_at DESC) and nothing on actor_user_ref:
-- the only reader was the admin viewer, where the actor filter is
-- optional and the common query is an unfiltered newest-first scan
-- served by `audit_events__type_time_idx`. /account/activity is the
-- first reader that asks "rows where I am the ACTOR" as a matter of
-- course — it is half of every request it serves — and audit_events is
-- the highest-volume table in the schema. Without this the actor half
-- is a sequential scan over the whole log on a page an ordinary user
-- can open.
--
-- Mirrors the subject index exactly, including the partial predicate:
-- a large share of rows have no actor (self-service logins, system
-- sweeps, federation events all write actor NULL), and those rows can
-- never satisfy `actor_user_ref = caller`, so keeping them out costs
-- nothing and keeps the index proportional to the rows it answers for.
-- The caller ref is never NULL at the call site (the handler refuses
-- an anonymous or zero ref before querying), so the partial index is
-- always usable by that query.

-- +goose Up
-- +goose StatementBegin
CREATE INDEX audit_events__actor_time_idx
    ON public.audit_events USING btree (actor_user_ref, occurred_at DESC)
    WHERE (actor_user_ref IS NOT NULL);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS public.audit_events__actor_time_idx;
-- +goose StatementEnd
