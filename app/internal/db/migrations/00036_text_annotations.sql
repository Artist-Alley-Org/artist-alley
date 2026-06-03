-- artist-alley migration 00036 — text-range annotations.
--
-- Phase 1.18.D-B (document viewer review tools).
--
-- Background
-- ----------
-- The Doc viewer (Phase A — CodeMirror 6 for txt / md / code) needs
-- a review-mode layer: users select a range of text and apply one of
-- five visual styles — highlight / strikethrough / underline / text-
-- comment / text-note. The styles aren't enforced server-side; the
-- annotation just carries the user's choice in annotation_data and
-- the editor decoration extension paints accordingly.
--
-- Same trick migration 00029 used for whiteboard annotations: we
-- already have the `comments` table with annotation_type + annotation_
-- data + threading + likes + soft-delete + federation hooks. Text
-- annotations are a comment with annotation_type='text-range' and
-- annotation_data carrying { style, color, range }.
--
-- annotation_data shape:
--   {
--     "style":      "highlight" | "strikethrough" | "underline" |
--                   "comment" | "note",
--     "color":      "#fef08a",
--     "start_line": 12,
--     "start_col":  5,
--     "end_line":   14,
--     "end_col":    22,
--     "resolved":   false
--   }
--
-- Threading is preserved — a user can reply to an annotation, just
-- like a whiteboard or frame annotation. Replies inherit the parent's
-- range via the (root_id, parent_id, depth) thread keys; the reply
-- body shows up in the panel under the parent's anchor.
--
-- Capabilities — none new. The existing posts.comment / assets.comment
-- gates apply since annotations live in the comments table; deletion
-- continues through comments.delete.own / comments.delete.any.

-- +goose Up

-- ---------------------------------------------------------------------------
-- Extend annotation_type CHECK to admit 'text-range'.
-- ---------------------------------------------------------------------------
ALTER TABLE comments
    DROP CONSTRAINT IF EXISTS comments_annotation_type_check;

ALTER TABLE comments
    ADD CONSTRAINT comments_annotation_type_check
    CHECK (annotation_type IN (
        'point', 'rect', 'timestamp', 'frame', 'whiteboard', 'text-range'
    ));

-- ---------------------------------------------------------------------------
-- Per-asset text-annotation listing index. Narrow partial so it stays
-- tiny even as comments grows. The general comments_annotation_idx
-- (00020) covers (target_kind, target_id, annotation_type) but doesn't
-- sort by created_at — the doc-viewer panel wants newest-first so a
-- dedicated DESC index helps that page.
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS comments_text_annotations_idx
    ON comments (target_kind, target_id, created_at DESC)
    WHERE annotation_type = 'text-range' AND deleted_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS comments_text_annotations_idx;

ALTER TABLE comments
    DROP CONSTRAINT IF EXISTS comments_annotation_type_check;

ALTER TABLE comments
    ADD CONSTRAINT comments_annotation_type_check
    CHECK (annotation_type IN ('point', 'rect', 'timestamp', 'frame', 'whiteboard'));
