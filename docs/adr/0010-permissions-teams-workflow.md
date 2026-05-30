---
id: "0010"
title: Permissions, teams, and workflow states
status: proposed
date: 2026-05-26
area: architecture
phases: 
  - "1.3"
  - "1.7.B"
  - "1.13.D-2b"
  - "1.13.D-3"
supersedes: []
related: []
tags:
  - architecture
  - ai
  - infrastructure
  - 3d
excerpt: >-
  The authorization model laid down in migration 00002_capabilities_roles.sql (Phase 1.3) gave us three of the seven layers a real production permissions system needs:
---
## Context

The authorization model laid down in migration `00002_capabilities_roles.sql`
(Phase 1.3) gave us three of the seven layers a real production
permissions system needs:

- **Capabilities** as atomic permission codes (`posts.admin`,
  `users.write`, ...).
- **Roles** as named bundles of capabilities with single-inheritance
  parent chains (`BASE → Artist → Art Director`).
- **Per-user grants and revokes** on top of the role-derived set.

The role chain works today — `TestEffectiveCapabilities_ResolvesRoleChain`
in `app/internal/auth/capabilities_test.go` proves it. What's missing
became obvious once we started designing the upload modal and asking
"who can upload to which collection, who can see whose posts, who
approves a draft":

1. **A user can only hold one role.** The `user_role` PK on
   `rs_user_id` forces every multi-hat scenario into a combinatorial
   `"ArtDirector + SecurityAuditor"` role explosion.
2. **There are no teams.** A studio installing artist-alley has
   sub-orgs (Diablo R&D, Diablo Live, R&D Character Art, Cross-Studio
   Art Review). RS doesn't model this and neither do we.
3. **There is no way to scope a role to a subset of resources.** "X
   is a Director *within team Diablo-R&D*" can't be expressed; today
   `posts.admin` is global or absent.
4. **There are no per-resource or per-collection ACLs.** RS has
   `resource_custom_access` and `usergroup_collection`; we have neither.
5. **There are no workflow states.** RS's `archive` column
   (`-2 pending submit / -1 pending review / 0 active / 1-2 archived /
   3 deleted`) and the `e{N}` capability codes that gate transitions
   are a real feature we need, even though RS's specific implementation
   is rough.
6. **Anonymous browse has no first-class principal.** RS encodes
   anonymous as a real user row named "guest", which scatters
   `username == $anonymous_login` special cases throughout the codebase.

The survey of RS's permission code surfaced eight design choices we
will explicitly *not* copy:

- CSV-in-text-column for permissions (no FK integrity, no enum,
  typos silently no-op).
- `usergroup.parent` as CSV varchar with `FIND_IN_SET` lookups.
- `config_options` text column that gets `eval`'d.
- 400-line `get_resource_access()` mixing eight orthogonal concerns.
- Mixed grant/deny semantics (`v` grants, `g`/`X`/`z`/`T` restrict,
  `XE-{id}` re-allows under `XE`).
- `update_archive_status()` not gating transitions itself — every
  caller has to remember the `checkperm`.
- Anonymous as a real user row.
- `approved` as a boolean on `user` with no audit trail.

This ADR locks in the design before Phase 1.7.B implements it, and
before Phase 1.13.D-2b (upload modal) which is the first surface
that needs team-aware checks on day one.

## Decision

### Seven layers, composed orthogonally

| Layer | What it answers | Status |
|---|---|---|
| 1. Capabilities | "What verbs exist?" | exists (00002) |
| 2. Roles with parent chain | "What does this role mean?" | exists (00002) |
| 3. Multi-role per user | "Who is this user?" | **change**: widen to many-to-many |
| 4. Teams (DAG via closure table) | "What organisational scope exists?" | **new** |
| 5. Team-scoped role assignments | "Where does this user have this role?" | **new** |
| 6. Per-resource / per-collection ACLs | "Has someone been granted an explicit exception?" | **new** |
| 7. Workflow states (per resource type) | "Where is this asset in its lifecycle, and who can move it?" | **new** |

A request is allowed iff *any* layer grants it after revokes are
applied. There is one capability resolution function. Handlers call
it; they do not implement their own permission logic.

### Layer 3: Multi-role per user

Rename `user_role` → `user_roles` (plural) and widen to many-to-many
with optional team scope (see Layer 5). A user's effective
capabilities are the union of:

