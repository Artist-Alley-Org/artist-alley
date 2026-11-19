-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00085_asset_version_reconcile.sql
--
-- #1596 (slice 2, reconciliation): a version's file set may now hold
-- verified access copies, and every terminal processing event asks for
-- the asset's versions to be reconciled.
--
-- Sealing access copies (reopened, under DB-enforced rules). A sealed
-- file set lists the original plus at most one `preview` and one
-- `access` row (the local byte classes of spec §7.5.7; both are wire role
-- `access` in §17.2.2, which a sender maps). A non-original row is
-- accepted only when ALL of these hold:
--   - it is derived from the version's own original, under a named key;
--   - its object is an original of its own (a promoted copy): recorded at
--     that size, with an original row that says verified for exactly that
--     digest and size (#1597);
--   - it is held: a live `version_promote` pin on the object (a promotion
--     in progress, storage.Service.PromoteVariant), or the same object in
--     the same role of an EARLIER sealed version of the same asset (a
--     copy carried forward unchanged into a rendition successor).
-- And no version is sealed unless its ORIGINAL's stored bytes are
-- verified for exactly its hash and size: a sealed file set never lists
-- bytes without a verified SHA-256 (spec §17.2.2).
--
-- Reconcile requests. asset_version_reconcile_enqueue(hash) asks for one
-- `asset.version_reconcile` job for the hash. The jobs table coalesces a
-- pending or running job with the same idempotency key; a request that
-- arrives while one is RUNNING must not be lost to it (the running job
-- may already have read the state), so it is keyed after the running job
-- (`<key>:after:<job id>`): at most one pending job follows each running
-- one. It is called by:
--   - assets_version_one, when a local asset is inserted with bytes and
--     needs no processing (status ready) or has already failed;
--   - a new trigger on every terminal processing event, an UPDATE that
--     sets processing_status to ready or failed (MarkAssetReady and
--     MarkAssetFailed, for all thirteen preview handlers at once);
--   - storage_variants_recorded, in the SAME statement that records a
--     verified digest for a derived variant whose key the access policy
--     can choose (asset_access_candidate_key), unless a preview job for
--     the bytes is pending or running (its terminal event requests it, so
--     a re-render never seals a half-written ladder). The request commits
--     with the record or not at all;
--   - the operator command (Go).
--
-- Provenance. storage_variant_digest_history keeps, immutably, every
-- verified digest recorded for a candidate-key derived variant: (object,
-- key, generation) -> digest and size, written by the same trigger and
-- accepted only when it mirrors the verified row. A regeneration adds a
-- row and removes none. An access copy is sealed only if its digest and
-- size are in that history for exactly the version's original and the
-- named key, so an unrelated object cannot be presented as this asset's
-- rendition, while a copy promoted before a regeneration still seals.
--
-- Worker capacity: the new type is capped at 2 concurrent jobs, like the
-- other storage-heavy types (loaded at boot).

-- +goose Up

-- +goose StatementBegin
-- The SQL twin of assets.IsAccessCandidateKey (a parity test pins them):
-- a plain ladder token that no other writer reserves.
CREATE FUNCTION public.asset_access_candidate_key(p_key text) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT p_key ~ '^[a-z0-9][a-z0-9_-]{0,63}$'
       AND p_key <> ALL (ARRAY['original', 'poster', 'cover', 'embedded-preview', 'iso']);
$$;
-- +goose StatementEnd

CREATE TABLE public.storage_variant_digest_history (
    object_hash text NOT NULL,
    variant_key text NOT NULL,
    generation bigint NOT NULL,
    content_sha256 text NOT NULL,
    content_size_bytes bigint NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_variant_digest_history_pkey PRIMARY KEY (object_hash, variant_key, generation),
    CONSTRAINT storage_variant_digest_history_check CHECK ((content_sha256 ~ '^[0-9a-f]{64}$'::text)
        AND (content_size_bytes >= 0) AND (variant_key <> 'original'::text))
);
CREATE INDEX storage_variant_digest_history_digest_idx ON public.storage_variant_digest_history USING btree (content_sha256);
COMMENT ON TABLE public.storage_variant_digest_history IS 'Every verified digest recorded for a derived variant the access policy can choose (#1596): (object, key, generation) -> digest. Insert-only, each row mirroring a verified storage_variants row; the provenance an access copy is sealed against.';

-- +goose StatementBegin
-- History rows mirror a verified variant row, and are never changed.
CREATE FUNCTION public.storage_variant_digest_history_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'storage variant digest history is immutable (#1596)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.storage_variants
                    WHERE object_hash = NEW.object_hash AND variant_key = NEW.variant_key AND generation = NEW.generation
                      AND fixity_status = 'verified' AND content_sha256 = NEW.content_sha256
                      AND content_size_bytes = NEW.content_size_bytes) THEN
        RAISE EXCEPTION 'storage variant digest history: %/% generation % is not a verified variant row with that digest (#1596)',
            NEW.object_hash, NEW.variant_key, NEW.generation
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER storage_variant_digest_history_guard_trg BEFORE INSERT OR UPDATE OR DELETE ON public.storage_variant_digest_history
    FOR EACH ROW EXECUTE FUNCTION public.storage_variant_digest_history_guard();

