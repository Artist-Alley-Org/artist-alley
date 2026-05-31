-- artist-alley migration 00030 — brush packs (Phase 1.21).
--
-- Stores user-imported brush packs (Photoshop .abr files, our own
-- native packs once we add an in-app editor) + the per-stamp
-- metadata. The actual stamp bitmaps live in object storage —
-- this table just carries the manifest.
--
-- Design notes
-- ------------
-- * Packs are owned by a single user (`owner_ref`). Sharing /
--   team-wide visibility is a later phase; for now any "public"
--   stamp lives in the built-in pack shipped with the frontend
--   (no DB row).
-- * `origin_server_id` is set for federation-readiness — every
--   federated row carries the home server's id so cross-instance
--   sync can dedup. NULL = local to this instance.
-- * Stamps are NOT deduplicated across packs even when they share
--   the same `abr_id`. Two users importing the same Kyle T. Webster
--   pack each get their own pack + stamp rows; the storage backend
--   is what gets to dedup the bitmap bytes (e.g., S3 content-
--   addressed storage in prod).
-- * `storage_key` is the abstract storage path the backend resolves
--   via `internal/storage`. Format: `brush-packs/<pack_ref>/<stamp_ref>.png`
--   — the same key works for fs (dev) + S3 (prod).
-- * Dynamics (spacing / jitter) are split into columns so a query
--   can filter by behaviour (e.g., "all packs with align-to-path
--   stamps") without unpacking JSONB. New dynamics fields land as
--   their own columns rather than a JSONB blob — keeps the data
--   contract typed end-to-end.

-- +goose Up

CREATE TABLE brush_packs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_ref BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    name TEXT NOT NULL,
    -- Original filename when the pack was imported (e.g., "kyle-pack.abr").
    -- Display-only; for re-export the user picks a new name.
    source_file TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Federation: NULL = local to this instance; set when the pack
    -- was synced from another server.
    origin_server_id UUID
);

CREATE INDEX brush_packs_owner_idx ON brush_packs(owner_ref);

CREATE TABLE brush_pack_stamps (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pack_id UUID NOT NULL REFERENCES brush_packs(id) ON DELETE CASCADE,
    -- The original ABR UUID when imported from .abr. NULL for stamps
    -- authored in-app (future feature). Used at re-import time to
    -- dedup against the same source pack.
    abr_id TEXT,
    -- Optional human label. ABR imports populate from the descriptor
    -- block when available; falls back to a hashed short name when
    -- the descriptor doesn't carry one.
    label TEXT,
    -- Stamp bitmap dimensions in source pixels (matches the PNG in
    -- object storage). The frontend renderer scales at draw time.
    width INTEGER NOT NULL CHECK (width > 0),
    height INTEGER NOT NULL CHECK (height > 0),
    -- Abstract storage key resolved by internal/storage. Always
    -- `brush-packs/<pack_id>/<stamp_id>.png` today; could become
    -- content-addressed later (`brush-packs/sha256/<hash>.png`) for
    -- dedup without changing the schema.
    storage_key TEXT NOT NULL,
    -- Stamp dynamics. spacing is the fraction-of-diameter step;
    -- jitter values are 0..1 (size / opacity) or 0..360 (angle in
    -- degrees). Jitter columns are nullable = "no jitter, render
    -- deterministically" so the renderer can short-circuit.
    spacing DOUBLE PRECISION NOT NULL DEFAULT 0.1
        CHECK (spacing > 0 AND spacing <= 10),
    align_to_path BOOLEAN NOT NULL DEFAULT FALSE,
    size_jitter DOUBLE PRECISION CHECK (size_jitter IS NULL OR (size_jitter >= 0 AND size_jitter <= 1)),
    opacity_jitter DOUBLE PRECISION CHECK (opacity_jitter IS NULL OR (opacity_jitter >= 0 AND opacity_jitter <= 1)),
    angle_jitter DOUBLE PRECISION CHECK (angle_jitter IS NULL OR (angle_jitter >= 0 AND angle_jitter <= 360)),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX brush_pack_stamps_pack_idx ON brush_pack_stamps(pack_id);
-- Re-import dedup hint: lookup is per-pack so the unique constraint
-- composes pack_id + abr_id. NULL abr_ids (in-app authored stamps)
-- are allowed to repeat freely.
CREATE UNIQUE INDEX brush_pack_stamps_pack_abr_uniq
    ON brush_pack_stamps(pack_id, abr_id)
    WHERE abr_id IS NOT NULL;

-- +goose Down

DROP TABLE IF EXISTS brush_pack_stamps;
DROP TABLE IF EXISTS brush_packs;
