-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00091_federation_asset_withdrawal.sql
--
-- #1575 (spec v1 §17.3): a received asset can be withdrawn by its origin.
-- An aa:tombstone (on a Delete of the asset) from the record's own peer
-- and origin:
--   - is kept, once per (peer, asset), in federation_received_tombstones,
--     including when it arrives before any announcement of the asset, so
--     a later or delayed announcement is refused (reason withdrawn) and
--     nothing is resurrected;
--   - moves every pending or complete record of that asset from that peer
--     to the new state withdrawn (reason withdrawn), releases the
--     record's own pins (federation_received_asset_release_pins: other pin
--     owners are untouched) and marks its originals withdrawn (no longer
--     verified: 00086's rule that only a complete record holds pins or
--     verified files still holds);
--   - writes one ledger event of the new type deaccession, outcome
--     success, record_state_after withdrawn.
-- The record keeps its identity, version and file set (§17.3: "keeps the
-- identity and fixity of a withdrawn asset").
--
-- On the sending side, federation_asset_announcement_claims keeps what
-- each aa:AssetVersion delivery told its peer (version number, sealed
-- files' wire role and sha256), so that the asset's withdrawal names the
-- highest version actually delivered to that peer, also after the asset
-- is purged.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.federation_received_tombstones (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    peer_id uuid NOT NULL,
    origin_asset_uri text NOT NULL,
    activity_uri text NOT NULL,
    actor_uri text NOT NULL,
    tombstone jsonb NOT NULL,
    deleted text NOT NULL,
    reason text,
    last_version text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_received_tombstones_pkey PRIMARY KEY (id),
    CONSTRAINT federation_received_tombstones_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_received_tombstones_asset_key UNIQUE (peer_id, origin_asset_uri),
    CONSTRAINT federation_received_tombstones_reason_check CHECK (reason IS NULL OR octet_length(reason) <= 4096)
);

COMMENT ON TABLE public.federation_received_tombstones IS 'The aa:tombstone an origin sent for an asset (#1575, spec v1 §17.3), kept once per (peer, asset), also before any announcement of it. Its presence withdraws every record of that asset from that peer and refuses later announcements.';

-- The sender's record of what each aa:AssetVersion delivery told its
-- peer, which an asset's withdrawal names (the highest one delivered).
CREATE TABLE IF NOT EXISTS public.federation_asset_announcement_claims (
    activity_id uuid NOT NULL,
    peer_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    version_number bigint NOT NULL,
    files jsonb NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_asset_announcement_claims_pkey PRIMARY KEY (activity_id, peer_id),
    CONSTRAINT federation_asset_announcement_claims_activity_id_fkey FOREIGN KEY (activity_id) REFERENCES public.activities(id) ON DELETE CASCADE,
    CONSTRAINT federation_asset_announcement_claims_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_asset_announcement_claims_version_check CHECK (version_number >= 1),
    CONSTRAINT federation_asset_announcement_claims_files_check CHECK (jsonb_typeof(files) = 'array'::text AND jsonb_array_length(files) >= 1 AND jsonb_array_length(files) <= 256)
);

CREATE INDEX IF NOT EXISTS federation_asset_announcement_claims_asset_peer_idx ON public.federation_asset_announcement_claims USING btree (asset_id, peer_id);

COMMENT ON TABLE public.federation_asset_announcement_claims IS 'What each aa:AssetVersion delivery told its peer (#1575): the version number and the sealed files (wire role and sha256) of the aa:asset member, written by the sender as it builds the member, under a share lock of the live asset row, before the request. An asset''s withdrawal names the highest of these whose announcement was delivered. No foreign key to the asset: kept after a purge, removed with the activity or the peer.';

ALTER TABLE public.federation_received_assets ADD COLUMN IF NOT EXISTS withdrawn_at timestamp with time zone;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_check CHECK (state = ANY (ARRAY['pending'::text, 'complete'::text, 'rejected'::text, 'withdrawn'::text]));

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_shape_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_shape_check CHECK (CASE state
        WHEN 'complete' THEN reason IS NULL AND completed_at IS NOT NULL AND authorizing_grant_id IS NOT NULL AND withdrawn_at IS NULL
        WHEN 'rejected' THEN reason IS NOT NULL AND completed_at IS NULL AND authorizing_grant_id IS NULL AND withdrawn_at IS NULL
        WHEN 'withdrawn' THEN reason = 'withdrawn'::text AND withdrawn_at IS NOT NULL
        ELSE completed_at IS NULL AND authorizing_grant_id IS NULL AND withdrawn_at IS NULL
    END);

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text, 'withdrawn'::text]));

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_state_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_state_check CHECK (state = ANY (ARRAY['pending'::text, 'verified'::text, 'not_transferred'::text, 'withdrawn'::text]));

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_role_state_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_role_state_check CHECK (CASE role
        WHEN 'original' THEN state = ANY (ARRAY['pending'::text, 'verified'::text, 'withdrawn'::text])
        ELSE state = 'not_transferred'::text
    END);

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_detected_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_detected_check CHECK (
        ((detected_media_type IS NULL) = (detected_by IS NULL))
        AND (detected_media_type IS NULL OR (role = 'original'::text AND state = ANY (ARRAY['verified'::text, 'withdrawn'::text])
             AND octet_length(detected_media_type) >= 3 AND octet_length(detected_media_type) <= 255
             AND octet_length(detected_by) >= 1 AND octet_length(detected_by) <= 128)));

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_event_type_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_event_type_check CHECK (event_type = ANY (ARRAY['transfer'::text, 'fixity-check'::text, 'ingestion'::text, 'deaccession'::text]));

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_state_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_state_check CHECK (CASE event_type
        WHEN 'deaccession' THEN outcome = 'success'::text AND record_state_after = 'withdrawn'::text
        ELSE record_state_after = CASE outcome
            WHEN 'success' THEN 'complete'::text WHEN 'failure' THEN 'rejected'::text ELSE 'pending'::text END
    END);

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_reason_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text, 'withdrawn'::text])));