-- +goose StatementBegin
-- The access-copy rules above, for one file row of p_version.
CREATE FUNCTION public.asset_version_copy_check(p_version uuid, p_role text, p_hash text, p_size bigint,
                                                p_from text, p_key text) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v record;
    o record;
BEGIN
    SELECT asset_id, number, original_hash INTO v FROM public.asset_versions WHERE id = p_version;
    IF p_role NOT IN ('preview', 'access') THEN
        RAISE EXCEPTION 'asset version %: % is not an access role (#1596)', p_version, p_role
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF p_from IS DISTINCT FROM v.original_hash OR p_key IS NULL THEN
        RAISE EXCEPTION 'asset version %: an access copy is a named derivative of the version''s own original (#1596)', p_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT so.size_bytes AS object_size, sv.fixity_status, sv.content_sha256, sv.content_size_bytes INTO o
      FROM public.storage_objects so
      LEFT JOIN public.storage_variants sv ON sv.object_hash = so.hash AND sv.variant_key = 'original'
     WHERE so.hash = p_hash;
    IF NOT FOUND OR o.object_size <> p_size OR o.fixity_status IS DISTINCT FROM 'verified'
       OR o.content_sha256 IS DISTINCT FROM p_hash OR o.content_size_bytes IS DISTINCT FROM p_size THEN
        RAISE EXCEPTION 'asset version %: access copy % is not a verified object of its own (#1596)', p_version, p_hash
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.storage_variant_digest_history
                    WHERE object_hash = v.original_hash AND variant_key = p_key
                      AND content_sha256 = p_hash AND content_size_bytes = p_size) THEN
        RAISE EXCEPTION 'asset version %: access copy % has no recorded provenance as variant % of this original (#1596)', p_version, p_hash, p_key
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.storage_pins
                    WHERE object_hash = p_hash AND pin_subject_type = 'version_promote')
       AND NOT EXISTS (SELECT 1 FROM public.asset_version_files f
                         JOIN public.asset_versions pv ON pv.id = f.version_id
                        WHERE pv.asset_id = v.asset_id AND pv.number < v.number AND pv.sealed_at IS NOT NULL
                          AND f.role = p_role AND f.object_hash = p_hash) THEN
        RAISE EXCEPTION 'asset version %: access copy % is neither a held promotion nor carried forward from an earlier version (#1596)', p_version, p_hash
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- 00084's file write check, with access rows accepted under the rules.
CREATE OR REPLACE FUNCTION public.asset_version_files_write_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'asset version file: a file set is immutable (#1596)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF EXISTS (SELECT 1 FROM public.asset_versions WHERE id = OLD.version_id) THEN
            RAISE EXCEPTION 'asset version %: a file set is immutable (#1596)', OLD.version_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN OLD;
    END IF;
    SELECT sealed_xid, sealed_at, original_hash, original_size_bytes INTO v
      FROM public.asset_versions WHERE id = NEW.version_id FOR UPDATE;
    IF v.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'asset version %: sealed; its file set cannot change (#1596)', NEW.version_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v.sealed_xid IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'asset version %: files are written only by asset_version_seal() in the sealing transaction (#1596)', NEW.version_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.role = 'original' THEN
        IF NEW.object_hash <> v.original_hash OR NEW.size_bytes <> v.original_size_bytes THEN
            RAISE EXCEPTION 'asset version %: the original file is the version''s own original (#1596)', NEW.version_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSE
        PERFORM public.asset_version_copy_check(NEW.version_id, NEW.role, NEW.object_hash, NEW.size_bytes,
                                                NEW.derived_from_hash, NEW.derived_variant_key);
    END IF;
    PERFORM public.asset_version_pin(NEW.object_hash, NEW.version_id, NEW.size_bytes);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The controlled seal: claim, require the original's stored bytes to be
