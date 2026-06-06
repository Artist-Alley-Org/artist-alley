-- artist-alley migration 00056 — federation shares + 4-tier visibility.
-- Phase 1.22.C-a, feat/user-surfaces.
--
-- The load-bearing security layer per
-- docs/proposals/1.22.C-shares-design.md (approved). Three pieces:
--
--   1. federation_shares table — per-object grants to peers
--   2. Visibility CHECK constraint updates on posts + collections
--      (drop legacy 'public' + 'shared'; install 4-tier closed
--      catalogue: private / org-only / followers / explicit-share)
--   3. Data migration: existing rows mapped to the closest new
--      semantic. Walled-garden context: 'public' meant "any
--      logged-in local user" not "anyone on the internet", so
--      it maps to org-only.
--
-- Per the reviewer's locked-in answers (§12.5 of the proposal):
--   - 'public' tier OMITTED from v1 enum (reserved for future
--     public-fediverse phase)
--   - aa:RevokeShare reserved but only aa:Unshare ships v1
--   - Defederation chunked via job queue (uses revoked_at +
--     revoked_activity_id; FK CASCADE on peer_id is the
--     safety net, not the primary path)
--
-- CHECK constraints mirror the typed Go catalogues per ADR 0042:
--   federation.ObjectVisibility    — private/org-only/followers/explicit-share
--   federation.ShareScope          — view/comment/annotate/remix
--   federation.ShareObjectKind     — asset/post/collection/workspace/brand_kit/user

-- +goose Up

-- --- 4-tier visibility CHECK + data migration -----------------------------

-- posts: legacy 'public' → 'org-only' (walled-garden semantic match).
-- Drop the constraint FIRST so the UPDATE can land the new
-- value; re-add the constraint with the 4-tier closed catalogue
-- AFTER the data is in the new shape.
ALTER TABLE posts DROP CONSTRAINT posts_visibility_check;
UPDATE posts SET visibility = 'org-only' WHERE visibility = 'public';
ALTER TABLE posts
    ADD CONSTRAINT posts_visibility_check
        CHECK (visibility IN ('private', 'org-only', 'followers', 'explicit-share'));
ALTER TABLE posts ALTER COLUMN visibility SET DEFAULT 'org-only';

-- collections: 'public' → 'org-only' (same reasoning),
-- 'shared' → 'explicit-share' (legacy ACL-driven shared was the
-- per-recipient grant model).
ALTER TABLE collections DROP CONSTRAINT collections_visibility_check;
UPDATE collections SET visibility = 'org-only' WHERE visibility = 'public';
UPDATE collections SET visibility = 'explicit-share' WHERE visibility = 'shared';
ALTER TABLE collections
    ADD CONSTRAINT collections_visibility_check
        CHECK (visibility IN ('private', 'org-only', 'followers', 'explicit-share'));

-- --- activities catalogue extension --------------------------------------

-- aa:RevokeShare reserved per the 1.22.C design proposal §12.5 #3:
-- v1 implementations MUST treat any inbound aa:RevokeShare as
-- aa:Unshare, but the value needs to be CHECK-valid so we can
-- record + forward the originating activity without rewriting
-- the type. Drop + re-add the CHECK constraint to include it.
ALTER TABLE activities DROP CONSTRAINT activities_activity_type_check;
ALTER TABLE activities ADD CONSTRAINT activities_activity_type_check
    CHECK (activity_type IN (
        'Create', 'Update', 'Delete',
        'Follow', 'Accept', 'Reject',
        'Undo', 'Like', 'Announce', 'Block',
        'Add', 'Remove',
        'aa:Share', 'aa:Unshare', 'aa:RevokeShare',
        'aa:Approve', 'aa:RequestChanges', 'aa:MarkReviewed',
        'aa:Annotation', 'aa:WorkflowTransition', 'aa:AssetVersion',
        'aa:Subscribe', 'aa:Mention'
    ));

-- --- federation_shares table ----------------------------------------------

