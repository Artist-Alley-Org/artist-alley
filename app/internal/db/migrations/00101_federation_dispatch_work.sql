-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00101_federation_dispatch_work.sql
--
-- #1577: durable dispatch admission for local activities.
--
-- Until now the outbox dispatcher admitted local activities by position:
-- every row whose (created_at, id) sorted after a single cursor. created_at
-- is the inserting transaction's start time (DEFAULT now()), but rows become
-- visible when their transaction COMMITS. An activity whose transaction
-- began earlier and committed after a later one had been dispatched sorted
-- behind the cursor and was never enqueued (reproduced deterministically in
-- review, rev794).
--
-- This migration admits by membership instead. federation_dispatch_work
-- holds one row per local activity still to be dispatched: its id and a copy
-- of its created_at, used ONLY to order pending rows, never to decide
-- whether a row is pending. A row is written by a trigger in the same
-- transaction as its activity (so a rolled-back activity leaves none, and a
-- late commit is still pending whatever its created_at), and deleted by the
-- dispatcher in the same transaction as that activity's outbox rows. The
-- cursor in federation_dispatch_state remains as monotone progress only.
--
-- UPGRADE BOUNDARY. Up is supported only at a quiescent boundary: stop and
-- drain every OLD application process (its dispatcher and every activity
-- writer) before migrating, and start only new binaries after. Old and new
-- binaries sharing one database are NOT supported: an old dispatcher would
-- neither consume nor respect the work rows. Within that boundary Up still
-- locks out writers itself (below), so the backfill and the trigger cover
-- every activity exactly once.
--
-- BACKLOG. Only local activities strictly AFTER the legacy cursor (all local
-- activities when it is NULL) become pending. Rows at or before it are
-- legacy history the old dispatcher already considered, and are NOT
-- replayed: an activity it skipped, kept private or addressed differently
-- must not reach a peer that became eligible since. An activity the old
-- dispatcher MISSED (the late-commit case) sits behind the cursor and is not
-- repaired here; recovering one needs a separately reviewed repair, never an
-- automatic rewind or replay. Up refuses, changing nothing, when the cursor
-- row is missing or names an activity that no longer exists, because
-- neither "all" nor "none" would be a safe guess.

-- +goose Up

-- Exclude writers (every INSERT/UPDATE/DELETE on activities) and cursor
-- advances until this transaction commits. SHARE ROW EXCLUSIVE waits for any
-- writer already in flight, so every activity committed before the trigger
-- exists is visible to the backfill below, and none can commit between the
-- backfill and the trigger. Readers (and the foreign-key share locks an
-- outbox insert takes) are not blocked.
LOCK TABLE public.activities IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE public.federation_dispatch_state IN SHARE ROW EXCLUSIVE MODE;

CREATE TABLE IF NOT EXISTS public.federation_dispatch_work (
    activity_id uuid NOT NULL,
    activity_created_at timestamp with time zone NOT NULL,
    CONSTRAINT federation_dispatch_work_pkey PRIMARY KEY (activity_id),
    CONSTRAINT federation_dispatch_work_activity_id_fkey FOREIGN KEY (activity_id)
        REFERENCES public.activities(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS federation_dispatch_work_order_idx
    ON public.federation_dispatch_work USING btree (activity_created_at, activity_id);

COMMENT ON TABLE public.federation_dispatch_work IS 'Local activities still to be dispatched to the federation outbox (#1577): one row per local activity, written by a trigger in the activity''s own transaction and deleted by the dispatcher in the transaction that writes its outbox rows (or records its skip). activity_created_at only orders pending rows; membership alone decides what is pending. Holds UUID references and that time only.';

-- +goose StatementBegin
CREATE FUNCTION public.federation_dispatch_work_schedule() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO public.federation_dispatch_work (activity_id, activity_created_at) VALUES (NEW.id, NEW.created_at);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_dispatch_work_schedule_trg
    AFTER INSERT ON public.activities
    FOR EACH ROW
    WHEN (NEW.source = 'local')
    EXECUTE FUNCTION public.federation_dispatch_work_schedule();

-- +goose StatementBegin
DO $$
DECLARE
    has_state boolean;
    cursor_id uuid;
    cursor_at timestamp with time zone;
BEGIN
    SELECT TRUE, s.last_dispatched_activity_id INTO has_state, cursor_id
      FROM public.federation_dispatch_state s WHERE s.id = 1;
    IF has_state IS NULL THEN
        RAISE EXCEPTION '00101 up: federation_dispatch_state has no cursor row; which activities are still to dispatch is unknown (#1577)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF cursor_id IS NULL THEN
        INSERT INTO public.federation_dispatch_work (activity_id, activity_created_at)
        SELECT a.id, a.created_at FROM public.activities a WHERE a.source = 'local';
        RETURN;
    END IF;
    SELECT a.created_at INTO cursor_at FROM public.activities a WHERE a.id = cursor_id;
    IF cursor_at IS NULL THEN
        RAISE EXCEPTION '00101 up: the dispatch cursor names activity %, which no longer exists; point it at a reviewed activity (or NULL) before migrating (#1577)', cursor_id
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    INSERT INTO public.federation_dispatch_work (activity_id, activity_created_at)
    SELECT a.id, a.created_at FROM public.activities a
     WHERE a.source = 'local' AND (a.created_at, a.id) > (cursor_at, cursor_id);
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- Down refuses while any work is pending, rather than discard activities a
-- committed transaction scheduled. Both locks are taken BEFORE the check and
-- held by this migration's transaction through the removal: the activities
-- lock waits for any writer in flight (whose trigger may be adding work) and
-- excludes new ones; the work lock waits for a dispatcher that holds claimed
-- rows. So the check sees every row the drop would remove.
LOCK TABLE public.activities IN SHARE ROW EXCLUSIVE MODE;
LOCK TABLE public.federation_dispatch_work IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.federation_dispatch_work) THEN
        RAISE EXCEPTION '00101 down: local activities are still to be dispatched; Down would lose them (#1577)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_dispatch_work_schedule_trg ON public.activities;
DROP FUNCTION IF EXISTS public.federation_dispatch_work_schedule();
DROP TABLE IF EXISTS public.federation_dispatch_work;
