---
id: "0009"
title: Collection model — three-axis primitives, query-or-manual membership, per-row TTL
status: accepted
date: 2026-05-25
area: architecture
phases: 
  - "1.8"
supersedes: []
related: 
  - "0007"
  - "0010"
tags:
  - architecture
  - ai
  - infrastructure
  - commerce
  - 3d
excerpt: >-
  The prior generation of DAM tooling presents users with at least six visible collection "types" — Personal, Public, Featured, Smart, Request, Upload — plus a parallel structure for "featured categories" (a curator-maintained tree), a separate table for smart collections that pretend to be real ones in the UI, and external access keys grafted on as a sharing mechanism.
---
## Amendment (2026-08-05): a prior-art pass on collections × search — one confirmation, one gap, one deliberate divergence

Two mature DAMs were read for how collections and search interact (facts in memory
`reference_rs_search_baseline`). This ADR's core decision holds; three things are worth
recording so they are not re-derived or re-opened.

**1. CONFIRMED — a collection is both a thing you find and a thing you search inside.**
Both products return collections as ordinary rows in the main result set rather than on a
separate surface: one exposes them through the same control that picks resource types (the
equivalent of our kind chips), the other models containers as `DocType`s and interleaves them.
The owner reached the same call independently on 2026-08-05 — *"we can search collections"* —
and #850 shipped it. **No change needed; recorded because an unrecorded null result is why the
question gets asked again.**

The corollary the owner drew is the sharper half and belongs here: *a collection you searched
for is a result; a collection promoted at you while you were reading results is not.* One
product's curated surface lives on its home page (its collection table has `home_page_publish` /
`home_page_text` / `home_page_image` columns naming it) and its results page carries no curated
shelf. Ours did, unconditionally — tracked as **#908**.

**2. GAP — "smart" membership is declared and unimplemented.** `collections.membership` CHECKs
`('manual','query','hybrid')` and `collections.smart_query` exists, but
`collections/handler.go:237` rejects anything but `manual` with *"only membership=manual is
supported in this release"*, and `smart_query` is SELECTed everywhere and evaluated nowhere.
"Save as collection" materialises a **snapshot** (`search/saveas.go:125`), which is a real
feature but not what the column promises.

This ADR's Context criticises the prior generation for *"a separate table for smart collections
that pretend to be real ones in the UI"* — and it was right that a parallel table is the wrong
shape. But **declaring three membership modes and implementing one is its own version of the
same problem**: the model claims a capability the system does not have. Either implement the
modes or narrow the CHECK. Tracked as **#911**, which also carries the decisions it needs
(read-time vs materialised, per-caller evaluation, and what a query-backed collection means when
it federates).

**3. DELIBERATE DIVERGENCE — container membership does not grant access, and we are the outlier.**
One product surveyed cascades permissions from container to contents **by default**
(`DisableInheritance` maps to "Apply to Contained Assets" and defaults to on). We chose the
opposite in #883 / #892 / #894: membership never widens an item's visibility, a foreign
restricted member renders as a placeholder carrying only its owner's name, and a federated
collection share stops at the members its grantor owned.

That divergence is intentional and should stay. Cascade-by-default makes "add it to a collection
you can share" a one-click privilege escalation, which is precisely the primitive #883 exists to
prevent. Any future work on collection sharing inherits this, including **#910**
(search-inside-a-collection): scoping is a narrowing, never a widening.

## Context

The prior generation of DAM tooling presents users with at least six
visible collection "types" — Personal, Public, Featured, Smart, Request,
Upload — plus a parallel structure for "featured categories" (a
curator-maintained tree), a separate table (`collection_savedsearch`)
for smart collections that pretend to be real ones in the UI, and
external access keys grafted on as a sharing mechanism. The result is a
model where the same underlying concept (an ordered, optionally-shared
bag of resources) is fragmented across columns, types, and tables, and
where every new feature requires deciding which enum value it belongs
to.

For artist-alley we want a single coherent model that:

- Collapses the typical collection type enum into orthogonal axes that
  compose freely (a collection is private *and* manually populated *and*
  permanent; another is shared-by-link *and* query-driven *and* expires
  in 7 days; both are the same primitive).
- Treats smart collections as a *membership policy* on a regular
  collection, not a separate entity.
- Supports temporary collections natively, with per-collection and
  per-membership TTL — useful for ad-hoc review playlists, time-boxed
  shareable links, and "I'm including this in the showcase for a
  week only" cases the typical DAM can't express.
- Makes editing genuinely usable: move (not just copy) resources
  between collections, duplicate a collection in one step, bulk
  operations.

This ADR locks in the model before Phase 1.8 (the resource entity
port) implements it.

## Decision

### Three orthogonal axes, one `collection` table

A collection is defined by independent choices along three axes plus
a few optional purpose hints.