-- A deaccession names no file, like an ingestion.
ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_file_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_file_check CHECK (
        (file_position IS NULL AND file_role IS NULL AND object_hash IS NULL AND declared_media_type IS NULL
         AND expected_sha256 IS NULL AND expected_size_bytes IS NULL AND event_type = ANY (ARRAY['ingestion'::text, 'deaccession'::text]))
        OR (file_position IS NOT NULL AND file_role IS NOT NULL AND object_hash IS NOT NULL
            AND declared_media_type IS NOT NULL AND expected_sha256 IS NOT NULL AND expected_size_bytes IS NOT NULL
            AND expected_sha256 = object_hash AND file_position >= 0 AND file_position < 256
            AND file_role = ANY (ARRAY['original'::text, 'preservation'::text, 'access'::text])
            AND event_type <> ALL (ARRAY['ingestion'::text, 'deaccession'::text])));

-- A withdrawn original keeps when it was verified (verified_at).
ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_verified_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_verified_check CHECK (CASE state
        WHEN 'verified' THEN verified_at IS NOT NULL
        WHEN 'withdrawn' THEN TRUE
        ELSE verified_at IS NULL
    END);

-- +goose StatementBegin
-- A complete record is final: its claims, file set and grant are never
-- rewritten, and it never goes back to pending or rejected. Its origin
-- can withdraw it (00091, §17.3): pending or complete to withdrawn, which
-- is final too.
CREATE OR REPLACE FUNCTION public.federation_received_assets_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state = 'withdrawn' THEN
        RAISE EXCEPTION 'received asset %: a withdrawn record is not changed (#1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'rejected' THEN
        RAISE EXCEPTION 'received asset %: a rejected record is not changed (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'complete' AND (NEW.state <> 'withdrawn'
       OR NEW.completed_at IS DISTINCT FROM OLD.completed_at
       OR NEW.authorizing_grant_id IS DISTINCT FROM OLD.authorizing_grant_id
       OR NEW.attempts <> OLD.attempts) THEN
        RAISE EXCEPTION 'received asset %: a complete record is not changed, only withdrawn (#1574, #1575)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.peer_id <> OLD.peer_id OR NEW.activity_uri <> OLD.activity_uri OR NEW.actor_uri <> OLD.actor_uri
       OR NEW.origin_asset_uri <> OLD.origin_asset_uri OR NEW.origin_asset_id <> OLD.origin_asset_id
       OR NEW.version_uri <> OLD.version_uri OR NEW.version_number <> OLD.version_number
       OR NEW.file_set_sha256 <> OLD.file_set_sha256 OR NEW.record <> OLD.record
       OR NEW.sensitivity <> OLD.sensitivity OR NEW.signer_key_id <> OLD.signer_key_id
       OR NEW.signer_actor_uri IS DISTINCT FROM OLD.signer_actor_uri OR NEW.received_at <> OLD.received_at
       OR NEW.created_xid <> OLD.created_xid THEN
        RAISE EXCEPTION 'received asset %: only its state, reason, attempts and commit are updated (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The file set is written with its record and changes only by being
