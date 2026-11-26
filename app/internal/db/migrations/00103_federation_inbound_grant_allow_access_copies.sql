-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00103_federation_inbound_grant_allow_access_copies.sql
--
-- #1597: the receiver's record that a peer's grant carries the access-copy
-- grant (aa:accessTransfer, spec v1 §16.7), beside allow_original (00077).
--
-- It is independent of the grant's scope and of allow_original: neither
-- implies the other. Only asset and collection grants can carry it, as
-- for allow_original. It is recorded from a validated aa:Share only when
-- derived-transfer-1 (with archival-1, original-transfer-1 and
-- share-lifecycle-1) is in the stored intersection, and it authorises an
-- access copy only while the grant is live (inboundgrants.Store.Allows).
--
-- Every existing row becomes false: no grant received before this column
-- existed carried it. Plain DDL; the constant default fills existing rows
-- without a rewrite.

-- +goose Up

ALTER TABLE public.federation_inbound_grants
    ADD COLUMN allow_access_copies boolean DEFAULT false NOT NULL,
    ADD CONSTRAINT federation_inbound_grants_allow_access_copies_kind_check CHECK (((NOT allow_access_copies) OR (object_kind = ANY (ARRAY['asset'::text, 'collection'::text]))));

COMMENT ON COLUMN public.federation_inbound_grants.allow_access_copies IS 'The peer''s grant carries the access-copy grant (aa:accessTransfer, #1597, spec v1 §16.7): the receiver may fetch the access copies of the granted asset''s sealed versions, or of each member asset of a granted collection, while the grant is live. Independent of scope and of allow_original. Default false; only asset and collection grants may carry it.';

-- +goose Down

ALTER TABLE public.federation_inbound_grants
    DROP CONSTRAINT IF EXISTS federation_inbound_grants_allow_access_copies_kind_check,
    DROP COLUMN IF EXISTS allow_access_copies;
