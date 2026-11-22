-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00093_federation_version_gaps.sql
--
-- #1575 (spec v1 §17.2.1): a version that stayed a draft is never
-- announced, so the versions a peer receives may skip its number. A
-- sealed version names as its predecessor the asset's previous SEALED
-- version, and a receiver accepts a successor more than one number above
-- the version it holds when the successor names that version.
--
-- Sending side (asset_versions): only the asset's LATEST version is ever
-- sealed. Once a higher version exists, a lower draft can no longer be
-- sealed, so the set of sealed versions below a version is final when it
-- is created, and the predecessor a sealed version is announced with
-- (the greatest sealed version below it) never changes. Every path that
-- seals already seals the latest version (the version reconciler seals
-- the current version or a successor it just created); this makes it a
-- rule of the database.
--
-- The check serialises with version creation: asset_versions_insert_check
-- (00084) locks the asset row FOR UPDATE before it adds a version, so the
-- seal takes the same lock before it reads whether a higher version
-- exists. asset_version_seal() takes the asset row first and the version
-- row second, the order the version reconciler (asset row, then version
-- row) and a replacement (asset row, then the new version's insert)
-- already use, so a seal and an insert of the same asset wait for each
-- other and never deadlock. The update trigger takes the asset row again
-- (a no-op when it is held) and repeats the check, which covers a seal
-- written without the function. The check reads committed data under
-- READ COMMITTED, the isolation every writer of versions uses.
--
-- Receiving side (federation_received_assets): the chain trigger accepts
-- a predecessor link to a lower-numbered version of the same peer's asset
-- that has the version id the record names, and refuses a link that
-- passes over a version this instance holds between the two.

-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_version_seal(p_version uuid, p_access jsonb DEFAULT '[]'::jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_asset uuid;
    v record;
    e jsonb;
BEGIN
    -- The asset row first (the order version creation uses), then the
    -- version row.
    SELECT asset_id INTO v_asset FROM public.asset_versions WHERE id = p_version;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset version % does not exist (#1596)', p_version USING ERRCODE = 'no_data_found';
    END IF;
    PERFORM 1 FROM public.assets WHERE id = v_asset FOR UPDATE;
    SELECT id, number, sealed_at, original_hash, original_size_bytes, original_media_type INTO v
      FROM public.asset_versions WHERE id = p_version FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset version % does not exist (#1596)', p_version USING ERRCODE = 'no_data_found';
    END IF;
    IF v.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'asset version %: already sealed (#1596)', p_version USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM public.asset_versions WHERE asset_id = v_asset AND number > v.number) THEN
        RAISE EXCEPTION 'asset version %: a later version exists; only the latest version is sealed (#1575)', p_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.storage_variants
                    WHERE object_hash = v.original_hash AND variant_key = 'original' AND fixity_status = 'verified'
                      AND content_sha256 = v.original_hash AND content_size_bytes = v.original_size_bytes) THEN
        RAISE EXCEPTION 'asset version %: the original''s stored bytes are not verified (#1596)', p_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF p_access IS NOT NULL AND jsonb_typeof(p_access) <> 'array' THEN
        RAISE EXCEPTION 'asset version %: access entries are an array (#1596)', p_version
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE public.asset_versions SET sealed_xid = pg_current_xact_id() WHERE id = p_version;
    INSERT INTO public.asset_version_files (version_id, role, object_hash, size_bytes, media_type)
    VALUES (p_version, 'original', v.original_hash, v.original_size_bytes, v.original_media_type);
    FOR e IN SELECT * FROM jsonb_array_elements(COALESCE(p_access, '[]'::jsonb)) LOOP
        INSERT INTO public.asset_version_files (version_id, role, object_hash, size_bytes, media_type, derived_from_hash, derived_variant_key)
        VALUES (p_version, e->>'role', e->>'object_hash', (e->>'size_bytes')::bigint, e->>'media_type',
                e->>'derived_from_hash', e->>'derived_variant_key');
    END LOOP;
    UPDATE public.asset_versions SET sealed_at = now() WHERE id = p_version;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_versions_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (to_jsonb(NEW) - 'sealed_xid' - 'sealed_at' - 'announced_at')
       IS DISTINCT FROM (to_jsonb(OLD) - 'sealed_xid' - 'sealed_at' - 'announced_at') THEN
        RAISE EXCEPTION 'asset version %: a version is immutable (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.sealed_xid IS DISTINCT FROM OLD.sealed_xid
       AND (OLD.sealed_xid IS NOT NULL OR NEW.sealed_xid IS DISTINCT FROM pg_current_xact_id()) THEN
        RAISE EXCEPTION 'asset version %: only the sealing transaction may claim it, once (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.sealed_at IS DISTINCT FROM OLD.sealed_at THEN
        IF OLD.sealed_at IS NOT NULL OR NEW.sealed_at IS NULL OR NEW.sealed_xid IS DISTINCT FROM pg_current_xact_id() THEN
            RAISE EXCEPTION 'asset version %: only the transaction that claimed it may seal it, once (#1596)', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.asset_version_files WHERE version_id = OLD.id AND role = 'original') THEN
            RAISE EXCEPTION 'asset version %: a sealed file set includes its original (#1596)', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        -- Only the latest version is sealed (#1575), checked under the
        -- asset row lock version creation takes (see the file header).
        PERFORM 1 FROM public.assets WHERE id = OLD.asset_id FOR UPDATE;
        IF EXISTS (SELECT 1 FROM public.asset_versions WHERE asset_id = OLD.asset_id AND number > OLD.number) THEN
            RAISE EXCEPTION 'asset version %: a later version exists; only the latest version is sealed (#1575)', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF NEW.announced_at IS DISTINCT FROM OLD.announced_at AND (OLD.announced_at IS NOT NULL OR NEW.sealed_at IS NULL) THEN
        RAISE EXCEPTION 'asset version %: only a sealed version is announced, once (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The predecessor link names the same peer's record of the same asset, a
-- lower version number, with the version id the record must follow, and
-- no version this instance holds of the asset lies between the two.
CREATE OR REPLACE FUNCTION public.federation_received_assets_chain_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    p record;
BEGIN
    IF NEW.predecessor_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT peer_id, origin_asset_uri, version_number, version_uri INTO p
      FROM public.federation_received_assets WHERE id = NEW.predecessor_id;
    IF NOT FOUND OR p.peer_id <> NEW.peer_id OR p.origin_asset_uri <> NEW.origin_asset_uri
       OR p.version_number >= NEW.version_number OR p.version_uri IS DISTINCT FROM NEW.predecessor_version_uri THEN
        RAISE EXCEPTION 'received asset %: % is not an earlier version of the same asset from the same peer (#1575)', NEW.id, NEW.predecessor_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM public.federation_received_assets r
                WHERE r.peer_id = NEW.peer_id AND r.origin_asset_uri = NEW.origin_asset_uri AND r.state <> 'rejected'
                  AND r.version_number > p.version_number AND r.version_number < NEW.version_number) THEN
        RAISE EXCEPTION 'received asset %: a version held between it and its predecessor % is passed over (#1575)', NEW.id, NEW.predecessor_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down

-- Down refuses, before it changes anything, while the data holds what the
-- earlier rules cannot keep:
--   - a local asset with a sealed version above a version that stayed a
--     draft: the 00085 seal could seal that draft later, and the 00092
--     sender would then name it as the predecessor of the sealed version
--     already announced, a different claim for the same version;
--   - a received version that waits to follow a version this instance
--     does not hold: its predecessor's number is unknown, so the 00092
--     code, which looks one number below, could strand a version a
--     sender announced across a gap;
--   - a received version that follows, or waits to follow, a held
--     version more than one number below it: the 00092 chain holds only
--     adjacent numbers.
-- Each is resolved first (the draft's asset deleted, the waiting version
-- received or rejected).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.asset_versions d
                 JOIN public.asset_versions s ON s.asset_id = d.asset_id AND s.number > d.number
                WHERE d.sealed_at IS NULL AND s.sealed_at IS NOT NULL) THEN
        RAISE EXCEPTION '00093 down: a local asset has a sealed version above a version that stayed a draft; the 00085 seal could seal that draft and change what the sealed version follows (#1575)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF EXISTS (SELECT 1 FROM public.federation_received_assets s
                WHERE s.state = 'pending' AND s.predecessor_version_uri IS NOT NULL
                  AND NOT EXISTS (SELECT 1 FROM public.federation_received_assets p
                                   WHERE p.peer_id = s.peer_id AND p.origin_asset_uri = s.origin_asset_uri
                                     AND p.version_uri = s.predecessor_version_uri AND p.state <> 'rejected')) THEN
        RAISE EXCEPTION '00093 down: a received version waits for a predecessor this instance does not hold; its number is unknown, so the 00092 chain cannot be checked (#1575)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF EXISTS (SELECT 1 FROM public.federation_received_assets s
                 JOIN public.federation_received_assets p
                   ON p.peer_id = s.peer_id AND p.origin_asset_uri = s.origin_asset_uri
                  AND p.version_uri = s.predecessor_version_uri AND p.state <> 'rejected'
                WHERE s.state <> 'rejected' AND p.version_number <> s.version_number - 1)
       OR EXISTS (SELECT 1 FROM public.federation_received_assets s
                    JOIN public.federation_received_assets p ON p.id = s.predecessor_id
                   WHERE p.version_number <> s.version_number - 1) THEN
        RAISE EXCEPTION '00093 down: a received version follows a version more than one number below it; the 00092 chain is contiguous (#1575)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_assets_chain_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    p record;
