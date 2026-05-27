-- artist-alley migration 00018 — workflow states, transitions, and audit.
--
-- See ADR 0010 Layer 7.
--
-- One generic state machine that can describe many different workflows
-- on many different resource kinds. The state machine is configurable
-- per "domain" — a stable string that identifies which set of states
-- and transitions apply to which rows.
--
-- Domain shape:
--
--   'post'           — applies to ALL posts (one workflow for the
--                      post entity itself).
--   'asset:1'        — applies to assets where resource_type = 1 (Photo).
--   'asset:3'        — applies to assets where resource_type = 3 (Video).
--   etc.
--
-- We use a TEXT domain rather than two FK columns (one to a resource
-- kind, one to resource_type.ref) because:
--   * Posts don't have a resource_type, so a strict FK would have to
--     be nullable + the "kind" column would still be needed.
--   * Adding new domains later (e.g. 'collection' or
--     'asset:plugin:custom') doesn't require schema changes.
--   * RS owns the resource_type table; coupling our state machine to
--     its data via FK creates a cross-ownership headache.
--
-- The application layer is responsible for matching a resource's
-- (kind, resource_type) to the right domain when fetching states.
--
-- ADR-mentioned helpers:
--   * workflow.Service.Transition(ctx, kind, id, toState, caller, note)
--     is the ONLY supported state-change path. It checks that
--     (from, to) is in workflow_transitions, that the caller holds
--     the required_capability (with team scope if requires_team_scope),
--     records workflow_audit, and updates the resource. Direct UPDATE
--     of state_id is allowed only for owner-initiated initial state
--     and for seed/migration code.

-- +goose Up

-- ---------------------------------------------------------------------------
-- States: the nodes of every state machine.
-- ---------------------------------------------------------------------------

CREATE TABLE workflow_states (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    domain              TEXT         NOT NULL,
    code                TEXT         NOT NULL,
    label               TEXT         NOT NULL,
    sort_order          INTEGER      NOT NULL DEFAULT 0,
    is_initial          BOOLEAN      NOT NULL DEFAULT FALSE,
    is_terminal         BOOLEAN      NOT NULL DEFAULT FALSE,
    visible_by_default  BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (domain, code)
);

CREATE INDEX workflow_states_domain_idx ON workflow_states (domain, sort_order);

-- Exactly one initial state per domain. Enforced by partial unique index.
CREATE UNIQUE INDEX workflow_states_one_initial_per_domain
    ON workflow_states (domain) WHERE is_initial;

