-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00098_federation_received_asset_metadata.sql
--
-- #1577 (spec v1 §18.4): the complete descriptive snapshot of an
-- asset-metadata-1 Update, as this instance received it.
--
-- One row per accepted aa:metadata member, linked one to one to the
-- immutable revision (migration 00094) its activity left. The revision
-- holds the activity's identity (peer, activity URI), actor, version,
-- file set and flags; this row holds what the profile adds:
--
--   - snapshot: the four-claim member exactly as validated (title,
--     description, rights, requiredStatement), each a claim or JSON null.
--     Four nulls are a snapshot, not its absence. The full wire rules
--     (raw byte bound, text, URI and language-map rules) are the
--     validator's (federation.ParseAssetMetadata); the shape check below
--     only keeps the stored form honest. Its jsonb text is a storage
--     form, not the wire span, so no byte bound is checked on it.
--   - published_seconds and published_nanos: the activity's signed
--     published to the nanosecond. A timestamptz keeps microseconds, so
--     the order and the duplicate check of a snapshot use these.
--     published_text is the same instant in UTC, as RFC 3339.
--   - the FIRST accepted origin signature as declared on the envelope
--     (its key id, algorithm and value), which the caller verified before
--     admission; it is never replaced by a later re-signed duplicate. It
--     is not key material and proves nothing by itself.
--   - recipient_signer_key_id: THIS instance's identity the grant names
--     (the local fetch signer). It is not the origin's key.
--
-- A row is immutable and goes with its revision (and so with its peer).
-- Nothing that decides access reads it: whether a snapshot is the
-- current one is derived when it is read (a matching verified version
-- held here, no tombstone), never stored. No local field definition,
-- value, default or permission is created from it.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.federation_received_asset_metadata (
    revision_id uuid NOT NULL,
    profile_token text NOT NULL,
    snapshot jsonb NOT NULL,
    published_seconds bigint NOT NULL,
    published_nanos integer NOT NULL,
    published_text text NOT NULL,
    origin_signer_key_id text NOT NULL,
    origin_signature_algorithm text NOT NULL,
    origin_signature_value text NOT NULL,
    recipient_signer_key_id text NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_received_asset_metadata_pkey PRIMARY KEY (revision_id),
    CONSTRAINT federation_received_asset_metadata_revision_id_fkey FOREIGN KEY (revision_id)
        REFERENCES public.federation_received_asset_revisions(id) ON DELETE CASCADE,
    CONSTRAINT federation_received_asset_metadata_profile_check CHECK (profile_token = 'asset-metadata-1'::text),
    CONSTRAINT federation_received_asset_metadata_nanos_check CHECK (published_nanos >= 0 AND published_nanos <= 999999999),
    CONSTRAINT federation_received_asset_metadata_snapshot_check CHECK (
        jsonb_typeof(snapshot) = 'object'::text
        AND snapshot ?& ARRAY['title'::text, 'description'::text, 'rights'::text, 'requiredStatement'::text]
        AND (snapshot - ARRAY['title'::text, 'description'::text, 'rights'::text, 'requiredStatement'::text]) = '{}'::jsonb
        AND jsonb_typeof(snapshot -> 'title') = ANY (ARRAY['string'::text, 'null'::text])
        AND jsonb_typeof(snapshot -> 'description') = ANY (ARRAY['string'::text, 'null'::text])
        AND jsonb_typeof(snapshot -> 'rights') = ANY (ARRAY['string'::text, 'null'::text])
        AND jsonb_typeof(snapshot -> 'requiredStatement') = ANY (ARRAY['object'::text, 'null'::text])),
    CONSTRAINT federation_received_asset_metadata_signers_check CHECK (origin_signer_key_id <> ''::text
        AND origin_signature_algorithm <> ''::text AND origin_signature_value <> ''::text
        AND recipient_signer_key_id <> ''::text)
);

COMMENT ON TABLE public.federation_received_asset_metadata IS 'The complete descriptive snapshot of an asset-metadata-1 Update as this instance received it (#1577, spec v1 §18.4): one immutable row per accepted aa:metadata member, linked to its revision, with the signed published to the nanosecond, the first accepted origin signature as declared, and this instance''s recipient identity. Which snapshot is current is derived when read; nothing that decides access reads it.';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_asset_metadata_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'received asset metadata %: a snapshot is immutable (#1577)', OLD.revision_id
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_asset_metadata_immutable_trg ON public.federation_received_asset_metadata;
CREATE TRIGGER federation_received_asset_metadata_immutable_trg BEFORE UPDATE ON public.federation_received_asset_metadata
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_asset_metadata_immutable();

-- +goose Down

-- Down refuses while a snapshot is kept, rather than discard what a peer
-- told this instance: once no row is left (each removed with its
-- revision and peer), it drops the table.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.federation_received_asset_metadata) THEN
        RAISE EXCEPTION '00098 down: received asset metadata is kept; Down would discard what peers told this instance (#1577)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_asset_metadata_immutable_trg ON public.federation_received_asset_metadata;
DROP FUNCTION IF EXISTS public.federation_received_asset_metadata_immutable();
DROP TABLE IF EXISTS public.federation_received_asset_metadata;
