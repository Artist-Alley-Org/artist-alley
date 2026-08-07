---
id: "0010"
title: Permissions, teams, and workflow states
status: accepted
date: 2026-05-26
area: architecture
phases: 
  - "1.3"
  - "1.7.B"
  - "1.13.D-2b"
  - "1.13.D-3"
  - "1.17"
supersedes: []
related: 
  - "0042"
  - "0044"
tags:
  - architecture
  - ai
  - infrastructure
  - 3d
excerpt: >-
  The authorization model laid down in migration 00002_capabilities_roles.sql (Phase 1.3) gave us three of the seven layers a real production permissions system needs:
---
## Amendment (2026-08-04): Layer 7 is NOT implemented — and one shape changed

~~The "fully implemented" claim below covers Layer 7.~~ It does not, and a
verification pass on 2026-08-04 established that it never did. Layers 1–6 shipped
as described; **Layer 7 shipped its schema and stopped.** Corrections, with
evidence:

- **`workflow.Transition()` has zero production callers.** The helper exists
  (`app/internal/workflow/service.go:118`) and is covered by seven passing tests,
  but nothing outside those tests calls it. The only non-test importer of
  `internal/workflow` is `app/internal/http/api.go:300`, wiring the read-only
  `GET /workflow/states` handler — the single workflow path in the API.
- **Therefore the central-helper property this ADR argued for does not hold.**
  `state_id` is written as a raw client-supplied UUID on create *and* on update:
  `posts/handler.go:271-276`, `posts/queries.sql:36` (`UpdatePost` sets
  `state_id = COALESCE(narg, state_id)`, so `PATCH /posts/{id}` reaches any state),
  `assets/handler.go:318-319`, and `scheduledactions/executor.go:165`. The eleven
  seeded `workflow_transitions` rows and their `required_capability` values are
  never consulted, and `workflow_audit` is never written by the API.

  The Decision section's claim that this design is *"the explicit fix for the
  'every call site has to remember the permission check' anti-pattern"* is the
  intent, not the current behaviour. Tracked as **#896**.
- **States are seeded-only.** The `workflow.admin` capability is seeded but gates
  no endpoint; there is no way to define a state or a transition without SQL
  against the database. Only `post` and `asset:1` (Photo) have any states, so
  every other asset type carries `state_id = NULL`. Tracked as **#897**.
- **`visible_by_default` is unread.** It is set deliberately in the baseline
  (false for draft/pending_review/archived/deleted) but no code consumes it.
  Whether workflow state should gate visibility at all is **undecided**, and it
  bears directly on ADR 0063/0064 — a second row-hiding plane keyed on state would
  contradict the single-predicate arrangement those ADRs establish. Decide there
  before building on it; part of #897.

### Shape change: `domain TEXT`, not `asset_type_id UUID`

The schema below specifies
`workflow_states.asset_type_id UUID NOT NULL REFERENCES asset_types(id)`. **The
implemented table has `domain TEXT NOT NULL` instead**, with an
`asset:<resource_type_ref>` convention produced by
`workflow.AssetDomain(int64) string` (`service.go:47`), plus a bare `post` domain.

This supersedes the ADR's shape and the reason is sound: posts have no asset type,
so an `asset_type_id` FK could not have expressed the `post` domain at all. Record
the cost too — the key is stringly-typed with no referential integrity, so
deleting an asset type silently orphans its states, and a typo is
indistinguishable from an empty domain. That is not hypothetical: **#895** is a
whole feature made dead by the difference between `asset` and `asset:1`.

Read this amendment together with the memory `reference_workflow_states_baseline`,
which holds the verified per-file detail.

## Implementation status (2026-06-19)

The decision recorded here is **fully implemented** as of the
1.17 arc landing on `dev` — **except Layer 7; see the 2026-08-04
amendment above**:

- **1.17.A — User approval states + admin approval workflow** ✅
  PR #138. Typed state machine, single-gate authn, session-cascade
  invalidation, last-admin invariant.
- **1.17.B — Capability grants with expiry** ✅ PR #139. Typed
  grant rows, deny precedence, scoped subject lookup,
  best-effort audit.
- **1.17.C — Capability sweeper** ✅ PR #140. Background job
  retires expired grants, NOTIFY-broadcasts cap-cache evictions
  across instances.
- **1.17.D — Profile-update audit ledger** ✅ PR #141. Reflective
  diff helper, changeset-only metadata, fail-open audit recording.
- **1.17.E — Resource request workflow** ✅ PR #142. Typed
  request lifecycle (open → approved/denied/expired), cache
  broadcast, owner + approver gate split.
- **1.17.F — Self-service profile editing + operator gates** ✅
  PR #143. Per-field `users.allow_self_edit.*` gates, new
  `profile.update_self` capability, 422 reason shape
  (`field_disabled_by_operator`), cross-instance cache
  invalidation. Closes issue #20.

