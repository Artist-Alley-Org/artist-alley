-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00074_remote_actor_signing_key.sql
--
-- #1526: the pinned Ed25519 signing key of a remote actor.
--
-- The inbox verifies every envelope's per-actor signature (spec §5.3,
-- erratum E1). The key comes from the actor document the sending
-- peer serves at the actor URI, fetched once with this instance's
-- signed HTTP request and then pinned here: trust on first use, bound
-- to the actor URI (the primary key) and to peer_id. A pinned key is
-- never replaced automatically; a peer that rotates a user's key is
-- refused with sig_invalid until an operator clears the pin (key
-- rotation is not implemented, see #1581). Deleting the peer cascades
-- to these rows, so re-adding a peer starts from fresh pins.
--
-- The three columns are all NULL (no key fetched yet) or all set, the
-- same shape as the encryption-key columns. Plain DDL, no backfill.

-- +goose Up

ALTER TABLE public.federation_remote_actors
    ADD COLUMN signing_public_key bytea,
    ADD COLUMN signing_key_id text,
    ADD COLUMN signing_key_pinned_at timestamp with time zone,
    ADD CONSTRAINT federation_remote_actors_signing_key_atomic CHECK (((signing_public_key IS NULL) AND (signing_key_id IS NULL) AND (signing_key_pinned_at IS NULL)) OR ((signing_public_key IS NOT NULL) AND (octet_length(signing_public_key) = 32) AND (signing_key_id IS NOT NULL) AND (signing_key_pinned_at IS NOT NULL)));

COMMENT ON COLUMN public.federation_remote_actors.signing_public_key IS 'Pinned raw 32-byte Ed25519 public key of the remote actor (#1526), from its actor document. Envelopes from this actor must verify against it. Never replaced automatically.';
COMMENT ON COLUMN public.federation_remote_actors.signing_key_id IS 'Key id the actor document published with the pinned key: <actor_uri>#main-key.';
COMMENT ON COLUMN public.federation_remote_actors.signing_key_pinned_at IS 'When the signing key was first fetched and pinned.';

-- +goose Down

ALTER TABLE public.federation_remote_actors
    DROP CONSTRAINT IF EXISTS federation_remote_actors_signing_key_atomic,
    DROP COLUMN IF EXISTS signing_key_pinned_at,
    DROP COLUMN IF EXISTS signing_key_id,
    DROP COLUMN IF EXISTS signing_public_key;
