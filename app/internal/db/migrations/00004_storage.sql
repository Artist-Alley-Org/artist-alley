-- artist-alley migration 00004 — content-addressed storage layer.
--
-- See docs/adr/0008-storage-architecture.md for the full design.
-- Three tables, each with one job:
--
--   storage_objects   — one row per unique byte stream (deduped on hash)
--   storage_variants  — renditions of an object (preview/thumb/HLS/etc.)
--   storage_pins      — reference counts; bytes stay alive while any pin exists
--
-- Federation prep (ADR 0007): origin_server_id on storage_objects so
-- peer-replicated bytes can be distinguished from locally-uploaded ones.

-- +goose Up

CREATE TABLE storage_objects (
    hash             TEXT         PRIMARY KEY
        CHECK (hash ~ '^[0-9a-f]{64}$'),    -- lowercase hex sha256
    size_bytes       BIGINT       NOT NULL CHECK (size_bytes >= 0),
    content_type     TEXT         NOT NULL DEFAULT 'application/octet-stream',
    backend          TEXT         NOT NULL,
    backend_bucket   TEXT         NULL,
    origin_server_id UUID         NULL,
    gc_eligible_at   TIMESTAMPTZ  NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX storage_objects__gc_idx
    ON storage_objects (gc_eligible_at)
    WHERE gc_eligible_at IS NOT NULL;

COMMENT ON TABLE  storage_objects IS 'One row per unique blob, addressed by sha256.';
COMMENT ON COLUMN storage_objects.gc_eligible_at IS
    'Set to NOW()+grace when the last pin is removed. A sweeper deletes the row + backend bytes after this point.';

CREATE TABLE storage_variants (
    object_hash  TEXT         NOT NULL REFERENCES storage_objects(hash) ON DELETE CASCADE,
    variant_key  TEXT         NOT NULL,        -- 'original' | 'preview_2048' | 'hls/seg00001.ts'
    size_bytes   BIGINT       NOT NULL CHECK (size_bytes >= 0),
    content_type TEXT         NOT NULL DEFAULT 'application/octet-stream',
    metadata     JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (object_hash, variant_key)
);

COMMENT ON TABLE storage_variants IS
    'Renditions of an object — original plus any derived size/format. variant_key examples: original, preview_2048, thumb_512, hls/index.m3u8.';

CREATE TABLE storage_pins (
    object_hash      TEXT         NOT NULL REFERENCES storage_objects(hash) ON DELETE RESTRICT,
    pin_subject_type TEXT         NOT NULL,    -- 'resource' | 'avatar' | 'thumbnail' | ...
    pin_subject_id   TEXT         NOT NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (object_hash, pin_subject_type, pin_subject_id)
);

CREATE INDEX storage_pins__subject_idx
    ON storage_pins (pin_subject_type, pin_subject_id);

COMMENT ON TABLE storage_pins IS
    'Reference counts. Many resources can pin the same hash; bytes stay alive while any pin exists.';

-- +goose Down

DROP TABLE IF EXISTS storage_pins;
DROP TABLE IF EXISTS storage_variants;
DROP TABLE IF EXISTS storage_objects;
