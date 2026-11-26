-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00104_federation_received_access_copy_outcomes.sql
--
-- #1597 (spec v1 §16.7, §17.2.1): the receiver's outcome for each access
-- copy a received version declared. Until now every access entry of a
-- received file set (00086) was a claim only, not_transferred. The
-- existing rows are reused; no row is added or removed:
--
--   not_transferred, no reason   unattempted (every row written before
--                                this migration reads so)
--   not_transferred, a reason    the last attempt failed; the reason is
--                                one of the bounded transfer reasons
--                                (access_failure_reason)
--   verified                     the copy was fetched under a live
--                                access-copy grant, verified here and
--                                pinned under its record
--   withdrawn                    a verified copy whose record was
--                                withdrawn (§17.3); it keeps verified_at
--
-- An access row records a reason or becomes verified only while its exact
-- record is the asset's current complete version, and none of its declared
-- facts (position, role, hash, length, checksums, media type, file name)
-- ever change. verified is assigned once: nothing rewrites it or its
-- verified_at, and it leaves only to withdrawn with its record. Original
-- states, the 00092 record checks (originals verified, pinned and
-- identified; supersession and predecessors) and preservation entries
-- (claims only, not_transferred) are unchanged. A record's completion
-- never waits for an access copy.
--
-- Pins: a verified access copy is pinned under its record, as an
-- original is (pin_subject_type federation_received_asset, the record id).
-- The record's commit check now also requires that pin, at the stored
-- size, for every verified access row of a complete or superseded record;
-- a deferred check queued by an access row's move into verified enforces
-- it when only that row changed (for example when the record already holds
-- a pin of the same hash through an original, or another owner pins it).
-- Nothing else queues it: an inserted claim cannot be verified
-- (federation_received_asset_files_write_check), a recorded failure holds
-- no pin, and a withdrawal is checked through its record and pins. Release on
-- withdrawal, delete and truncate is unchanged: it releases every pin the
-- record owns, access copies included, one pin per hash, and never a pin
-- of another owner.

-- +goose Up

ALTER TABLE public.federation_received_asset_files ADD COLUMN IF NOT EXISTS access_failure_reason text;

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT IF EXISTS federation_received_asset_files_access_reason_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_access_reason_check CHECK (
        access_failure_reason IS NULL OR (role = 'access'::text AND state = 'not_transferred'::text AND access_failure_reason = ANY (ARRAY[
        'invalid_request'::text, 'peer_unavailable'::text, 'profile_not_negotiated'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'class_not_available'::text, 'sensitivity_refused'::text, 'too_large'::text,
        'no_inbound_grant'::text, 'origin_refused'::text, 'redirect_refused'::text, 'http_status'::text,
        'length_mismatch'::text, 'fixity_mismatch'::text, 'timeout'::text, 'transport_error'::text,
        'revoked_during_fetch'::text, 'lease_lost'::text, 'storage_error'::text])));

COMMENT ON COLUMN public.federation_received_asset_files.access_failure_reason IS 'Why the last attempt to receive this access copy failed (#1597, spec v1 §16.7): a bounded transfer reason, only on a not_transferred access row. NULL: unattempted (every row written before 00104), or not an access row.';

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_role_state_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_role_state_check CHECK (CASE role
        WHEN 'original' THEN state = ANY (ARRAY['pending'::text, 'verified'::text, 'withdrawn'::text])
        WHEN 'access' THEN state = ANY (ARRAY['not_transferred'::text, 'verified'::text, 'withdrawn'::text])
        ELSE state = 'not_transferred'::text
    END);

-- A withdrawn access row was verified, and keeps when.
ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT IF EXISTS federation_received_asset_files_access_withdrawn_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_access_withdrawn_check CHECK (
        role <> 'access'::text OR state <> 'withdrawn'::text OR verified_at IS NOT NULL);

