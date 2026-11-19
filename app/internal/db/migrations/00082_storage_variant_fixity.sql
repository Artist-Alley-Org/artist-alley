-- SPDX-License-Identifier: AGPL-3.0-only
-- Copyright (C) 2026 Kenneth Blossom

-- 00082_storage_variant_fixity.sql
--
-- #1597: every stored variant carries its own digest, bound to the write
-- that produced it, and an explicit fixity status.
--
-- Until now only the ORIGINAL variant could be checked: it is addressed
-- by its own SHA-256 (object_hash). A derived variant (a preview, a
-- ladder rung, a poster, a sprite sheet) lives under its parent's hash,
-- so the row recorded a size and nothing that could prove which bytes it
-- describes. archival-1 (#1513) rightly omits such copies.
--
-- New columns:
--   content_sha256 / content_size_bytes
--       SHA-256 and length of the bytes actually written for this
--       variant, computed by the server from the stream it handed the
--       backend (storage.Service.PutVariant), or re-read from the backend
--       by an explicit recheck. Never taken from a caller.
--   digest_source   'write' or 'recheck': how the digest was obtained.
--   generation      bumped at the start of every write of the variant;
--                   a digest belongs to the generation whose write
--                   recorded it, and every fixity update is a
--                   compare-and-set on it.
--   fixity_status   verified      digest recorded and, if a sweep has
--                                 read the bytes since, still matching
--                   unverifiable  no trustworthy digest: every row that
--                                 predates this migration, a row healed
--                                 from a backend Stat (#827), and a write
--                                 in progress or interrupted
--                   mismatch      the stored bytes no longer match the
--                                 recorded digest or size
--                   missing       the row's bytes are not on the backend
--   fixity_checked_at  when the status was last established.
--
-- NO BACKFILL HERE. Existing rows take the defaults, so every one of
-- them is 'unverifiable' until an explicit recheck
-- (`aa backfill-variant-digests`) reads its bytes, or a regeneration
-- writes new ones. Nothing is promoted on the strength of a parent hash.
--
-- Cost: ADD COLUMN with constant defaults is metadata-only (PostgreSQL
-- 11+); the CHECKs are validated by one scan of storage_variants inside
-- this migration's transaction.

-- +goose Up

ALTER TABLE public.storage_variants
    ADD COLUMN content_sha256 text,
    ADD COLUMN content_size_bytes bigint,
    ADD COLUMN digest_source text,
    ADD COLUMN generation bigint DEFAULT 0 NOT NULL,
    ADD COLUMN fixity_status text DEFAULT 'unverifiable'::text NOT NULL,
    ADD COLUMN fixity_checked_at timestamp with time zone;

ALTER TABLE public.storage_variants
    ADD CONSTRAINT storage_variants_fixity_status_check
        CHECK (fixity_status = ANY (ARRAY['verified'::text, 'unverifiable'::text, 'mismatch'::text, 'missing'::text])),
    ADD CONSTRAINT storage_variants_content_sha256_check
        CHECK (content_sha256 IS NULL OR content_sha256 ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT storage_variants_content_size_bytes_check
        CHECK (content_size_bytes IS NULL OR content_size_bytes >= 0),
    ADD CONSTRAINT storage_variants_digest_source_check
        CHECK (digest_source IS NULL OR digest_source = ANY (ARRAY['write'::text, 'recheck'::text])),
    -- A digest is recorded whole or not at all.
    ADD CONSTRAINT storage_variants_digest_complete_check
        CHECK (((content_sha256 IS NULL) = (content_size_bytes IS NULL)) AND ((content_sha256 IS NULL) = (digest_source IS NULL))),
    -- verified and mismatch are statements about a recorded digest;
    -- unverifiable means there is none.
    ADD CONSTRAINT storage_variants_fixity_digest_check
        CHECK (CASE fixity_status
                   WHEN 'verified' THEN content_sha256 IS NOT NULL
                   WHEN 'mismatch' THEN content_sha256 IS NOT NULL
                   WHEN 'unverifiable' THEN content_sha256 IS NULL
                   ELSE true
               END),
    ADD CONSTRAINT storage_variants_generation_check CHECK (generation >= 0),
    -- A recorded digest and the row's size describe the same bytes:
    -- whenever there is a digest, its size is size_bytes. Legacy rows
    -- have no digest, so this holds for them as they are.
    ADD CONSTRAINT storage_variants_digest_size_check
        CHECK (content_sha256 IS NULL OR content_size_bytes = size_bytes);

COMMENT ON COLUMN public.storage_variants.content_sha256 IS 'SHA-256 of the bytes actually written for THIS variant (#1597), computed by the server while streaming them to the backend, or re-read by an explicit recheck. Never caller-supplied. NULL when unverifiable.';
COMMENT ON COLUMN public.storage_variants.generation IS 'Bumped at the start of every write of this variant (#1597). The digest belongs to the generation that recorded it; every fixity update compares-and-sets on it.';
COMMENT ON COLUMN public.storage_variants.fixity_status IS 'verified | unverifiable | mismatch | missing (#1597). Rows predating migration 00082 are unverifiable until rechecked or regenerated; nothing is promoted from a parent hash.';

-- +goose Down

ALTER TABLE public.storage_variants
    DROP CONSTRAINT IF EXISTS storage_variants_digest_size_check,
    DROP CONSTRAINT IF EXISTS storage_variants_generation_check,
    DROP CONSTRAINT IF EXISTS storage_variants_fixity_digest_check,
    DROP CONSTRAINT IF EXISTS storage_variants_digest_complete_check,
    DROP CONSTRAINT IF EXISTS storage_variants_digest_source_check,
    DROP CONSTRAINT IF EXISTS storage_variants_content_size_bytes_check,
    DROP CONSTRAINT IF EXISTS storage_variants_content_sha256_check,
    DROP CONSTRAINT IF EXISTS storage_variants_fixity_status_check,
    DROP COLUMN IF EXISTS fixity_checked_at,
    DROP COLUMN IF EXISTS fixity_status,
    DROP COLUMN IF EXISTS generation,
    DROP COLUMN IF EXISTS digest_source,
    DROP COLUMN IF EXISTS content_size_bytes,
    DROP COLUMN IF EXISTS content_sha256;
