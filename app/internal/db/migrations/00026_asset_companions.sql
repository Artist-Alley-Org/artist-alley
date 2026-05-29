-- artist-alley migration 00026 — companion files for 3D model assets.
--
-- Phase 1.18.B-12c-1.
--
-- Many 3D model formats reference external resources by relative path:
--
--   * OBJ + MTL — the .obj line `mtllib character.mtl` plus the .mtl's
--                 own `map_Kd textures/foo.png` chain
--   * glTF JSON + .bin + textures
--   * GLB with external (non-embedded) textures
--   * FBX exports that ship sibling texture folders
--
-- The model file itself becomes the asset; its companions live here
-- and are referenced by the relative path the model expects them at.
-- The frontend viewer wires a three.js LoadingManager URL modifier to
-- rewrite every relative path the loader asks for into a companion
-- fetch URL — so as long as the user uploaded the right files, the
-- model just resolves.
--
-- Storage shape: each companion is its own content-addressed blob in
-- storage_objects, deduped on hash like everything else. The
-- asset_companions row is metadata that maps "this asset, this
-- relative path → this blob". Two assets uploading the same texture
-- share storage automatically.
--
-- Pinning: each companion row owns one pin (subject_type =
-- 'asset_companion', subject_id = the asset_companions.id). Deleting
-- the companion row removes the pin, the sweeper GCs the blob when
-- the last pin on that hash goes away.
--
-- Federation prep: origin_server_id on the asset already covers the
-- whole bundle — companions inherit the asset's origin.

-- +goose Up

CREATE TABLE asset_companions (
    id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id        UUID         NOT NULL
                    REFERENCES assets(id) ON DELETE CASCADE,
    -- The relative path the model file uses to reference this
    -- companion. For OBJ this is the bare filename ('character.mtl').
    -- For glTF with a textures subdir this is 'textures/foo.png'. The
    -- LoadingManager URL modifier on the frontend matches against
    -- this string.
    companion_path  TEXT         NOT NULL
                    CHECK (length(companion_path) BETWEEN 1 AND 512),
    -- The bytes themselves live in storage_objects by hash. Same
    -- deduplication semantics as every other blob.
    object_hash     TEXT         NOT NULL
                    REFERENCES storage_objects(hash),
    content_type    TEXT         NOT NULL DEFAULT 'application/octet-stream',
    size_bytes      BIGINT       NOT NULL CHECK (size_bytes >= 0),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (asset_id, companion_path)
);

CREATE INDEX asset_companions__asset_idx
    ON asset_companions (asset_id);

CREATE INDEX asset_companions__hash_idx
    ON asset_companions (object_hash);

COMMENT ON TABLE asset_companions IS
    'Sidecar files for an asset (textures, MTL, .bin) referenced by relative path.';
COMMENT ON COLUMN asset_companions.companion_path IS
    'Relative path the model file references this companion at — used by the viewer LoadingManager to rewrite resource URLs.';

-- +goose Down

DROP TABLE IF EXISTS asset_companions;