-- +goose StatementBegin
-- The file set is the record's claim and is never edited: its rows are
-- written only by the transaction that created the record (created_xid),
-- an access row as an unattempted claim (00104), and removed only as the
-- record goes (the FK cascade runs after the record row is gone; a
-- TRUNCATE fires no row trigger).
CREATE OR REPLACE FUNCTION public.federation_received_asset_files_write_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_assets
                        WHERE id = NEW.received_asset_id AND created_xid = pg_current_xact_id()) THEN
            RAISE EXCEPTION 'received asset file %/%: the file set is written only by the transaction that created its record (#1574)', NEW.received_asset_id, NEW."position"
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.role = 'access' AND (NEW.state <> 'not_transferred' OR NEW.access_failure_reason IS NOT NULL OR NEW.verified_at IS NOT NULL) THEN
            RAISE EXCEPTION 'received asset file %/%: an access copy is written as an unattempted claim (#1597)', NEW.received_asset_id, NEW."position"
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = OLD.received_asset_id) THEN
        RAISE EXCEPTION 'received asset file %/%: the file set is removed only with its record (#1574)', OLD.received_asset_id, OLD."position"
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The file set is written with its record and changes only by an original
-- being verified while the record is pending, by an original or a verified
-- access copy being withdrawn with its record (keeping when it was
-- verified and identified), or (00104) by an access copy of the asset's
-- current complete version recording a failure or being verified, once.
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
    IF OLD.role = 'access' AND OLD.state = 'not_transferred'
       AND NEW.detected_media_type IS NULL AND NEW.detected_by IS NULL
       AND ((NEW.state = 'not_transferred' AND NEW.access_failure_reason IS NOT NULL AND NEW.verified_at IS NULL)
            OR (NEW.state = 'verified' AND NEW.access_failure_reason IS NULL AND NEW.verified_at IS NOT NULL)) THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = OLD.received_asset_id AND state = 'complete') THEN
            RAISE EXCEPTION 'received asset file %/%: an access copy is recorded only for the current complete version (#1597)', OLD.received_asset_id, OLD."position"
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'received asset file %/%: only a pending original is updated, to verified, an original or verified access copy withdrawn with its record, or an unverified access copy of the current version recorded (#1574, #1575, #1597)', OLD.received_asset_id, OLD."position"
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The commit-time invariant of one record: a complete or superseded
-- record has at least one original and every original verified, pinned
-- under it at its size and identified, every verified access copy pinned
-- under it at its size (00104), and a superseded one has its successor; a
-- pending, rejected or withdrawn record holds no pin and no verified file.
-- A record that no longer exists is not checked.
CREATE OR REPLACE FUNCTION public.federation_received_asset_require_consistent(p_record uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    SELECT id, state INTO r FROM public.federation_received_assets WHERE id = p_record;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF r.state = ANY (ARRAY['complete'::text, 'superseded'::text]) THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND role = 'original') THEN
            RAISE EXCEPTION 'received asset %: % without an original file (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (
            SELECT 1 FROM public.federation_received_asset_files f
             WHERE f.received_asset_id = r.id AND f.role = 'original'
               AND (f.state <> 'verified' OR NOT EXISTS (
                    SELECT 1 FROM public.storage_pins p
                      JOIN public.storage_objects o ON o.hash = p.object_hash
                     WHERE p.pin_subject_type = 'federation_received_asset' AND p.pin_subject_id = r.id::text
                       AND p.object_hash = f.object_hash AND o.size_bytes = f.size_bytes))) THEN
            RAISE EXCEPTION 'received asset %: % with an original that is not verified and pinned (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM public.federation_received_asset_files f
                    WHERE f.received_asset_id = r.id AND f.role = 'original' AND f.detected_media_type IS NULL) THEN
            RAISE EXCEPTION 'received asset %: % with an original that was not identified (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (
            SELECT 1 FROM public.federation_received_asset_files f
             WHERE f.received_asset_id = r.id AND f.role = 'access' AND f.state = 'verified'
               AND NOT EXISTS (
                    SELECT 1 FROM public.storage_pins p
                      JOIN public.storage_objects o ON o.hash = p.object_hash
                     WHERE p.pin_subject_type = 'federation_received_asset' AND p.pin_subject_id = r.id::text
                       AND p.object_hash = f.object_hash AND o.size_bytes = f.size_bytes)) THEN
            RAISE EXCEPTION 'received asset %: % with a verified access copy that is not pinned (#1597)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF r.state = 'superseded' AND NOT EXISTS (
            SELECT 1 FROM public.federation_received_assets s
             WHERE s.predecessor_id = r.id AND s.state = ANY (ARRAY['complete'::text, 'superseded'::text, 'withdrawn'::text])) THEN
            RAISE EXCEPTION 'received asset %: superseded without a successor (#1575)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM public.storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = r.id::text)
       OR EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND state = 'verified') THEN
        RAISE EXCEPTION 'received asset %: % but holding a pin or a verified file (#1574)', r.id, r.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Deferred, on an access row becoming verified (00104): a transaction that
