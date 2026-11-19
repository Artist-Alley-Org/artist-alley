-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00081_asset_original_basename.sql
--
-- #1595: the base name an upload request carried, kept as write-once
-- ingest provenance on the asset, separate from the editable
-- metadata.original_filename claim.
--
-- What the value is, and is not: the file name the uploading client sent
-- in the upload request's X-Original-Filename header, reduced to a safe
-- base name. The server cannot see the user's disk, so it is the name as
-- RECEIVED, not an attested name of a file anywhere. archival-1 calls it
-- the same thing: "untrusted, and never a path" (spec v1 §17.2.2).
--
-- assets.original_basename / original_basename_source: the typed pair.
--   upload_request   the base name arrived in the upload request itself
--                    and was carried to the asset by its upload receipt.
--   client_reported  backfilled below from metadata.original_filename,
--                    a value any client could send and anyone with edit
--                    rights could change. Never upgraded to upload_request.
--   unknown          nothing trustworthy was supplied. original_basename
--                    is NULL exactly when the source is unknown.
-- Both are WRITE-ONCE: set by the INSERT, never by an UPDATE (trigger
-- below). PATCH /assets offers no way to change them, and the trigger
-- stops any other path too.
--
-- upload_receipts: the bridge from POST /storage/objects to POST /assets.
-- Content-addressed storage is shared: one storage_objects row and its
-- pins serve every upload of the same bytes, so a per-upload name cannot
-- live there. Each successful upload instead gets a receipt of its own,
-- bound to the uploading user, the exact hash and the base name that
-- request carried. POST /assets consumes it (DELETE ... RETURNING) inside
-- the asset's transaction, checking user and hash, so a receipt is used
-- at most once, by its own user, for its own bytes; a rolled-back create
-- leaves it usable. An unused receipt expires and is swept in bounded
-- batches by later uploads. The row goes with its storage object.

-- +goose Up

ALTER TABLE public.assets
    ADD COLUMN original_basename text,
    ADD COLUMN original_basename_source text DEFAULT 'unknown'::text NOT NULL;

-- Backfill BEFORE the constraints and the write-once trigger exist: only
-- a metadata.original_filename that is already a safe bare base name is
-- carried, and only as client_reported. A path is not reduced to its
-- last component here (that would be inventing a name from a path); it,
-- and anything else invalid or absent, stays unknown. The character rule
-- (no C0/C1 controls or DEL, no bidi formatting marks) is the one
-- provenance.Basename applies to an upload request.
UPDATE public.assets
   SET original_basename = metadata->>'original_filename',
       original_basename_source = 'client_reported'
 WHERE jsonb_typeof(metadata->'original_filename') = 'string'
   AND octet_length(metadata->>'original_filename') BETWEEN 1 AND 255
   AND metadata->>'original_filename' NOT IN ('.', '..')
   AND strpos(metadata->>'original_filename', '/') = 0
   AND strpos(metadata->>'original_filename', E'\\') = 0
   AND metadata->>'original_filename' !~ '[\x01-\x1f\x7f-\x9f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]';

ALTER TABLE public.assets
    ADD CONSTRAINT assets_original_basename_source_check
        CHECK (original_basename_source = ANY (ARRAY['upload_request'::text, 'client_reported'::text, 'unknown'::text])),
    ADD CONSTRAINT assets_original_basename_known_check
        CHECK ((original_basename IS NULL) = (original_basename_source = 'unknown'::text)),
    ADD CONSTRAINT assets_original_basename_safe_check
        CHECK (original_basename IS NULL OR (
            octet_length(original_basename) BETWEEN 1 AND 255
            AND original_basename <> ALL (ARRAY['.'::text, '..'::text])
            AND strpos(original_basename, '/') = 0
            AND strpos(original_basename, E'\\') = 0
            AND original_basename !~ '[\x01-\x1f\x7f-\x9f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]'));

COMMENT ON COLUMN public.assets.original_basename IS 'The base name the upload request carried (#1595): untrusted, never a path, never an attestation of a name on a user''s disk. Write-once. NULL exactly when original_basename_source is unknown.';
COMMENT ON COLUMN public.assets.original_basename_source IS 'Where original_basename came from (#1595): upload_request (the upload request itself, via its receipt), client_reported (backfilled from editable metadata.original_filename), unknown. Write-once.';

-- +goose StatementBegin
CREATE FUNCTION public.assets_original_basename_write_once() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.original_basename IS DISTINCT FROM OLD.original_basename
       OR NEW.original_basename_source IS DISTINCT FROM OLD.original_basename_source THEN
        RAISE EXCEPTION 'assets.original_basename and original_basename_source are write-once (#1595)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- On EVERY UPDATE, not UPDATE OF the two columns: an OF trigger fires only
-- when the UPDATE command names those columns, and another BEFORE UPDATE
-- trigger could rewrite NEW.original_basename during an UPDATE that names
-- only title. The function's IS DISTINCT FROM test lets same-value and
-- unrelated updates through.
CREATE TRIGGER assets_original_basename_write_once_trg
    BEFORE UPDATE ON public.assets
    FOR EACH ROW EXECUTE FUNCTION public.assets_original_basename_write_once();

CREATE TABLE public.upload_receipts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    user_ref bigint NOT NULL,
    file_hash text NOT NULL,
    original_basename text,
    original_basename_source text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT upload_receipts_pkey PRIMARY KEY (id),
    CONSTRAINT upload_receipts_file_hash_fkey FOREIGN KEY (file_hash) REFERENCES public.storage_objects(hash) ON DELETE CASCADE,
    CONSTRAINT upload_receipts_source_check
        CHECK (original_basename_source = ANY (ARRAY['upload_request'::text, 'unknown'::text])),
    CONSTRAINT upload_receipts_known_check
        CHECK ((original_basename IS NULL) = (original_basename_source = 'unknown'::text)),
    CONSTRAINT upload_receipts_basename_safe_check
        CHECK (original_basename IS NULL OR (
            octet_length(original_basename) BETWEEN 1 AND 255
            AND original_basename <> ALL (ARRAY['.'::text, '..'::text])
            AND strpos(original_basename, '/') = 0
            AND strpos(original_basename, E'\\') = 0
            AND original_basename !~ '[\x01-\x1f\x7f-\x9f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]')),
    CONSTRAINT upload_receipts_expiry_check CHECK (expires_at > created_at)
);

CREATE INDEX upload_receipts_expires_at_idx ON public.upload_receipts USING btree (expires_at);
CREATE INDEX upload_receipts_file_hash_idx ON public.upload_receipts USING btree (file_hash);

COMMENT ON TABLE public.upload_receipts IS 'One per successful POST /storage/objects (#1595): the uploading user, the exact hash and the base name that request carried. Consumed once by POST /assets inside its transaction (user and hash must match); expired rows are swept in bounded batches by later uploads. Never on storage_objects or storage_pins, which are shared by every upload of the same bytes.';

-- +goose Down

DROP TABLE IF EXISTS public.upload_receipts;
DROP TRIGGER IF EXISTS assets_original_basename_write_once_trg ON public.assets;
DROP FUNCTION IF EXISTS public.assets_original_basename_write_once();
ALTER TABLE public.assets
    DROP CONSTRAINT IF EXISTS assets_original_basename_safe_check,
    DROP CONSTRAINT IF EXISTS assets_original_basename_known_check,
    DROP CONSTRAINT IF EXISTS assets_original_basename_source_check,
    DROP COLUMN IF EXISTS original_basename_source,
    DROP COLUMN IF EXISTS original_basename;
