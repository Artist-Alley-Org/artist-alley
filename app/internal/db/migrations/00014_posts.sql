-- artist-alley migration 00014 — posts (Phase 1.13.D-2).
--
-- Per the post-model decision: a Post is the feed entity. It owns
-- 1+ Assets via post_assets, the descriptive metadata (title,
-- description, tags, visibility) and counters. Browse shows Posts;
-- Assets are the file-bearing members beneath them.
--
-- Single-asset uploads still become Posts (a Post with one member).
-- This removes the "RS alt-files hack" — every file is a real Asset,
-- and the social/feed shell sits cleanly on top.
--
-- Ships in this migration:
--   - posts                — the feed entity
--   - post_assets          — many-to-many post ↔ asset (with cover)
--   - post_tags            — tags belong to the post, not the asset
--   - collection_posts     — collections curate Posts (not raw assets)
--   - search_text TSVECTOR on posts, maintained by triggers across
--     the post fields, post_tags, and member-asset search_text
--   - data migration: one Post per existing owned Asset, so the
--     current browse view doesn't go dark
--
-- Phase 1.13.D-4 will add comments/likes/follows tables and their
-- count triggers. Like_count/comment_count columns ship here as
-- NOT NULL DEFAULT 0 so the API surface can include them now.

-- +goose Up

-- ---------------------------------------------------------------------------
-- posts: the feed entity
-- ---------------------------------------------------------------------------