BEGIN
    IF NEW.predecessor_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT peer_id, origin_asset_uri, version_number, version_uri INTO p
      FROM public.federation_received_assets WHERE id = NEW.predecessor_id;
    IF NOT FOUND OR p.peer_id <> NEW.peer_id OR p.origin_asset_uri <> NEW.origin_asset_uri
       OR p.version_number <> NEW.version_number - 1 OR p.version_uri IS DISTINCT FROM NEW.predecessor_version_uri THEN
        RAISE EXCEPTION 'received asset %: % is not the previous version of the same asset from the same peer (#1575)', NEW.id, NEW.predecessor_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_versions_update_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF (to_jsonb(NEW) - 'sealed_xid' - 'sealed_at' - 'announced_at')
       IS DISTINCT FROM (to_jsonb(OLD) - 'sealed_xid' - 'sealed_at' - 'announced_at') THEN
        RAISE EXCEPTION 'asset version %: a version is immutable (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.sealed_xid IS DISTINCT FROM OLD.sealed_xid
       AND (OLD.sealed_xid IS NOT NULL OR NEW.sealed_xid IS DISTINCT FROM pg_current_xact_id()) THEN
        RAISE EXCEPTION 'asset version %: only the sealing transaction may claim it, once (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.sealed_at IS DISTINCT FROM OLD.sealed_at THEN
        IF OLD.sealed_at IS NOT NULL OR NEW.sealed_at IS NULL OR NEW.sealed_xid IS DISTINCT FROM pg_current_xact_id() THEN
            RAISE EXCEPTION 'asset version %: only the transaction that claimed it may seal it, once (#1596)', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.asset_version_files WHERE version_id = OLD.id AND role = 'original') THEN
            RAISE EXCEPTION 'asset version %: a sealed file set includes its original (#1596)', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF NEW.announced_at IS DISTINCT FROM OLD.announced_at AND (OLD.announced_at IS NOT NULL OR NEW.sealed_at IS NULL) THEN
        RAISE EXCEPTION 'asset version %: only a sealed version is announced, once (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_version_seal(p_version uuid, p_access jsonb DEFAULT '[]'::jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v record;
    e jsonb;
