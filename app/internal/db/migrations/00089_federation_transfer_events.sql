-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00089_federation_transfer_events.sql
--
-- #1574 slice 2: the receiver's durable ledger of transfer and fixity
-- events, so an operator can see what happened to a received asset after
-- its record, or the peer it came from, is gone.
--
-- federation_received_assets cascades with its peer
-- (federation_received_assets_peer_id_fkey ON DELETE CASCADE), and #1574
-- requires the evidence of a failed or verified transfer to be kept. So
-- this table has NO foreign key: every identity in a row is a snapshot
-- taken when the event was written (the peer's id, address and name; the
-- record's id, activity, source and version; the file's position, role,
-- listed digest and declared type). Deleting the record or the peer leaves
-- every row in place.
--
-- One row per event, written by the receiver core (assetingest) in the SAME
-- transaction as the record's state change:
--   ingestion     the record was refused before or after its transfer
--                 (failure), deferred to a later attempt (warning), or
--                 completed (success)
--   transfer      fetching one original from the origin failed (failure),
--                 or failed and will be retried (warning)
--   fixity-check  one original's received bytes were checked: matched
--                 (success) or did not match its listed size or checksums
--                 (failure; warning only when the process stopped first)
-- The record's state after the event is snapshotted too: a warning leaves
-- it pending, a failure rejected, a success complete.
--
-- Evidence, never bytes. A row holds the expected (listed) and observed
-- digest and size of a file; nothing in it points at stored bytes.
-- Mismatched bytes are never stored (casfetch verifies while streaming and
-- discards them), a rejected record holds no pin, and no local asset is
-- created. observed_basis says where an observation came from:
--   received_bytes   the bytes the origin sent, as they streamed in
--   stored_bytes     bytes this instance already held, re-verified before
--                    reuse
--   response_header  the origin's Content-Length, refused before any byte
--                    was read
--   unavailable      a mismatch was detected where no observation is
--                    captured (the storage import's own re-check)
-- An overlong body is read only to one byte past its listed size:
-- observed_truncated marks that, with that size as a lower bound and no
-- digest.
--
-- Immutable. UPDATE is always refused. DELETE is refused unless the row is
-- older than the retention period: the transaction-local setting
-- aa.federation_event_retention (an interval), default 365 days, never less
-- than 30 days. The app's daily sweep sets it from
-- AA_FEDERATION_EVENT_RETENTION_DAYS and deletes what is older; nothing
-- removes a row early. TRUNCATE is not row-level and is not blocked (it is
-- an operator's or a test's wholesale reset, never the app's).
--
-- asset_preservation_events stays the ledger of local sweeps of local
-- assets (its asset_id FK and source = 'local' are unchanged).

-- +goose Up

-- The record's closed reason set (00086) lacked archival_invalid, which the
-- receiver core records when a pending record's stored announcement no
-- longer validates or its stored file-set digest no longer matches. The
-- record's UPDATE to rejected then failed its CHECK and it stayed pending
-- (#1574 slice 2 review). The set gains archival_invalid; nothing else
-- changes, and every existing row already satisfies the wider set.
ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text]));

