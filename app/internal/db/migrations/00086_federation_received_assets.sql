-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00086_federation_received_assets.sql
--
-- #1574 (receiver core): an asset record a paired peer announced under
-- archival-1 (spec v1 §17.2) and whether its original files arrived
-- intact (§17.6), on the receiving instance.
--
-- federation_received_assets: one row per announcing activity. A record
-- is the sender's claims (the aa:asset member as received, kept as data,
-- never trusted) plus what THIS instance established:
--   pending   the announcement was accepted and its original files are
--             being fetched, or a transient failure (outage, timeout)
--             left it to be resumed by a later delivery; holds no pin
--   complete  every original file was fetched under a live inbound grant
--             (#1571), verified here (size and every listed checksum), and
--             pinned under this record in ONE transaction that re-checked
--             the peer and the grant; authorizing_grant_id names that grant
--   rejected  refused, with a closed reason; holds no pin
-- A complete record is held and verified, not shown: no assets row is
-- created and no page or API reads this table yet (materialising a
-- received record as a visible asset is a later #1574 slice).
--
-- At most one live (pending or complete) record per (peer, origin asset):
-- version lifecycle is #1575, so a second version id is refused
-- (version_unsupported) and the same version id with another file set is
-- refused (version_conflict, §17.2.1). A rejected record never blocks a
-- later announcement. The same activity delivered again finds its own row
-- (UNIQUE (peer_id, activity_uri)).
--
-- federation_received_asset_files: the announced file set, one row per
-- entry. Only original files are fetched (§8.5 serves the original class
-- only; derivatives are #1597); preservation and access entries are kept
-- as declared claims, state not_transferred, and never described as
-- fixity-checked. declared_* columns are the sender's claims.
--
-- The file set is immutable: its rows are inserted only by the
-- transaction that created the record (created_xid) and deleted only
-- with the record; the only change is a pending original becoming
-- verified.
--
-- Pins: a complete record pins each original under
-- ('federation_received_asset', <record id>). The deferred commit check
-- refuses a commit that leaves a complete record with an original that is
-- unverified or unpinned, or a pending or rejected record with any pin or
-- verified file. Deleting a record (directly or with its peer) releases
-- its pins and schedules objects left unpinned for GC, like
-- asset_version_release_pins (00084).
--
-- authorizing_grant_id is a snapshot of federation_inbound_grants.id,
-- not a foreign key: the evidence that a grant authorised the commit
-- outlives the grant row, and both rows go with their peer.

-- +goose Up

CREATE TABLE public.federation_received_assets (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    peer_id uuid NOT NULL,
    activity_uri text NOT NULL,
    actor_uri text NOT NULL,
    origin_asset_uri text NOT NULL,
    origin_asset_id uuid NOT NULL,
    version_uri text NOT NULL,
    version_number bigint NOT NULL,
    file_set_sha256 text NOT NULL,
    record jsonb NOT NULL,
    sensitivity text NOT NULL,
    signer_key_id text NOT NULL,
    signer_actor_uri text,
    state text DEFAULT 'pending'::text NOT NULL,
    reason text,
    attempts integer DEFAULT 0 NOT NULL,
    authorizing_grant_id uuid,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    completed_at timestamp with time zone,
    created_xid xid8 DEFAULT pg_current_xact_id() NOT NULL,
    CONSTRAINT federation_received_assets_pkey PRIMARY KEY (id),
    CONSTRAINT federation_received_assets_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_received_assets_state_check CHECK (state = ANY (ARRAY['pending'::text, 'complete'::text, 'rejected'::text])),
    -- complete: no reason, a commit time and the grant; rejected: a
    -- reason; pending: possibly the last transient reason.
    CONSTRAINT federation_received_assets_state_shape_check CHECK (CASE state
        WHEN 'complete' THEN reason IS NULL AND completed_at IS NOT NULL AND authorizing_grant_id IS NOT NULL
        WHEN 'rejected' THEN reason IS NOT NULL AND completed_at IS NULL AND authorizing_grant_id IS NULL
        ELSE completed_at IS NULL AND authorizing_grant_id IS NULL
    END),
    CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text])),
    CONSTRAINT federation_received_assets_sensitivity_check CHECK (sensitivity = ANY (ARRAY['public'::text, 'team'::text, 'restricted'::text, 'embargo'::text])),
    -- Only public and team records are ever held (§7.5.7.4: v1 has no
    -- encrypted file transport).
    CONSTRAINT federation_received_assets_held_tier_check CHECK (state = 'rejected'::text OR sensitivity = ANY (ARRAY['public'::text, 'team'::text])),
    CONSTRAINT federation_received_assets_version_number_check CHECK (version_number >= 1 AND version_number <= 9007199254740991),
    CONSTRAINT federation_received_assets_file_set_check CHECK (file_set_sha256 ~ '^[0-9a-f]{64}$'::text),
    CONSTRAINT federation_received_assets_attempts_check CHECK (attempts >= 0),
    CONSTRAINT federation_received_assets_record_check CHECK (jsonb_typeof(record) = 'object'::text),
    CONSTRAINT federation_received_assets_activity_key UNIQUE (peer_id, activity_uri)
);

