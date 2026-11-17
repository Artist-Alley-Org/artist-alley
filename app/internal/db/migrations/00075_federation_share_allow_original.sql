-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00075_federation_share_allow_original.sql
--
-- #1573: an explicit, default-off permission for a peer to receive the
-- ORIGINAL bytes of a shared asset.
--
-- A share's scope (view < comment < annotate < remix) is an interaction
-- ladder and never implies original bytes, remix included. Original
-- transfer needs this separate grant on the origin's own row, and the
-- origin's row is the only authority: nothing on the wire turns it on.
--
-- Only asset shares and collection shares can carry it, because only
-- they reach stored bytes (a collection reaches its member assets, each
-- checked on its own at fetch time). The sensitivity cap (public and
-- team only) is enforced when the grant is made and again at every
-- fetch decision, never stored here.
--
-- Every existing row becomes false: no grant made before this column
-- existed ever gave original bytes, and none does now. Plain DDL; the
-- constant default fills existing rows without a rewrite.

-- +goose Up

ALTER TABLE public.federation_shares
    ADD COLUMN allow_original boolean DEFAULT false NOT NULL,
    ADD CONSTRAINT federation_shares_allow_original_kind_check CHECK (((NOT allow_original) OR (object_kind = ANY (ARRAY['asset'::text, 'collection'::text]))));

COMMENT ON COLUMN public.federation_shares.allow_original IS 'Explicit grant for the peer to receive the ORIGINAL bytes of the shared asset, or of each member asset of a shared collection (#1573). Independent of scope: no scope, remix included, implies it. Default false; only asset and collection shares may set it. Public and team assets only, checked at grant time and at every fetch decision; restricted, embargoed and unresolved tiers are refused even when this is true. Ends with the share (revoke, expiry). Does not recall bytes already delivered.';

-- +goose Down

ALTER TABLE public.federation_shares
    DROP CONSTRAINT IF EXISTS federation_shares_allow_original_kind_check,
    DROP COLUMN IF EXISTS allow_original;