- Every assigned role's chain (transitive via `roles.parent_id`)
- Every row in `user_capability_grants`
- Minus every row in `user_capability_revokes`

Each of those rows can itself carry a `team_id` (Layer 5) which
constrains where the grant applies. The chain walk is unchanged from
today's implementation — it just runs once per assigned role instead
of once per user.

### Layer 4: Teams as a DAG (closure table)

A team can have multiple parents. `Diablo R&D Character Art` may
belong to both `Diablo R&D` and `Cross-Studio Art Review`. The
storage:

- `teams` — the node table.
- `team_parents` — direct parent edges. Multiple rows per team allowed.
- `team_closure` — materialised `(ancestor, descendant, depth)`
  tuples maintained by trigger on `team_parents`. Self-row (depth 0)
  per team makes "is X in team Y or any descendant" a single indexed
  lookup.

A cycle-detection trigger on `team_parents` rejects any insert that
would create one (`SELECT 1 FROM team_closure WHERE ancestor =
NEW.child AND descendant = NEW.parent`).

`team_memberships` is a flat `(team_id, rs_user_id)` table. Membership
in a team does **not** propagate to descendants — you join each team
you're a member of explicitly. (Capability grants propagate downward
through the closure; membership doesn't.)

### Layer 5: Team-scoped role and capability grants

`user_roles`, `user_capability_grants`, and `user_capability_revokes`
all gain a nullable `team_id UUID REFERENCES teams(id)`:

