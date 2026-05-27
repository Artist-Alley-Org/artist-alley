-- artist-alley migration 00022 — upload pipeline seam.
--
-- A handful of small additions that prepare the asset/post/workflow
-- model for the frictionless upload modal (Phase 1.13.D-2b) and the
-- later processing pipeline + render-farm offload phases. None of
-- this changes existing reads; the new columns are nullable / default
-- to safe values so old rows remain valid.
--
-- 1. posts.cover_thumbnail_asset_id
--    Optional standalone thumbnail that is NOT a member of the post.
--    Distinct from `cover_asset_id` which must reference one of
--    `post_assets.asset_id`. Lets the upload modal offer "upload a
--    different image as the post thumbnail" without making that image
--    a member of the post itself. NULL = use cover_asset_id behaviour.
--
-- 2. assets.processing_status
--    Post-upload pipeline state. `pending` means async jobs are still
--    in flight (variant generation, EXIF parsing, etc.). `ready` is
--    the steady state. `failed` surfaces in the UI as a retry
--    affordance. The original /file URL is always available regardless
--    — this column only gates whether the variant URLs are populated.
--    DEFAULT 'ready' keeps existing rows valid; the create-asset
--    handler sets 'pending' for image/video uploads.
--
-- 3. assets.thumbhash
--    ~30 bytes per asset. Computed synchronously at create time for
--    image resources (1–3 ms on a 4K image via go-thumbhash). Used
--    by the frontend to paint a blurred placeholder before any
--    network round-trip — the "fast access" guarantee in action.
--    NULL for non-image assets.
--
-- 4. workflow_states.icon / .color / .requires_note
--    Cosmetic + workflow-policy additions borrowed from RSE's
--    archive_states. `icon` is a short token (e.g. "check", "clock")
--    that the frontend maps to an svg in its iconset — keeps the
--    state vocabulary out of the frontend code. `color` is a hex
--    string like "#16a34a" for the badge background. `requires_note`
--    makes Service.Transition reject calls with an empty note when
--    moving INTO this state — useful for "rejected" / "needs work"
--    states where the why matters. All three default to safe values
--    so existing seeded states stay valid until an admin sets them.

-- +goose Up

ALTER TABLE posts
    ADD COLUMN cover_thumbnail_asset_id UUID NULL
        REFERENCES assets(id) ON DELETE SET NULL;

CREATE INDEX posts_cover_thumbnail_idx
    ON posts (cover_thumbnail_asset_id)
    WHERE cover_thumbnail_asset_id IS NOT NULL;

ALTER TABLE assets
    ADD COLUMN processing_status TEXT NOT NULL DEFAULT 'ready'
        CHECK (processing_status IN ('pending', 'ready', 'failed')),
    ADD COLUMN thumbhash BYTEA NULL;

-- Partial index: most rows will be 'ready', so we only index the
-- non-steady states. Used by the future job system to find work
-- and by the admin "processing queue" surface.
CREATE INDEX assets_processing_status_idx
    ON assets (processing_status)
    WHERE processing_status <> 'ready';

ALTER TABLE workflow_states
    ADD COLUMN icon          TEXT    NOT NULL DEFAULT '',
    ADD COLUMN color         TEXT    NOT NULL DEFAULT '',
    ADD COLUMN requires_note BOOLEAN NOT NULL DEFAULT FALSE;

-- Backfill icons + colors for the existing seeded states so the
-- frontend has something to render. Values picked to match the
-- frontend's lucide-style iconset and a high-contrast palette.

UPDATE workflow_states SET icon = 'check-circle',  color = '#16a34a' WHERE domain = 'post'    AND code = 'published';
UPDATE workflow_states SET icon = 'file-edit',     color = '#64748b' WHERE domain = 'asset:1' AND code = 'draft';
UPDATE workflow_states SET icon = 'clock',         color = '#f59e0b' WHERE domain = 'asset:1' AND code = 'pending_review';
UPDATE workflow_states SET icon = 'check-circle',  color = '#16a34a' WHERE domain = 'asset:1' AND code = 'published';
UPDATE workflow_states SET icon = 'archive',       color = '#0ea5e9' WHERE domain = 'asset:1' AND code = 'archived';
UPDATE workflow_states SET icon = 'trash-2',       color = '#ef4444' WHERE domain = 'asset:1' AND code = 'deleted';

-- Add a 'Work In Progress' state to the post domain. Distinct from
-- post.visibility (which is "who can see it"): wip means the author
-- considers the post unfinished. The upload modal lets uploaders
-- choose between WIP and Published at create time; default stays
-- Published (is_initial=TRUE on the existing 'published' row).
--
-- Transitions: NULL → wip is allowed so create-with-explicit-state
-- works; wip → published lets the user publish later; published →
-- wip lets them roll back (useful when realising a published post
-- needs more work).

INSERT INTO workflow_states
    (domain, code, label, sort_order, is_initial, is_terminal,
     visible_by_default, icon, color, requires_note)
VALUES
    ('post', 'wip', 'WIP', -10, FALSE, FALSE, TRUE, 'pencil-line', '#f59e0b', FALSE)
ON CONFLICT (domain, code) DO NOTHING;

WITH s AS (
    SELECT code, id FROM workflow_states WHERE domain = 'post'
)
INSERT INTO workflow_transitions (from_state_id, to_state_id, required_capability, requires_team_scope) VALUES
    -- Initial entry for posts explicitly created as WIP.
    (NULL,                                         (SELECT id FROM s WHERE code = 'wip'),       NULL,             FALSE),
    -- WIP → Published (any authenticated post-author can publish their own).
    ((SELECT id FROM s WHERE code = 'wip'),        (SELECT id FROM s WHERE code = 'published'), 'posts.publish',  FALSE),
    -- Published → WIP (un-publish to keep working on it).
    ((SELECT id FROM s WHERE code = 'published'),  (SELECT id FROM s WHERE code = 'wip'),       'posts.publish',  FALSE)
ON CONFLICT DO NOTHING;

-- +goose Down

-- Wind back the WIP seed by deleting the transitions first (so we
-- don't violate the FK on workflow_transitions when the state goes).
DELETE FROM workflow_transitions
WHERE to_state_id IN (SELECT id FROM workflow_states WHERE domain = 'post' AND code = 'wip')
   OR from_state_id IN (SELECT id FROM workflow_states WHERE domain = 'post' AND code = 'wip');
DELETE FROM workflow_states WHERE domain = 'post' AND code = 'wip';

ALTER TABLE workflow_states
    DROP COLUMN IF EXISTS requires_note,
    DROP COLUMN IF EXISTS color,
    DROP COLUMN IF EXISTS icon;

DROP INDEX IF EXISTS assets_processing_status_idx;
ALTER TABLE assets
    DROP COLUMN IF EXISTS thumbhash,
    DROP COLUMN IF EXISTS processing_status;

DROP INDEX IF EXISTS posts_cover_thumbnail_idx;
ALTER TABLE posts
    DROP COLUMN IF EXISTS cover_thumbnail_asset_id;
