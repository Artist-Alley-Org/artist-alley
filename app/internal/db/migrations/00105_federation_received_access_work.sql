-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00105_federation_received_access_work.sql
--
-- #1597 (spec v1 §16.7): the bounded background queue of access copies the
-- receiver is fetching (assetingest access worker). The authoritative
-- candidates and outcomes stay the received file rows (00086, 00104):
-- this table holds only work that was admitted and is not yet decided,
-- one row per access entry (received_asset_id, position), never one row
-- per received file.
--
--   - Admission: an access entry of a current complete record, not
--     verified and never attempted (no access_failure_reason), from an
--     enabled, connected peer with all four derived-transfer-1 profiles
--     and a live access-copy grant. At most a fixed number of rows exist
--     at once (the worker's outstanding bound), counted under a
--     transaction-level try-advisory lock, so concurrent processes never
--     admit past it and never wait for each other.
--   - Claim: a row is taken for a lease by setting a fresh claim_token and
--     claim_expires_at (FOR UPDATE SKIP LOCKED), at most a fixed number at
--     once across processes. Every write that decides the work (the
--     verified copy, a failure reason, a reschedule, a release) checks the
--     token, unexpired, in its own transaction, so a worker whose claim
--     lapsed (crashed, stalled) cannot finish it.
--   - Retry: a transient transfer failure (timeout, transport error, HTTP
--     429/5xx, lease lost) reschedules the row (next_attempt_at, a capped
--     backoff) until attempts reaches the worker's bound; every other
--     failure, and the last transient one, is recorded on the file row
--     (access_failure_reason) and the work row is deleted.
--   - A row whose record is no longer current, or whose file was decided,
--     is deleted without a file result; the file row (or its record) going
--     deletes it too (FK cascade).
--
-- last_reason is the last transient reason only (bounded set), for
-- operators; the decided reason lives on the file row.
--
-- The partial index on federation_received_asset_files supports bounded
-- discovery: keyset pages over the unattempted access entries only.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.federation_received_access_work (
    received_asset_id uuid NOT NULL,
    "position" integer NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    claim_token uuid,
    claim_expires_at timestamp with time zone,
    last_reason text,
    admitted_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_received_access_work_pkey PRIMARY KEY (received_asset_id, "position"),
    CONSTRAINT federation_received_access_work_file_fkey FOREIGN KEY (received_asset_id, "position")
        REFERENCES public.federation_received_asset_files(received_asset_id, "position") ON DELETE CASCADE,
    CONSTRAINT federation_received_access_work_attempts_check CHECK (attempts >= 0 AND attempts <= 100),
    CONSTRAINT federation_received_access_work_claim_check CHECK ((claim_token IS NULL) = (claim_expires_at IS NULL)),
    CONSTRAINT federation_received_access_work_last_reason_check CHECK (last_reason IS NULL OR last_reason = ANY (ARRAY[
        'timeout'::text, 'transport_error'::text, 'http_status'::text, 'lease_lost'::text]))
);

CREATE INDEX IF NOT EXISTS federation_received_access_work_due_idx
    ON public.federation_received_access_work USING btree (next_attempt_at, received_asset_id, "position");

CREATE INDEX IF NOT EXISTS federation_received_asset_files_access_unattempted_idx
    ON public.federation_received_asset_files USING btree (received_asset_id, "position")
    WHERE ((role = 'access'::text) AND (state = 'not_transferred'::text) AND (access_failure_reason IS NULL));

COMMENT ON TABLE public.federation_received_access_work IS 'Admitted, undecided access-copy work (#1597, spec v1 §16.7): one row per access entry being fetched, bounded in number, with its due time, attempts and claim. The file row (federation_received_asset_files) is the candidate and the outcome; this row only schedules it and is deleted when the work is decided or stale.';
COMMENT ON COLUMN public.federation_received_access_work.claim_token IS 'The current claim (#1597): set with claim_expires_at when a worker takes the row; every write that decides the work requires this token, unexpired.';
COMMENT ON COLUMN public.federation_received_access_work.last_reason IS 'The last transient failure that rescheduled the row (#1597): timeout, transport_error, http_status (429/5xx) or lease_lost.';

-- +goose Down

-- The queue is derived: every admitted row is an unattempted access entry
-- of a current version, which a later Up rediscovers. Nothing decided
-- lives here (outcomes are on the file rows).
DROP INDEX IF EXISTS public.federation_received_asset_files_access_unattempted_idx;
DROP TABLE IF EXISTS public.federation_received_access_work;
