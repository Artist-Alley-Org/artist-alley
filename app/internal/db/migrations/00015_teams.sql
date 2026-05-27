-- artist-alley migration 00015 — teams as a DAG (closure table).
--
-- See docs/adr/0010-permissions-teams-workflow.md, Layer 4.
--
-- Storage shape:
--   teams           — node table (id, slug, name, ...)
--   team_parents    — direct parent edges. Multiple parents allowed per team
--                     (this is what makes the structure a DAG rather than a
--                     tree).
--   team_closure    — materialised (ancestor, descendant, depth) triples for
--                     fast "is X in the descendants of Y" lookups. Includes
--                     a depth-0 self-row per team so descendant queries
--                     don't need a UNION with the node table.
--   team_memberships — flat (team_id, rs_user_id) — joining a team is
--                     explicit per-team (does NOT propagate down the
--                     closure; capability grants do, membership doesn't).
--
-- Why a closure table instead of a recursive CTE on every read:
--   The capability resolver consults this closure on every scoped
--   permission check. A page can produce 50+ permission checks per render;
--   re-running a recursive CTE per check would be wasteful even for small
--   trees. The closure is small (O(node_count^2) worst case, but in
--   practice O(node_count * depth)), the maintenance triggers fire only on
--   edge changes (which are rare admin actions), and lookups become a
--   single indexed pair-check.
--
-- DAG considerations:
--   In a DAG a pair (ancestor, descendant) may be reachable via multiple
--   paths with different lengths. We store one row per pair; `depth`
--   reflects whichever path inserted it first (insertion order). For our
--   use case only existence matters (the resolver asks "is A an ancestor
--   of D?"), so this is fine. If a future feature needs shortest-path
--   depth we'll need a more careful maintenance algorithm.
--
-- Federation prep (ADR 0007):
--   teams.origin_server_id is nullable. NULL = locally-owned team. Non-NULL
--   = mirror of a remote site's team; we maintain a local row per remote
--   team referenced by federated content. ACLs only ever reference local
--   IDs.

-- +goose Up

CREATE TABLE teams (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    slug             TEXT         NOT NULL,
    name             TEXT         NOT NULL,
    description      TEXT         NOT NULL DEFAULT '',
    origin_server_id UUID         NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ  NULL,
    -- Slug uniqueness is per origin: site A and site B can both have a
    -- team called "rnd", but within a single origin slugs must be unique.
    -- The NULL-distinct UNIQUE handles local origin (NULL) the same way
    -- as remote origins.
    UNIQUE NULLS NOT DISTINCT (origin_server_id, slug)
);

CREATE INDEX teams_origin_idx ON teams (origin_server_id) WHERE origin_server_id IS NOT NULL;
CREATE INDEX teams_active_idx ON teams (id) WHERE deleted_at IS NULL;

CREATE TABLE team_parents (
    child_id  UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    parent_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    PRIMARY KEY (child_id, parent_id),
    CHECK (child_id <> parent_id)
);

CREATE INDEX team_parents_parent_idx ON team_parents (parent_id);

CREATE TABLE team_closure (
    ancestor_id   UUID    NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    descendant_id UUID    NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    depth         INTEGER NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id)
);

CREATE INDEX team_closure_descendant_idx ON team_closure (descendant_id);

CREATE TABLE team_memberships (
    team_id             UUID         NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    rs_user_id          BIGINT       NOT NULL,
    added_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    added_by_rs_user_id BIGINT       NULL,
    PRIMARY KEY (team_id, rs_user_id)
);

CREATE INDEX team_memberships_user_idx ON team_memberships (rs_user_id);

-- ---------------------------------------------------------------------------
-- Closure maintenance triggers
-- ---------------------------------------------------------------------------

-- On INSERT into teams: add the (id, id, 0) self-row so descendant
-- queries always include the team itself.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION teams_insert_self_closure() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    VALUES (NEW.id, NEW.id, 0)
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER teams_self_closure
AFTER INSERT ON teams
FOR EACH ROW EXECUTE FUNCTION teams_insert_self_closure();

-- On INSERT into team_parents: reject self-edges and cycles, then
-- materialise the new transitive pairs.
--
-- Cycle check: if the closure already contains a path (child_id ->
-- parent_id), inserting (parent_id, child_id) creates a cycle.
--
-- Pair materialisation: every ancestor of parent_id pairs with every
-- descendant of child_id (including the endpoints themselves, via the
-- depth-0 self rows). ON CONFLICT DO NOTHING preserves whichever depth
-- got there first via an alternate path — for our use case (existence
-- check only) this is fine.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION team_parents_before_insert() RETURNS TRIGGER AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM team_closure
        WHERE ancestor_id   = NEW.child_id
          AND descendant_id = NEW.parent_id
    ) THEN
        RAISE EXCEPTION
            'team_parents: cycle detected (child % is already an ancestor of parent %)',
            NEW.child_id, NEW.parent_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER team_parents_reject_cycle