| Axis | Values | Meaning |
|---|---|---|
| `visibility` | `private` / `shared` / `public` | who can see the collection |
| `membership` | `manual` / `query` / `hybrid` | how resources get into it |
| `lifecycle` | `expires_at NULL` (permanent) / `expires_at` set (TTL) | when the collection auto-deletes |
| `featured` | bool | surface on the dashboard (orthogonal to visibility) |
| `purpose` | optional tag string | `review_playlist`, `upload_staging`, etc. — UI hint, not behaviour-gating |

Smart collections become `membership = 'query'`. Personal collections
become `visibility = 'private', membership = 'manual'`. The legacy
"Featured Categories" (a curator-maintained tree of public collections)
is *not* re-implemented: any collection can carry `featured = true` and
appear on a `/featured` listing. The tree-of-collections concept is
dropped — three users we surveyed informally couldn't tell us when
they last used it.

### Hybrid membership

A collection can be query-driven *and* let the owner manually pin or
exclude specific resources. The materialized membership is:

```
(resources matching the query) ∪ (manually pinned) − (manually excluded)
```

This unlocks "everything tagged 'character art', plus these three
extras, minus that draft" — a real workflow the typical DAM forces
users to either maintain manually or rebuild a search for.

### Per-membership TTL

Manual `collection_resource` rows can carry their own `expires_at`.
The cron sweeper that prunes expired collections also prunes
expired memberships. Used for:

- Review session playlists where individual pieces drop out after the
  session date.
- Featured rotation: include a piece for 7 days, then it falls out.
- Trial inclusion when a curator hasn't decided.

### Mutation operations as first-class

Beyond CRUD, the API explicitly supports:

- **Move**: relocate resources from collection A to collection B in
  one transactional call (single atomic operation, no half-state).
- **Copy**: same as move but leaves the source membership.
- **Duplicate**: clone a whole collection (its membership rows, and
  its query definition if it has one). The duplicate defaults to
  `visibility = 'private'` regardless of source — duplicating doesn't
  silently grant publicness.
- **Bulk add/remove** with idempotent semantics (re-adding an
  existing resource is a no-op).

### TTL ergonomics

Both presets and arbitrary dates. The UI offers chips (1h / 1d / 7d /
30d / never) plus a date-time picker for the rest. Stored as a
single `expires_at TIMESTAMPTZ NULL`.

### Schema

```sql
CREATE TABLE collection (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_ref    BIGINT       NOT NULL,
    name              TEXT         NOT NULL,
    description       TEXT         NOT NULL DEFAULT '',
    visibility        TEXT         NOT NULL DEFAULT 'private'
                                   CHECK (visibility IN ('private','shared','public')),
    membership        TEXT         NOT NULL DEFAULT 'manual'
                                   CHECK (membership IN ('manual','query','hybrid')),
    expires_at        TIMESTAMPTZ  NULL,
    featured          BOOLEAN      NOT NULL DEFAULT FALSE,
    purpose           TEXT         NULL,
    origin_server_id  UUID         NULL,                -- federation prep (ADR 0007)
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX collection_owner_idx  ON collection (owner_user_ref);
CREATE INDEX collection_expires_idx ON collection (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX collection_featured_idx ON collection (featured) WHERE featured;

CREATE TABLE collection_resource (
    collection_id     UUID         NOT NULL REFERENCES collection(id) ON DELETE CASCADE,
    resource_id       UUID         NOT NULL,            -- references the future resource entity
    sort_order        INTEGER      NOT NULL DEFAULT 0,
    pinned            BOOLEAN      NOT NULL DEFAULT TRUE,   -- false = excluded from query result
    expires_at        TIMESTAMPTZ  NULL,                -- per-membership TTL
    added_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (collection_id, resource_id)
);

CREATE INDEX collection_resource_resource_idx ON collection_resource (resource_id);
CREATE INDEX collection_resource_expires_idx
    ON collection_resource (expires_at) WHERE expires_at IS NOT NULL;

CREATE TABLE collection_query (
    collection_id      UUID         PRIMARY KEY REFERENCES collection(id) ON DELETE CASCADE,
    search_dsl         JSONB        NOT NULL,
    last_evaluated_at  TIMESTAMPTZ  NULL,
    materialize_policy TEXT         NOT NULL DEFAULT 'on_read'
                                    CHECK (materialize_policy IN ('on_read','cached','materialized'))
);

CREATE TABLE collection_grant (
    collection_id     UUID         NOT NULL REFERENCES collection(id) ON DELETE CASCADE,
    grantee_user_ref  BIGINT       NOT NULL,
    permission        TEXT         NOT NULL CHECK (permission IN ('view','edit')),
    granted_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (collection_id, grantee_user_ref)
);

CREATE TABLE collection_access_link (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id     UUID         NOT NULL REFERENCES collection(id) ON DELETE CASCADE,
    token_hash        BYTEA        NOT NULL UNIQUE,        -- sha256(token); plaintext only in URL
    permission        TEXT         NOT NULL CHECK (permission IN ('view','edit')),
    label             TEXT         NOT NULL DEFAULT '',
    expires_at        TIMESTAMPTZ  NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    revoked_at        TIMESTAMPTZ  NULL
);
```