-- changes only that row (neither its record nor a pin row) is still held
-- to its record's commit check.
CREATE OR REPLACE FUNCTION public.federation_received_asset_files_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.federation_received_asset_require_consistent(NEW.received_asset_id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_asset_files_commit_check_trg ON public.federation_received_asset_files;
CREATE CONSTRAINT TRIGGER federation_received_asset_files_commit_check_trg AFTER UPDATE ON public.federation_received_asset_files
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (((new.role = 'access'::text) AND (old.state <> 'verified'::text) AND (new.state = 'verified'::text)))
    EXECUTE FUNCTION public.federation_received_asset_files_commit_check();

-- +goose Down

-- The 00103 schema holds an access entry only as a not_transferred claim.
-- A copy this instance received (verified, or withdrawn with its
-- verification history) cannot be represented there, so Down refuses
-- while one exists rather than discard it.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE role = 'access' AND state <> 'not_transferred') THEN
        RAISE EXCEPTION '00104 down: a received access copy is held or was withdrawn with its history; the 00103 schema keeps access entries as claims only (#1597)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_asset_files_commit_check_trg ON public.federation_received_asset_files;
DROP FUNCTION IF EXISTS public.federation_received_asset_files_commit_check();

ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT IF EXISTS federation_received_asset_files_access_withdrawn_check;
ALTER TABLE public.federation_received_asset_files DROP CONSTRAINT federation_received_asset_files_role_state_check;
ALTER TABLE public.federation_received_asset_files ADD CONSTRAINT federation_received_asset_files_role_state_check CHECK (CASE role
        WHEN 'original' THEN state = ANY (ARRAY['pending'::text, 'verified'::text, 'withdrawn'::text])
        ELSE state = 'not_transferred'::text
    END);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_asset_files_write_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_assets
                        WHERE id = NEW.received_asset_id AND created_xid = pg_current_xact_id()) THEN
            RAISE EXCEPTION 'received asset file %/%: the file set is written only by the transaction that created its record (#1574)', NEW.received_asset_id, NEW."position"
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = OLD.received_asset_id) THEN
        RAISE EXCEPTION 'received asset file %/%: the file set is removed only with its record (#1574)', OLD.received_asset_id, OLD."position"
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_asset_require_consistent(p_record uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    r record;
BEGIN
    SELECT id, state INTO r FROM public.federation_received_assets WHERE id = p_record;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    IF r.state = ANY (ARRAY['complete'::text, 'superseded'::text]) THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND role = 'original') THEN
            RAISE EXCEPTION 'received asset %: % without an original file (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (
            SELECT 1 FROM public.federation_received_asset_files f
             WHERE f.received_asset_id = r.id AND f.role = 'original'
               AND (f.state <> 'verified' OR NOT EXISTS (
                    SELECT 1 FROM public.storage_pins p
                      JOIN public.storage_objects o ON o.hash = p.object_hash
                     WHERE p.pin_subject_type = 'federation_received_asset' AND p.pin_subject_id = r.id::text
                       AND p.object_hash = f.object_hash AND o.size_bytes = f.size_bytes))) THEN
            RAISE EXCEPTION 'received asset %: % with an original that is not verified and pinned (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM public.federation_received_asset_files f
                    WHERE f.received_asset_id = r.id AND f.role = 'original' AND f.detected_media_type IS NULL) THEN
            RAISE EXCEPTION 'received asset %: % with an original that was not identified (#1574)', r.id, r.state
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF r.state = 'superseded' AND NOT EXISTS (
            SELECT 1 FROM public.federation_received_assets s
             WHERE s.predecessor_id = r.id AND s.state = ANY (ARRAY['complete'::text, 'superseded'::text, 'withdrawn'::text])) THEN
            RAISE EXCEPTION 'received asset %: superseded without a successor (#1575)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM public.storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = r.id::text)
       OR EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND state = 'verified') THEN
        RAISE EXCEPTION 'received asset %: % but holding a pin or a verified file (#1574)', r.id, r.state
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

-- access_failure_reason and its CHECK are kept, as data: every remaining
-- access row is a not_transferred claim, the 00103 code neither reads nor
-- changes them, and a later Up reuses them (IF NOT EXISTS), so a recorded
-- failure is not forgotten by a round trip.