-- verified while the record is pending, or (00091) by an original being
-- withdrawn with its record, keeping when it was verified and identified.
CREATE OR REPLACE FUNCTION public.federation_received_asset_files_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.received_asset_id <> OLD.received_asset_id OR NEW."position" <> OLD."position" OR NEW.role <> OLD.role
       OR NEW.object_hash <> OLD.object_hash OR NEW.size_bytes <> OLD.size_bytes OR NEW.checksums <> OLD.checksums
       OR NEW.declared_media_type <> OLD.declared_media_type
       OR NEW.declared_original_filename IS DISTINCT FROM OLD.declared_original_filename THEN
        RAISE EXCEPTION 'received asset file %/%: the file set is not rewritten (#1574)', OLD.received_asset_id, OLD."position"
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'pending' AND NEW.state = 'verified' THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = OLD.received_asset_id AND state = 'pending') THEN
            RAISE EXCEPTION 'received asset file %/%: its record is not pending (#1574)', OLD.received_asset_id, OLD."position"
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.state = ANY (ARRAY['pending'::text, 'verified'::text]) AND NEW.state = 'withdrawn'
       AND NEW.verified_at IS NOT DISTINCT FROM OLD.verified_at
       AND NEW.detected_media_type IS NOT DISTINCT FROM OLD.detected_media_type
       AND NEW.detected_by IS NOT DISTINCT FROM OLD.detected_by THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = OLD.received_asset_id AND state = 'withdrawn') THEN
            RAISE EXCEPTION 'received asset file %/%: its record is not withdrawn (#1575)', OLD.received_asset_id, OLD."position"
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'received asset file %/%: only a pending original is updated, to verified, or an original withdrawn with its record (#1574, #1575)', OLD.received_asset_id, OLD."position"
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- The 00090 sets and shapes again, for new writes. NOT VALID: a record,
-- file or ledger row that already says withdrawn or deaccession keeps what
-- it says happened; only new rows are held to the old rules. Nothing is
-- dropped (see the end).
ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_file_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_file_check CHECK (
        (file_position IS NULL AND file_role IS NULL AND object_hash IS NULL AND declared_media_type IS NULL
         AND expected_sha256 IS NULL AND expected_size_bytes IS NULL AND event_type = 'ingestion'::text)
        OR (file_position IS NOT NULL AND file_role IS NOT NULL AND object_hash IS NOT NULL
            AND declared_media_type IS NOT NULL AND expected_sha256 IS NOT NULL AND expected_size_bytes IS NOT NULL
            AND expected_sha256 = object_hash AND file_position >= 0 AND file_position < 256
            AND file_role = ANY (ARRAY['original'::text, 'preservation'::text, 'access'::text])
            AND event_type <> 'ingestion'::text)) NOT VALID;

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_reason_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text]))) NOT VALID;

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_state_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_state_check CHECK (record_state_after = CASE outcome
        WHEN 'success' THEN 'complete'::text WHEN 'failure' THEN 'rejected'::text ELSE 'pending'::text END) NOT VALID;

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_event_type_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_event_type_check CHECK (event_type = ANY (ARRAY['transfer'::text, 'fixity-check'::text, 'ingestion'::text])) NOT VALID;

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_detected_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_detected_check CHECK (
        ((detected_media_type IS NULL) = (detected_by IS NULL))
        AND (detected_media_type IS NULL OR (role = 'original'::text AND state = 'verified'::text
             AND octet_length(detected_media_type) >= 3 AND octet_length(detected_media_type) <= 255
             AND octet_length(detected_by) >= 1 AND octet_length(detected_by) <= 128))) NOT VALID;

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_state_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_state_check CHECK (state = ANY (ARRAY['pending'::text, 'verified'::text, 'not_transferred'::text])) NOT VALID;

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_role_state_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_role_state_check CHECK (CASE role
        WHEN 'original' THEN state = ANY (ARRAY['pending'::text, 'verified'::text])
        ELSE state = 'not_transferred'::text
    END) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text])) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_shape_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_shape_check CHECK (CASE state
        WHEN 'complete' THEN reason IS NULL AND completed_at IS NOT NULL AND authorizing_grant_id IS NOT NULL
        WHEN 'rejected' THEN reason IS NOT NULL AND completed_at IS NULL AND authorizing_grant_id IS NULL
        ELSE completed_at IS NULL AND authorizing_grant_id IS NULL
    END) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_state_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_state_check CHECK (state = ANY (ARRAY['pending'::text, 'complete'::text, 'rejected'::text])) NOT VALID;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_assets_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state = 'complete' THEN
        RAISE EXCEPTION 'received asset %: a complete record is not changed (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state = 'rejected' THEN
        RAISE EXCEPTION 'received asset %: a rejected record is not changed (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.peer_id <> OLD.peer_id OR NEW.activity_uri <> OLD.activity_uri OR NEW.actor_uri <> OLD.actor_uri
       OR NEW.origin_asset_uri <> OLD.origin_asset_uri OR NEW.origin_asset_id <> OLD.origin_asset_id
       OR NEW.version_uri <> OLD.version_uri OR NEW.version_number <> OLD.version_number
       OR NEW.file_set_sha256 <> OLD.file_set_sha256 OR NEW.record <> OLD.record
       OR NEW.sensitivity <> OLD.sensitivity OR NEW.signer_key_id <> OLD.signer_key_id
       OR NEW.signer_actor_uri IS DISTINCT FROM OLD.signer_actor_uri OR NEW.received_at <> OLD.received_at
       OR NEW.created_xid <> OLD.created_xid THEN
        RAISE EXCEPTION 'received asset %: only its state, reason, attempts and commit are updated (#1574)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_asset_files_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.received_asset_id <> OLD.received_asset_id OR NEW."position" <> OLD."position" OR NEW.role <> OLD.role
       OR NEW.object_hash <> OLD.object_hash OR NEW.size_bytes <> OLD.size_bytes OR NEW.checksums <> OLD.checksums
       OR NEW.declared_media_type <> OLD.declared_media_type
       OR NEW.declared_original_filename IS DISTINCT FROM OLD.declared_original_filename
       OR OLD.state <> 'pending' OR NEW.state <> 'verified' THEN
        RAISE EXCEPTION 'received asset file %/%: only a pending original is updated, to verified (#1574)', OLD.received_asset_id, OLD."position"
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = OLD.received_asset_id AND state = 'pending') THEN
        RAISE EXCEPTION 'received asset file %/%: its record is not pending (#1574)', OLD.received_asset_id, OLD."position"
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_verified_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_verified_check CHECK ((state = 'verified'::text) = (verified_at IS NOT NULL)) NOT VALID;

-- The withdrawn_at column, federation_received_tombstones and
-- federation_asset_announcement_claims are kept, as data: the old code
-- ignores them, and a later Up reuses them (IF NOT EXISTS), so neither a
-- withdrawal received nor what was announced is forgotten by a round
-- trip.
