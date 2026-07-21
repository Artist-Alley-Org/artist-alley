-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00012_scheduled_actions.sql
--
-- The generic scheduled-action engine (#40 sprint 1, ADR 0020
-- §"Scheduled actions").
--
-- A declarative layer over the existing jobs queue. The queue already
-- runs future-dated work (jobs.ClaimNextJob filters
-- `scheduled_for IS NULL OR scheduled_for <= NOW()`), so this is NOT a
-- second deferred runner. What the queue does not give us, and what
-- ADR 0020 requires, is:
--
--   * a durable, LISTABLE record of every pending action by target,
--     owner and date — the "Scheduled actions" admin surface;
--   * CANCELLATION before fire — an expiry scheduled when a
--     subscription lapses must be cancellable if it renews (#51), which
--     a raw enqueued job cannot express cleanly.
--
-- So the table is the source of truth for WHAT is scheduled; a recurring
-- reaper job (on the queue) is the executor. The two-layer split is the
-- ADR's design and the reason this is a table rather than N enqueued
-- jobs with a cancelled flag.
--
-- ---------------------------------------------------------------
-- WHY target_id IS TEXT, NOT UUID
-- ---------------------------------------------------------------
-- The target is polymorphic across asset/post/collection (UUID keys)
-- and user (bigint ref). No single typed column spans both, and this
-- mirrors the notifications table, which solved the identical problem
-- with (target_kind TEXT, target_id TEXT). Each executor parses its own
-- kind's id. Deliberately FK-free for the same reason featured_items is:
-- an action may outlive its target (a delete action whose asset was
-- already hard-deleted resolves to a no-op the executor records, rather
-- than cascading or failing a constraint).
--
-- ---------------------------------------------------------------
-- THE TRAIL IS THE AUDIT LOG, NOT A COLUMN
-- ---------------------------------------------------------------
-- ADR 0020's action shape shows a `trail: []`. That trail lives in
-- audit_events (the audit package), written by each executor, not in a
-- column here — it is what makes the history Enterprise-exportable. So
-- there is no trail column; scheduled_actions records STATE, audit
-- records WHAT HAPPENED.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE public.scheduled_actions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action        text        NOT NULL,
    target_kind   text        NOT NULL,
    target_id     text        NOT NULL,
    params        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    scheduled_for timestamptz NOT NULL,
    state         text        NOT NULL DEFAULT 'pending',
    error         text,
    created_by    bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    executed_at   timestamptz,
    CONSTRAINT scheduled_actions_action_check
        CHECK (action = ANY (ARRAY['restrict'::text, 'delete'::text,
                                   'change_state'::text, 'change_sensitivity'::text,
                                   'notify'::text])),
    CONSTRAINT scheduled_actions_target_kind_check
        CHECK (target_kind = ANY (ARRAY['asset'::text, 'post'::text,
                                        'collection'::text, 'user'::text])),
    CONSTRAINT scheduled_actions_state_check
        CHECK (state = ANY (ARRAY['pending'::text, 'done'::text,
                                  'cancelled'::text, 'failed'::text]))
);
-- +goose StatementEnd

-- The reaper's due-scan: pending actions whose time has come, oldest
-- first. Partial on state='pending' so the index stays small — done /
-- cancelled / failed rows accumulate but never slow the scan.
-- +goose StatementBegin
CREATE INDEX scheduled_actions_due_idx
    ON public.scheduled_actions (scheduled_for)
    WHERE state = 'pending';
-- +goose StatementEnd

-- The admin list surface reads recent actions across all states,
-- newest first.
-- +goose StatementBegin
CREATE INDEX scheduled_actions_created_idx
    ON public.scheduled_actions (created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE public.scheduled_actions;
-- +goose StatementEnd