CREATE TABLE federation_shares (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    -- WHO is granting access (local user_ref).
    grantor_user_ref    BIGINT       NOT NULL,

    -- WHAT is being shared. CHECK mirrors federation.ShareObjectKind.
    -- workspace + brand_kit included so future table additions
    -- don't need a CHECK migration; their tables land in later
    -- phases and just point to this column then.
    object_kind         TEXT         NOT NULL
        CHECK (object_kind IN ('asset', 'post', 'collection',
                                'workspace', 'brand_kit', 'user')),
    object_id           UUID         NOT NULL,

    -- WHERE it's going. ON DELETE CASCADE is the safety net for
    -- defederation; the primary path is the chunked job that
    -- marks revoked_at first (§12.5 #6 of the proposal).
    peer_id             UUID         NOT NULL
        REFERENCES federation_peers(id) ON DELETE CASCADE,

    -- target_user_url:
    --   NULL = broadcast within the peer (any user on that peer)
    --   non-null = specific user (actor URL at the peer)
    target_user_url     TEXT         NULL,

    -- HOW MUCH access. CHECK mirrors federation.ShareScope; the
    -- ladder is view < comment < annotate < remix. Remix means
    -- "incorporate into recipient's own posts/collections/etc.
    -- on their instance" — never edit the original (origin always
    -- wins). Future fifth scope `edit` reserved for cross-instance
    -- edit of the original.
    scope               TEXT         NOT NULL DEFAULT 'view'
        CHECK (scope IN ('view', 'comment', 'annotate', 'remix')),

    -- WHEN it expires (NULL = no expiry). The inbox-filter checks
    -- expires_at per request. The expiry sweeper (1.22.C-d job)
    -- proactively emits aa:Unshare so recipients purge cached
    -- bytes — without the sweeper, the recipient could indefinitely
    -- hold bytes they no longer have access to.
    expires_at          TIMESTAMPTZ  NULL,

    -- WHY (operator note) + correlation back to the originating
    -- aa:Share activity. RESTRICT (not CASCADE) on the activity
    -- so we never lose the audit chain by accident.
    notes               TEXT         NOT NULL DEFAULT '',
    granted_activity_id UUID         NOT NULL
        REFERENCES activities(id) ON DELETE RESTRICT,

    -- BOOKKEEPING.
    granted_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    revoked_at          TIMESTAMPTZ  NULL,
    revoked_activity_id UUID         NULL
        REFERENCES activities(id) ON DELETE RESTRICT,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE federation_shares IS
    'Per-object federation grants. Pairing alone shares no '
    'content (ADR 0043 §"Trust model"); every row here is an '
    'explicit grantor-side decision. Followers tier uses '
    'object_kind=user — there is no separate followers table.';

COMMENT ON COLUMN federation_shares.target_user_url IS
    'NULL = any user on the peer (broadcast within peer). '
    'Non-null = specific user (actor URL). Restricted/embargo '
    'objects require non-null target_user_url so NaCl-box '
    'wrapping has a key to wrap to (enforced at the API layer).';

COMMENT ON COLUMN federation_shares.scope IS
    'view<comment<annotate<remix. Remix = "incorporate into own '
    'objects on recipient instance"; original never modified.';

-- Active-only uniqueness. Revoked rows persist for audit; only
-- one ACTIVE row per (grantor, object, peer, target_user) at a time.
-- Use a generated string for the COALESCE so the index expression
-- is immutable.
CREATE UNIQUE INDEX federation_shares_active_uniq_idx
    ON federation_shares (
        grantor_user_ref, object_kind, object_id, peer_id,
        COALESCE(target_user_url, '')
    )
    WHERE revoked_at IS NULL;

-- Hot read #1: inbox filter. "Does an active share exist for
-- this (object, peer)?" Partial index keeps it sub-ms even with
-- many revoked rows.
CREATE INDEX federation_shares_lookup_idx
    ON federation_shares (object_kind, object_id, peer_id)
    WHERE revoked_at IS NULL;

-- Hot read #2: outbox dispatch. For a new activity about object
-- X, iterate every active share row to know who to deliver to.
CREATE INDEX federation_shares_delivery_idx
    ON federation_shares (object_kind, object_id)
    WHERE revoked_at IS NULL;

-- Hot read #3: defederation cascade. Count + select shares per
-- peer for the chunked-defederate job + the cascade-preview
-- modal.
CREATE INDEX federation_shares_by_peer_idx
    ON federation_shares (peer_id)
    WHERE revoked_at IS NULL;

-- Hot read #4: expiry sweeper. "Find active shares whose
-- expires_at has passed." Partial index keeps the working set
-- to (expirable + active) rows only.
CREATE INDEX federation_shares_expiring_idx
    ON federation_shares (expires_at)
    WHERE revoked_at IS NULL AND expires_at IS NOT NULL;

-- Hot read #5: per-grantor lookup ("what am I sharing?"). Admin
-- UI's per-user share view. Lower priority than the others.
CREATE INDEX federation_shares_by_grantor_idx
    ON federation_shares (grantor_user_ref, granted_at DESC)
    WHERE revoked_at IS NULL;

-- +goose Down

DROP TABLE federation_shares;

-- Restore the activities CHECK (drop aa:RevokeShare).
ALTER TABLE activities DROP CONSTRAINT activities_activity_type_check;
ALTER TABLE activities ADD CONSTRAINT activities_activity_type_check
    CHECK (activity_type IN (
        'Create', 'Update', 'Delete',
        'Follow', 'Accept', 'Reject',
        'Undo', 'Like', 'Announce', 'Block',
        'Add', 'Remove',
        'aa:Share', 'aa:Unshare',
        'aa:Approve', 'aa:RequestChanges', 'aa:MarkReviewed',
        'aa:Annotation', 'aa:WorkflowTransition', 'aa:AssetVersion',
        'aa:Subscribe', 'aa:Mention'
    ));

-- Restore legacy visibility CHECK constraints. Data migration is
-- non-reversible (the 'public'/'shared' semantic information is
-- lost on the way down) but the CHECK constraint can be restored
-- so downgrade scripts succeed.
UPDATE posts SET visibility = 'public'
    WHERE visibility NOT IN ('private', 'followers', 'public');
ALTER TABLE posts DROP CONSTRAINT posts_visibility_check;
ALTER TABLE posts
    ADD CONSTRAINT posts_visibility_check
        CHECK (visibility IN ('private', 'followers', 'public'));
ALTER TABLE posts ALTER COLUMN visibility SET DEFAULT 'public';

UPDATE collections SET visibility = 'private'
    WHERE visibility NOT IN ('private', 'shared', 'public');
ALTER TABLE collections DROP CONSTRAINT collections_visibility_check;
ALTER TABLE collections
    ADD CONSTRAINT collections_visibility_check
        CHECK (visibility IN ('private', 'shared', 'public'));
