-- artist-alley migration 00029 — whiteboard + annotation surfaces.
--
-- Phase 1.18.C-1 (foundations + whiteboard).
--
-- Background
-- ----------
-- We're adding two related surfaces:
--
--   * **Whiteboards** — free-form brush sketches attached to a post.
--     Brainstorm sessions, sketch passes, off-asset notes. No frame
--     anchor; the post is the canvas.
--   * **Annotations** — frame-accurate brush markup on a specific
--     asset (video frame, image, PDF page). Frame-pinned for time-
--     line kinds; region-bound for static.
--
-- Both surfaces use the same brush engine (perfect-freehand on the
-- frontend), the same data shape (vector strokes in JSONB + layers),
-- and the same threading + likes + soft-delete + federation slots.
--
-- The big move: NOT a new table. The existing `comments` row already
-- carries annotation_type + annotation_data (see migration 00020,
-- lines 76–84). Whiteboards are comments-with-annotation_type=
-- 'whiteboard' on a post; annotations are comments-with-annotation_
-- type='frame' on an asset (or 'point' / 'rect' for image regions).
-- This buys us threading (replies on whiteboards + annotations) for
-- free, likes for free, federation for free, audit for free.
--
-- All this migration does is extend the CHECK constraint on
-- annotation_type so 'whiteboard' becomes a permitted value, and add
-- one partial index for the per-post whiteboard listing pattern. The
-- existing comments_annotation_idx already covers the per-asset
-- annotation listing (Phase 1.18.C-2 will exercise that path).
--
-- Capabilities
-- ------------
-- We deliberately do NOT seed new capabilities. Whiteboards and
-- annotations are CommentS at the storage layer; the existing
-- posts.comment / assets.comment gates apply to creation. Deletion
-- continues to flow through the existing comments.delete.own (author)
-- and comments.delete.any (moderator) caps. When we want finer
-- governance — e.g. "can post whiteboards but not comment" — a future
-- migration can split them out without restructuring the data.

-- +goose Up

-- ---------------------------------------------------------------------------
-- Extend the annotation_type CHECK to include 'whiteboard'.
--
-- Postgres auto-named the original constraint; the name is stable for
-- a given column + ordinal so the DROP-then-ADD pattern is safe in
-- the deterministic schema this migration runs against. If a future
-- deploy somewhere has a renamed constraint, the IF EXISTS shields us.
-- ---------------------------------------------------------------------------
ALTER TABLE comments
    DROP CONSTRAINT IF EXISTS comments_annotation_type_check;

ALTER TABLE comments
    ADD CONSTRAINT comments_annotation_type_check
    CHECK (annotation_type IN ('point', 'rect', 'timestamp', 'frame', 'whiteboard'));

-- ---------------------------------------------------------------------------
-- Per-post whiteboard listing index — narrow partial so it stays
-- small even when comments grows large. The general
-- comments_annotation_idx (00020) covers (target_kind, target_id,
-- annotation_type) but doesn't sort by created_at; the sidebar list
-- wants newest-first so a dedicated DESC index pays off here.
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS comments_whiteboards_idx
    ON comments (target_kind, target_id, created_at DESC)
    WHERE annotation_type = 'whiteboard' AND deleted_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS comments_whiteboards_idx;

ALTER TABLE comments
    DROP CONSTRAINT IF EXISTS comments_annotation_type_check;

ALTER TABLE comments
    ADD CONSTRAINT comments_annotation_type_check
    CHECK (annotation_type IN ('point', 'rect', 'timestamp', 'frame'));