-- verified, write the original and the access entries ([{role,
-- object_hash, size_bytes, media_type, derived_from_hash,
-- derived_variant_key}], each checked by the file trigger), seal. Two
-- concurrent seals serialise on the version row; the second fails.
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

-- +goose StatementBegin
-- One `asset.version_reconcile` job for p_hash: coalesced with a pending
-- one, and keyed after any RUNNING one so a request is never absorbed by
-- a job that may already have read the state.
CREATE FUNCTION public.asset_version_reconcile_enqueue(p_hash text) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    k text;
    running uuid;
    i int := 0;
BEGIN
    IF p_hash IS NULL THEN
        RETURN;
    END IF;
    k := 'version-reconcile:' || p_hash;
    LOOP
        SELECT id INTO running FROM public.jobs
         WHERE type = 'asset.version_reconcile' AND idempotency_key = k AND status = 'running'
         LIMIT 1;
        EXIT WHEN running IS NULL OR i >= 8;
        k := k || ':after:' || running::text;
        i := i + 1;
    END LOOP;
    INSERT INTO public.jobs (type, payload, priority, idempotency_key)
    VALUES ('asset.version_reconcile', jsonb_build_object('object_hash', p_hash), 200, k)
    ON CONFLICT (type, idempotency_key)
       WHERE ((idempotency_key IS NOT NULL) AND (status = ANY (ARRAY['pending'::text, 'running'::text])))
    DO NOTHING;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- In the statement that records a verified digest for a candidate-key
-- derived variant: keep its provenance, and request a reconcile unless a
-- preview job for the bytes is in flight. A re-verification of the same
-- digest and generation (a sweep) does neither.
CREATE FUNCTION public.storage_variants_recorded() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.variant_key = 'original' OR NEW.fixity_status <> 'verified' OR NEW.content_sha256 IS NULL
       OR NOT public.asset_access_candidate_key(NEW.variant_key) THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.fixity_status = 'verified' AND OLD.content_sha256 IS NOT DISTINCT FROM NEW.content_sha256
       AND OLD.generation = NEW.generation THEN
        RETURN NULL;
    END IF;
    INSERT INTO public.storage_variant_digest_history (object_hash, variant_key, generation, content_sha256, content_size_bytes)
    VALUES (NEW.object_hash, NEW.variant_key, NEW.generation, NEW.content_sha256, NEW.content_size_bytes)
    ON CONFLICT (object_hash, variant_key, generation) DO NOTHING;
    IF NOT EXISTS (SELECT 1 FROM public.jobs
                    WHERE type LIKE 'preview.%' AND status IN ('pending', 'running')
                      AND payload->>'file_hash' = NEW.object_hash) THEN
        PERFORM public.asset_version_reconcile_enqueue(NEW.object_hash);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER storage_variants_recorded_trg AFTER INSERT OR UPDATE ON public.storage_variants
    FOR EACH ROW EXECUTE FUNCTION public.storage_variants_recorded();

-- The provenance of every candidate-key variant verified before 00085.
INSERT INTO public.storage_variant_digest_history (object_hash, variant_key, generation, content_sha256, content_size_bytes)
SELECT object_hash, variant_key, generation, content_sha256, content_size_bytes
  FROM public.storage_variants
 WHERE variant_key <> 'original' AND fixity_status = 'verified' AND content_sha256 IS NOT NULL
   AND public.asset_access_candidate_key(variant_key);

-- +goose StatementBegin
-- 00084's v1 trigger; an asset that needs no processing (or has already
-- failed it) asks for reconciliation at once.
CREATE OR REPLACE FUNCTION public.assets_version_one() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.current_version_id IS NOT NULL THEN
        PERFORM public.asset_version_create(NEW.id, NEW.current_version_id, 1, NULL, 'ingest', NEW.file_hash, NEW.file_extension,
                                            NEW.original_basename, NEW.original_basename_source, NEW.owner_user_ref);
        IF NEW.processing_status IN ('ready', 'failed') THEN
            PERFORM public.asset_version_reconcile_enqueue(NEW.file_hash);
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- A terminal processing event (ready or failed) asks for reconciliation.
CREATE FUNCTION public.assets_version_terminal() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.origin_server_id IS NULL AND NEW.file_hash IS NOT NULL AND NEW.processing_status IN ('ready', 'failed') THEN
        PERFORM public.asset_version_reconcile_enqueue(NEW.file_hash);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER assets_version_terminal_trg AFTER UPDATE OF processing_status ON public.assets
    FOR EACH ROW EXECUTE FUNCTION public.assets_version_terminal();

