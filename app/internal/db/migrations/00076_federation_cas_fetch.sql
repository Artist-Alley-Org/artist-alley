-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00076_federation_cas_fetch.sql
--
-- #72: receiving a peer's original file safely.
--
-- federation_cas_fetch_leases: at most one download per
-- (peer, origin asset, hash, signer) across every worker. The key is
-- the whole tuple, never the hash alone: a download authorised for one
-- signer of one peer is never shared with another caller, who must pass
-- its own receiver and origin checks first. A lease is taken in one
-- short transaction (no transaction is held across network I/O), kept
-- alive by a heartbeat, released when the import commits or fails, and
-- taken over by anyone once expires_at has passed (a crashed holder).
--
-- storage_import_intents: written BEFORE the backend Put of verified
-- bytes, removed in the same transaction that records the object,
-- variant and pin. An intent left behind means a Put may have happened
-- without its rows (a crash or cancellation in between). The reconciler
-- deletes such bytes only while it holds the per-hash import lock and
-- no storage_objects row and no live intent for the hash exist, so it
-- never removes bytes a concurrent import of the same hash relies on.

-- +goose Up

CREATE TABLE public.federation_cas_fetch_leases (
    peer_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    object_hash text NOT NULL,
    signer text NOT NULL,
    lease_id uuid NOT NULL,
    owner text NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT federation_cas_fetch_leases_pkey PRIMARY KEY (peer_id, asset_id, object_hash, signer),
    CONSTRAINT federation_cas_fetch_leases_hash_check CHECK ((object_hash ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT federation_cas_fetch_leases_peer_id_fkey FOREIGN KEY (peer_id) REFERENCES public.federation_peers(id) ON DELETE CASCADE
);

COMMENT ON TABLE public.federation_cas_fetch_leases IS 'One in-progress original-file download per (peer, origin asset, hash, signer) across all workers (#72). Never keyed by hash alone. Taken in a short transaction, renewed by heartbeat, released on commit or failure, taken over after expires_at.';

CREATE TABLE public.storage_import_intents (
    lease_id uuid NOT NULL,
    object_hash text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT storage_import_intents_pkey PRIMARY KEY (lease_id),
    CONSTRAINT storage_import_intents_hash_check CHECK ((object_hash ~ '^[0-9a-f]{64}$'::text))
);

CREATE INDEX storage_import_intents_hash_idx ON public.storage_import_intents USING btree (object_hash);

COMMENT ON TABLE public.storage_import_intents IS 'A verified import about to Put bytes to the backend (#72). Inserted before the Put, deleted in the transaction that records object, variant and pin. A stale intent is reconciled under the per-hash import lock: the bytes are deleted only when no storage_objects row and no live intent for the hash exist.';

-- +goose Down

DROP TABLE IF EXISTS public.storage_import_intents;
DROP TABLE IF EXISTS public.federation_cas_fetch_leases;
