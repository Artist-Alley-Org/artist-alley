-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00090_federation_awaiting_grant.sql
--
-- #1574 slice 3: a received asset record may wait for its grant. The
-- sender announces an asset only after its aa:Share was delivered, but the
-- receiver's inbox dispatches the two independently, so the announcement
-- can be bound before the share is applied. A pending record whose peer and
-- asset have NO inbound grant row at all now waits with the reason
-- awaiting_grant, bounded (assetingest Config.GrantWait, default 24 hours
-- from received_at), fetching nothing and using no attempt; the grant's
-- arrival wakes it, and the wait running out rejects it as
-- no_inbound_grant. The record's and the ledger's closed reason sets gain
-- awaiting_grant; nothing else changes, and every existing row already
-- satisfies the wider sets.

-- +goose Up

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text]));

ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_reason_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text, 'awaiting_grant'::text])));

-- +goose Down

-- The 00089 sets again, for new writes. NOT VALID: a record or ledger row
-- that already says awaiting_grant keeps the reason that says what
-- happened; only new rows are held to the old sets.
ALTER TABLE public.federation_transfer_events DROP CONSTRAINT federation_transfer_events_reason_check;
ALTER TABLE public.federation_transfer_events ADD CONSTRAINT federation_transfer_events_reason_check CHECK ((outcome = 'success'::text) = (reason IS NULL) AND (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text]))) NOT VALID;

ALTER TABLE public.federation_received_assets DROP CONSTRAINT federation_received_assets_reason_check;
ALTER TABLE public.federation_received_assets ADD CONSTRAINT federation_received_assets_reason_check CHECK (reason IS NULL OR reason = ANY (ARRAY[
        'no_inbound_grant'::text, 'fixity_mismatch'::text, 'length_mismatch'::text, 'origin_refused'::text,
        'revoked_during_fetch'::text, 'too_large'::text, 'sensitivity_refused'::text, 'version_conflict'::text,
        'version_unsupported'::text, 'redirect_refused'::text, 'class_not_available'::text, 'foreign_origin'::text,
        'foreign_signer'::text, 'invalid_request'::text, 'http_status'::text, 'timeout'::text,
        'transport_error'::text, 'lease_lost'::text, 'storage_error'::text, 'peer_unavailable'::text,
        'profile_not_negotiated'::text, 'archival_invalid'::text])) NOT VALID;
