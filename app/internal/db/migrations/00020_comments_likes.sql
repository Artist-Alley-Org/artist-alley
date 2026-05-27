-- artist-alley migration 00020 — comments + likes (Phase 1.13.D-4 schema).
--
-- Gold-standard, future-proof shape. Two polymorphic tables, both keyed
-- by (target_kind, target_id), so:
--   * comments and likes apply to posts, assets, and (later) collections
--     without parallel tables
--   * threading + annotations live on a single comments row; the review-
--     mode feature reuses this schema rather than bolting on a new one
--   * the existing posts.like_count / posts.comment_count denormalised
--     counters are maintained by triggers — handlers stay fast
--
-- Why polymorphic single-table instead of comment_post / comment_asset /
-- comment_collection? Three reasons. (1) Code path: one set of sqlc
-- queries answers "give me a thread on X" regardless of what X is. (2)
-- Review-mode: annotations attach to assets, comments attach to posts;
-- with the same shape, an annotation IS a comment with extra columns,
-- so the thread view already shows both. (3) Federation: we mirror
-- remote comments by content shape, not by target type.
--
-- Why threading via parent_id + denorm root_id+depth? root_id makes
-- "give me the whole thread sorted by created_at" a single indexed
-- query without recursive CTEs. depth makes the rendering-side
-- indentation cap easy. Both columns are maintainable cheaply by
-- triggers on INSERT.
--
-- Why polymorphic likes (target_kind includes 'comment')? So users can
-- like individual comments in a thread — a real engagement signal for
-- which replies are useful. comment likes get a denormalised count on
-- the comments row itself, same pattern.
--
-- Capabilities seeded here:
--   posts.comment         — write comments on posts (Base by default)
--   posts.like            — like posts (Base by default)
--   comments.delete.own   — delete a comment you authored (Base)
--   comments.delete.any   — moderator override (Admin)
--
-- Why posts.* not social.* — comments and likes are first-class on
-- posts. The verb gates "can this user comment on / like THIS post".
-- Assets and collections get parallel caps when we surface comments
-- on them.

-- +goose Up

-- ---------------------------------------------------------------------------
-- Comments
-- ---------------------------------------------------------------------------

