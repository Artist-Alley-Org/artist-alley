-- artist-alley migration 00016 — widen user_role to user_roles (multi-role
-- per user) and add team_id scoping to role assignments, capability
-- grants, and capability revokes.
--
-- See ADR 0010, Layer 3 (multi-role per user) and Layer 5 (team-scoped
-- role/grant/revoke rows).
--
-- Pre-existing rows in user_role / user_capability_grants /
-- user_capability_revokes carry over with team_id = NULL ("global
-- assignment"), preserving their current effect exactly.
--
-- Uniqueness shape:
--   - Each table gets a UNIQUE NULLS NOT DISTINCT constraint (PG15+)
--     over (rs_user_id, *, team_id) so global rows (team_id IS NULL)
--     also enforce one-per-pair. Without NULLS NOT DISTINCT, Postgres
--     would allow duplicate global rows because NULL != NULL.
--   - We DON'T use a real PK over the nullable team_id (Postgres
--     forbids PK on nullable columns); the unique constraint plays
--     the same role for ON CONFLICT targets.
--
-- Query implications (sqlc-level updates land in this same commit):
--   - Renames FROM user_role → FROM user_roles in every existing query.
--   - The recursive role-chain CTE in EffectiveCapabilitiesForUser now
--     seeds from MANY assigned roles instead of exactly one. The CTE
--     was already DISTINCTing the cap set, so multi-role union is
--     correct without further change. Phase 1.7.B-3 introduces a
--     separate scoped-resolution path that consults team_id; this
--     migration's queries still ignore team_id (returning the global-
--     scope view) so the existing handlers keep their current
--     behaviour during the transition.
--   - SetUserRole's old ON CONFLICT (rs_user_id) DO UPDATE semantics
--     don't fit a multi-row world. The query becomes "replace the
--     user's global role assignment" via a CTE-driven delete+insert,
--     preserving caller semantics exactly.

-- +goose Up

-- ---------------------------------------------------------------------------
-- user_role → user_roles
-- ---------------------------------------------------------------------------

ALTER TABLE user_role DROP CONSTRAINT user_role_pkey;
ALTER TABLE user_role RENAME TO user_roles;
ALTER TABLE user_roles
    ADD COLUMN team_id UUID NULL REFERENCES teams(id) ON DELETE CASCADE;
ALTER TABLE user_roles
    ADD CONSTRAINT user_roles_unique
        UNIQUE NULLS NOT DISTINCT (rs_user_id, role_id, team_id);

-- The old PK was on rs_user_id alone (one role per user). Replace
-- the indexes accordingly.
DROP INDEX IF EXISTS user_role__role_idx;
CREATE INDEX user_roles_user_idx ON user_roles (rs_user_id);
CREATE INDEX user_roles_role_idx ON user_roles (role_id);
CREATE INDEX user_roles_team_idx ON user_roles (team_id) WHERE team_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Per-user grants + revokes: add team_id (NULL = global).
-- ---------------------------------------------------------------------------

ALTER TABLE user_capability_grants DROP CONSTRAINT user_capability_grants_pkey;
ALTER TABLE user_capability_grants
    ADD COLUMN team_id UUID NULL REFERENCES teams(id) ON DELETE CASCADE;
ALTER TABLE user_capability_grants
    ADD CONSTRAINT user_capability_grants_unique
        UNIQUE NULLS NOT DISTINCT (rs_user_id, capability_code, team_id);

CREATE INDEX user_capability_grants_user_idx ON user_capability_grants (rs_user_id);
CREATE INDEX user_capability_grants_team_idx ON user_capability_grants (team_id) WHERE team_id IS NOT NULL;

ALTER TABLE user_capability_revokes DROP CONSTRAINT user_capability_revokes_pkey;
ALTER TABLE user_capability_revokes
    ADD COLUMN team_id UUID NULL REFERENCES teams(id) ON DELETE CASCADE;
ALTER TABLE user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_unique
        UNIQUE NULLS NOT DISTINCT (rs_user_id, capability_code, team_id);

CREATE INDEX user_capability_revokes_user_idx ON user_capability_revokes (rs_user_id);
CREATE INDEX user_capability_revokes_team_idx ON user_capability_revokes (team_id) WHERE team_id IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS user_capability_revokes_team_idx;
DROP INDEX IF EXISTS user_capability_revokes_user_idx;
ALTER TABLE user_capability_revokes DROP CONSTRAINT IF EXISTS user_capability_revokes_unique;
ALTER TABLE user_capability_revokes DROP COLUMN IF EXISTS team_id;
ALTER TABLE user_capability_revokes
    ADD CONSTRAINT user_capability_revokes_pkey PRIMARY KEY (rs_user_id, capability_code);

DROP INDEX IF EXISTS user_capability_grants_team_idx;
DROP INDEX IF EXISTS user_capability_grants_user_idx;
ALTER TABLE user_capability_grants DROP CONSTRAINT IF EXISTS user_capability_grants_unique;
ALTER TABLE user_capability_grants DROP COLUMN IF EXISTS team_id;
ALTER TABLE user_capability_grants
    ADD CONSTRAINT user_capability_grants_pkey PRIMARY KEY (rs_user_id, capability_code);

DROP INDEX IF EXISTS user_roles_team_idx;
DROP INDEX IF EXISTS user_roles_role_idx;
DROP INDEX IF EXISTS user_roles_user_idx;
ALTER TABLE user_roles DROP CONSTRAINT IF EXISTS user_roles_unique;
ALTER TABLE user_roles DROP COLUMN IF EXISTS team_id;
ALTER TABLE user_roles RENAME TO user_role;
ALTER TABLE user_role
    ADD CONSTRAINT user_role_pkey PRIMARY KEY (rs_user_id);
CREATE INDEX user_role__role_idx ON user_role (role_id);