The design held end-to-end through the arc: typed catalogues per
ADR 0042, NOTIFY/LISTEN cache invalidation per ADR 0013, and the
audit ledger anchored on ADR 0044 — no design changes against
implementation reality.

Related companion ADRs that share the typed-vocabulary discipline:
[ADR 0042](0042-distributed-catalogs-typed-per-package.md) (typed
constants per package) + [ADR 0044](0044-activities-ledger-cqrs-lite.md)
(activity ledger as audit substrate).

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
   sub-orgs (Aurora R&D, Aurora Live, R&D Character Art, Cross-Studio
   Art Review). The typical DAM doesn't model this and neither do we.
3. **There is no way to scope a role to a subset of resources.** "X
   is a Director *within team Aurora R&D*" can't be expressed; today
   `posts.admin` is global or absent.
4. **There are no per-resource or per-collection ACLs.** A typical DAM
   has `resource_custom_access` and `usergroup_collection`; we have neither.
5. **There are no workflow states.** A configurable `archive` column
   (`-2 pending submit / -1 pending review / 0 active / 1-2 archived /
   3 deleted`) with capability codes that gate transitions is a real
   feature we need, even if existing implementations are rough.
6. **Anonymous browse has no first-class principal.** Industry practice
   often encodes anonymous as a real user row named "guest", which
   scatters `username == $anonymous_login` special cases throughout the
   codebase.

The survey of existing DAM permission code surfaced eight design choices
we will explicitly *not* copy:

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

A team can have multiple parents. `Aurora R&D Character Art` may
belong to both `Aurora R&D` and `Cross-Studio Art Review`. The
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
restrict below them.

#### Amendment 2026-08-06 (#930, PR #936) — Layer 5 reaches content mutation, and a mutate gate must not also be a grant gate

Layer 5 was implemented for capability *resolution* (`Can(code, InTeam(id))`, `scopedCaps`
pre-expanded through `team_closure`) and **never called by a content handler**. `UpdateAsset` and
`DeleteAsset` had no authorisation at all — any authenticated caller could edit or delete any
asset, while only a super-admin could restore. Posts and collections were gated; assets were the
outlier.

`assets.admin` now exists, and `canMutateAsset` is `owner ∨ Can(cap, InTeam(team)) ∨ Can(cap) ∨
system.admin`. That is the "an art director manages their team's files, a team member does not
manage a colleague's" requirement, and it is Layer 4's closure doing the cascading.

**Two findings worth recording, because both were invisible until something depended on them:**

**1. `posts.admin` and `collections.admin` had never been grantable.** They existed as Go string
constants and were **never rows in `capabilities`**. Since `user_capability_grants.capability_code`
and `role_capabilities.capability_code` are both FK-constrained to `capabilities(code)`, neither
could ever be granted to a user or a role — so both moderator gates were, in practice,
`system.admin`-only. A whole permission tier was declared and unreachable. Seeded in 00037.

**⛔ 2. A gate that guards editing must NOT also guard granting.** `canMutatePost` had **seven**
call sites, and one of them was `AddPostAcl`. Adding the team-scoped disjunct to it — which is what
the sprint brief instructed — would have let a team-scoped holder **grant a stranger read access to
a colleague's post**. The escalation hid behind the gate's *name*: it reads as "may edit this post"
and in fact answered "may administer this post", including who else may reach it.

The rule this establishes:

> **Scope a capability to what the operation DOES, not to the object it acts on.** Mutation and
> access-widening are different rights over the same row and need different gates.

So the post surface now splits:

| gate | team-scoped? | guards |
|---|---|---|
| `canMutatePost` | **yes** | edit, delete, restore, and the rest |
| `canWidenPostAccess` | **no** — owner ∨ *global* `posts.admin` ∨ `system.admin` | post `visibility`, `AddPostAcl` |

`RemovePostAcl` deliberately keeps the wider gate: revoking narrows access, and narrowing is not
an escalation.

