-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00088_federation_received_asset_view.sql
--
-- #1574 (slice 1, the received-asset view): what THIS instance found the
-- verified bytes of a received original to be, beside what the sender
-- declared.
--
-- federation_received_asset_files.detected_media_type / detected_by: the
-- receiver's own identification of an original's verified bytes, and the
-- tool that made it. declared_media_type stays the sender's claim. Both are
-- written in the transaction that completes the record (the 00086 update
-- guard already limits a file row's only change to pending -> verified, so
-- they cannot change afterwards), and a complete record's originals must
-- carry them: federation_received_asset_require_consistent is replaced
-- below with that one added rule. Non-original rows are claims only and are
-- never identified.
--
-- Complete records written before this migration have originals without a
-- detection; they are not re-checked (a complete record never changes) and
-- the view reports their type as not identified.

-- +goose Up

ALTER TABLE public.federation_received_asset_files
    ADD COLUMN detected_media_type text,
    ADD COLUMN detected_by text,
    ADD CONSTRAINT federation_received_asset_files_detected_check CHECK (
        ((detected_media_type IS NULL) = (detected_by IS NULL))
        AND (detected_media_type IS NULL OR (role = 'original'::text AND state = 'verified'::text
             AND octet_length(detected_media_type) >= 3 AND octet_length(detected_media_type) <= 255
             AND octet_length(detected_by) >= 1 AND octet_length(detected_by) <= 128)));

COMMENT ON COLUMN public.federation_received_asset_files.detected_media_type IS 'The receiver''s own identification of a verified original''s bytes (#1574), written when the record completes; declared_media_type is the sender''s claim.';
COMMENT ON COLUMN public.federation_received_asset_files.detected_by IS 'The tool that produced detected_media_type (#1574).';

-- +goose StatementBegin
-- 00086's consistency rule, plus: a complete record's originals are
-- identified.
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
    IF r.state = 'complete' THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND role = 'original') THEN
            RAISE EXCEPTION 'received asset %: complete without an original file (#1574)', r.id
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
            RAISE EXCEPTION 'received asset %: complete with an original that is not verified and pinned (#1574)', r.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM public.federation_received_asset_files f
                    WHERE f.received_asset_id = r.id AND f.role = 'original' AND f.detected_media_type IS NULL) THEN
            RAISE EXCEPTION 'received asset %: complete with an original that was not identified (#1574)', r.id
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

-- +goose Down

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
    IF r.state = 'complete' THEN
        IF NOT EXISTS (SELECT 1 FROM public.federation_received_asset_files WHERE received_asset_id = r.id AND role = 'original') THEN
            RAISE EXCEPTION 'received asset %: complete without an original file (#1574)', r.id
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
            RAISE EXCEPTION 'received asset %: complete with an original that is not verified and pinned (#1574)', r.id
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

ALTER TABLE public.federation_received_asset_files
    DROP CONSTRAINT IF EXISTS federation_received_asset_files_detected_check,
    DROP COLUMN IF EXISTS detected_by,
    DROP COLUMN IF EXISTS detected_media_type;
