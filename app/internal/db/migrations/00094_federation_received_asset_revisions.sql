-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00094_federation_received_asset_revisions.sql
--
-- #1575 (spec v1 §17.2.8): an origin's mature and aiProvenance flags of
-- an asset, as this instance received them, each with the identity and
-- order of the activity that carried it.
--
-- One row per accepted aa:asset of an asset from a peer: every
-- aa:AssetVersion bound to a record that is not rejected (a new version,
-- or a version already held, announced again under a new activity), and
-- every accepted Update of the asset's flags. Its identity is the
-- activity (peer, activity_uri); its order is the activity's signed
-- published, then its URI. The flags a record is shown with are those of
-- the greatest row for its peer and asset, which a sender makes the
-- newest (§17.2.8).
--
-- A row is immutable: what a peer said under an activity id never
-- changes. It names the version the aa:asset carried and that version's
-- file set, so an Update naming a version held here with other bytes is
-- refused (version_conflict). Rows go with their peer.
--
-- Nothing else reads a row: the viewing rules are unchanged, and a
-- record's tier is still its own.

-- +goose Up

CREATE TABLE IF NOT EXISTS public.federation_received_asset_revisions (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    peer_id uuid NOT NULL,
    origin_asset_uri text NOT NULL,
    activity_uri text NOT NULL,
    activity_type text NOT NULL,
    actor_uri text NOT NULL,
    published timestamp with time zone NOT NULL,
    version_uri text NOT NULL,
    file_set_sha256 text NOT NULL,
    mature boolean NOT NULL,
    ai_provenance text,
    record jsonb NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_received_asset_revisions_pkey PRIMARY KEY (id),
    CONSTRAINT federation_received_asset_revisions_peer_id_fkey FOREIGN KEY (peer_id)
        REFERENCES public.federation_peers(id) ON DELETE CASCADE,
    CONSTRAINT federation_received_asset_revisions_activity_key UNIQUE (peer_id, activity_uri),
    CONSTRAINT federation_received_asset_revisions_type_check CHECK (activity_type = ANY (ARRAY['aa:AssetVersion'::text, 'Update'::text])),
    CONSTRAINT federation_received_asset_revisions_ai_check CHECK (ai_provenance IS NULL
        OR ai_provenance = ANY (ARRAY['none'::text, 'assisted'::text, 'generated'::text])),
    CONSTRAINT federation_received_asset_revisions_digest_check CHECK (file_set_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX IF NOT EXISTS federation_received_asset_revisions_order_idx
    ON public.federation_received_asset_revisions USING btree (peer_id, origin_asset_uri, published DESC, activity_uri DESC);

COMMENT ON TABLE public.federation_received_asset_revisions IS 'An origin''s mature and aiProvenance flags of an asset as this instance received them (#1575, spec v1 §17.2.8): one immutable row per accepted aa:AssetVersion or flag Update, ordered by the activity''s signed published, then its URI. The flags shown are the greatest row''s. Read by nothing that decides access.';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.federation_received_asset_revisions_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'received asset revision %: a revision is immutable (#1575)', OLD.id
        USING ERRCODE = 'integrity_constraint_violation';
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_asset_revisions_immutable_trg ON public.federation_received_asset_revisions;
CREATE TRIGGER federation_received_asset_revisions_immutable_trg BEFORE UPDATE ON public.federation_received_asset_revisions
    FOR EACH ROW EXECUTE FUNCTION public.federation_received_asset_revisions_immutable();

-- +goose Down

-- Down refuses while a revision is kept, rather than discard what a peer
-- told this instance: once no row is left (each removed with its peer),
-- it drops the table.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.federation_received_asset_revisions) THEN
        RAISE EXCEPTION '00094 down: received asset revisions are kept; Down would discard what peers told this instance (#1575)'
            USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS federation_received_asset_revisions_immutable_trg ON public.federation_received_asset_revisions;
DROP FUNCTION IF EXISTS public.federation_received_asset_revisions_immutable();
DROP TABLE IF EXISTS public.federation_received_asset_revisions;
