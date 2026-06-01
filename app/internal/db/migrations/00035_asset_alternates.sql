-- artist-alley migration 00035 — alternative files for assets.
--
-- Sprite-tool Phase 9 / future painting track Phase 13+.
--
-- Where companions are SIDECARS the primary asset references
-- (textures for a 3D model, JSON frame atlas for a sprite),
-- alternates are SIBLING VERSIONS of the asset itself:
--
--   * a sprite sheet with a palette swap applied (sprite tool
--     Phase 9 — "what if this character had the green-team
--     colours")
--   * a low-res JPEG of a high-res TIFF (web preview pipeline)
--   * a transcoded 1080p MP4 of a 4K source (video pipeline)
--   * an authored variant produced by the future painting track
--     (Phase 13+) — same asset, different pixels
--
-- The original asset stays primary. Alternates are listed in the
-- UI as additional downloads and can carry kind-specific metadata
-- in the JSONB column (sprite-tool palette swap records its
-- "old colour → new colour" map there, the thumbnail pipeline
-- records dimensions, etc.).
--
-- Storage shape: same as companions. Each alternate is a content-
-- addressed blob in storage_objects, deduped on hash; the row
-- carries metadata + a pin so the GC keeps the bytes alive.
--
-- Pinning: each alternate gets its own pin
-- (subject_type='asset_alternate', subject_id=alternate.id). Two
-- alternates with the same bytes (e.g., generated twice with
-- identical settings) share a storage object and each holds a
-- distinct pin.
--
-- Federation prep: origin_server_id inherits from the parent
-- asset's federation routing for now but lives on the alternate
-- so a future per-alternate origin (federated palette-swap done
-- by another instance and replicated back) doesn't need a
-- schema change.

-- +goose Up

CREATE TABLE asset_alternates (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id        UUID         NOT NULL
                    REFERENCES assets(id) ON DELETE CASCADE,
    -- Human label shown in alt-file lists. Required so the UI never
    -- has to invent a name; the sprite tool fills it as
    -- "palette swap — <pretty name>", the thumbnail pipeline as
    -- "thumb-512", etc. Unique per asset so the UI list is stable.
    label           TEXT         NOT NULL
                    CHECK (length(label) BETWEEN 1 AND 256),
    -- Provenance taxonomy. Free-form text so new generators can
    -- show up without a migration; the UI groups + filters by kind
    -- but doesn't enforce a closed enum.
    --   'palette_swap'  — sprite tool palette remap
    --   'thumbnail'     — image pipeline thumbnail (web)
    --   'low_res'       — image pipeline lower-res variant
    --   'authored'      — human-authored variant (paint track)
    --   'transcode'     — codec / container swap
    kind            TEXT         NOT NULL DEFAULT 'authored'
                    CHECK (length(kind) BETWEEN 1 AND 64),
    -- Content-addressed blob — same dedup story as everything else.
    object_hash     TEXT         NOT NULL
                    REFERENCES storage_objects(hash),
    content_type    TEXT         NOT NULL DEFAULT 'application/octet-stream',
    size_bytes      BIGINT       NOT NULL CHECK (size_bytes >= 0),
    -- Federation: explicit so a future per-alternate origin can be
    -- routed without altering the table. NULL means "inherits from
    -- the parent asset's origin", which is the local instance for
    -- everything pre-federation.
    origin_server_id UUID,
    -- Who generated this. NULL when system-generated (thumbnail
    -- worker, etc.). BIGINT user_ref matches the rest of the schema
    -- (assets.owner_user_ref, fields.created_by_user_ref); no FK so
    -- the audit row survives the creator being deleted later.
    created_by_user_ref BIGINT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Free-form per-kind metadata. Sprite-tool palette swap stores
    -- {"source_palette": [...], "remap": [{"from":"#aabbcc","to":"#112233"},...]}.
    -- Thumbnail worker stores {"width":512,"height":512,"format":"webp"}.
    -- JSONB so adding fields doesn't need a migration.
    metadata        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (asset_id, label)
);

CREATE INDEX asset_alternates__asset_idx
    ON asset_alternates (asset_id);

CREATE INDEX asset_alternates__hash_idx
    ON asset_alternates (object_hash);

CREATE INDEX asset_alternates__kind_idx
    ON asset_alternates (asset_id, kind);

COMMENT ON TABLE asset_alternates IS
    'Sibling-version variants of an asset (palette swaps, transcodes, thumbnails, authored variants).';
COMMENT ON COLUMN asset_alternates.kind IS
    'Provenance tag — palette_swap | thumbnail | low_res | authored | transcode; open-ended.';
COMMENT ON COLUMN asset_alternates.metadata IS
    'Kind-specific JSONB — sprite tool stores the colour remap, thumbnail worker stores dimensions, etc.';

-- +goose Down

DROP TABLE IF EXISTS asset_alternates;