`collection_resource.pinned = false` is how the hybrid "excluded"
case is stored: the row exists, but tells the materializer "do not
include even if the query matches."

### API surface (Phase 1.8 / 1.9)

```
GET    /collections                       — list mine + shared + public (paginated, filterable)
POST   /collections                       — create
GET    /collections/{id}                  — fetch (membership policy applied)
PATCH  /collections/{id}                  — partial update
DELETE /collections/{id}                  — soft-delete (cascades to memberships)
POST   /collections/{id}/duplicate        — clone collection + memberships + query

POST   /collections/{id}/resources        — bulk add {resource_ids[], sort_order?, expires_at?}
DELETE /collections/{id}/resources        — bulk remove {resource_ids[]}
POST   /collections/{id}/resources/move   — {to_collection_id, resource_ids[]}
POST   /collections/{id}/resources/copy   — {to_collection_id, resource_ids[]}
PATCH  /collections/{id}/resources/{rid}  — update sort_order / expires_at / pinned

PUT    /collections/{id}/query            — set or replace the query DSL
DELETE /collections/{id}/query            — drop, collection reverts to manual

GET    /collections/{id}/grants           — who it's shared with
POST   /collections/{id}/grants           — share with user
DELETE /collections/{id}/grants/{user}    — unshare

POST   /collections/{id}/links            — mint access link {permission, expires_at}
GET    /collections/{id}/links            — list active links
DELETE /collections/{id}/links/{id}       — revoke
```

### Background jobs

- **Expiry sweeper** (runs every 5 min): deletes collections where
  `expires_at < NOW()`. Cascades to memberships. Also nulls
  individual `collection_resource` rows whose `expires_at` has
  fired.
- **Query materializer** (optional, runs hourly for collections with
  `materialize_policy='cached'`): re-evaluates the query and caches
  the resulting resource set so list-views don't re-execute the
  search on every read.

### Migration from legacy data

Any existing `collection.type` enum maps onto the new model:

| Legacy `type` | New `visibility` | `membership` | `purpose` | `featured` |
|---|---|---|---|---|
| 0 standard, public=0 | private | manual | NULL | false |
| 0 standard, public=1 | public  | manual | NULL | false |
| 1 themed/public      | public  | manual | NULL | true  |
| 2 featured category  | public  | manual | NULL | true  |
| 3 request            | private | manual | request | false |
| 4 upload             | private | manual | upload_staging | false |
| 5 research           | private | manual | research | false |
| 6 selection          | private | manual | session_selection | false |
| `collection_savedsearch` row exists | inherit | **query** | inherit | inherit |

Legacy `external_access_keys` migrate to `collection_access_link` per
collection. Legacy `user_collection` migrates to `collection_grant` with
`permission='view'` (the legacy schema doesn't distinguish view vs edit
at the share level; we add that here).

The migration runs as a one-shot Go command (`aa migrate-collections`)
that the operator invokes when ready to cut over. The old `collection`
table stays in place during the transition so legacy PHP pages keep
rendering until the corresponding Go endpoints land.

## Consequences

**Positive:**

- Single primitive instead of six type-enum values, plus a separate
  table for smart collections.
- Hybrid membership (manual ± query) is a new capability the typical
  DAM doesn't have.
- Per-membership TTL handles "include for N days" workflows that the
  typical DAM forces users to maintain manually.
- Move / duplicate as first-class operations close ergonomic gaps.
- Schema is federation-ready (`origin_server_id` on collection,
  UUIDs for collection ids).

**Negative:**

- Any legacy data needs the migration step; the legacy backend and Go
  briefly read the same underlying table differently during cutover.
  The new schema is a fresh set of tables, so the data lives in both
  worlds until legacy routes for collections are retired.
- "Featured categories tree" use cases (rare but real for some
  installs) are not preserved. If a real user surfaces a need, we
  can re-introduce them later as collections-of-collections without
  breaking the model.

**Deferred:**

- The search DSL stored in `collection_query.search_dsl` is its own
  design problem (out of scope for this ADR). We need it to be
  expressive enough to replicate the typical saved-search surface
  but simple enough for non-engineers to compose. Likely lands as
  ADR 0010.
- Resource entity itself (UUID-keyed, the `resource_id` referenced
  here) lands as Phase 1.8. This ADR assumes it exists; the
  collection migration runs *after* the resource port.

## Open questions

- Whether to soft-delete or hard-delete collections at expiry —
  soft-delete keeps an undo window but adds query complexity. Default
  to hard-delete unless real demand surfaces.
- Whether per-membership TTL applies to `membership='query'`
  collections (it semantically can't — the row doesn't exist for
  query results). The schema only allows TTL on manual/hybrid
  membership rows, and the API rejects it on pure-query collections.
- Pagination defaults for `GET /collections` listings — likely
  cursor-based on `(updated_at DESC, id DESC)`. Decide when
  building.