CREATE TABLE comments (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Polymorphic target. The (kind, id) pair is treated as an
    -- application-level FK; we don't enforce it at the DB level because
    -- each kind lives in a different table and we don't want a
    -- four-way conditional FK. Sweep triggers (below) keep things
    -- tidy when a post is deleted.
    target_kind         TEXT         NOT NULL CHECK (target_kind IN ('post','asset','collection')),
    target_id           UUID         NOT NULL,

    -- Threading. parent_id is the direct parent; root_id is the
    -- top-of-thread (root_id = self for top-level comments). depth
    -- caps at the application layer (we'll render up to N levels of
    -- indent and fold the rest). NULL parent + self root + depth 0 =
    -- a top-level comment.
    parent_id           UUID         NULL REFERENCES comments(id) ON DELETE CASCADE,
    root_id             UUID         NOT NULL,
    depth               INTEGER      NOT NULL DEFAULT 0,

    -- Content.
    author_user_ref     BIGINT       NOT NULL,
    body                TEXT         NOT NULL,
    -- Rendered safe HTML (server-side markdown -> sanitised HTML).
    -- Cached so the read path doesn't re-render on every fetch.
    -- Empty string while we haven't shipped the markdown renderer.
    body_html           TEXT         NOT NULL DEFAULT '',

    -- Review-mode annotations (future). NULL on plain comments.
    --   annotation_type: 'point' (x,y on asset) | 'rect' (x,y,w,h) |
    --                    'timestamp' (t in seconds for video/audio) |
    --                    'frame' (frame number)
    --   annotation_data: JSON shape depends on type. Schema enforcement
    --                    happens at the application layer (Phase 1.13.X).
    annotation_type     TEXT         NULL CHECK (annotation_type IN ('point','rect','timestamp','frame')),
    annotation_data     JSONB        NULL,

    -- Engagement counters maintained by the likes trigger below.
    like_count          BIGINT       NOT NULL DEFAULT 0,

    -- Audit + lifecycle.
    edited_at           TIMESTAMPTZ  NULL,
    deleted_at          TIMESTAMPTZ  NULL,
    origin_server_id    UUID         NULL,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- The thread-fetch query: "all live comments on (kind,id), ordered by
-- root then time within root". Two indexes cover the common reads:
CREATE INDEX comments_target_active_idx
    ON comments (target_kind, target_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX comments_thread_idx
    ON comments (root_id, depth, created_at ASC)
    WHERE deleted_at IS NULL;

-- "What has user X commented on" — user profile feed.
CREATE INDEX comments_author_idx
    ON comments (author_user_ref, created_at DESC)
    WHERE deleted_at IS NULL;

-- Annotation lookup for review mode (filtered partial index — tiny).
CREATE INDEX comments_annotation_idx
    ON comments (target_kind, target_id, annotation_type)
    WHERE annotation_type IS NOT NULL AND deleted_at IS NULL;

-- ---------------------------------------------------------------------------
-- Likes (also polymorphic, including comments)
-- ---------------------------------------------------------------------------

CREATE TABLE likes (
    target_kind TEXT         NOT NULL CHECK (target_kind IN ('post','asset','comment')),
    target_id   UUID         NOT NULL,
    rs_user_id  BIGINT       NOT NULL,
    liked_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (target_kind, target_id, rs_user_id)
);

CREATE INDEX likes_target_idx
    ON likes (target_kind, target_id);

-- "Posts user X liked" — for a future activity feed.
CREATE INDEX likes_user_idx
    ON likes (rs_user_id, liked_at DESC);

-- ---------------------------------------------------------------------------
-- Counter maintenance triggers
-- ---------------------------------------------------------------------------
-- The Posts handler already reads posts.like_count and posts.comment_count
-- straight from the row — we promised those would start being maintained
-- by triggers in this phase. Same pattern for the comments.like_count
-- column above.
--
-- Why triggers vs application code: a like or comment is the kind of
-- thing that races (two tabs liking simultaneously, federation imports
-- backfilling, etc.). Triggers run inside the inserting transaction so
-- the counter is always consistent with the row count. Application
-- code would have to use SELECT FOR UPDATE or risk drift.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION likes_after_insert() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.target_kind = 'post' THEN
        UPDATE posts SET like_count = like_count + 1 WHERE id = NEW.target_id;
    ELSIF NEW.target_kind = 'comment' THEN
        UPDATE comments SET like_count = like_count + 1 WHERE id = NEW.target_id;
    END IF;
    -- 'asset' target lands when we add assets.like_count later.
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION likes_after_delete() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.target_kind = 'post' THEN
        UPDATE posts SET like_count = GREATEST(like_count - 1, 0) WHERE id = OLD.target_id;
    ELSIF OLD.target_kind = 'comment' THEN
        UPDATE comments SET like_count = GREATEST(like_count - 1, 0) WHERE id = OLD.target_id;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER likes_maintain_counter_insert
AFTER INSERT ON likes
FOR EACH ROW EXECUTE FUNCTION likes_after_insert();

CREATE TRIGGER likes_maintain_counter_delete
AFTER DELETE ON likes
FOR EACH ROW EXECUTE FUNCTION likes_after_delete();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_after_insert() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.target_kind = 'post' THEN
        UPDATE posts SET comment_count = comment_count + 1 WHERE id = NEW.target_id;
    END IF;
    -- asset/collection comment counters land with the columns themselves.
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Soft delete (UPDATE deleted_at) is what we want to count as "gone".
-- The trigger fires on the deleted_at transition rather than DELETE,
-- because we keep the row for audit.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_after_update() RETURNS TRIGGER AS $$
BEGIN
    -- Transitioned from live to soft-deleted.
    IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
        IF NEW.target_kind = 'post' THEN
            UPDATE posts SET comment_count = GREATEST(comment_count - 1, 0) WHERE id = NEW.target_id;
        END IF;
    -- Transitioned the other way (un-delete) — unlikely in MVP but safe.
    ELSIF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
        IF NEW.target_kind = 'post' THEN
            UPDATE posts SET comment_count = comment_count + 1 WHERE id = NEW.target_id;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Hard delete (FK cascade from post deletion, etc.).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION comments_after_delete() RETURNS TRIGGER AS $$
BEGIN
    -- Only decrement for comments that were live; soft-deleted ones
    -- were already decremented when soft-deleted.
    IF OLD.deleted_at IS NULL THEN
        IF OLD.target_kind = 'post' THEN
            UPDATE posts SET comment_count = GREATEST(comment_count - 1, 0) WHERE id = OLD.target_id;
        END IF;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER comments_maintain_counter_insert
AFTER INSERT ON comments
FOR EACH ROW EXECUTE FUNCTION comments_after_insert();

CREATE TRIGGER comments_maintain_counter_update
AFTER UPDATE OF deleted_at ON comments
FOR EACH ROW EXECUTE FUNCTION comments_after_update();

CREATE TRIGGER comments_maintain_counter_delete
AFTER DELETE ON comments
FOR EACH ROW EXECUTE FUNCTION comments_after_delete();

-- ---------------------------------------------------------------------------
-- Sweep triggers: when a post is hard-deleted, cascade likes + comments.
-- ---------------------------------------------------------------------------
-- The polymorphic shape means we can't use ON DELETE CASCADE — we'd
-- need a four-way conditional FK that Postgres doesn't have. Triggers
-- on the target tables do the work.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION social_sweep_on_post_delete() RETURNS TRIGGER AS $$
BEGIN
    DELETE FROM likes WHERE target_kind = 'post' AND target_id = OLD.id;
    -- Comments cascade via comments.target then their replies via the
    -- parent_id FK (ON DELETE CASCADE).
    DELETE FROM comments WHERE target_kind = 'post' AND target_id = OLD.id;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER social_sweep_after_post_delete
AFTER DELETE ON posts
FOR EACH ROW EXECUTE FUNCTION social_sweep_on_post_delete();

-- ---------------------------------------------------------------------------
-- Capabilities
-- ---------------------------------------------------------------------------

INSERT INTO capabilities (code, description) VALUES
    ('posts.comment',       'Write comments on posts'),
    ('posts.like',          'Like (and unlike) posts and comments'),
    ('comments.delete.own', 'Delete a comment you authored'),
    ('comments.delete.any', 'Delete any comment (moderator)')
ON CONFLICT (code) DO NOTHING;

-- Base users get the engagement basics; moderation is Admin-only.
WITH base  AS (SELECT id FROM roles WHERE name = 'Base'),
     admin AS (SELECT id FROM roles WHERE name = 'Admin')
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'posts.comment'       FROM base  UNION ALL
SELECT id, 'posts.like'          FROM base  UNION ALL
SELECT id, 'comments.delete.own' FROM base  UNION ALL
SELECT id, 'comments.delete.any' FROM admin
ON CONFLICT DO NOTHING;

-- +goose Down

DROP TRIGGER IF EXISTS social_sweep_after_post_delete   ON posts;
DROP TRIGGER IF EXISTS comments_maintain_counter_delete ON comments;
DROP TRIGGER IF EXISTS comments_maintain_counter_update ON comments;
DROP TRIGGER IF EXISTS comments_maintain_counter_insert ON comments;
DROP TRIGGER IF EXISTS likes_maintain_counter_delete    ON likes;
DROP TRIGGER IF EXISTS likes_maintain_counter_insert    ON likes;

DROP FUNCTION IF EXISTS social_sweep_on_post_delete();
DROP FUNCTION IF EXISTS comments_after_delete();
DROP FUNCTION IF EXISTS comments_after_update();
DROP FUNCTION IF EXISTS comments_after_insert();
DROP FUNCTION IF EXISTS likes_after_delete();
DROP FUNCTION IF EXISTS likes_after_insert();

DELETE FROM role_capabilities
 WHERE capability_code IN ('posts.comment','posts.like','comments.delete.own','comments.delete.any');
DELETE FROM capabilities
 WHERE code IN ('posts.comment','posts.like','comments.delete.own','comments.delete.any');

DROP TABLE IF EXISTS likes;
DROP TABLE IF EXISTS comments;
