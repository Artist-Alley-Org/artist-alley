-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00084_asset_versions.sql
--
-- #1596 (slice 1): every local asset with bytes has an immutable,
-- numbered, predecessor-linked version history, and every version's
-- bytes are pinned for as long as the version exists.
--
-- asset_versions: one row per version of a LOCAL asset (origin_server_id
-- IS NULL). Remote assets are versioned by their sender (#1574, #1575)
-- and get no rows here. An asset with no bytes (file_hash NULL) has no
-- version either: there is nothing to version.
--   number / predecessor_id  1, 2, 3 ... per asset, each version's
--                predecessor the same asset's previous number. No gaps,
--                no forks: a unique number per asset, one successor per
--                version, and an insert check that the predecessor is
--                the asset's latest version.
--   origin       ingest (created with the asset) | replacement (new
--                bytes, a later slice) | rendition (same original, new
--                access copies, a later slice) | backfill_current (made
--                by this migration from the bytes present at the time:
--                a snapshot of the surviving current file, NOT a claim
--                about what was ingested, and its created_at is the
--                migration time, not the asset's).
--   original_*   the version's original file, fixed when the version is
--                created; never changed.
--   sealed_xid / sealed_at   the file set's seal. A version is created
--                as a DRAFT. Sealing is a controlled procedure,
--                asset_version_seal(): it claims the version for the
--                current transaction (sealed_xid = pg_current_xact_id()),
--                writes the file rows, and sets sealed_at. File rows can
--                be written ONLY by the transaction that holds the claim
--                and ONLY before sealed_at; after it, nothing in the file
--                set can be inserted, updated or deleted. A claim must be
--                completed in its own transaction (deferred check).
--   announced_at reserved for the sender (a later slice): set once, only
--                on a sealed version. Nothing is announced yet.
--
-- asset_version_files: a sealed version's file set. This slice seals
-- nothing on its own (every version stays a draft until the later
-- reconciliation slice adds verified access copies); the procedure and
-- its guards exist now so the immutability rules are in place first.
-- Until that slice promotes #1597-verified variants, the only file row
-- accepted is the original: asset_version_seal() refuses any access
-- entry and the file trigger refuses every role but 'original'. The
-- preview/access roles stay in the schema for that slice.
--
-- Pins and retention: each version pins its files with
-- (object_hash, 'asset_version', <version id>), written by a trigger on
-- the version or file row itself (so every path, direct SQL included,
-- pins), under the storage object's row lock with GC eligibility cleared
-- first (storage.Service.AddPin's semantics, in casfetch's order). The
-- invariant "every existing version's original and file rows are pinned
-- to it" is re-checked at commit after any version insert or update, any
-- file row insert, and any change to or removal of a version pin; a
-- TRUNCATE of storage_pins is refused while versions exist. Soft delete
-- keeps these pins. When a
-- version goes (only with its asset: a hard purge or any other delete),
-- the same statement releases its pins and schedules every object left
-- without a pin for GC after the grace period (storage.Service.RemovePin's
-- semantics; the grace is aa.storage_gc_grace when the caller sets it,
-- else 24 hours, storage.DefaultGCGracePeriod). A deferred check refuses
-- any commit that would leave a version pin behind. TRUNCATE (seed reset)
-- releases the pins of every version it removed.
--
-- assets.current_version_id: the version whose original is the asset's
-- file. A local asset has one exactly when it has bytes, and its file is
-- that version's original (deferred check). Only the version machinery
-- sets it; a direct change of file_hash is refused, so bytes can change
-- only by creating a new version (a later slice). Metadata edits never
-- touch it.

-- +goose Up

CREATE TABLE public.asset_versions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    asset_id uuid NOT NULL,
    number bigint NOT NULL,
    predecessor_id uuid,
    origin text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by_user_ref bigint,
    original_hash text NOT NULL,
    original_size_bytes bigint NOT NULL,
    original_media_type text NOT NULL,
    original_extension text,
    original_basename text,
    original_basename_source text DEFAULT 'unknown'::text NOT NULL,
    sealed_xid xid8,
    sealed_at timestamp with time zone,
    announced_at timestamp with time zone,
    CONSTRAINT asset_versions_pkey PRIMARY KEY (id),
    CONSTRAINT asset_versions_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id) ON DELETE CASCADE,
    CONSTRAINT asset_versions_asset_number_key UNIQUE (asset_id, number),
    CONSTRAINT asset_versions_asset_id_id_key UNIQUE (asset_id, id),
    CONSTRAINT asset_versions_predecessor_fkey FOREIGN KEY (asset_id, predecessor_id) REFERENCES public.asset_versions(asset_id, id),
    CONSTRAINT asset_versions_original_hash_fkey FOREIGN KEY (original_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT,
    CONSTRAINT asset_versions_number_check CHECK (number >= 1),
    CONSTRAINT asset_versions_chain_check CHECK (((number = 1) = (predecessor_id IS NULL)) AND (predecessor_id IS DISTINCT FROM id)),
    CONSTRAINT asset_versions_origin_check
        CHECK (origin = ANY (ARRAY['ingest'::text, 'replacement'::text, 'rendition'::text, 'backfill_current'::text])),
    CONSTRAINT asset_versions_origin_number_check
        CHECK ((origin <> ALL (ARRAY['replacement'::text, 'rendition'::text])) OR (number > 1)),
    CONSTRAINT asset_versions_original_check
        CHECK ((original_hash ~ '^[0-9a-f]{64}$'::text) AND (original_size_bytes >= 0) AND (length(original_media_type) >= 1)),
    CONSTRAINT asset_versions_original_basename_known_check
        CHECK ((original_basename IS NULL) = (original_basename_source = 'unknown'::text)),
    CONSTRAINT asset_versions_original_basename_source_check
        CHECK (original_basename_source = ANY (ARRAY['upload_request'::text, 'client_reported'::text, 'unknown'::text])),
    CONSTRAINT asset_versions_original_basename_safe_check
        CHECK ((original_basename IS NULL) OR ((octet_length(original_basename) >= 1) AND (octet_length(original_basename) <= 255)
               AND (original_basename <> ALL (ARRAY['.'::text, '..'::text])) AND (strpos(original_basename, '/'::text) = 0)
               AND (strpos(original_basename, '\'::text) = 0)
               AND (original_basename !~ '[\x01-\x1f\x7f-\x9f؜‎‏‪-‮⁦-⁩]'::text))),
    CONSTRAINT asset_versions_seal_check CHECK ((sealed_at IS NULL) OR (sealed_xid IS NOT NULL)),
    CONSTRAINT asset_versions_announced_check CHECK ((announced_at IS NULL) OR (sealed_at IS NOT NULL))
);

-- A linear history: one successor per version.
CREATE UNIQUE INDEX asset_versions_one_successor
    ON public.asset_versions USING btree (predecessor_id) WHERE (predecessor_id IS NOT NULL);
CREATE INDEX asset_versions_original_hash_idx ON public.asset_versions USING btree (original_hash);

CREATE TABLE public.asset_version_files (
    version_id uuid NOT NULL,
    role text NOT NULL,
    object_hash text NOT NULL,
    size_bytes bigint NOT NULL,
    media_type text NOT NULL,
    derived_from_hash text,
    derived_variant_key text,
    CONSTRAINT asset_version_files_pkey PRIMARY KEY (version_id, role),
    CONSTRAINT asset_version_files_version_id_fkey FOREIGN KEY (version_id) REFERENCES public.asset_versions(id) ON DELETE CASCADE,
    CONSTRAINT asset_version_files_object_hash_fkey FOREIGN KEY (object_hash) REFERENCES public.storage_objects(hash) ON DELETE RESTRICT,
    CONSTRAINT asset_version_files_role_check CHECK (role = ANY (ARRAY['original'::text, 'preview'::text, 'access'::text])),
    CONSTRAINT asset_version_files_derived_check
        CHECK ((role = 'original'::text) = ((derived_from_hash IS NULL) AND (derived_variant_key IS NULL))),
    CONSTRAINT asset_version_files_hash_check
        CHECK ((object_hash ~ '^[0-9a-f]{64}$'::text) AND ((derived_from_hash IS NULL) OR (derived_from_hash ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT asset_version_files_size_check CHECK ((size_bytes >= 0) AND (length(media_type) >= 1)),
    CONSTRAINT asset_version_files_variant_key_check
        CHECK ((derived_variant_key IS NULL) OR ((length(derived_variant_key) >= 1) AND (length(derived_variant_key) <= 256)))
);
CREATE INDEX asset_version_files_object_hash_idx ON public.asset_version_files USING btree (object_hash);

ALTER TABLE public.assets ADD COLUMN current_version_id uuid;
ALTER TABLE public.assets ADD CONSTRAINT assets_current_version_fkey
    FOREIGN KEY (id, current_version_id) REFERENCES public.asset_versions(asset_id, id) DEFERRABLE INITIALLY DEFERRED;

COMMENT ON TABLE public.asset_versions IS 'Immutable, numbered, predecessor-linked versions of a LOCAL asset (#1596). A version is a draft until asset_version_seal() fixes its file set; nothing is announced to peers yet. origin backfill_current is a snapshot of the bytes present when migration 00084 ran, not an ingest record.';
COMMENT ON TABLE public.asset_version_files IS 'The sealed file set of an asset version (#1596): written only by asset_version_seal() in the transaction that claimed the version, and never changed afterwards.';
COMMENT ON COLUMN public.assets.current_version_id IS 'The version whose original is this asset''s file (#1596). Set only by the version machinery; a local asset has one exactly when it has bytes.';

-- +goose StatementBegin
-- The storage-object side of a version reference, shared by every path:
-- lock the object row (as casfetch does, so concurrent pins serialise),
-- clear any pending GC, then pin it to the version. A missing object is
-- an integrity failure, never a silent skip.
CREATE FUNCTION public.asset_version_pin(p_hash text, p_version uuid, p_size bigint) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_size bigint;
BEGIN
    SELECT size_bytes INTO v_size FROM public.storage_objects WHERE hash = p_hash FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset version %: storage object % does not exist (#1596)', p_version, p_hash
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF p_size IS NOT NULL AND v_size <> p_size THEN
        RAISE EXCEPTION 'asset version %: storage object % is % bytes, not % (#1596)', p_version, p_hash, v_size, p_size
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    UPDATE public.storage_objects SET gc_eligible_at = NULL WHERE hash = p_hash;
    INSERT INTO public.storage_pins (object_hash, pin_subject_type, pin_subject_id)
    VALUES (p_hash, 'asset_version', p_version::text)
    ON CONFLICT (object_hash, pin_subject_type, pin_subject_id) DO NOTHING;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Creates one draft version from the storage object's own size and type,
-- in the caller's transaction. The version's pin is written by the
-- asset_versions insert trigger, as it is for any other insert.
CREATE FUNCTION public.asset_version_create(p_asset uuid, p_version uuid, p_number bigint, p_predecessor uuid, p_origin text,
                                            p_hash text, p_extension text, p_basename text, p_basename_source text,
                                            p_by bigint) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_size bigint;
    v_type text;
BEGIN
    SELECT size_bytes, content_type INTO v_size, v_type FROM public.storage_objects WHERE hash = p_hash FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset %: storage object % does not exist (#1596)', p_asset, p_hash
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    INSERT INTO public.asset_versions (id, asset_id, number, predecessor_id, origin, created_by_user_ref,
                                       original_hash, original_size_bytes, original_media_type, original_extension,
                                       original_basename, original_basename_source)
    VALUES (p_version, p_asset, p_number, p_predecessor, p_origin, p_by,
            p_hash, v_size, v_type, p_extension, p_basename, COALESCE(p_basename_source, 'unknown'));
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Chain rules for a new version.
CREATE FUNCTION public.asset_versions_insert_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    prev record;
BEGIN
    IF NEW.sealed_xid IS NOT NULL OR NEW.sealed_at IS NOT NULL OR NEW.announced_at IS NOT NULL THEN
        RAISE EXCEPTION 'asset version %: a version is created as a draft (#1596)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM public.assets WHERE id = NEW.asset_id AND origin_server_id IS NOT NULL) THEN
        RAISE EXCEPTION 'asset %: remote assets are versioned by their origin, not here (#1596)', NEW.asset_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- Serialise chain growth per asset.
    PERFORM 1 FROM public.assets WHERE id = NEW.asset_id FOR UPDATE;
    IF NEW.number = 1 THEN
        IF EXISTS (SELECT 1 FROM public.asset_versions WHERE asset_id = NEW.asset_id) THEN
            RAISE EXCEPTION 'asset %: already has a version 1 (#1596)', NEW.asset_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    SELECT number, original_hash INTO prev FROM public.asset_versions
     WHERE id = NEW.predecessor_id AND asset_id = NEW.asset_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'asset version %: predecessor % is not a version of asset % (#1596)', NEW.id, NEW.predecessor_id, NEW.asset_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF prev.number <> NEW.number - 1
       OR EXISTS (SELECT 1 FROM public.asset_versions WHERE asset_id = NEW.asset_id AND number > prev.number) THEN
        RAISE EXCEPTION 'asset version %: number % must follow the asset''s latest version (#1596)', NEW.id, NEW.number
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.origin = 'rendition' AND NEW.original_hash <> prev.original_hash THEN
        RAISE EXCEPTION 'asset version %: a rendition keeps its predecessor''s original (#1596)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Every new version pins its original, whatever wrote the row: the
-- object row is locked, GC is cleared, and a size that differs from the
-- stored object's is refused.
CREATE FUNCTION public.asset_versions_after_insert() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.asset_version_pin(NEW.original_hash, NEW.id, NEW.original_size_bytes);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The retention invariant for one version: while it exists, its original
-- and every file row's object are pinned to it. Run at commit by the
-- deferred checks on asset_versions, asset_version_files and storage_pins.
CREATE FUNCTION public.asset_version_require_pinned(p_version uuid) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_hash text;
BEGIN
    SELECT h.hash INTO v_hash
      FROM public.asset_versions v
      CROSS JOIN LATERAL (SELECT v.original_hash AS hash
                          UNION
                          SELECT f.object_hash FROM public.asset_version_files f WHERE f.version_id = v.id) h
     WHERE v.id = p_version
       AND NOT EXISTS (SELECT 1 FROM public.storage_pins p
                        WHERE p.object_hash = h.hash AND p.pin_subject_type = 'asset_version'
                          AND p.pin_subject_id = v.id::text)
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'asset version %: storage object % is not pinned to it (#1596)', p_version, v_hash
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- A version's defining columns never change. The only transitions are the
-- seal (claim by the current transaction, then sealed_at, with the
-- original in the file set) and, once sealed, a single announced_at.
CREATE FUNCTION public.asset_versions_update_check() RETURNS trigger
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
-- A version goes only with its asset.
CREATE FUNCTION public.asset_versions_delete_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.assets WHERE id = OLD.asset_id) THEN
        RAISE EXCEPTION 'asset version %: versions are removed only with their asset (#1596)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Release a removed version's pins, scheduling every object left without
-- a pin for GC (storage.Service.RemovePin semantics).
CREATE FUNCTION public.asset_version_release_pins(p_version_ids text[]) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE
    v_grace interval := COALESCE(NULLIF(current_setting('aa.storage_gc_grace', true), '')::interval, interval '24 hours');
    v_hashes text[];
BEGIN
    -- The order every pin writer uses: lock the object rows first (in
    -- hash order, so concurrent releases cannot deadlock), then change
    -- the pin rows, then decide GC in a separate statement. A transaction
    -- pinning one of these objects holds its lock until it commits; the
    -- final UPDATE then runs with a fresh snapshot and sees that pin. A
    -- single UPDATE ... NOT EXISTS would re-check the row after waiting,
    -- but with its old snapshot, and schedule a just-pinned object.
    SELECT array_agg(DISTINCT object_hash ORDER BY object_hash) INTO v_hashes
      FROM public.storage_pins
     WHERE pin_subject_type = 'asset_version' AND pin_subject_id = ANY (p_version_ids);
    IF v_hashes IS NULL THEN
        RETURN;
    END IF;
    PERFORM 1 FROM public.storage_objects WHERE hash = ANY (v_hashes) ORDER BY hash FOR UPDATE;
    DELETE FROM public.storage_pins
     WHERE pin_subject_type = 'asset_version' AND pin_subject_id = ANY (p_version_ids);
    UPDATE public.storage_objects o
       SET gc_eligible_at = now() + v_grace
     WHERE o.hash = ANY (v_hashes)
       AND NOT EXISTS (SELECT 1 FROM public.storage_pins p WHERE p.object_hash = o.hash);
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION public.asset_versions_after_delete() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.asset_version_release_pins(ARRAY[OLD.id::text]);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- TRUNCATE fires no row triggers: release every version pin whose
-- version no longer exists.
CREATE FUNCTION public.asset_versions_after_truncate() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.asset_version_release_pins(ARRAY(
        SELECT p.pin_subject_id FROM public.storage_pins p
         WHERE p.pin_subject_type = 'asset_version'
           AND NOT EXISTS (SELECT 1 FROM public.asset_versions v WHERE v.id::text = p.pin_subject_id)));
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Deferred: a commit may not leave a removed version's pin behind, a
-- claimed version unsealed, or an existing version's bytes unpinned.
CREATE FUNCTION public.asset_versions_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF EXISTS (SELECT 1 FROM public.storage_pins WHERE pin_subject_type = 'asset_version' AND pin_subject_id = OLD.id::text)
           AND NOT EXISTS (SELECT 1 FROM public.asset_versions WHERE id = OLD.id) THEN
            RAISE EXCEPTION 'asset version %: removed with its pins still in place (#1596)', OLD.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NULL;
    END IF;
    IF EXISTS (SELECT 1 FROM public.asset_versions WHERE id = NEW.id AND sealed_xid IS NOT NULL AND sealed_at IS NULL) THEN
        RAISE EXCEPTION 'asset version %: claimed for sealing but not sealed in the same transaction (#1596)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    PERFORM public.asset_version_require_pinned(NEW.id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- File rows are written only by the transaction that claimed the version,
-- before it is sealed; the original row is the version's own original.
CREATE FUNCTION public.asset_version_files_write_check() RETURNS trigger
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

-- +goose StatementBegin
-- Deferred: a file row's object is pinned to its version at commit.
CREATE FUNCTION public.asset_version_files_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM public.asset_version_require_pinned(NEW.version_id);
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Deferred: a version pin may be removed or changed only if, at commit,
-- its version is gone or still has every one of its objects pinned (a
-- hard purge, a TRUNCATE reset and migration Down remove the version
-- first; a direct removal from a live version is refused).
CREATE FUNCTION public.storage_pins_version_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.pin_subject_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        PERFORM public.asset_version_require_pinned(OLD.pin_subject_id::uuid);
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- TRUNCATE fires no row triggers: refuse emptying storage_pins while any
-- version exists (a TRUNCATE of the assets as well, in the same
-- statement, leaves none).
CREATE FUNCTION public.storage_pins_after_truncate() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.asset_versions) THEN
        RAISE EXCEPTION 'storage_pins: cannot be truncated while asset versions exist (#1596)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- The controlled seal: claim, write the original, seal. Two concurrent
-- seals serialise on the version row; the second finds it sealed and
-- fails. p_access is reserved for access copies promoted from a
-- #1597-verified variant; until that promotion exists (a later slice),
-- any entry is refused, so nothing unverified can be sealed.
CREATE FUNCTION public.asset_version_seal(p_version uuid, p_access jsonb DEFAULT '[]'::jsonb) RETURNS void
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
-- Before an asset row is inserted: only the version machinery names its
-- current version, and a local asset with bytes is given its v1 id here.
CREATE FUNCTION public.assets_version_assign() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.current_version_id IS NOT NULL THEN
        RAISE EXCEPTION 'asset %: current_version_id is set by the version machinery (#1596)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.origin_server_id IS NULL AND NEW.file_hash IS NOT NULL THEN
        NEW.current_version_id := gen_random_uuid();
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- After an asset row is inserted: its version 1, pinned, in the same
-- transaction. Every insert path reaches this (CreateAsset, the aiedit
-- job's own INSERT, seed inserts).
CREATE FUNCTION public.assets_version_one() RETURNS trigger
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

-- +goose StatementBegin
-- An asset's bytes change only together with a move to a LATER version
-- whose original they are. A direct file_hash change is refused.
CREATE FUNCTION public.assets_version_guard() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    nv record;
    old_number bigint;
BEGIN
    IF NEW.file_hash IS NOT DISTINCT FROM OLD.file_hash
       AND NEW.current_version_id IS NOT DISTINCT FROM OLD.current_version_id
       AND NEW.origin_server_id IS NOT DISTINCT FROM OLD.origin_server_id THEN
        RETURN NEW;
    END IF;
    IF NEW.origin_server_id IS NOT NULL THEN
        IF NEW.current_version_id IS NOT NULL OR OLD.current_version_id IS NOT NULL THEN
            RAISE EXCEPTION 'asset %: a versioned local asset cannot become remote (#1596)', NEW.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.current_version_id IS NULL THEN
        RAISE EXCEPTION 'asset %: file_hash is changed only by creating a new version (#1596)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT number, original_hash INTO nv FROM public.asset_versions WHERE id = NEW.current_version_id AND asset_id = NEW.id;
    IF NOT FOUND OR nv.original_hash IS DISTINCT FROM NEW.file_hash THEN
        RAISE EXCEPTION 'asset %: file_hash is changed only by creating a new version (#1596)', NEW.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.current_version_id IS NOT NULL THEN
        SELECT number INTO old_number FROM public.asset_versions WHERE id = OLD.current_version_id;
        IF nv.number <= old_number THEN
            RAISE EXCEPTION 'asset %: the current version only moves forward (#1596)', NEW.id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
-- Deferred: a local asset has a current version exactly when it has
-- bytes, and its bytes are that version's original.
CREATE FUNCTION public.assets_version_commit_check() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    a record;
BEGIN
    SELECT id, file_hash, origin_server_id, current_version_id INTO a FROM public.assets WHERE id = NEW.id;
    IF NOT FOUND OR a.origin_server_id IS NOT NULL THEN
        RETURN NULL;
    END IF;
    IF (a.file_hash IS NULL) <> (a.current_version_id IS NULL) THEN
        RAISE EXCEPTION 'asset %: a local asset has a current version exactly when it has bytes (#1596)', a.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF a.current_version_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.asset_versions WHERE id = a.current_version_id AND asset_id = a.id AND original_hash = a.file_hash) THEN
        RAISE EXCEPTION 'asset %: its bytes are not its current version''s original (#1596)', a.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER asset_versions_insert_check_trg BEFORE INSERT ON public.asset_versions
    FOR EACH ROW EXECUTE FUNCTION public.asset_versions_insert_check();
CREATE TRIGGER asset_versions_pin_trg AFTER INSERT ON public.asset_versions
    FOR EACH ROW EXECUTE FUNCTION public.asset_versions_after_insert();
CREATE TRIGGER asset_versions_update_check_trg BEFORE UPDATE ON public.asset_versions
    FOR EACH ROW EXECUTE FUNCTION public.asset_versions_update_check();
CREATE TRIGGER asset_versions_delete_check_trg BEFORE DELETE ON public.asset_versions
    FOR EACH ROW EXECUTE FUNCTION public.asset_versions_delete_check();
CREATE TRIGGER asset_versions_release_pins_trg AFTER DELETE ON public.asset_versions
    FOR EACH ROW EXECUTE FUNCTION public.asset_versions_after_delete();
CREATE TRIGGER asset_versions_truncate_trg AFTER TRUNCATE ON public.asset_versions
    FOR EACH STATEMENT EXECUTE FUNCTION public.asset_versions_after_truncate();
CREATE CONSTRAINT TRIGGER asset_versions_commit_check_trg AFTER INSERT OR UPDATE OR DELETE ON public.asset_versions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.asset_versions_commit_check();
CREATE TRIGGER asset_version_files_write_check_trg BEFORE INSERT OR UPDATE OR DELETE ON public.asset_version_files
    FOR EACH ROW EXECUTE FUNCTION public.asset_version_files_write_check();
CREATE CONSTRAINT TRIGGER asset_version_files_commit_check_trg AFTER INSERT ON public.asset_version_files
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.asset_version_files_commit_check();
CREATE CONSTRAINT TRIGGER storage_pins_version_commit_check_trg AFTER UPDATE OR DELETE ON public.storage_pins
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN ((old.pin_subject_type = 'asset_version'::text))
    EXECUTE FUNCTION public.storage_pins_version_commit_check();
CREATE TRIGGER storage_pins_truncate_trg AFTER TRUNCATE ON public.storage_pins
    FOR EACH STATEMENT EXECUTE FUNCTION public.storage_pins_after_truncate();
CREATE TRIGGER assets_version_assign_trg BEFORE INSERT ON public.assets
    FOR EACH ROW EXECUTE FUNCTION public.assets_version_assign();
CREATE TRIGGER assets_version_one_trg AFTER INSERT ON public.assets
    FOR EACH ROW EXECUTE FUNCTION public.assets_version_one();
CREATE TRIGGER assets_version_guard_trg BEFORE UPDATE OF file_hash, current_version_id, origin_server_id ON public.assets
    FOR EACH ROW EXECUTE FUNCTION public.assets_version_guard();
CREATE CONSTRAINT TRIGGER assets_version_commit_check_trg AFTER INSERT OR UPDATE OF file_hash, current_version_id, origin_server_id
    ON public.assets DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION public.assets_version_commit_check();

-- +goose StatementBegin
-- Backfill: version 1 for every existing LOCAL asset with bytes, soft-
-- deleted ones included (they can be restored, and their history and bytes
-- must survive until a hard purge). A snapshot of the bytes present now:
-- origin backfill_current, created_at = migration time, left a draft, no
-- preservation event. A reference to a missing storage object is reported
-- and stops the migration rather than being invented or skipped.
DO $$
DECLARE
    n bigint;
    a record;
    v uuid;
BEGIN
    SELECT count(*) INTO n FROM public.assets x
     WHERE x.origin_server_id IS NULL AND x.file_hash IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM public.storage_objects o WHERE o.hash = x.file_hash);
    IF n > 0 THEN
        RAISE EXCEPTION '00084: % local assets reference a storage object that does not exist; resolve them before migrating', n;
    END IF;
    FOR a IN SELECT id, file_hash, file_extension, original_basename, original_basename_source, owner_user_ref
               FROM public.assets
              WHERE origin_server_id IS NULL AND file_hash IS NOT NULL
              ORDER BY created_at, id LOOP
        v := gen_random_uuid();
        PERFORM public.asset_version_create(a.id, v, 1, NULL, 'backfill_current', a.file_hash, a.file_extension,
                                            a.original_basename, a.original_basename_source, a.owner_user_ref);
        UPDATE public.assets SET current_version_id = v WHERE id = a.id;
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- +goose Down

DROP TRIGGER IF EXISTS storage_pins_truncate_trg ON public.storage_pins;
DROP TRIGGER IF EXISTS storage_pins_version_commit_check_trg ON public.storage_pins;
DROP TRIGGER IF EXISTS assets_version_commit_check_trg ON public.assets;
DROP TRIGGER IF EXISTS assets_version_guard_trg ON public.assets;
DROP TRIGGER IF EXISTS assets_version_one_trg ON public.assets;
DROP TRIGGER IF EXISTS assets_version_assign_trg ON public.assets;
ALTER TABLE public.assets DROP CONSTRAINT IF EXISTS assets_current_version_fkey;
ALTER TABLE public.assets DROP COLUMN IF EXISTS current_version_id;
-- Release every version pin (scheduling GC for objects left unpinned),
-- then drop the tables without firing their delete guards.
SELECT public.asset_version_release_pins(ARRAY(
    SELECT pin_subject_id FROM public.storage_pins WHERE pin_subject_type = 'asset_version'));
DROP TABLE IF EXISTS public.asset_version_files;
DROP TABLE IF EXISTS public.asset_versions;
DROP FUNCTION IF EXISTS public.assets_version_commit_check();
DROP FUNCTION IF EXISTS public.assets_version_guard();
DROP FUNCTION IF EXISTS public.assets_version_one();
DROP FUNCTION IF EXISTS public.assets_version_assign();
DROP FUNCTION IF EXISTS public.storage_pins_after_truncate();
DROP FUNCTION IF EXISTS public.storage_pins_version_commit_check();
DROP FUNCTION IF EXISTS public.asset_version_files_commit_check();
DROP FUNCTION IF EXISTS public.asset_version_seal(uuid, jsonb);
DROP FUNCTION IF EXISTS public.asset_version_files_write_check();
DROP FUNCTION IF EXISTS public.asset_versions_commit_check();
DROP FUNCTION IF EXISTS public.asset_versions_after_truncate();
DROP FUNCTION IF EXISTS public.asset_versions_after_delete();
DROP FUNCTION IF EXISTS public.asset_version_release_pins(text[]);
DROP FUNCTION IF EXISTS public.asset_versions_delete_check();
DROP FUNCTION IF EXISTS public.asset_versions_update_check();
DROP FUNCTION IF EXISTS public.asset_version_require_pinned(uuid);
DROP FUNCTION IF EXISTS public.asset_versions_after_insert();
DROP FUNCTION IF EXISTS public.asset_versions_insert_check();
DROP FUNCTION IF EXISTS public.asset_version_create(uuid, uuid, bigint, uuid, text, text, text, text, text, bigint);
DROP FUNCTION IF EXISTS public.asset_version_pin(text, uuid, bigint);