CREATE TABLE posts (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    author_user_ref   BIGINT       NOT NULL,
    title             TEXT         NOT NULL DEFAULT '',
    description       TEXT         NOT NULL DEFAULT '',
    visibility        TEXT         NOT NULL DEFAULT 'public'
                                   CHECK (visibility IN ('private','followers','public')),
    -- The asset that represents the post in feed/grid views. NULL
    -- only transiently during multi-step upload (post created, no
    -- members yet). NOT NULL would force us to upload+attach
    -- atomically; we allow the looser shape and let the handler
    -- decide the policy.
    cover_asset_id    UUID         NULL REFERENCES assets(id) ON DELETE SET NULL,
    posted_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Denormalised counters. Triggers in 1.13.D-4 will maintain
    -- these. Kept here so the API shape doesn't churn between phases.
    like_count        BIGINT       NOT NULL DEFAULT 0,
    comment_count     BIGINT       NOT NULL DEFAULT 0,
    search_text       TSVECTOR     NULL,
    -- Federation prep — see ADR 0007. NULL = local.
    origin_server_id  UUID         NULL,
    deleted_at        TIMESTAMPTZ  NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX posts_author_idx
    ON posts (author_user_ref, posted_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX posts_public_feed_idx
    ON posts (posted_at DESC)
    WHERE deleted_at IS NULL AND visibility = 'public';
CREATE INDEX posts_visibility_idx
    ON posts (visibility)
    WHERE deleted_at IS NULL;
CREATE INDEX posts_search_text_gin
    ON posts USING gin (search_text)
    WHERE deleted_at IS NULL;

COMMENT ON TABLE  posts IS
    'Feed entity per the post-model decision. Wraps 1+ assets with descriptive metadata. Browse shows posts; assets are members. Phase 1.13.D-2.';
COMMENT ON COLUMN posts.cover_asset_id IS
    'Asset shown in feed/grid views. Transiently NULL during multi-step upload.';
COMMENT ON COLUMN posts.visibility IS
    'public = anyone (incl. anon if anonymous browse is on); followers = author''s followers; private = author only.';
COMMENT ON COLUMN posts.like_count IS
    'Denormalised counter, maintained by triggers from likes table (Phase 1.13.D-4).';
COMMENT ON COLUMN posts.comment_count IS
    'Denormalised counter, maintained by triggers from comments table (Phase 1.13.D-4).';

-- ---------------------------------------------------------------------------
-- post_assets: many-to-many post ↔ asset
-- ---------------------------------------------------------------------------

CREATE TABLE post_assets (
    post_id     UUID         NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    asset_id    UUID         NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    sort_order  INTEGER      NOT NULL DEFAULT 0,
    added_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (post_id, asset_id)
);

CREATE INDEX post_assets_asset_idx ON post_assets (asset_id);
CREATE INDEX post_assets_sort_idx  ON post_assets (post_id, sort_order, added_at);

COMMENT ON TABLE post_assets IS
    'Membership of assets in a post. A single asset may belong to multiple posts (rare in practice — e.g. cross-posted images).';

-- ---------------------------------------------------------------------------
-- post_tags: tags belong to the post, not the underlying asset
-- ---------------------------------------------------------------------------

CREATE TABLE post_tags (
    post_id  UUID  NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    tag      TEXT  NOT NULL,
    PRIMARY KEY (post_id, tag)
);

CREATE INDEX post_tags_tag_idx ON post_tags (tag);

COMMENT ON TABLE post_tags IS
    'Tags scoped to a post. asset_tag remains for low-level/legacy tagging. Phase 1.12 (search DSL) will canonicalise the two.';

-- ---------------------------------------------------------------------------
-- collection_posts: collections curate Posts
-- ---------------------------------------------------------------------------
--
-- Collections from Phase 1.11.A pointed at raw assets via
-- collection_resources. Per the post-model decision, collections
-- should curate Posts (which can themselves wrap multiple assets).
-- collection_resources stays for the asset-level legacy/granular case;
-- collection_posts is the new primary entity the feed-aware UI uses.

CREATE TABLE collection_posts (
    collection_id  UUID         NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    post_id        UUID         NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    sort_order     INTEGER      NOT NULL DEFAULT 0,
    pinned         BOOLEAN      NOT NULL DEFAULT TRUE,
    expires_at     TIMESTAMPTZ  NULL,
    added_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (collection_id, post_id)
);

CREATE INDEX collection_posts_post_idx    ON collection_posts (post_id);
CREATE INDEX collection_posts_expires_idx ON collection_posts (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX collection_posts_sort_idx    ON collection_posts (collection_id, sort_order, added_at);

COMMENT ON TABLE collection_posts IS
    'Curated post-set membership. pinned/expires_at mirror collection_resources; smart-collection auto-add lands with Phase 1.12.';

-- ---------------------------------------------------------------------------
-- search_text rebuild trigger
-- ---------------------------------------------------------------------------
--
-- A post's searchable text is the union of:
--   - the post's own title + description
--   - the post's tags
--   - the search_text of every member asset
--
-- Rebuilt on: post insert/update of title/description; post_tags
-- insert/update/delete; post_assets insert/update/delete. The member
-- assets' own search_text is maintained by the Phase 1.9 metadata
-- trigger; we just read from it.

CREATE OR REPLACE FUNCTION rebuild_post_search_text(p_post_id UUID) RETURNS VOID AS $$
DECLARE
    asset_search TEXT;
    post_tag_text TEXT;
BEGIN
    SELECT COALESCE(string_agg(COALESCE(a.search_text::text, ''), ' '), '')
      INTO asset_search
      FROM post_assets pa
      JOIN assets a ON a.id = pa.asset_id
     WHERE pa.post_id = p_post_id AND a.deleted_at IS NULL;

    SELECT COALESCE(string_agg(tag, ' '), '')
      INTO post_tag_text
      FROM post_tags
     WHERE post_id = p_post_id;

    UPDATE posts
       SET search_text = to_tsvector('english',
                coalesce(title, '')       || ' ' ||
                coalesce(description, '') || ' ' ||
                coalesce(post_tag_text, '') || ' ' ||
                coalesce(asset_search, ''))
     WHERE id = p_post_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION posts_search_text_trigger() RETURNS TRIGGER AS $$
BEGIN
    PERFORM rebuild_post_search_text(NEW.id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER posts_search_text
AFTER INSERT OR UPDATE OF title, description ON posts
FOR EACH ROW EXECUTE FUNCTION posts_search_text_trigger();

CREATE OR REPLACE FUNCTION post_tags_search_text_trigger() RETURNS TRIGGER AS $$
BEGIN
    PERFORM rebuild_post_search_text(COALESCE(NEW.post_id, OLD.post_id));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER post_tags_search_text
AFTER INSERT OR UPDATE OR DELETE ON post_tags
FOR EACH ROW EXECUTE FUNCTION post_tags_search_text_trigger();

CREATE OR REPLACE FUNCTION post_assets_search_text_trigger() RETURNS TRIGGER AS $$
BEGIN
    PERFORM rebuild_post_search_text(COALESCE(NEW.post_id, OLD.post_id));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER post_assets_search_text
AFTER INSERT OR UPDATE OR DELETE ON post_assets
FOR EACH ROW EXECUTE FUNCTION post_assets_search_text_trigger();

-- ---------------------------------------------------------------------------
-- One-time data migration: existing Assets → Posts
-- ---------------------------------------------------------------------------
--
-- The Phase 1.13.D browse page reads from /api/v1/assets directly.
-- After this migration the browse page switches to /api/v1/posts. To
-- avoid the feed going dark, every owned asset gets a single-member
-- post with the asset's title/description and default public
-- visibility. Orphan assets (NULL owner_user_ref) are skipped — they
-- shouldn't exist in production, and the existing test-data ones
-- aren't visible from the browse path anyway.
--
-- search_text is rebuilt by the triggers above as the rows are
-- inserted.

WITH new_posts AS (
    INSERT INTO posts (
        author_user_ref, title, description, visibility, cover_asset_id,
        posted_at, created_at, updated_at
    )
    SELECT
        a.owner_user_ref,
        a.title,
        a.description,
        'public',
        a.id,
        a.created_at,
        a.created_at,
        a.updated_at
    FROM assets a
    WHERE a.deleted_at IS NULL
      AND a.owner_user_ref IS NOT NULL
    RETURNING id, cover_asset_id, created_at
)
INSERT INTO post_assets (post_id, asset_id, sort_order, added_at)
SELECT id, cover_asset_id, 0, created_at FROM new_posts;

-- +goose Down

DROP TABLE IF EXISTS collection_posts;
DROP TABLE IF EXISTS post_tags;
DROP TABLE IF EXISTS post_assets;
DROP TRIGGER IF EXISTS posts_search_text ON posts;
DROP TRIGGER IF EXISTS post_tags_search_text ON post_tags;
DROP TRIGGER IF EXISTS post_assets_search_text ON post_assets;
DROP FUNCTION IF EXISTS posts_search_text_trigger();
DROP FUNCTION IF EXISTS post_tags_search_text_trigger();
DROP FUNCTION IF EXISTS post_assets_search_text_trigger();
DROP FUNCTION IF EXISTS rebuild_post_search_text(UUID);
DROP TABLE IF EXISTS posts;
