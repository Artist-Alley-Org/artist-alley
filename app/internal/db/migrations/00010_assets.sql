-- artist-alley migration 00010 — the assets entity (Phase 1.8).
--
-- See ADR 0011 for the rationale. Three tables:
--   assets       — UUID-keyed user-facing record
--   asset_tag    — many-to-many tag join (indexable, beats jsonb at scale)
--   (storage_objects already exists from migration 00004; assets.file_hash FKs into it)
--
-- RS's `resource` table (migration 00007) stays. The two coexist until
-- PHP pages that read `resource` move to Go and the legacy table can
-- be dropped.

-- +goose Up

CREATE TABLE assets (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    title              TEXT         NOT NULL DEFAULT '',
    description        TEXT         NOT NULL DEFAULT '',
    resource_type      BIGINT       NOT NULL REFERENCES resource_type(ref),
    owner_user_ref     BIGINT       NULL,                     -- nullable: system/imported uploads
    status             TEXT         NOT NULL DEFAULT 'active'
                                    CHECK (status IN ('draft','active','archived')),
    file_hash          TEXT         NULL REFERENCES storage_objects(hash) ON DELETE SET NULL,
    file_extension     TEXT         NULL,
    file_size_bytes    BIGINT       NULL,

    -- RS columns kept until porting reveals what's used. Drop in a
    -- follow-up migration once the last PHP page that reads them is
    -- replaced by Go.
    rating             INTEGER      NULL,
    user_rating        REAL         NULL,
    hit_count          BIGINT       NOT NULL DEFAULT 0,
    new_hit_count      BIGINT       NOT NULL DEFAULT 0,
    request_count      BIGINT       NOT NULL DEFAULT 0,
    archive_state      INTEGER      NOT NULL DEFAULT 0,
    access             INTEGER      NOT NULL DEFAULT 0,
    thumb_width        INTEGER      NULL,
    thumb_height       INTEGER      NULL,
    image_red          SMALLINT     NULL,
    image_green        SMALLINT     NULL,
    image_blue         SMALLINT     NULL,
    colour_key         TEXT         NULL,
    geo_lat            DOUBLE PRECISION NULL,
    geo_long           DOUBLE PRECISION NULL,
    country            TEXT         NULL,
    has_image          BOOLEAN      NOT NULL DEFAULT FALSE,
    is_transcoding     BOOLEAN      NOT NULL DEFAULT FALSE,

    -- artist-alley additions.
    metadata           JSONB        NOT NULL DEFAULT '{}'::jsonb,
    origin_server_id   UUID         NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ  NULL
);

CREATE INDEX assets_owner_idx
    ON assets (owner_user_ref) WHERE deleted_at IS NULL AND owner_user_ref IS NOT NULL;
CREATE INDEX assets_type_idx
    ON assets (resource_type)  WHERE deleted_at IS NULL;
CREATE INDEX assets_status_idx
    ON assets (status)         WHERE deleted_at IS NULL;
CREATE INDEX assets_file_hash_idx
    ON assets (file_hash)      WHERE file_hash IS NOT NULL;
CREATE INDEX assets_created_at_idx
    ON assets (created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX assets_metadata_gin
    ON assets USING gin (metadata);

COMMENT ON TABLE  assets IS
    'User-facing artwork/file entity. UUID-keyed for federation. file_hash points at storage_objects for the byte plane.';
COMMENT ON COLUMN assets.status IS
    'artist-alley lifecycle: draft (work-in-progress, owner-only) / active / archived. Distinct from RS archive_state which is kept for PHP-side compat.';
COMMENT ON COLUMN assets.metadata IS
    'Free-form JSONB for EXIF, extracted technical metadata, custom fields. GIN-indexed.';

CREATE TABLE asset_tag (
    asset_id   UUID         NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag        TEXT         NOT NULL,
    added_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (asset_id, tag)
);

CREATE INDEX asset_tag_tag_idx ON asset_tag (tag);

-- +goose Down

DROP TABLE IF EXISTS asset_tag;
DROP TABLE IF EXISTS assets;