CREATE TABLE public.federation_transfer_events (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    -- Snapshots: no foreign keys, by design.
    peer_id uuid NOT NULL,
    peer_instance_url text NOT NULL,
    peer_display_name text,
    received_asset_id uuid NOT NULL,
    activity_uri text NOT NULL,
    origin_asset_uri text NOT NULL,
    version_uri text NOT NULL,
    version_number bigint NOT NULL,
    sensitivity text NOT NULL,
    -- The file, for transfer and fixity-check events.
    file_position integer,
    file_role text,
    object_hash text,
    declared_media_type text,
    -- What happened.
    event_type text NOT NULL,
    outcome text NOT NULL,
    reason text,
    expected_sha256 text,
    expected_size_bytes bigint,
    observed_basis text,
    observed_sha256 text,
    observed_size_bytes bigint,
    observed_truncated boolean DEFAULT false NOT NULL,
    mismatch_algorithm text,
    mismatch_expected text,
    mismatch_observed text,
    attempt integer NOT NULL,
    agent_name text NOT NULL,
    agent_version text,
    record_state_after text NOT NULL,
    detail text,
    CONSTRAINT federation_transfer_events_pkey PRIMARY KEY (id),
    CONSTRAINT federation_transfer_events_event_type_check CHECK (event_type = ANY (ARRAY['transfer'::text, 'fixity-check'::text, 'ingestion'::text])),
    CONSTRAINT federation_transfer_events_outcome_check CHECK (outcome = ANY (ARRAY['success'::text, 'failure'::text, 'warning'::text])),
    CONSTRAINT federation_transfer_events_state_check CHECK (record_state_after = CASE outcome
        WHEN 'success' THEN 'complete'::text WHEN 'failure' THEN 'rejected'::text ELSE 'pending'::text END),
    -- The same closed reason set as the record (00086, plus archival_invalid
    -- added to both below).
    CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text]))),
    -- A file is named whole or not at all; transfer and fixity-check events
    -- always name one, and its expected digest is its listed one.
    CONSTRAINT federation_transfer_events_file_check CHECK (
        (file_position IS NULL AND file_role IS NULL AND object_hash IS NULL AND declared_media_type IS NULL
         AND expected_sha256 IS NULL AND expected_size_bytes IS NULL AND event_type = 'ingestion'::text)
        OR (file_position IS NOT NULL AND file_role IS NOT NULL AND object_hash IS NOT NULL
            AND declared_media_type IS NOT NULL AND expected_sha256 IS NOT NULL AND expected_size_bytes IS NOT NULL
            AND expected_sha256 = object_hash AND file_position >= 0 AND file_position < 256
            AND file_role = ANY (ARRAY['original'::text, 'preservation'::text, 'access'::text])
            AND event_type <> 'ingestion'::text)),
    -- An observation needs a file and says where it came from.
    CONSTRAINT federation_transfer_events_observed_check CHECK (COALESCE(
        (observed_basis IS NULL AND observed_sha256 IS NULL AND observed_size_bytes IS NULL
         AND NOT observed_truncated AND mismatch_algorithm IS NULL)
        OR (object_hash IS NOT NULL AND observed_basis = ANY (ARRAY['received_bytes'::text, 'stored_bytes'::text,
            'response_header'::text, 'unavailable'::text])), false)),
    CONSTRAINT federation_transfer_events_basis_check CHECK (CASE observed_basis
        WHEN 'unavailable' THEN observed_sha256 IS NULL AND observed_size_bytes IS NULL AND NOT observed_truncated
        WHEN 'response_header' THEN observed_sha256 IS NULL AND observed_size_bytes IS NOT NULL AND NOT observed_truncated
        WHEN 'received_bytes' THEN observed_size_bytes IS NOT NULL
        WHEN 'stored_bytes' THEN observed_size_bytes IS NOT NULL
        ELSE true END),
    -- Read only to one byte past the listed size: a lower bound, no digest.
    CONSTRAINT federation_transfer_events_truncated_check CHECK (COALESCE(NOT observed_truncated
        OR (observed_size_bytes = expected_size_bytes + 1 AND observed_sha256 IS NULL), false)),
    -- A mismatch on an additional listed checksum (sha256 is observed_sha256).
    CONSTRAINT federation_transfer_events_mismatch_check CHECK (COALESCE(
        (mismatch_algorithm IS NULL AND mismatch_expected IS NULL AND mismatch_observed IS NULL)
        OR (mismatch_algorithm = ANY (ARRAY['sha512'::text, 'md5'::text])
            AND mismatch_expected ~ '^[0-9a-f]{32,128}$'::text AND mismatch_observed ~ '^[0-9a-f]{32,128}$'::text
            AND mismatch_expected <> mismatch_observed AND reason = 'fixity_mismatch'::text), false)),
    -- Each outcome and reason is bound to the evidence it claims. Every
    -- evidence rule is COALESCEd to false: a CHECK that evaluates to NULL
    -- passes, so a missing value must never read as evidence.
    CONSTRAINT federation_transfer_events_evidence_check CHECK (COALESCE(CASE
        -- A fixity check is a success or a size/checksum mismatch, nothing else.
        WHEN event_type = 'fixity-check'::text AND outcome = 'success'::text THEN
            observed_basis = ANY (ARRAY['received_bytes'::text, 'stored_bytes'::text])
            AND observed_sha256 = expected_sha256 AND observed_size_bytes = expected_size_bytes
            AND NOT observed_truncated AND mismatch_algorithm IS NULL
        WHEN event_type = 'fixity-check'::text THEN reason = ANY (ARRAY['fixity_mismatch'::text, 'length_mismatch'::text])
            AND observed_basis IS NOT NULL
        WHEN reason = ANY (ARRAY['fixity_mismatch'::text, 'length_mismatch'::text]) THEN false
        ELSE observed_basis IS NULL END, false)),
    CONSTRAINT federation_transfer_events_fixity_check CHECK (reason IS DISTINCT FROM 'fixity_mismatch'::text
        OR COALESCE(observed_basis = 'unavailable'::text
        OR (observed_basis = ANY (ARRAY['received_bytes'::text, 'stored_bytes'::text])
            AND (observed_size_bytes <> expected_size_bytes
                 OR (observed_sha256 IS NOT NULL AND observed_sha256 <> expected_sha256)
                 OR mismatch_algorithm IS NOT NULL)), false)),
    CONSTRAINT federation_transfer_events_length_check CHECK (reason IS DISTINCT FROM 'length_mismatch'::text
        OR COALESCE(observed_basis = 'unavailable'::text
        OR (observed_size_bytes IS NOT NULL AND observed_size_bytes <> expected_size_bytes), false)),
    CONSTRAINT federation_transfer_events_hash_check CHECK ((object_hash IS NULL OR object_hash ~ '^[0-9a-f]{64}$'::text)
        AND (observed_sha256 IS NULL OR observed_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT federation_transfer_events_size_check CHECK ((expected_size_bytes IS NULL OR (expected_size_bytes >= 0 AND expected_size_bytes <= 9007199254740991))
        AND (observed_size_bytes IS NULL OR (observed_size_bytes >= 0 AND observed_size_bytes <= 9007199254740992))),
    CONSTRAINT federation_transfer_events_attempt_check CHECK (attempt >= 0),
    CONSTRAINT federation_transfer_events_version_number_check CHECK (version_number >= 1 AND version_number <= 9007199254740991),
    CONSTRAINT federation_transfer_events_sensitivity_check CHECK (sensitivity = ANY (ARRAY['public'::text, 'team'::text, 'restricted'::text, 'embargo'::text])),
    CONSTRAINT federation_transfer_events_agent_check CHECK (octet_length(agent_name) >= 1 AND octet_length(agent_name) <= 128
        AND (agent_version IS NULL OR (octet_length(agent_version) >= 1 AND octet_length(agent_version) <= 128))),
    CONSTRAINT federation_transfer_events_detail_check CHECK (detail IS NULL OR octet_length(detail) <= 2000)
);

COMMENT ON TABLE public.federation_transfer_events IS 'The receiver''s durable ledger of transfer and fixity events (#1574): snapshots with no foreign key, immutable, deleted only past the retention period (aa.federation_event_retention, default 365 days, at least 30).';
COMMENT ON COLUMN public.federation_transfer_events.observed_basis IS 'Where the observation came from: received_bytes, stored_bytes, response_header (Content-Length, no byte read) or unavailable.';
COMMENT ON COLUMN public.federation_transfer_events.observed_truncated IS 'The body was read only to one byte past its listed size: observed_size_bytes is a lower bound and there is no digest.';

CREATE INDEX federation_transfer_events_occurred_idx ON public.federation_transfer_events USING btree (occurred_at DESC, id DESC);
CREATE INDEX federation_transfer_events_peer_idx ON public.federation_transfer_events USING btree (peer_id, occurred_at DESC);
CREATE INDEX federation_transfer_events_record_idx ON public.federation_transfer_events USING btree (received_asset_id, occurred_at DESC);

-- +goose StatementBegin
-- The retention period in force: the transaction's
-- aa.federation_event_retention, default 365 days, never under 30 days.
CREATE FUNCTION public.federation_transfer_event_retention() RETURNS interval
    LANGUAGE sql STABLE
    AS $$
    SELECT GREATEST(
        COALESCE(NULLIF(current_setting('aa.federation_event_retention', true), '')::interval, interval '365 days'),
        interval '30 days')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION public.federation_transfer_events_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'federation transfer event %: events are immutable (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.occurred_at >= now() - public.federation_transfer_event_retention() THEN
        RAISE EXCEPTION 'federation transfer event %: kept until it is older than the retention period (%)',
            OLD.id, public.federation_transfer_event_retention()
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER federation_transfer_events_guard_trg
    BEFORE UPDATE OR DELETE ON public.federation_transfer_events
    FOR EACH ROW EXECUTE FUNCTION public.federation_transfer_events_guard();

-- +goose Down

DROP TRIGGER IF EXISTS federation_transfer_events_guard_trg ON public.federation_transfer_events;
DROP FUNCTION IF EXISTS public.federation_transfer_events_guard();
DROP FUNCTION IF EXISTS public.federation_transfer_event_retention();
DROP TABLE IF EXISTS public.federation_transfer_events;

-- The 00086 reason set again, for new writes. NOT VALID: a record already
-- rejected as archival_invalid keeps the reason that says what happened
-- (rewriting it would lose that); only new rows are held to the old set.
ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text])) NOT VALID;