- `team_id IS NULL` → global assignment ("Sarah is an Art Director
  everywhere").
- `team_id = X` → scoped assignment ("Sarah is an Art Director within
  team X *and all its descendants*"). Descendant propagation is what
  the closure table makes cheap.

Uniqueness:

- `PRIMARY KEY (rs_user_id, role_id, team_id)` handles uniqueness
  for team-scoped rows.
- `CREATE UNIQUE INDEX … WHERE team_id IS NULL` prevents duplicate
  global assignments (Postgres treats `NULL ≠ NULL`, so the partial
  index is required).

### Capability resolution

The single check the handler layer calls:

```go
id.Can(capability string, scope ...TeamScope) bool
```

Resolution:

1. Compute the user's effective capability set on session load
   (cached in the Identity). Each entry is `(code, team_id_or_nil)`.
2. For an unscoped check (`id.Can("users.read")`): allow iff any
   entry has the code with `team_id_or_nil = NULL`.
3. For a scoped check (`id.Can("posts.edit", InTeam(post.TeamID))`):
   allow iff any entry has the code with either `team_id_or_nil = NULL`
   (global grant) **or** `team_id_or_nil` is an ancestor of the
   target team per `team_closure`.
4. `system.admin` continues to satisfy every check (global wildcard).

Revokes apply at the entry level — a revoke of `posts.delete` scoped
to team X removes only the team-X entry, not the global one.

### Layer 6: Per-resource and per-collection ACLs

ACLs are additive exceptions, not the primary access mechanism.
The primary path is:

- Public visibility → readable per the resource's `visibility` column.
- Team-scoped resource → readable per Layer 5 (capability +
  team-closure check).
- Owner → always readable and writable.

ACLs grant *additional* access beyond those defaults. They never
restrict below them. Schema:

```sql
CREATE TABLE post_acls (
    post_id         UUID NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    principal_type  TEXT NOT NULL CHECK (principal_type IN ('user','role','team')),
    principal_id    TEXT NOT NULL,
    permission      TEXT NOT NULL CHECK (permission IN ('read','write','admin')),
    granted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    granted_by_rs_user_id BIGINT NULL,
    expires_at      TIMESTAMPTZ NULL,
    PRIMARY KEY (post_id, principal_type, principal_id, permission)
);
CREATE INDEX post_acls_principal_idx ON post_acls (principal_type, principal_id);
```

`collection_acls` is identical with `collection_id`. The polymorphic
`principal_type` + `principal_id` shape loses per-table FK integrity
in exchange for a single query path; the trigger-maintained
referential cleanup (when a user/role/team is deleted, sweep matching
ACL rows) covers the gap.

`expires_at` enables time-boxed shares ("Marketing has read access
to this post for 7 days") without a separate share-links table.

### Layer 7: Workflow states (RS-style, configurable per resource type)

Each resource type owns its own state list and its own transition
graph. A concept-art pipeline can run `idea → wip → review → final`;
a final-render pipeline runs `draft → qa → published → archived`.

```sql
CREATE TABLE workflow_states (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_type_id    UUID NOT NULL REFERENCES asset_types(id) ON DELETE CASCADE,
    code                TEXT NOT NULL,
    label               TEXT NOT NULL,
    sort_order          INTEGER NOT NULL DEFAULT 0,
    is_initial          BOOLEAN NOT NULL DEFAULT FALSE,
    is_terminal         BOOLEAN NOT NULL DEFAULT FALSE,
    visible_by_default  BOOLEAN NOT NULL DEFAULT TRUE,
    UNIQUE (asset_type_id, code)
);

CREATE TABLE workflow_transitions (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    from_state_id       UUID NULL REFERENCES workflow_states(id) ON DELETE CASCADE,
    to_state_id         UUID NOT NULL REFERENCES workflow_states(id) ON DELETE CASCADE,
    required_capability TEXT NULL REFERENCES capabilities(code) ON DELETE SET NULL,
    requires_team_scope BOOLEAN NOT NULL DEFAULT FALSE,
    UNIQUE (from_state_id, to_state_id)
);
```

- `from_state_id IS NULL` represents the create-from-nothing
  transition. Exactly one row per resource type has both
  `from_state_id IS NULL` and a `to_state_id` whose row has
  `is_initial = TRUE`.
- `required_capability` is the cap a caller must hold to fire this
  transition. `NULL` means "any authenticated user" (owner is
  always allowed by the central helper regardless).
- `requires_team_scope = TRUE` means the capability must be held
  *within the resource's team scope* (Layer 5), not just globally.

The state lives on the resource (`posts.state_id`, `assets.state_id`).
All state changes go through one central helper —
`workflow.Transition(ctx, resource, toState)` — which:

1. Looks up the `workflow_transitions` row for `(from, to)`. Rejects
   if absent.
2. Checks the required capability (scoped if the row demands it).
3. Records the transition in `workflow_audit` (who, when, from, to,
   note).
4. Updates the resource row.

This is the explicit fix for RS's "every call site has to remember
`checkperm('e{N}')`" anti-pattern.

### Layer 7b: Anonymous as a synthetic role

A single seeded role, `Anonymous`, with no capabilities by default.
The auth middleware, when no session is present, injects an
`Identity{Roles: [Anonymous]}` instead of returning 401. Handlers
that should allow anonymous access (`/posts` with public visibility,
once `system.anonymous_browse_enabled` is set) ask the same
`id.Can(...)` question — the middleware just made Anonymous a
real principal.

Enabling anonymous browse becomes one config flag plus one capability
grant: `INSERT INTO role_capabilities (role_id, capability_code)
SELECT id, 'posts.read.public' FROM roles WHERE name = 'Anonymous'`.

### Federation: per-site teams mirrored on demand

When a remote site sends us an asset, any team references it carries
become local mirror rows:

```
teams(id=<new local UUID>, slug='diablo.rnd', name='Diablo R&D',
      origin_server_id=<remote site UUID>)
```

Our local users and our local ACLs only ever reference local team IDs.
The federation layer (post-MVP) translates between remote team
references and local mirror rows. This matches how we already mirror
roles (`roles.origin_server_id`) and matches ActivityPub-style
federation patterns.

Permissions never cross sites: a grant authored by Site B applies
only to Site B's local users. Our users see federated content per
*our* ACLs, applied to *our* mirrored team rows.

### Schema (consolidated)

```sql
-- Layer 3 widening: drop old user_role, create user_roles many-to-many.

DROP TABLE user_role;

CREATE TABLE user_roles (
    rs_user_id             BIGINT       NOT NULL,
    role_id                UUID         NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    team_id                UUID         NULL REFERENCES teams(id) ON DELETE CASCADE,
    assigned_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    assigned_by_rs_user_id BIGINT       NULL,
    PRIMARY KEY (rs_user_id, role_id, team_id)
);
CREATE UNIQUE INDEX user_roles_global_unique
    ON user_roles (rs_user_id, role_id) WHERE team_id IS NULL;
CREATE INDEX user_roles_user_idx ON user_roles (rs_user_id);
CREATE INDEX user_roles_team_idx ON user_roles (team_id) WHERE team_id IS NOT NULL;

-- user_capability_grants and user_capability_revokes gain team_id
-- (NULL = global) with the same partial-index trick.

-- Layer 4: teams.

CREATE TABLE teams (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    slug             TEXT         NOT NULL,
    name             TEXT         NOT NULL,
    description      TEXT         NOT NULL DEFAULT '',
    origin_server_id UUID         NULL,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (origin_server_id, slug)
);

CREATE TABLE team_parents (
    child_id  UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    parent_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    PRIMARY KEY (child_id, parent_id),
    CHECK (child_id <> parent_id)
);

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

-- Layer 6: ACLs (post_acls + collection_acls, identical shape; one shown).

CREATE TABLE post_acls (
    post_id               UUID         NOT NULL REFERENCES posts(id) ON DELETE CASCADE,
    principal_type        TEXT         NOT NULL CHECK (principal_type IN ('user','role','team')),
    principal_id          TEXT         NOT NULL,
    permission            TEXT         NOT NULL CHECK (permission IN ('read','write','admin')),
    granted_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    granted_by_rs_user_id BIGINT       NULL,
    expires_at            TIMESTAMPTZ  NULL,
    PRIMARY KEY (post_id, principal_type, principal_id, permission)
);
CREATE INDEX post_acls_principal_idx ON post_acls (principal_type, principal_id);

-- Layer 7: workflow.

CREATE TABLE workflow_states (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_type_id    UUID         NOT NULL REFERENCES asset_types(id) ON DELETE CASCADE,
    code                TEXT         NOT NULL,
    label               TEXT         NOT NULL,
    sort_order          INTEGER      NOT NULL DEFAULT 0,
    is_initial          BOOLEAN      NOT NULL DEFAULT FALSE,
    is_terminal         BOOLEAN      NOT NULL DEFAULT FALSE,
    visible_by_default  BOOLEAN      NOT NULL DEFAULT TRUE,
    UNIQUE (asset_type_id, code)
);

CREATE TABLE workflow_transitions (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    from_state_id       UUID         NULL REFERENCES workflow_states(id) ON DELETE CASCADE,
    to_state_id         UUID         NOT NULL REFERENCES workflow_states(id) ON DELETE CASCADE,
    required_capability TEXT         NULL REFERENCES capabilities(code) ON DELETE SET NULL,
    requires_team_scope BOOLEAN      NOT NULL DEFAULT FALSE,
    UNIQUE (from_state_id, to_state_id)
);

CREATE TABLE workflow_audit (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_kind       TEXT         NOT NULL,  -- 'post' | 'asset' | future
    resource_id         UUID         NOT NULL,
    from_state_id       UUID         NULL REFERENCES workflow_states(id),
    to_state_id         UUID         NOT NULL REFERENCES workflow_states(id),
    actor_rs_user_id    BIGINT       NULL,
    note                TEXT         NOT NULL DEFAULT '',
    transitioned_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX workflow_audit_resource_idx ON workflow_audit (resource_kind, resource_id, transitioned_at DESC);

-- Posts and assets gain state_id and (optional) team_id.

ALTER TABLE posts  ADD COLUMN state_id UUID NULL REFERENCES workflow_states(id);
ALTER TABLE posts  ADD COLUMN team_id  UUID NULL REFERENCES teams(id);
ALTER TABLE assets ADD COLUMN state_id UUID NULL REFERENCES workflow_states(id);
ALTER TABLE assets ADD COLUMN team_id  UUID NULL REFERENCES teams(id);

CREATE INDEX posts_state_idx  ON posts  (state_id);
CREATE INDEX posts_team_idx   ON posts  (team_id)  WHERE team_id  IS NOT NULL;
CREATE INDEX assets_state_idx ON assets (state_id);
CREATE INDEX assets_team_idx  ON assets (team_id)  WHERE team_id  IS NOT NULL;
```

### API surface

```
GET    /teams                          — list (filterable by ancestor)
POST   /teams                          — create
GET    /teams/{id}                     — fetch
PATCH  /teams/{id}                     — rename / re-parent
DELETE /teams/{id}                     — delete (rejects if non-empty)
GET    /teams/{id}/members             — list memberships
POST   /teams/{id}/members             — add member
DELETE /teams/{id}/members/{user_ref}  — remove member
GET    /teams/{id}/roles               — list role assignments scoped to this team

GET    /posts/{id}/acls                — list ACL rows
POST   /posts/{id}/acls                — add ACL row
DELETE /posts/{id}/acls/{principal_type}/{principal_id}/{permission}

GET    /collections/{id}/acls          — same shape

GET    /asset-types/{id}/workflow   — list states + transitions for this type
POST   /asset-types/{id}/workflow/states
POST   /asset-types/{id}/workflow/transitions
POST   /posts/{id}/transitions         — { to_state_id, note? }   — fires workflow.Transition
GET    /posts/{id}/transitions         — audit history
```

Capability codes added by this ADR:

```
teams.read           teams.admin       teams.create
posts.read.public    posts.read.team   posts.review     posts.publish
collections.read     collections.write collections.admin
workflow.admin
```

### Migration plan

This lands as Phase 1.7.B in five migrations:

- **00015_teams.sql** — teams, team_parents, team_closure, the
  cycle-rejection + closure-maintenance triggers, team_memberships.
- **00016_user_roles_widen.sql** — drop `user_role`, create
  `user_roles` with `team_id`, copy any existing assignment over.
  Same `team_id` column added to grants/revokes.
- **00017_acls.sql** — `post_acls`, `collection_acls`, plus the
  trigger that sweeps ACL rows when a referenced principal is
  deleted.
- **00018_workflow.sql** — `workflow_states`, `workflow_transitions`,
  `workflow_audit`. Seeds the default state set for the `image`
  resource type (`draft → pending_review → published → archived`)
  to bootstrap the upload modal.
- **00019_anonymous_role.sql** — seed the `Anonymous` role and the
  middleware path that injects it on sessionless requests.

Each migration carries its own goose Down. Pre-MVP wipe-and-reseed
remains the working assumption — we don't engineer a data-preserving
backfill.

## Consequences

**Positive:**

- Multi-hat users without combinatorial role explosion.
- Team-scoped admins are a first-class concept — `id.Can("posts.admin",
  InTeam(t))` is the same one-liner today's code already writes.
- DAG-shaped teams handle cross-cutting org structures that trees
  can't (Cross-Studio Art Review claiming sub-teams from multiple
  game teams).
- ACLs as additive exceptions, not the primary path, keeps reads
  fast: the common case never touches the ACL tables.
- Workflow transitions are gated centrally; we cannot regress to
  RS's "every caller remembers `checkperm`" failure mode.
- Anonymous is a real principal, not a magic username string —
  one code path handles authenticated and anonymous reads.
- Federation hooks (`origin_server_id` on teams) mirror the pattern
  already on roles and resources.

**Negative:**

- Migration `00016` rewrites `user_role` — but Phase 1.13.D-3 has
  basically two real role assignments in dev DBs (the seeded admin
  and the test user), so cost is near zero.
- The polymorphic `principal_type` + `principal_id` ACL shape loses
  per-table FK integrity. We mitigate with sweep triggers on
  user/role/team delete. If that proves brittle we split into
  `post_user_acls` / `post_role_acls` / `post_team_acls` later
  (one-shot table split, no API change).
- Closure-table maintenance is one more trigger to reason about. The
  pattern is well-trodden (`SELECT ancestor_id, NEW.descendant_id, depth+1
  FROM team_closure WHERE descendant_id = NEW.parent_id …`), but
  it's still infrastructure we own.

**Deferred:**

- Auditable role/cap *grants* (who gave Sarah `posts.admin` and when)
  — `user_capability_grants` has `granted_by_rs_user_id` but no
  historical log. Add when an audit surface needs it.
- Approval workflows for team membership (does joining a team need
  the team admin's approval, or is it self-serve when invited?).
  Default: invite-based, admin-approved.
- Quota-style constraints ("Sarah's `posts.create` is limited to
  100/day"). Out of scope; treated as a future rate-limit feature
  built on the audit log.

## Open questions

- Whether `team_memberships` should carry a `role_id` of its own to
  support a "team contributor" role implicitly. Current design says
  no — role assignment is in `user_roles` with a `team_id`, and
  membership is a separate "I'm a person in this team" fact. Revisit
  if real installs want one-row "add Sarah as a Reviewer in team X"
  shortcut.
- Whether ACLs should also support a `principal_type = 'access_link'`
  for share-link tokens, or whether share-links get their own table
  like collections did. Lean toward the latter: links are
  cryptographic principals with different lifecycle, not the same
  thing as user/role/team grants.
- Whether `posts.team_id` and `assets.team_id` should be `NOT NULL`
  with a default `'public'` team, or remain nullable. Nullable is
  simpler today; making it NOT NULL would close the gap where a
  resource has no team and falls back to visibility-only checks.
  Default to nullable; revisit if a real install demands every
  resource have a team.