-- ---------------------------------------------------------------------------
-- Transitions: the edges. Each row says "from state A you may move to
-- state B, optionally requiring capability C (optionally scoped to the
-- resource's team)."
-- ---------------------------------------------------------------------------

CREATE TABLE workflow_transitions (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    from_state_id       UUID         NULL REFERENCES workflow_states(id) ON DELETE CASCADE,
    to_state_id         UUID         NOT NULL REFERENCES workflow_states(id) ON DELETE CASCADE,
    required_capability TEXT         NULL REFERENCES capabilities(code) ON DELETE SET NULL,
    requires_team_scope BOOLEAN      NOT NULL DEFAULT FALSE,
    UNIQUE NULLS NOT DISTINCT (from_state_id, to_state_id)
);

CREATE INDEX workflow_transitions_from_idx ON workflow_transitions (from_state_id);
CREATE INDEX workflow_transitions_to_idx   ON workflow_transitions (to_state_id);

-- ---------------------------------------------------------------------------
-- Audit: every Transition() call appends one row. Read for history;
-- never updated, never deleted (the resource cascade is the only way
-- audit rows go).
-- ---------------------------------------------------------------------------

CREATE TABLE workflow_audit (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_kind       TEXT         NOT NULL,    -- 'post' | 'asset'
    resource_id         UUID         NOT NULL,
    from_state_id       UUID         NULL REFERENCES workflow_states(id) ON DELETE SET NULL,
    to_state_id         UUID         NOT NULL REFERENCES workflow_states(id) ON DELETE SET NULL,
    actor_rs_user_id    BIGINT       NULL,
    note                TEXT         NOT NULL DEFAULT '',
    transitioned_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX workflow_audit_resource_idx
    ON workflow_audit (resource_kind, resource_id, transitioned_at DESC);

-- ---------------------------------------------------------------------------
-- Posts + Assets gain state_id and team_id columns.
--
-- state_id NULL = the resource pre-dates the workflow migration or was
-- created via a path that didn't set it. The handler-side checks
-- treat NULL state_id as "not yet in any workflow" — readable per
-- visibility rules, writable per role/team checks, no transition gates.
--
-- team_id NULL = global resource (not scoped to any team). Visibility
-- still applies. Once teams are widely used the upload handler will
-- default this to the user's currently-selected team.
-- ---------------------------------------------------------------------------

ALTER TABLE posts
    ADD COLUMN state_id UUID NULL REFERENCES workflow_states(id) ON DELETE SET NULL,
    ADD COLUMN team_id  UUID NULL REFERENCES teams(id)           ON DELETE SET NULL;

CREATE INDEX posts_state_idx ON posts (state_id) WHERE state_id IS NOT NULL;
CREATE INDEX posts_team_idx  ON posts (team_id)  WHERE team_id  IS NOT NULL;

ALTER TABLE assets
    ADD COLUMN state_id UUID NULL REFERENCES workflow_states(id) ON DELETE SET NULL,
    ADD COLUMN team_id  UUID NULL REFERENCES teams(id)           ON DELETE SET NULL;

CREATE INDEX assets_state_idx ON assets (state_id) WHERE state_id IS NOT NULL;
CREATE INDEX assets_team_idx  ON assets (team_id)  WHERE team_id  IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Capabilities for the workflow system itself + the seeded workflows
-- below.
-- ---------------------------------------------------------------------------

INSERT INTO capabilities (code, description) VALUES
    ('workflow.admin',    'Manage workflow_states and workflow_transitions'),
    ('posts.publish',     'Move a post into the published state'),
    ('assets.submit',     'Submit an asset for review (draft → pending_review)'),
    ('assets.review',     'Approve or reject an asset in review (pending_review → published)'),
    ('assets.publish',    'Publish an asset directly without review (draft → published)'),
    ('assets.archive',    'Archive a published asset (published → archived)'),
    ('assets.unarchive',  'Restore an archived asset (archived → published)')
ON CONFLICT (code) DO NOTHING;

WITH base AS  (SELECT id FROM roles WHERE name = 'Base'),
     admin AS (SELECT id FROM roles WHERE name = 'Admin')
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'assets.submit'    FROM base  UNION ALL
SELECT id, 'posts.publish'    FROM admin UNION ALL
SELECT id, 'assets.review'    FROM admin UNION ALL
SELECT id, 'assets.publish'   FROM admin UNION ALL
SELECT id, 'assets.archive'   FROM admin UNION ALL
SELECT id, 'assets.unarchive' FROM admin UNION ALL
SELECT id, 'workflow.admin'   FROM admin
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------
-- Seeded workflows
-- ---------------------------------------------------------------------------

-- Posts: trivial one-state workflow ('published'). The post.visibility
-- column already conveys who can see it; state is reserved for future
-- draft / scheduled-publish workflows.

INSERT INTO workflow_states (domain, code, label, sort_order, is_initial, is_terminal, visible_by_default)
VALUES
    ('post', 'published', 'Published', 0, TRUE, FALSE, TRUE)
ON CONFLICT (domain, code) DO NOTHING;

-- Asset:1 (Photo): RS-style draft → pending_review → published → archived,
-- with a 'deleted' terminal mirror of RS's archive=3 lifecycle stage.

INSERT INTO workflow_states (domain, code,             label,             sort_order, is_initial, is_terminal, visible_by_default) VALUES
    ('asset:1', 'draft',           'Draft',            0, TRUE,  FALSE, FALSE),
    ('asset:1', 'pending_review',  'Pending Review',   1, FALSE, FALSE, FALSE),
    ('asset:1', 'published',       'Published',        2, FALSE, FALSE, TRUE),
    ('asset:1', 'archived',        'Archived',         3, FALSE, FALSE, FALSE),
    ('asset:1', 'deleted',         'Deleted',          4, FALSE, TRUE,  FALSE)
ON CONFLICT (domain, code) DO NOTHING;

-- Transitions for the Photo workflow. requires_team_scope=TRUE for
-- the review/publish/archive gates so a "Reviewer in team X" can only
-- act on team X's assets, not someone else's. Submission is open-
-- scoped (anyone with assets.submit can submit their own).

WITH s AS (
    SELECT code, id FROM workflow_states WHERE domain = 'asset:1'
)
INSERT INTO workflow_transitions (from_state_id, to_state_id, required_capability, requires_team_scope) VALUES
    -- draft → pending_review
    ((SELECT id FROM s WHERE code = 'draft'),          (SELECT id FROM s WHERE code = 'pending_review'), 'assets.submit',   FALSE),
    -- pending_review → published (review approval)
    ((SELECT id FROM s WHERE code = 'pending_review'), (SELECT id FROM s WHERE code = 'published'),      'assets.review',   TRUE),
    -- pending_review → draft (review rejection, send back)
    ((SELECT id FROM s WHERE code = 'pending_review'), (SELECT id FROM s WHERE code = 'draft'),          'assets.review',   TRUE),
    -- draft → published (skip-review publish; admin escape hatch)
    ((SELECT id FROM s WHERE code = 'draft'),          (SELECT id FROM s WHERE code = 'published'),      'assets.publish',  TRUE),
    -- published → archived
    ((SELECT id FROM s WHERE code = 'published'),      (SELECT id FROM s WHERE code = 'archived'),       'assets.archive',  TRUE),
    -- archived → published (unarchive)
    ((SELECT id FROM s WHERE code = 'archived'),       (SELECT id FROM s WHERE code = 'published'),      'assets.unarchive',TRUE),
    -- any → deleted is a soft-delete path; handled by the existing
    -- assets.delete capability via the asset delete handler, NOT via
    -- workflow.Transition. We don't seed transitions to the terminal
    -- 'deleted' state here; the existing soft-delete path stays the
    -- source of truth.
    -- Initial-state entry: NULL → draft (asset creation seeds 'draft')
    (NULL,                                              (SELECT id FROM s WHERE code = 'draft'),          NULL,              FALSE)
ON CONFLICT DO NOTHING;

-- And for posts: NULL → published is the only allowed entry.
WITH s AS (
    SELECT code, id FROM workflow_states WHERE domain = 'post'
)
INSERT INTO workflow_transitions (from_state_id, to_state_id, required_capability, requires_team_scope) VALUES
    (NULL, (SELECT id FROM s WHERE code = 'published'), NULL, FALSE)
ON CONFLICT DO NOTHING;

-- +goose Down

DROP INDEX IF EXISTS assets_team_idx;
DROP INDEX IF EXISTS assets_state_idx;
ALTER TABLE assets DROP COLUMN IF EXISTS team_id;
ALTER TABLE assets DROP COLUMN IF EXISTS state_id;

DROP INDEX IF EXISTS posts_team_idx;
DROP INDEX IF EXISTS posts_state_idx;
ALTER TABLE posts DROP COLUMN IF EXISTS team_id;
ALTER TABLE posts DROP COLUMN IF EXISTS state_id;

DELETE FROM role_capabilities WHERE capability_code IN (
    'workflow.admin','posts.publish','assets.submit','assets.review',
    'assets.publish','assets.archive','assets.unarchive'
);
DELETE FROM capabilities WHERE code IN (
    'workflow.admin','posts.publish','assets.submit','assets.review',
    'assets.publish','assets.archive','assets.unarchive'
);

DROP TABLE IF EXISTS workflow_audit;
DROP TABLE IF EXISTS workflow_transitions;
DROP TABLE IF EXISTS workflow_states;
