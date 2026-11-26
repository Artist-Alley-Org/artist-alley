-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00102_federation_share_allow_access_copies.sql
--
-- #1597: an explicit, default-off permission for a peer to receive the
-- ACCESS COPIES of a shared asset's sealed versions (spec v1 §7.5.7
-- "Access copies", ADR 0103).
--
-- It is independent of the share's scope and of allow_original (00075):
-- no scope implies it, allow_original does not imply it, and it does not
-- imply allow_original. Only asset and collection shares can carry it,
-- for the same reason as allow_original: only they reach stored bytes.
-- The origin's row is the only authority; the tier cap, the sealed
-- version, the verified copy and the per-peer announcement are checked
-- at every fetch decision (shares.CanPeerFetchAccessCopy), never stored
-- here.
--
-- Every existing row becomes false: no grant made before this column
-- existed gave access copies, and none does now. Nothing sets it yet.
-- Plain DDL; the constant default fills existing rows without a rewrite.

-- +goose Up

ALTER TABLE public.federation_shares
    ADD COLUMN allow_access_copies boolean DEFAULT false NOT NULL,
    ADD CONSTRAINT federation_shares_allow_access_copies_kind_check CHECK (((NOT allow_access_copies) OR (object_kind = ANY (ARRAY['asset'::text, 'collection'::text]))));

COMMENT ON COLUMN public.federation_shares.allow_access_copies IS 'Explicit grant for the peer to receive the ACCESS COPIES of the shared asset''s sealed versions, or of each member asset of a shared collection (#1597, spec v1 §7.5.7). Independent of scope and of allow_original: neither implies the other. Default false; only asset and collection shares may set it. Public and team assets only, checked at every fetch decision. Ends with the share (revoke, expiry). Does not recall bytes already delivered.';

-- +goose Down

ALTER TABLE public.federation_shares
    DROP CONSTRAINT IF EXISTS federation_shares_allow_access_copies_kind_check,
    DROP COLUMN IF EXISTS allow_access_copies;