BEFORE INSERT ON team_parents
FOR EACH ROW EXECUTE FUNCTION team_parents_before_insert();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION team_parents_after_insert() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    SELECT ca.ancestor_id, cd.descendant_id, ca.depth + cd.depth + 1
      FROM team_closure ca
     CROSS JOIN team_closure cd
     WHERE ca.descendant_id = NEW.parent_id
       AND cd.ancestor_id   = NEW.child_id
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER team_parents_propagate_closure
AFTER INSERT ON team_parents
FOR EACH ROW EXECUTE FUNCTION team_parents_after_insert();

-- On DELETE from team_parents: rebuilding the closure for the affected
-- portion of the DAG incrementally is genuinely hard in the
-- multiple-parents case (the same pair can have multiple supporting
-- paths and we'd need to track which edges contributed). Since edge
-- removal is a rare admin operation and our team counts are small,
-- we do a full rebuild. O(teams * average_depth) per delete.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION team_closure_rebuild() RETURNS VOID AS $$
BEGIN
    TRUNCATE team_closure;
    -- Self-rows for every team
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    SELECT id, id, 0 FROM teams;
    -- Transitive pairs via recursive walk
    INSERT INTO team_closure (ancestor_id, descendant_id, depth)
    SELECT ancestor, descendant, MIN(depth)
      FROM (
        WITH RECURSIVE walk(ancestor, descendant, depth) AS (
            SELECT parent_id, child_id, 1 FROM team_parents
            UNION ALL
            SELECT w.ancestor, tp.child_id, w.depth + 1
              FROM walk w
              JOIN team_parents tp ON tp.parent_id = w.descendant
        )
        SELECT * FROM walk
      ) AS pairs
    GROUP BY ancestor, descendant
    ON CONFLICT DO NOTHING;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION team_parents_after_delete() RETURNS TRIGGER AS $$
BEGIN
    PERFORM team_closure_rebuild();
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER team_parents_rebuild_on_delete
AFTER DELETE ON team_parents
FOR EACH ROW EXECUTE FUNCTION team_parents_after_delete();

-- ---------------------------------------------------------------------------
-- Capabilities introduced by this migration
-- ---------------------------------------------------------------------------

INSERT INTO capabilities (code, description) VALUES
    ('teams.read',   'List teams and view team membership'),
    ('teams.create', 'Create new teams'),
    ('teams.admin',  'Edit any team (rename, re-parent, delete, manage members)')
ON CONFLICT (code) DO NOTHING;

-- Grant teams.read to Base (so any signed-in user can see the team list);
-- teams.admin and teams.create go to Admin only.
WITH base_role AS (SELECT id FROM roles WHERE name = 'Base'),
     admin_role AS (SELECT id FROM roles WHERE name = 'Admin')
INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'teams.read'   FROM base_role  UNION ALL
SELECT id, 'teams.create' FROM admin_role UNION ALL
SELECT id, 'teams.admin'  FROM admin_role
ON CONFLICT DO NOTHING;

-- +goose Down

DROP TRIGGER IF EXISTS team_parents_rebuild_on_delete    ON team_parents;
DROP TRIGGER IF EXISTS team_parents_propagate_closure    ON team_parents;
DROP TRIGGER IF EXISTS team_parents_reject_cycle         ON team_parents;
DROP TRIGGER IF EXISTS teams_self_closure                ON teams;

DROP FUNCTION IF EXISTS team_parents_after_delete();
DROP FUNCTION IF EXISTS team_closure_rebuild();
DROP FUNCTION IF EXISTS team_parents_after_insert();
DROP FUNCTION IF EXISTS team_parents_before_insert();
DROP FUNCTION IF EXISTS teams_insert_self_closure();

DELETE FROM role_capabilities WHERE capability_code IN ('teams.read','teams.create','teams.admin');
DELETE FROM capabilities      WHERE code            IN ('teams.read','teams.create','teams.admin');

DROP TABLE IF EXISTS team_memberships;
DROP TABLE IF EXISTS team_closure;
DROP TABLE IF EXISTS team_parents;
DROP TABLE IF EXISTS teams;