The same logic gates `assets.status` to owner + `system.admin` rather than to `assets.admin`:
`visibility/predicate.go` requires `status='active'` for the anonymous read branch, so publishing
a colleague's draft **is** the disclosure act even though `AssetUpdate` carries no `visibility`
field. (Delegating publication deliberately, via the unwired `assets.publish`, is **#938**.)

**This is the second privilege escalation to reach a sprint brief** — see ADR 0064's 2026-08-05
amendment for the first (#881, a decide gate scoped to its principal but not its payload). Both
were caught in implementation. The common shape: *a gate was widened without enumerating what it
authorised.*

**One seam left open on purpose**: a holder of `assets.admin` may mutate an asset whose content
they cannot read — `visibility.FieldsReadable` knows nothing about the capability. Whether mutation
should imply readability is **#939**, and it is a product decision rather than an oversight.

#### Amendment 2026-08-06 (#916, PR #932) — the three ACL surfaces do NOT accept the same principals

`principal_type` admits `user | role | team` on all three `*_acls` tables, and the CHECK
constraint is identical on each. **That uniformity is misleading, and the API now says so.**

| surface | `user` | `role` / `team` |
|---|---|---|
| `asset_type_acls` | works | **works** — `assettype/queries.sql` resolves them against `user_roles` and `team_memberships` |
| `post_acls` | works | **inert** — the read rule gates on `principal_type = 'user'` before it looks at `principal_id` |
| `collection_acls` | works | **inert** — same |

Role and team scoping *on content* is Layer 5, and Layer 5 is implemented for **capabilities**
(`Can(code, InTeam(id))`, pre-expanded through `team_closure`) but **not for content ACL rows**.
So a role or team grant on a post or collection was written, matched by nothing, and answered
`204`.

**Decision: the content ACL surfaces reject `role` and `team` with a 400 rather than storing an
inert row.** An API that accepts a reference it knows confers no access, and reports success, is
the same defect as accepting a username where a numeric ref is required — which is what #916 was.
Fixing one and not the other would have fixed half a bug.

Consequences worth stating:

- **This reversed an existing test.** `TestAddPostAcl_RoleAndTeamGrantsNotifyNobody` asserted the
  rows *were* stored, on the reasoning that *"notifies nobody is not licence to skip the grant"*.
  That reasoning was correct **about the notify path** — a notifier failure must never roll back a
  real grant — but it was applied to a grant that was never real.
- **`asset_type_acls` is deliberately exempt** and validates shape only. The distinction lives in
  one place, `internal/acls`: `ValidatePrincipalRef` (shape) versus `ValidateContentPrincipal`
  (shape **and** inertness, returning `ErrPrincipalInert`). One implementation, three call sites.
- **When Layer 5 extends to content ACLs, delete the validator call** — a 400 is trivially
  reversible. A table of inert rows would not have been: nothing distinguishes "granted before it
  worked" from "granted after" without archaeology.
- Pre-release, so there were no stored grants to preserve.

**A related divergence is NOT settled by this amendment**: `ListCollectionAcls` admits any caller
when the collection is `public`, while `ListPostAcls` requires owner-or-mutate. Tracked as **#933**;
whichever way it goes, the two surfaces should agree.

#### Amendment 2026-08-07 (#938, PR #952) — publication is delegable, per verb

The 2026-08-06 (#930) amendment above says *"the same logic gates `assets.status` to owner +
`system.admin` rather than to `assets.admin`"* and calls delegating it **#938**. That is now
implemented, and the sentence should be read as history: `assets.status` is gated to owner,
`system.admin`, **or the publication verb the specific transition requires**.

`assets.publish`, `assets.archive` and `assets.unarchive` were seeded in the baseline, granted to a
role there, listed in the admin capability surface — and consulted by nothing. That is the same
*accepted-but-inert* defect as #916's ACL rows: an operator granted one, believed they had
delegated publication, and had not.

The live enum is `draft | active | archived` — there is no `published` and no `pending_review` —
so the three verbs do not partition the six ordered transitions one-to-one. **One clause resolves
every overlap:**

> **Entering `active` always requires `assets.publish`, with no substitute.**

`active` is the state `visibility/predicate.go` tests for on the anonymous read branch, so
`→ active` is *the* disclosure act. A second route into it would silently turn some other verb into
a publication right. Leaving `active` is not a disclosure — it only removes reach — so the rest are
governed by whichever verb names them:

| transition | requires | why |
|---|---|---|
| `draft → active` | `publish` | the verb's own transition |
| `archived → active` | `publish` **and** `unarchive` | a disclosure *and* an exit from the archive; neither holder gets the other's decision |
| `draft → archived` | `archive` | entering the archive; neither endpoint is publicly reachable |
| `active → archived` | `archive` | retiring published work is a de-disclosure |
| `archived → draft` | `unarchive` | leaving the archive, to a private state |
| `active → draft` | `publish` | retraction is the publish decision reversed, and there is no `assets.unpublish` |

Consequences worth stating:

- **The two planes in `UpdateAsset` are now gated separately, and neither implies the other.** A
  content edit needs `canMutateAsset`; a status transition needs the matching verb. Requiring
  `assets.admin` *as well* would have meant publication could not be delegated without also handing
  over the power to rewrite — the same bundling this ADR's rule rejects, pointed the other way.
- **No new field-plane exposure.** A publication-only holder's `200` body is built by
  `enrichAssetDerived`, which already applies the #899/#939 withholding, so a publish grant is a
  right to decide reachability and not a side door into reading what you published.
- **`assets.submit` and `assets.review` remain unenforced**, and migration 00038 rewrites their
  descriptions to say so. They gate the exit from `pending_review`, which does not exist; building
  it is a schema decision for #895/#896/#897, and **#951** carries the choice between building it
  and deleting the two codes.
- **The seam was `assets.team_id`.** ~~Nothing in production writes it, so every asset created
  through the API has `team_id = NULL` and only a *global* publication grant reaches it.~~
  **Superseded 2026-08-07 by #953 / PR #955** — `AssetCreate` now carries an optional `team_id`, so
  the team-scoped path is reachable through the API.

  ⚠️ **The original claim here was wrong and is preserved struck through as a caution.** The seeder
  had been writing the column all along (`app/internal/seed/queries.sql`, ~1,900 rows in a seeded
  install); the search that "proved" its absence required `team_id` and `INSERT INTO assets` on one
  line, and that INSERT is multi-line. **To assert that nothing writes a column, query the data.**
  The real gap was narrower: the seeder could assign a team, the API could not.

Schema:

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

### Layer 7: Workflow states (configurable per resource type)

> **Not implemented as written — read the 2026-08-04 amendment at the top before
> designing against this section.** The tables exist and the central helper
> exists, but nothing calls it; `asset_type_id` shipped as `domain TEXT`; and
> "configurable" is aspirational (states are seeded, and there is no admin
> surface). Issues **#895**, **#896**, **#897**.

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

This is the explicit fix for the "every call site has to remember
`checkperm('e{N}')`" anti-pattern common in existing DAM tooling.

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
teams(id=<new local UUID>, slug='aurora.rnd', name='Aurora R&D',
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
  the "every caller remembers `checkperm`" failure mode.
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

---

### Amendment 2026-08-07 (#954 + #953, PR #955) — who may put something in a team

`team_id` on `posts` and `assets` is an **authorization input, not a label**. It decides who may
mutate the row — `canMutatePost` / `canMutateAsset` consult a Layer-5 scoped grant only when the row
carries a team — and for assets it also decides who may read it at `sensitivity='team'`.

It had been treated as neither. `CreatePost` accepted a **caller-asserted** `team_id` with only the
foreign key guarding it, so a post could be attributed to any team on the instance (#954). Assets
had the opposite defect: no API field at all, so only the seeder could assign one (#953).

**Decision: one rule, one implementation — `visibility.CanAssignToTeam`.** A caller may assign to a
team when they are a **direct member** of it, hold a **team-scoped** admin grant over it, or are
`system.admin`. Two thin adapters call it; nothing else expresses the rule.

Three parts of that are not obvious and are the reason this amendment exists:

- **A GLOBAL admin grant is deliberately insufficient.** `ScopedTeams` excludes globals and the
  wildcard by design, and assignment honours that. A global `posts.admin` is the *instance-moderator*
  role; #954 is precisely about content appearing in a studio's space it has nothing to do with, and
  a moderator is not thereby a member of every studio. `system.admin` is the sole escape hatch.
- **Membership is DIRECT; grants close over the hierarchy.** The asymmetry is deliberate.
  Delegated administration expands through `team_closure` (Layer 5), but membership does not —
  `is_team_member` is a plain `EXISTS` against `team_memberships` in `ContentReadable`,
  `ContentReadableSQL` and `FieldsColumnsSQL`, with no closure walk anywhere. A parent-team member
  permitted to assign into a descendant would hand out an audience they are not in, and could not
  read the result themselves. Direct is also the narrower reading, so it fails closed.
- **A soft-deleted team is unassignable by anyone**, `system.admin` included. The FK is
  `REFERENCES public.teams(id) ON DELETE SET NULL` and never consults `teams.deleted_at`, so without
  an explicit liveness probe a deleted team would satisfy it silently. The probe runs *before* the
  authorization disjunction.

**The capability is per-entity** — `posts.admin` for posts, `assets.admin` for assets — because
assignment is what *confers* the team-scoped mutation right over the new row, so the code naming
that right is the one entitled to hand it out. A single cross-entity code would let a holder of one
plant rows in the other's space; a new `teams.assign` code would be held by nobody and seeded by
nothing, which is how the team tier reached the state #953 describes in the first place.

**Refusals are indistinguishable.** An unauthorised-but-real team returns byte-identical output to a
nonexistent one; otherwise the endpoint is a team-existence probe across the instance. Same
discipline as #922 / #941 / #952.

**Not decided here: reassignment.** Neither `PostUpdate` nor `AssetUpdate` carries `team_id`. Moving
a row between teams changes who may mutate it *and* who may read it at the team tier, and wants its
own gate rather than a shared one.