CREATE UNIQUE INDEX federation_received_assets_live_key
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri)
    WHERE (state <> 'rejected'::text);

CREATE INDEX federation_received_assets_peer_asset_idx
    ON public.federation_received_assets USING btree (peer_id, origin_asset_uri, received_at DESC);

COMMENT ON TABLE public.federation_received_assets IS 'An aa:asset record a paired peer announced under archival-1 (#1574, spec v1 §17.2, §17.6), one row per announcing activity. complete = every original fetched under a live inbound grant, verified here and pinned under this record in one transaction; pending and rejected hold no pin. Held, not shown: no assets row, no reader yet. record and declared_* values are the sender''s untrusted claims.';

CREATE TABLE public.federation_received_asset_files (
    received_asset_id uuid NOT NULL,
    "position" integer NOT NULL,
    role text NOT NULL,
    object_hash text NOT NULL,
    size_bytes bigint NOT NULL,
    checksums jsonb NOT NULL,
    declared_media_type text NOT NULL,
    declared_original_filename text,
    state text DEFAULT 'pending'::text NOT NULL,
    verified_at timestamp with time zone,
    CONSTRAINT federation_received_asset_files_pkey PRIMARY KEY (received_asset_id, "position"),
    CONSTRAINT federation_received_asset_files_received_asset_id_fkey FOREIGN KEY (received_asset_id) REFERENCES public.federation_received_assets(id) ON DELETE CASCADE,
    CONSTRAINT federation_received_asset_files_position_check CHECK ("position" >= 0 AND "position" < 256),
    CONSTRAINT federation_received_asset_files_role_check CHECK (role = ANY (ARRAY['original'::text, 'preservation'::text, 'access'::text])),
    CONSTRAINT federation_received_asset_files_hash_check CHECK (object_hash ~ '^[0-9a-f]{64}$'::text),
    CONSTRAINT federation_received_asset_files_size_check CHECK (size_bytes >= 0 AND size_bytes <= 9007199254740991),
    CONSTRAINT federation_received_asset_files_checksums_check CHECK (jsonb_typeof(checksums) = 'array'::text),
    CONSTRAINT federation_received_asset_files_media_type_check CHECK (octet_length(declared_media_type) >= 3 AND octet_length(declared_media_type) <= 255),
    CONSTRAINT federation_received_asset_files_filename_check CHECK (declared_original_filename IS NULL OR (
        octet_length(declared_original_filename) >= 1 AND octet_length(declared_original_filename) <= 255
        AND declared_original_filename <> ALL (ARRAY['.'::text, '..'::text])
        AND strpos(declared_original_filename, '/'::text) = 0 AND strpos(declared_original_filename, '\'::text) = 0
        AND declared_original_filename !~ '[\x01-\x1f\x7f]'::text)),
    CONSTRAINT federation_received_asset_files_state_check CHECK (state = ANY (ARRAY['pending'::text, 'verified'::text, 'not_transferred'::text])),
    -- Originals are fetched and verified; other roles are claims only.
    CONSTRAINT federation_received_asset_files_role_state_check CHECK (CASE role
        WHEN 'original' THEN state = ANY (ARRAY['pending'::text, 'verified'::text])
        ELSE state = 'not_transferred'::text
    END),
    CONSTRAINT federation_received_asset_files_verified_check CHECK ((state = 'verified'::text) = (verified_at IS NOT NULL))
);

CREATE INDEX federation_received_asset_files_hash_idx
    ON public.federation_received_asset_files USING btree (object_hash);

COMMENT ON TABLE public.federation_received_asset_files IS 'The file set of a received asset record (#1574, spec v1 §17.2.2). Originals are fetched and verified (state verified only on a complete record); preservation and access entries are declared claims (not_transferred, #1597). declared_* are untrusted sender claims, never a path.';

-- +goose StatementBegin
-- Release the pins of removed records, scheduling every object left
-- without a pin for GC (storage.Service.RemovePin semantics; the same
-- lock order as asset_version_release_pins).
CREATE FUNCTION public.federation_received_asset_release_pins(p_record_ids text[]) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_grace interval := COALESCE(NULLIF(current_setting('aa.storage_gc_grace', true), '')::interval, interval '24 hours');
    v_hashes text[];
BEGIN
    SELECT array_agg(DISTINCT object_hash ORDER BY object_hash) INTO v_hashes
      FROM public.storage_pins
     WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = ANY (p_record_ids);
    IF v_hashes IS NULL THEN
        RETURN;
    END IF;
    PERFORM 1 FROM public.storage_objects WHERE hash = ANY (v_hashes) ORDER BY hash FOR UPDATE;
    DELETE FROM public.storage_pins
     WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = ANY (p_record_ids);
    UPDATE public.storage_objects o
       SET gc_eligible_at = now() + v_grace
     WHERE o.hash = ANY (v_hashes)
       AND NOT EXISTS (SELECT 1 FROM public.storage_pins p WHERE p.object_hash = o.hash);
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION public.federation_received_assets_after_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.federation_received_asset_release_pins(ARRAY[OLD.id::text]);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- TRUNCATE fires no row triggers: release every record pin whose record
-- no longer exists.
CREATE FUNCTION public.federation_received_assets_after_truncate() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.federation_received_asset_release_pins(ARRAY(
        SELECT p.pin_subject_id FROM public.storage_pins p
         WHERE p.pin_subject_type = 'federation_received_asset'
           AND NOT EXISTS (SELECT 1 FROM public.federation_received_assets r WHERE r.id::text = p.pin_subject_id)));
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The commit-time invariant of one record: a complete record has at
-- least one original and every original verified and pinned under it
-- at its size; a pending or rejected record holds no pin and no verified
-- file. A record that no longer exists is not checked (its pins went
-- with it, federation_received_asset_release_pins).
CREATE FUNCTION public.federation_received_asset_require_consistent(p_record uuid) RETURNS void
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

-- +goose StatementBegin
-- Deferred, on the record.
CREATE FUNCTION public.federation_received_assets_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.federation_received_asset_require_consistent(NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Deferred, on a record pin: a pin is added only by the completing
-- transaction and removed only with its record. A pin that does not
-- name an existing record is refused.
CREATE FUNCTION public.storage_pins_received_asset_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP IN ('INSERT', 'UPDATE') AND NEW.pin_subject_type = 'federation_received_asset' THEN
        IF NEW.pin_subject_id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           OR NOT EXISTS (SELECT 1 FROM public.federation_received_assets WHERE id = NEW.pin_subject_id::uuid) THEN
            RAISE EXCEPTION 'storage pin %: a federation_received_asset pin names an existing record (#1574)', NEW.pin_subject_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        PERFORM public.federation_received_asset_require_consistent(NEW.pin_subject_id::uuid);
    END IF;
    IF TG_OP IN ('UPDATE', 'DELETE') AND OLD.pin_subject_type = 'federation_received_asset'
       AND OLD.pin_subject_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        PERFORM public.federation_received_asset_require_consistent(OLD.pin_subject_id::uuid);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- A complete record is final: its claims, file set and grant are never
-- rewritten, and it never goes back to pending or rejected.
CREATE FUNCTION public.federation_received_assets_update_check() RETURNS trigger
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
-- The file set is the record's claim and is never edited: its rows are
-- written only by the transaction that created the record (created_xid),
-- and removed only as the record goes (the FK cascade runs after the
-- record row is gone; a TRUNCATE fires no row trigger).
CREATE FUNCTION public.federation_received_asset_files_write_check() RETURNS trigger
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
-- The file set is written with its record and changes only by being
-- verified while the record is pending.
CREATE FUNCTION public.federation_received_asset_files_update_check() RETURNS trigger
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

CREATE TRIGGER federation_received_assets_update_check_trg BEFORE UPDATE ON public.federation_received_assets
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_assets_update_check();
CREATE TRIGGER federation_received_asset_files_update_check_trg BEFORE UPDATE ON public.federation_received_asset_files
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_asset_files_update_check();
CREATE TRIGGER federation_received_asset_files_write_check_trg BEFORE INSERT OR DELETE ON public.federation_received_asset_files
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_asset_files_write_check();
CREATE CONSTRAINT TRIGGER federation_received_assets_commit_check_trg AFTER INSERT OR UPDATE ON public.federation_received_assets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.federation_received_assets_commit_check();
CREATE CONSTRAINT TRIGGER storage_pins_received_asset_insert_check_trg AFTER INSERT ON public.storage_pins
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN ((new.pin_subject_type = 'federation_received_asset'::text))
    EXECUTE FUNCTION public.storage_pins_received_asset_commit_check();
CREATE CONSTRAINT TRIGGER storage_pins_received_asset_update_check_trg AFTER UPDATE ON public.storage_pins
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (((old.pin_subject_type = 'federation_received_asset'::text) OR (new.pin_subject_type = 'federation_received_asset'::text)))
    EXECUTE FUNCTION public.storage_pins_received_asset_commit_check();
CREATE CONSTRAINT TRIGGER storage_pins_received_asset_delete_check_trg AFTER DELETE ON public.storage_pins
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN ((old.pin_subject_type = 'federation_received_asset'::text))
    EXECUTE FUNCTION public.storage_pins_received_asset_commit_check();
CREATE TRIGGER federation_received_assets_after_delete_trg AFTER DELETE ON public.federation_received_assets
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_assets_after_delete();
CREATE TRIGGER federation_received_assets_truncate_trg AFTER TRUNCATE ON public.federation_received_assets
    FOR EACH STATEMENT EXECUTE FUNCTION public.federation_received_assets_after_truncate();

-- +goose Down

DROP TRIGGER IF EXISTS storage_pins_received_asset_delete_check_trg ON public.storage_pins;
DROP TRIGGER IF EXISTS storage_pins_received_asset_update_check_trg ON public.storage_pins;
DROP TRIGGER IF EXISTS storage_pins_received_asset_insert_check_trg ON public.storage_pins;
DROP TRIGGER IF EXISTS federation_received_assets_truncate_trg ON public.federation_received_assets;
DROP TRIGGER IF EXISTS federation_received_assets_after_delete_trg ON public.federation_received_assets;
DROP TRIGGER IF EXISTS federation_received_assets_commit_check_trg ON public.federation_received_assets;
DROP TRIGGER IF EXISTS federation_received_asset_files_write_check_trg ON public.federation_received_asset_files;
DROP TRIGGER IF EXISTS federation_received_asset_files_update_check_trg ON public.federation_received_asset_files;
DROP TRIGGER IF EXISTS federation_received_assets_update_check_trg ON public.federation_received_assets;
-- Release every record pin (scheduling GC for objects left unpinned),
-- then drop the tables.
SELECT public.federation_received_asset_release_pins(ARRAY(
    SELECT pin_subject_id FROM public.storage_pins WHERE pin_subject_type = 'federation_received_asset'));
DROP TABLE IF EXISTS public.federation_received_asset_files;
DROP TABLE IF EXISTS public.federation_received_assets;
DROP FUNCTION IF EXISTS public.federation_received_asset_files_write_check();
DROP FUNCTION IF EXISTS public.federation_received_asset_files_update_check();
DROP FUNCTION IF EXISTS public.federation_received_assets_update_check();
DROP FUNCTION IF EXISTS public.storage_pins_received_asset_commit_check();
DROP FUNCTION IF EXISTS public.federation_received_assets_commit_check();
DROP FUNCTION IF EXISTS public.federation_received_asset_require_consistent(uuid);
DROP FUNCTION IF EXISTS public.federation_received_assets_after_truncate();
DROP FUNCTION IF EXISTS public.federation_received_assets_after_delete();
DROP FUNCTION IF EXISTS public.federation_received_asset_release_pins(text[]);