BEGIN
    SELECT id, sealed_at, original_hash, original_size_bytes, original_media_type INTO v
      FROM public.asset_versions WHERE id = p_version FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset version % does not exist (#1596)', p_version USING ERRCODE = 'no_data_found';
    END IF;
    IF v.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'asset version %: already sealed (#1596)', p_version USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.storage_variants
                    WHERE object_hash = v.original_hash AND variant_key = 'original' AND fixity_status = 'verified'
                      AND content_sha256 = v.original_hash AND content_size_bytes = v.original_size_bytes) THEN
        RAISE EXCEPTION 'asset version %: the original''s stored bytes are not verified (#1596)', p_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF p_access IS NOT NULL AND jsonb_typeof(p_access) <> 'array' THEN
        RAISE EXCEPTION 'asset version %: access entries are an array (#1596)', p_version
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    UPDATE public.asset_versions SET sealed_xid = pg_current_xact_id() WHERE id = p_version;
    INSERT INTO public.asset_version_files (version_id, role, object_hash, size_bytes, media_type)
    VALUES (p_version, 'original', v.original_hash, v.original_size_bytes, v.original_media_type);
    FOR e IN SELECT * FROM jsonb_array_elements(COALESCE(p_access, '[]'::jsonb)) LOOP
        INSERT INTO public.asset_version_files (version_id, role, object_hash, size_bytes, media_type, derived_from_hash, derived_variant_key)
        VALUES (p_version, e->>'role', e->>'object_hash', (e->>'size_bytes')::bigint, e->>'media_type',
                e->>'derived_from_hash', e->>'derived_variant_key');
    END LOOP;
    UPDATE public.asset_versions SET sealed_at = now() WHERE id = p_version;
END;
$$;
-- +goose StatementEnd