-- +goose StatementBegin
INSERT INTO public.system_config (key, value) VALUES
    ('jobs.type_concurrency.asset.version_reconcile', '2')
ON CONFLICT (key) DO NOTHING;
-- +goose StatementEnd

-- +goose Down

DELETE FROM public.system_config WHERE key = 'jobs.type_concurrency.asset.version_reconcile';
DROP TRIGGER IF EXISTS storage_variants_recorded_trg ON public.storage_variants;
DROP FUNCTION IF EXISTS public.storage_variants_recorded();
DROP TABLE IF EXISTS public.storage_variant_digest_history;
DROP FUNCTION IF EXISTS public.storage_variant_digest_history_guard();
DROP TRIGGER IF EXISTS assets_version_terminal_trg ON public.assets;
DROP FUNCTION IF EXISTS public.assets_version_terminal();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.assets_version_one() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.current_version_id IS NOT NULL THEN
        PERFORM public.asset_version_create(NEW.id, NEW.current_version_id, 1, NULL, 'ingest', NEW.file_hash, NEW.file_extension,
                                            NEW.original_basename, NEW.original_basename_source, NEW.owner_user_ref);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS public.asset_version_reconcile_enqueue(text);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_version_seal(p_version uuid, p_access jsonb DEFAULT '[]'::jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v record;
BEGIN
    IF p_access IS NOT NULL AND p_access <> '[]'::jsonb THEN
        RAISE EXCEPTION 'asset version %: access copies cannot be sealed until verified promotion exists (#1596)', p_version
            USING ERRCODE = 'feature_not_supported';
    END IF;
    SELECT id, sealed_at, original_hash, original_size_bytes, original_media_type INTO v
      FROM public.asset_versions WHERE id = p_version FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset version % does not exist (#1596)', p_version USING ERRCODE = 'no_data_found';
    END IF;
    IF v.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'asset version %: already sealed (#1596)', p_version USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    UPDATE public.asset_versions SET sealed_xid = pg_current_xact_id() WHERE id = p_version;
    INSERT INTO public.asset_version_files (version_id, role, object_hash, size_bytes, media_type)
    VALUES (p_version, 'original', v.original_hash, v.original_size_bytes, v.original_media_type);
    UPDATE public.asset_versions SET sealed_at = now() WHERE id = p_version;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.asset_version_files_write_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    v record;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'asset version file: a file set is immutable (#1596)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        IF EXISTS (SELECT 1 FROM public.asset_versions WHERE id = OLD.version_id) THEN
            RAISE EXCEPTION 'asset version %: a file set is immutable (#1596)', OLD.version_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN OLD;
    END IF;
    SELECT sealed_xid, sealed_at, original_hash, original_size_bytes INTO v
      FROM public.asset_versions WHERE id = NEW.version_id FOR UPDATE;
    IF v.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'asset version %: sealed; its file set cannot change (#1596)', NEW.version_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v.sealed_xid IS DISTINCT FROM pg_current_xact_id() THEN
        RAISE EXCEPTION 'asset version %: files are written only by asset_version_seal() in the sealing transaction (#1596)', NEW.version_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- Access and preview copies are accepted only once they can be
    -- promoted from a #1597-verified variant (a later slice).
    IF NEW.role <> 'original' THEN
        RAISE EXCEPTION 'asset version %: only the original is accepted until verified access copies exist (#1596)', NEW.version_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.object_hash <> v.original_hash OR NEW.size_bytes <> v.original_size_bytes THEN
        RAISE EXCEPTION 'asset version %: the original file is the version''s own original (#1596)', NEW.version_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM public.asset_version_pin(NEW.object_hash, NEW.version_id, NEW.size_bytes);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS public.asset_version_copy_check(uuid, text, text, bigint, text, text);
DROP FUNCTION IF EXISTS public.asset_access_candidate_key(text);
