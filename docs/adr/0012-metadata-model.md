---
id: "0012"
title: Metadata model — admin-extensible fields, audit history, federation-ready
status: accepted
date: 2026-05-26
area: architecture
phases: 
  - "1.9"
supersedes: []
related: 
  - "0011"
tags:
  - architecture
  - ai
  - infrastructure
  - 3d
excerpt: >-
  ADR 0011 ships assets.metadata jsonb as an extensibility safety valve and a asset_tag join table. Neither is enough on its own:
---
## Context

## Implementation status (2026-06-20)

The decision recorded here is **fully implemented and extended**:

- **Phase 1.9.A — Per-asset custom fields** ✅ shipped in the
  pre-MVP baseline (00001_baseline_v1.sql). `field_definition`,
  `asset_field_value`, `asset_field_value_history` tables + the
  `metadata` package + GET/POST/PATCH/DELETE on `/fields` and
  `/assets/{id}/fields/{field_id}` + history audit.
- **Phase 1.9.B — Per-collection custom fields** ✅ shipped via PR
  #144 (`2fccab9`). Added a `subject_kind` discriminator to
  `field_definition` + new `collection_field_value` +
  `collection_field_value_history` tables. The asset metadata
  pipeline is bit-for-bit preserved (federation soak invariant);
  the discriminator means future "things with metadata" (posts,
  users) reuse the same `field_definition` schema by adding their
  own kind value + value table.

The design held: typed field vocabulary at the schema layer,
per-field capability gates, append-only history triggers, federation
provenance via `origin_server_id`. No design changes against
implementation reality.

## Amendment (2026-07-31) — `field_set_id` is removed (#738)

**One part of the original decision did not hold: `field_set_id`.**
It is dropped from `field_definition` by migration
`00022_drop_field_definition_field_set_id.sql`, together with its
three `openapi.yaml` schema entries, its column in every `metadata`
sqlc query, and the `/field_sets` API surface sketched below. The
declarations further down this ADR are **historical** — read them
with this amendment.

### What it actually was

Declared as `field_set_id UUID NULL -- for bundling (export/import)`,
with the federation intent recorded below: operators publish a
`field_set` JSON, peers import it to adopt identical field schemas.

In the ~2 months since this ADR (2026-05-26; the baseline's
`field_definition` rows are stamped 2026-06-06) it never acquired a
producer, a consumer, a foreign key, an index, or a referent — **there has never been a
`field_set` table for it to point at.** Verified on a live instance:
15 of 15 `field_definition` rows `NULL`, and the only foreign key on
the table is `deprecated_replacement_id`.

### Why it is removed rather than completed

1. **The consumer was never designed, not merely unbuilt.**
   Federation transports no metadata whatsoever. The activity
   catalogue in `app/internal/federation/vocab.go` carries ~25 verbs
   and none of them is a field-definition or field-value verb; the
   outbox resolver projects no metadata onto an object. None of the
   four federation ADRs written *after* this one — 0007, 0042, 0043,
   0049 — mentions field sets.

   > ⚠️ **Corrected 2026-08-01 — see ADR 0083.** This paragraph
   > originally concluded that the silence of those four ADRs was
   > "the clearest evidence that the idea was not carried forward by
   > the people designing the thing it was prep for." That reads an
   > **absence of a decision as a decision against**, and the author
   > of the federation design says otherwise: peers exchanging field
   > schemas is wanted, and simply has not been built.
   >
   > The removal still stands — the column was unwritten, unreferenced
   > and the wrong shape regardless. But the requirement is live, not
   > rejected, and it is already concrete: per ADR 0053 a federated
   > IIIF manifest can span two instances **today**, each rendering
   > its own canvases' metadata from field definitions nothing has
   > ever reconciled. ADR 0083 records the requirement and carries
   > forward this amendment's envelope and collision analysis.

2. **It is not the bulk-import/export epic's dependency either.**
   #521 and ADR 0019 are about ingesting and dumping **assets**
   (CSV rows, folder trees, contact sheets). Neither references
   `field_set` anywhere. The column was speculative from the start.

3. **The grouping it is mistaken for already exists, twice.**
   `display_group` groups fields for the UI and is populated;
   `applies_to` scopes them by asset type and is populated. A
   persisted set would be a third grouping axis that must be kept
   consistent with the other two while granting no capability
   either of them does not already grant.

4. **An export unit does not need to be persisted state.** This is
   the substantive design correction. Exporting N field definitions
   to JSON needs an endpoint that takes a list of field codes — not
   a row that fields point at. Persisting the set buys only a saved
   selection, at the cost of a consistency burden and a second
   answer to "which fields belong together".

Keeping the column was therefore worse than neutral: it encoded the
wrong shape and actively misled. #738 was opened to *build the
`field_set` table* precisely because the column looked like intent —
which would have moved the dangling reference up one level and left
it just as unwritten. Compare #579 / migration `00016`
(`assets.has_image`), a writerless column four consumers read as
though it meant something. This one had no consumers yet. Dropping
it now is what keeps it that way.

### If schema exchange is built later, build this instead

Recorded so the shape does not have to be re-derived, and so the
next attempt does not reach for a stored set:

- **Transport:** `POST /fields/export` takes `{"codes": [...]}` and
  returns a versioned envelope; `POST /fields/import` consumes one.
  No entity, no `field_set_id`, no migration.
- **In the envelope** (the portable description of a field):
  `code`, `label`, `description`, `type`, `subject_kind`,
  `required`, `searchable`, `display_group`, `display_order`, and
  the `options` **values** (`value` + `label`, and `children` for
  trees).
- **Excluded, and why each is excluded** — everything here names
  something that exists only on the sending instance, so carrying it
  would either fail on import or silently bind the receiver to the
  sender's world:
  - `id`, `created_at`/`updated_at`, `created_by_user_ref`,
    `updated_by_user_ref` — local identity and local audit.
    `code` is the cross-instance identifier, per § Federation model.
  - `origin_server_id` — set by the *receiver* on import, never
    copied from the payload; otherwise provenance lies.
  - `applies_to` — local `asset_type` BIGINT refs. Meaningless on a
    peer, and numerically valid enough to bind to the *wrong* type.
  - `read_capability` / `write_capability` — local capability codes.
    Importing them would silently widen or narrow access on the
    receiver's instance, which is the worst possible failure mode
    for a schema import.
  - `extraction_source` / `extraction_mode` — wiring into the
    sender's extraction pipeline. A peer adopting `pipeline_stage`
    wants the type and the options; it does not want the sender's
    EXIF/IPTC bindings firing against its own uploads.
  - `default_value` and the `field_default_override` table (#803,
    migration `00021`) — team-scoped defaults keyed on teams that do
    not exist on the receiver. **Bundle-level defaults are a
    separate and larger feature; a field-schema envelope must not
    grow into one.**
  - per-option `status` / `replaced_by` (#737) — option *lifecycle*
    is the local operator's editorial history, not part of the
    vocabulary being adopted. Import the live options; do not import
    the sender's deprecations.
  - `deprecated_replacement_id` — a local UUID, and it may point at
    a field outside the exported selection.
- **Collision is the normal case, not an edge case.** `code` is
  unique per instance and this ADR tells admins to adopt the same
  slugs across peers, so importing `pipeline_stage` onto an instance
  that already has `pipeline_stage` is the expected path. The rule
  must be **reject the whole import by default** and return a
  per-field diff; the operator then chooses per field to skip, to
  overwrite, or to import under a new code. **Never merge silently
  and never auto-overwrite** — a schema import that quietly mutates
  a field an operator's assets already depend on is worse than no
  import feature at all, because the damage is invisible until a
  query returns the wrong rows. A whole-import reject also keeps the
  operation atomic, so a half-adopted schema is unreachable.

## Context

ADR 0011 ships `assets.metadata jsonb` as an extensibility safety
valve and a `asset_tag` join table. Neither is enough on its own:

- Free-form jsonb can't be required, validated, type-checked, or
  consistently queried across assets. An admin who wants every
  artwork to carry "copyright holder" has no way to enforce that.
- Tags are a degenerate multi-value text field. Fine for browsing,
  not enough for structured metadata.

The prior generation of DAM tooling solves this with a heavy
three-table model (`asset_type_field` + `node` + `resource_node`)
that supports admin-extensible fields, IPTC/EXIF auto-extraction, and
full-text search — but at the cost of ~70 columns per field definition,
every field value routing through a generic `node` row (even a 1-line
title), and 15 partially-redundant field-type enums where most of
the variation is UI-controlled (radio vs dropdown over the same data).

This ADR locks in artist-alley's metadata layer with three concrete
goals:

1. **Admin extensibility** — non-engineers can add fields at runtime
   via a Go API + UI, with proper validation and permissions.
2. **Gold-standard semantics** — typed values, source provenance per
   field write, append-only change history, field versioning so
   renaming doesn't lose data.
3. **Federation-ready** — the same logical field has a stable
   identifier across peers, and bundles of fields can be exported
   and imported as a unit.

## Decision

### Three tables

```sql
CREATE TABLE field_definition (
    id                          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    code                        TEXT         NOT NULL UNIQUE,    -- federation-stable slug
    label                       TEXT         NOT NULL,           -- localized display in i18n table later
    description                 TEXT         NOT NULL DEFAULT '',
    type                        TEXT         NOT NULL
                                             CHECK (type IN (
                                                'text','longtext','rich_text',
                                                'number','boolean',
                                                'date','datetime',
                                                'select','multi_select','tree',
                                                'reference')),
    options                     JSONB        NOT NULL DEFAULT '{}'::jsonb,
       -- type-dependent schema:
       --   select       -> {"values":[{"value":"slug","label":"Display"}, ...]}
       --   multi_select -> same shape
       --   tree         -> {"values":[{"value":"NA","label":"North America","children":[...]}]}
       --   number       -> {"min":0,"max":100,"step":1}
       --   text         -> {"max_length":255,"pattern":"^[a-z]+$"}
       --   reference    -> {"asset_filter":{"asset_type":3}}
    required                    BOOLEAN      NOT NULL DEFAULT FALSE,
    searchable                  BOOLEAN      NOT NULL DEFAULT TRUE,
    applies_to                  BIGINT[]     NOT NULL DEFAULT '{}',  -- asset_type refs; empty = all
    -- REMOVED 2026-07-31 by migration 00022 — see the amendment above.
    -- field_set_id             UUID         NULL,                    -- for bundling (export/import)

    -- Permissions: capability codes from auth system.
    -- NULL means "any user with read/write access to the parent asset."
    read_capability             TEXT         NULL,
    write_capability            TEXT         NULL,

    -- Display hints — UI may use; do not gate logic on these.
    display_order               INTEGER      NOT NULL DEFAULT 100,
    display_group               TEXT         NOT NULL DEFAULT 'general',

    -- Auto-extraction pipeline. Background job on asset upload reads
    -- this and populates the value if a source match is found.
    --   exif: {"type":"exif","tag":"DateTimeOriginal"}
    --   iptc: {"type":"iptc","tag":"Credit"}
    --   xmp:  {"type":"xmp","tag":"dc:rights"}
    source                      JSONB        NULL,

    -- Versioning. When a field is deprecated, point new readers at
    -- its replacement so renames don't break consumers.
    status                      TEXT         NOT NULL DEFAULT 'active'
                                             CHECK (status IN ('active','deprecated','archived')),
    deprecated_replacement_id   UUID         NULL REFERENCES field_definition(id),

    -- Federation.
    origin_server_id            UUID         NULL,

    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_user_ref         BIGINT       NULL,
    updated_by_user_ref         BIGINT       NULL
);

CREATE INDEX field_definition_status_idx ON field_definition (status) WHERE status = 'active';
CREATE INDEX field_definition_group_idx  ON field_definition (display_group, display_order);
CREATE INDEX field_definition_applies_to_gin ON field_definition USING gin (applies_to);
CREATE INDEX field_definition_options_gin    ON field_definition USING gin (options);

CREATE TABLE asset_field_value (
    asset_id        UUID         NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    field_id        UUID         NOT NULL REFERENCES field_definition(id) ON DELETE CASCADE,

    -- Typed value columns. Exactly one is populated per type:
    --   text/longtext/rich_text/select/tree(one slug)     -> value_text
    --   number/boolean                                    -> value_num
    --   date/datetime                                     -> value_date
    --   multi_select                                      -> value_options
    --   reference                                         -> value_ref
    --
    -- tree said "(path)" here until the 2026-07-31 amendment below.
    -- The column was right; the encoding was not. A tree value is ONE
    -- option slug, never a path string.
    value_text      TEXT         NULL,
    value_num       NUMERIC      NULL,
    value_date      TIMESTAMPTZ  NULL,
    value_options   TEXT[]       NULL,
    value_ref       UUID         NULL,

    -- Source provenance — was this set by a human, EXIF extraction,
    -- API import, etc.
    set_by          TEXT         NOT NULL DEFAULT 'manual'
                                 CHECK (set_by IN ('manual','exif','iptc','xmp','api','import','computed')),
    set_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    set_by_user_ref BIGINT       NULL,

    PRIMARY KEY (asset_id, field_id)
);

CREATE INDEX asset_field_value_asset_idx ON asset_field_value (asset_id);
CREATE INDEX asset_field_value_field_idx ON asset_field_value (field_id);
CREATE INDEX asset_field_value_text_idx
    ON asset_field_value (field_id, value_text)
    WHERE value_text IS NOT NULL;
CREATE INDEX asset_field_value_num_idx
    ON asset_field_value (field_id, value_num)
    WHERE value_num IS NOT NULL;
CREATE INDEX asset_field_value_date_idx
    ON asset_field_value (field_id, value_date)
    WHERE value_date IS NOT NULL;
CREATE INDEX asset_field_value_options_gin
    ON asset_field_value USING gin (value_options)
    WHERE value_options IS NOT NULL;
CREATE INDEX asset_field_value_ref_idx
    ON asset_field_value (value_ref)
    WHERE value_ref IS NOT NULL;

CREATE TABLE asset_field_value_history (
    id                  UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id            UUID         NOT NULL,
    field_id            UUID         NOT NULL,
    old_value           JSONB        NULL,         -- pre-change typed value, normalized to jsonb
    new_value           JSONB        NULL,         -- post-change typed value
    changed_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    changed_by_user_ref BIGINT       NULL,
    set_by              TEXT         NOT NULL DEFAULT 'manual'
);

CREATE INDEX afvh_asset_idx ON asset_field_value_history (asset_id, changed_at DESC);
CREATE INDEX afvh_field_idx ON asset_field_value_history (field_id, changed_at DESC);
```

History is append-only — no UPDATE, no DELETE in normal flow. A
background sweeper archives rows older than 1 year to cold storage
(out of scope for this ADR; lands when storage tiering does).

### Field-type primitives (11)

| code | storage column | semantic |
|---|---|---|
| `text` | value_text | single-line ≤ 255 chars (default; configurable via options.max_length) |
| `longtext` | value_text | multi-line plain text |
| `rich_text` | value_text | markdown/HTML, sanitized server-side |
| `number` | value_num | integer or decimal per options.step |
| `boolean` | value_num | 0/1 (so we can index numerically) |
| `date` | value_date | calendar date, stored at 00:00:00 UTC |
| `datetime` | value_date | full timestamp |
| `select` | value_text | single slug from options.values |
| `multi_select` | value_options | set of slugs |
| `tree` | value_text | **one** option slug naming a node in the field's nested `options.values`; unlimited depth. NOT a path-string — see the 2026-07-31 amendment, which corrects the "NA/US/CA" encoding this row used to specify |
| `reference` | value_ref | UUID of another asset |

UI controls (radio vs dropdown, slider vs number input, datepicker
variant) are a frontend concern — they don't change storage.

### Default seed field set

Shipped via the baseline migration so a fresh install isn't empty:

```
title          (text,       required, group="core",      order=10, source=iptc:ObjectName)
description    (longtext,   group="core",  order=20)
credit         (text,       group="rights", order=10, source=iptc:Credit)
copyright      (text,       group="rights", order=20, source=xmp:dc:rights)
capture_date   (datetime,   group="technical", order=10, source=exif:DateTimeOriginal)
keywords       (multi_select with empty options, group="core", order=30, source=iptc:Keywords)
country        (tree,       group="general", order=40, source=iptc:Country-PrimaryLocationName)
```

Admins delete or rename via API. Status flips to `deprecated`; new
readers get redirected via `deprecated_replacement_id` if set.

### Auto-extraction pipeline

On asset upload, a background job examines `field_definition.source`
entries that match resource type. Each match reads the corresponding
tag from the uploaded file (EXIF/IPTC/XMP) and writes
`asset_field_value` with `set_by` = extraction source. Human edits
overwrite extracted values; subsequent extractions on the same field
are skipped if `set_by == 'manual'`.

Implementation lands as part of the variant generator (currently
deferred).

### Search integration

A generated `assets.search_text TSVECTOR` column concatenates the
text content of every `searchable=true` field value. Maintained by
trigger when `asset_field_value` rows change. Indexed via GIN.
Full-text search uses `assets.search_text @@ to_tsquery(...)`.

This replaces the legacy `node_keyword` denormalized index. Single-source
of truth, no consistency drift.

### Federation model

- `field_definition.code` is the stable cross-peer identifier.
  Globally unique within an instance (DB constraint); admins
  coordinate across peers by adopting the same slugs.
- ~~`field_set_id` groups related fields into an export/import unit.
  Operators publish a `field_set` JSON to share with peers; peers
  import to adopt identical field schemas.~~ **Withdrawn 2026-07-31
  (#738).** The column never had a producer, a consumer or a
  referent, and federation transports no metadata for it to
  describe. See the amendment above for why, and for the envelope
  shape to use if schema exchange is built later.
- `field_definition.origin_server_id` records which peer authored a
  definition (federation prep — used by sync layer when it lands).
- `asset_field_value` carries no federation metadata of its own;
  it inherits from its asset.

### API surface (Phase 1.9)

```
GET    /fields                          — list field defs (paginated, filterable)
POST   /fields                          — admin: create new field
GET    /fields/{id}                     — fetch one
PATCH  /fields/{id}                     — admin: update (label, options, status)
DELETE /fields/{id}                     — admin: archive (sets status, doesn't drop data)
POST   /fields/{id}/deprecate           — admin: deprecate with replacement_id

                                        (the three /field_sets routes
                                         sketched here were never built
                                         and are withdrawn — see the
                                         amendment above)

GET    /assets/{id}/fields              — all field values for an asset
PUT    /assets/{id}/fields/{field_id}   — set/replace a field value
DELETE /assets/{id}/fields/{field_id}   — clear a field value
GET    /assets/{id}/fields/{field_id}/history  — audit trail
```

`PUT` writes go through a transaction:
1. Read current value into `old_value`.
2. Write the new typed columns.
3. Insert into `asset_field_value_history`.
4. Refresh `assets.search_text` via trigger.

## Consequences

**Positive:**
- Admins can model their domain at runtime — copyright managers,
  product teams, art directors all extend differently without code
  changes.
- Source provenance + history mean every value has a trail. "Where
  did this date come from?" has an answer.
- Type primitives are minimal and storage-efficient: a `select`
  doesn't allocate a `node` row.
- Field versioning means renames don't lose data; deprecated fields
  redirect on read.
- Federation-aware from day one via stable codes. (The field-set
  bundles originally claimed here were withdrawn in 2026-07-31's
  amendment; stable `code` remains the cross-peer identifier and is
  the part that carried its weight.)

**Negative:**
- More moving parts than the original `assets.metadata jsonb` blob.
- Field changes touch `assets.search_text` via trigger — write
  amplification on bulk imports. Mitigation: bulk-write path
  defers trigger updates and runs a batch refresh at the end.
- History table grows linearly with edits. 2M assets × ~15 fields ×
  ~10 edits ≈ 300M rows over time. Mitigation: yearly archive to
  cold storage (separate ADR when storage tiering lands).

**Deferred:**
- Field i18n (localized labels). `field_definition.label` is the
  default; a `field_definition_i18n(field_id, locale, label,
  description)` table lands when i18n becomes a real need.
- Field validation engine — the `options` jsonb declares
  constraints; the Go layer enforces them on write. A separate
  rules engine for cross-field validation ("if status=draft then
  copyright optional") is post-MVP.
- Auto-extraction implementation — needs the variant generator
  scaffolding which is still deferred.

## Amendment 2026-07-30 — an option has a lifecycle, and editing one is a conflict-detectable write

Two gaps in the `options` model above, both invisible at today's scale (5 fields carry
options; **zero `tree` fields exist**) and both blocking as soon as a real taxonomy arrives —
which is what #519's taxonomy tile is for.

### Gap 1: an option cannot be retired

`options` holds `{"values":[{"value": slug, "label": …, "children": […]}]}`. There is no way
to stop offering a term. Deleting one that assets already reference orphans those values;
keeping it means the vocabulary only ever grows.

**A term outliving its usefulness is not hypothetical.** A mature DAM in this space carries an
`active` flag on its option rows, which is evidence the requirement is real rather than
anticipated. The relevant standard is stronger still: SKOS deprecation is not a boolean — a
deprecated concept carries *instructions on what to use in its place*.

**We already implement exactly that, one level up.** `field_definition` has:

```
status                     text  CHECK (status IN ('active','deprecated','archived'))
deprecated_replacement_id  uuid
```

**Decision: an option carries the same lifecycle its field does.** Each entry in `values` gains
an optional `status` (defaulting to `active` when absent, so every existing document stays
valid) and an optional `replaced_by` naming another slug in the same field.

- **`active`** — offered for selection, resolves, displays.
- **`deprecated`** — **not offered for new values**, but existing values still resolve and
  display. Where `replaced_by` is set, the editing surface suggests the successor. This is the
  state that makes a vocabulary maintainable: a term stops spreading without breaking the
  assets that already carry it.
- **`archived`** — not offered, not resolved. A hard retire, for terms that were mistakes
  rather than terms that were superseded.

We are copying **our own** in-repo pattern, not importing one. The vocabulary, the semantics
and the replacement pointer are the ones this codebase already uses for fields, which means one
concept to learn rather than two.

**The slug indirection is what makes this cheap, and it must be preserved.** `asset_field_value`
stores the slug, never the label, so deprecating or relabelling a term rewrites nothing on any
asset. That is a genuine advantage over the row-per-option designs that denormalise labels onto
records and pay a cascade on every rename — **do not trade it away** for per-row editing.

### Gap 2: two admins editing one field's options clobber each other silently

Changing one term rewrites the whole `options` document, so concurrent edits are last-write-wins
and the loser is never told. For a four-option `select` that is noise. For a taxonomy of
hundreds — the case the tile exists for — it is data loss that looks like success.

**Decision: keep options in `jsonb` on the field, and make the write conflict-detectable.**

Options stay a document rather than becoming rows, for reasons that are ours rather than
inherited:

1. **Federation is real.** `field_definition` already carries `origin_server_id`. Options that
   live *in* the field travel with it for free; options as rows are a second entity needing its
   own federation story, ordering guarantees and conflict rules. That is a large cost paid now
   against a requirement no field currently exercises.
2. **Zero `tree` fields exist.** The contention this solves is real but currently
   unexercised. Moving to rows on the strength of an anticipated taxonomy is designing for a
   shape we have not yet met.
3. It preserves the rename-is-free property above.

**But silence is the defect, not the contention.** The write path takes the field's `updated_at`
as a precondition and rejects a stale one with a conflict rather than overwriting. Two admins
editing different terms now get a visible, retryable failure instead of one of them quietly
losing an afternoon's curation.

**The trigger for revisiting is named, so this is a decision rather than a deferral:** when a
single field's `options` document exceeds a few hundred entries, or when reordering a subtree
becomes a routine operation rather than a rare one, options become rows — and the slug
indirection comes with them. Until then, a document is the smaller correct thing.

### What this amendment rejects

- **A boolean `active`.** Two states cannot express "superseded by X", which is what makes a
  vocabulary navigable as it ages.
- **Hard-deleting an option.** It orphans stored values, and the orphan surfaces as a blank on
  an asset nobody edited.
- **Moving options to rows now.** Correct eventually, premature today, and it would cost the
  federation-for-free and rename-for-free properties in exchange for contention handling that
  a precondition check gives us for a fraction of the work.
- **Last-write-wins with a warning in the docs.** Documentation is not a concurrency control.

### Consequences

- Editing surfaces must send the field's `updated_at` back and handle a conflict. Reading one
  and writing it minutes later without re-reading is now an error rather than a silent
  overwrite.
- `status` and `replaced_by` are **optional** in the document, so every existing `options` value
  remains valid with no migration and no rewrite.
- Anything rendering a value must tolerate resolving a slug whose option is `deprecated` —
  that is the normal case for historical data, not an error path.
- An `archived` option can leave a stored value unresolvable. That is the intended, explicit
  cost of a hard retire, and the surface should show the raw slug rather than a blank.
- Federation: a peer receiving a field receives its option lifecycle with it, because it is the
  same document. Nothing new to design.

## Correction 2026-07-30 — `values` entries are strings in practice, and this document was wrong about it

**Everything above describing `options.values` as `[{value, label, children}]` describes a shape
that no live data has ever used.** Found while implementing #737 (PR #773).

Every field carrying options on `dev` stores **bare slug strings**:

```
color_space          → "sRGB"
engine_compatibility → "Unreal 5"
pipeline_stage       → "Greybox"
target_platforms     → "PC"
texture_resolution   → "256x256"
```

`jsonb_typeof(options->'values'->0)` is `string` for all five. The object form appears in this
ADR, in `schema.sql`, and in exactly one test fixture — nowhere else. The seeder
(`seed/runner.go`) marshals `{"values": []string}`, which is where the live shape comes from.

**This cost us a production bug, and the mechanism is the point.**
`FieldValueInput.svelte` cast `options.values` to `{value: string; label?: string}[]`, with a
comment citing this ADR by name as its authority. Against string data every `opt.value` was
`undefined`, so **every seeded `select` and `multi_select` rendered blank options in the
collection editor** — while `UploadFileRow.svelte`, which read strings, worked fine. The two
consumers disagreed for months because one trusted the document and the other trusted the data.

**An ADR is the thing people read *instead of* the code. When it is wrong, it does not fail
loudly — it gets implemented.**

### The decision, restated correctly

**Both shapes are valid. A `values` entry is either a bare slug string, or an object carrying
that slug plus whatever else it needs.** Readers must accept both. Writers emit the *narrowest
form that carries the entry's information* — a plain slug stays a plain slug, and an entry only
becomes an object once it has a label, a `status`, a `replaced_by`, or `children`.

That is what keeps untouched vocabularies byte-identical through an edit, which is the property
that made this safe to fix without a migration.

### What this changes about the amendment above

The 2026-07-30 lifecycle amendment says "each entry in `values` gains an optional `status`",
which silently assumed every entry was an object. It is still correct in substance — status and
`replaced_by` are optional and absent means `active` — but the mechanism is now explicit: **an
entry gains those keys by being promoted from a string to an object at the moment it needs
one.** No migration, and a vocabulary nobody has edited still serialises as a string array.

`schema.sql`'s comment carries the same wrong claim and should be corrected alongside.

## Amendment 2026-07-31 — the slug is resolved on the server, not by each reader

Slug indirection is the whole point of this ADR: `asset_field_value` stores the slug and never
the label, so relabelling a term is free and rewrites nothing. That stays. But the label has to
be resolved *somewhere*, and until #775 the only places that did it were the editing surfaces —
because they happen to load the field definition in order to build a picker.

Every other reader printed the raw slug. Shipping option labels and deprecation in #737 made
that visible and wrong: a term an operator had relabelled or deprecated still rendered as its
bare slug on the post/asset detail surface, which is the surface most people actually read.

**Decision: the server resolves.** `AssetFieldValue` gained `resolved_options` — a map from each
slug the value holds to `{label, status}` — assembled in `buildAssetValue`, the single helper
both the list and the upsert path already go through. Both callers already hold the options
document (the list query joins `field_definition` for the code/label/type anyway; the upsert path
loads the definition to validate against), so this costs no extra query and no extra join.

The alternative — every consumer fetches `/fields` and resolves client-side — was rejected on
the evidence: a consumer *did* forget, and that is exactly the bug. A read surface should not
have to know a controlled vocabulary exists in order to print a value.

Two properties the resolver must keep:

- **An entry that does not resolve is simply absent from the map, and the caller renders the raw
  slug.** Unknown term, malformed document, no `values` key — all degrade to the pre-resolution
  behaviour rather than to a blank.
- **A bare-string entry resolves to itself.** It carries no label, so the slug *is* the display
  text. Since that is the form every live field uses, getting this wrong would blank the entire
  catalogue.

Archived terms still resolve here, unlike in the picker. Suppressing an archived term stops it
being *offered*; blanking a value that already holds it just hides data from the one person able
to fix it.

## Open questions

- Whether asset edit endpoints (`PATCH /assets/{id}`) should also
  accept inline `field_values` in the body, or always require the
  explicit `/assets/{id}/fields/{field_id}` PUT. Convenience vs.
  consistency; default to convenient + delegate to the field PUT
  handler internally.

## Amendment 2026-07-31 — the taxonomy question is closed: tags stay flat, hierarchy stays in `tree`

Recorded because this decision has now been re-derived three times from scratch. **It was
already correct; what was missing was a written confirmation, so it kept getting re-opened.**

### The question

Epic #519's taxonomy tile is described as *"tag hierarchy, aliases, merge tools."* That
phrasing implies promoting `asset_tag` into a managed vocabulary with parents, aliases and
merge operations — which contradicts this ADR, where tags are *"a degenerate multi-value text
field"* and hierarchy belongs to the `tree` **field type**.

So: extend the field-options model, or promote tags?

### The evidence

A mature DAM in this market — twenty years, the same operator persona — has **none of the three
things the tile names**. No merge tooling for either options or keywords. No standalone
taxonomy, thesaurus or vocabulary admin surface. Its keyword table carries **no parent and no
alias column** at all; its answer to vocabulary drift is prevention, phonetic tolerance and
frequency ranking rather than cleanup. Hierarchy in that system lives on the *field option*
row, attached to a field, exactly where this ADR put it.

That is not a reason to do what they do. It is evidence that the requirement behind "tag
hierarchy and merge tooling" is far weaker than the tile's phrasing suggests, and that the
split this ADR already chose is the one the problem actually has.

### Decision

**Unchanged, and now confirmed rather than assumed.**

- **Tags stay flat.** No parent, no alias, no merge tooling on `asset_tag`.
- **Hierarchy stays in the `tree` field type**, as nested entries in the field's `options`
  document, reusing the lifecycle and conflict-detection the option editor already has.
- **Promoting tags to a managed vocabulary would supersede this ADR and requires its own.** It
  is not a tile-level implementation choice and must not be made inside one.

### Scope — `tree` is IN, and the "no tree fields exist" argument was wrong

An earlier draft of this amendment argued that nested-option admin should wait because **zero
`tree` fields exist**. That reasoning is circular and is withdrawn: **nobody can create a useful
`tree` field precisely because there is no admin to manage its options.** Absence of usage in a
system that cannot yet produce it is not evidence against the requirement — it is a consequence
of the gap. The prior art points the other way, and was misread here: an adjacency-list `parent`
column and a materialised branch-path function exist in a mature product *because* real
operators need hierarchical vocabularies.

**Hierarchical field options are in scope.** The `tree` type is declared in this ADR, accepted,
and unimplemented at the admin layer — which makes it a gap, not an open question.

### What the absence actually concealed — a three-way storage disagreement

Because no `tree` field has ever carried a value, nothing has exercised the `tree` path, and it
has rotted in three different directions:

| surface | stores/reads a `tree` value as |
|---|---|
| this ADR | `value_text` (the path) |
| asset write path (`metadata/handler.go`) | `value_text` — **agrees** |
| collection write path (`metadata/collection_handler.go`) | **`value_options`** |
| detail display (`PostHost.svelte`) | **`value_ref`** |

An asset-side value would write to one column, a collection-side value to another, and the
detail panel would read a third and render empty for both.

**This must be settled before the tree admin is built**, and it is the reason to build it sooner
rather than later: the disagreement is invisible only while the feature is unusable, and it
becomes three silent data bugs the moment an operator creates their first tree field. Whichever
column is correct, two of the three call sites are wrong today.

### Aliases

The one piece with genuine standards backing is **aliases** — SKOS `altLabel` — and it belongs
on *options*, not on tags. It arrives as an addition to the options document, reusing the same
editor, rather than as a second vocabulary system.

### Where we stand relative to the prior art, now that the option editor has shipped

| axis | prior art | here |
|---|---|---|
| rename a term | cascades across denormalised copies | **free** — the value stores the slug |
| delete a term in use | permitted, unguarded, though a use count is displayed | **not offered at all** |
| retire a term | boolean `active` | **`status` + `replaced_by`** — says what to use instead |
| merge terms | none | none |
| standalone taxonomy admin | none | none |

## Amendment 2026-07-31 (second) — where a `tree` value is stored, settled

The amendment above ended by saying the three-way storage disagreement "must be settled before
the tree admin is built". This settles it. Implemented in #778.

### The disagreement was larger than the table above recorded

That table named three surfaces. There were **eight**, and the sweep that found them started
from the observation that no `tree` field has ever carried a value, so nothing had ever
exercised any of them:

| surface | did what |
|---|---|
| this ADR (schema comment + primitives table) | `value_text`, encoded as the path `"NA/US/CA"` |
| `metadata/handler.go` — asset write | `value_text` |
| `seed/runner.go` — the seeder | `value_text` |
| `metadata/collection_handler.go` — collection write (×3: params, validator, in-tx seed) | **`value_options`** |
| `FieldValueInput.svelte` — the only editor | **`value_options`** |
| `PostHost.svelte` — the only display | **`value_ref`** |
| `metadata/options.go` — `resolveOptionSlugs` | scanned only the **top level**, on the stated grounds that "nested children belong to tree fields, whose values live in `value_ref` rather than as slugs" — so no nested term ever resolved |
| `metadata/handler.go` — `resolveValueOptions` | excluded `tree` from resolution entirely |
| `fieldOptions.ts` — `optionLabel`, `selectableOptions` | flat scans; a nested term rendered as its raw slug and was never offered |

An asset value and a collection value landed in **different columns**, and the display read a
**third**, so a tree value rendered empty however it had been written. The two resolvers meant
that even a correctly stored value could not have been turned into a label.

### Decision 1 — `tree` is single-valued

A `tree` field holds **one** value: the node selected. It is the hierarchical counterpart of
`select`, not of `multi_select`.

This keeps the three vocabulary types a coherent set — `select` is flat-and-single,
`multi_select` is flat-and-multiple, `tree` is hierarchical-and-single — and it matches the one
`tree` field that exists in the baseline (`country`, sourced from the IPTC tag
`Country-PrimaryLocationName`, which is singular by definition).

If a hierarchical *set* is ever needed, it arrives as a separate `multi_tree` type, exactly as
`multi_select` sits beside `select`. Adding a type later is cheap; splitting an overloaded one
after it holds data is not.

### Decision 2 — the value is ONE SLUG in `value_text`, not a path and not an array

**Storage: `value_text`, holding the slug of the selected node — `"london"`, not
`"europe/uk/london"` and not `["europe","uk","london"]`.**

The reasoning, and why both rejected options are rejected on the same grounds:

- **A path string denormalises every ancestor's slug into the value.** Renaming *or
  re-parenting* an ancestor would then require rewriting every descendant's stored row. That is
  precisely the cascade the slug indirection exists to avoid, and the axis on which the
  "rename a term" row in the table above claims we beat the prior art. Specifying `value_text`
  and specifying a path were two decisions, and only the first one was right.

- **An array of slugs along the path fixes the rename problem but misuses the column.**
  `value_options` is a `TEXT[]` with a GIN index, and it means *a set*: unordered, several
  independent values. A path is ordered and is one value. Storing one in the other overloads
  the column's meaning for every reader and every query.

- **Neither is necessary, because the ancestors are redundant.** `NormalizeOptionsDoc` runs
  `collectSlugs` over the **full depth** of the options document and rejects a duplicate slug
  anywhere in it, on every create and every update. Slugs are therefore unique across a field's
  entire tree, so the selected node's own slug is a **complete address**. The path is derived at
  read time and never stored.

  That uniqueness is load-bearing enough that there is exactly **one** enforcement path for it.
  `NormalizeOptionsDoc` is exported (#808) and the seed catalogue loader calls the same
  function, rather than parsing option documents on its own — decoding a `FieldOption` is free,
  which is precisely the trap: a writer that only unmarshals gets a document that parses but was
  never checked, and a duplicate slug in a hand-edited catalogue then resolves values to the
  wrong node with no error anywhere.

- **`value_ref` is wrong** and was never plausible: it holds the UUID of a row. An option is an
  entry in a jsonb document and has no identity of its own to point at.

Consequences that fall out of this, all of them good:

- **Ancestor rename is free, and so is re-parenting** — both are edits to the options document
  and touch no value row. Pinned by `TestTreeAncestorRenameDoesNotRewriteValues`, which asserts
  the stored value *and the row's `set_at`* are untouched.
- **Full-text search is unaffected** — `rebuild_asset_search_text` already aggregates
  `value_text`, so a tree value indexes exactly like a `select` value.
- **A subtree query** ("everything under Europe") expands the subtree's slugs from the options
  document and matches `value_text = ANY(...)`, served by the existing
  `asset_field_value (field_id, value_text)` index. This is more work than a `LIKE 'europe/%'`
  against a path, and it is the one place a path would have been cheaper — but the `LIKE` goes
  silently *wrong* the moment a node is re-parented, and this does not.

### What the API gained

`ResolvedOption` grew an optional `path`: the ancestor labels from the root down to and
including the term. Present only when the term is nested, so every flat `select` /
`multi_select` response is byte-identical to before. It is what lets a display surface print
"Europe / United Kingdom / London" while the record holds nothing but `london` — the same
"the server pays the indirection cost once, for every consumer" bargain `resolved_options`
already made.

### How this is kept from happening again

Six call sites drifted silently because nothing pinned the invariant. Three tests now do:

- `app/internal/metadata/valuecolumn_test.go` — **behavioural** pin. It calls each writer with
  every value column populated and observes which one comes back set, so it catches drift
  regardless of how a switch is spelled or whether a comment was updated. It also asserts the
  asset and collection sides agree per type.
- `app/internal/seed/valuecolumn_test.go` — the same pin for the seeder, which lives in another
  package and would otherwise sit outside it.
- `app/internal/metadata/tree_value_e2e_test.go` — the end-to-end path this feature never had:
  create the field, value it on an asset **and** a collection, then assert against the database
  columns and against the read model the display actually consumes.

### Known adjacent divergence, deliberately not fixed here

`boolean` has the same defect: the **asset write path stores `0`/`1` in `value_num`** while the
**collection write path and every display use `"true"`/`"false"` in `value_text`** — so an asset
boolean would render blank. It has never been hit for the same reason `tree` never was: no
`boolean` field definition exists either. It is recorded in
`collectionValueColumnOverride` so it is visible in code and so a *new* divergence still fails
the pin, but unifying the two encodings is a write-contract change with a real decision attached
(`0`/`1` vs `"true"`/`"false"`) rather than a drift repair, and belongs in its own change.

*(Closed by the boolean amendment below. The "real decision attached" reading was wrong: this
document had already made the decision — see `boolean -> value_num` in the typed-columns comment
and `0/1 (so we can index numerically)` in the field-type table, both written well before the
divergence appeared. It was a drift repair after all.)*

The same "never instantiated, therefore never exercised" condition applies to `longtext`,
`rich_text`, `date`, `datetime` and `reference`. One concrete instance found in passing: the
seeder's `parseTime` accepts **RFC3339 only**, so a bare `"2026-07-31"` for a `date` field is
silently dropped rather than rejected.

### Still out of scope

The `tree` **admin UI** (#779). `FieldEditor.svelte` still gates its vocabulary editor on
`select`/`multi_select`, so a tree field's nested options must be supplied through the API —
which the API has always accepted and now round-trips correctly. `FieldValueInput.svelte` gained
an indented flat `<select>` over the whole hierarchy, which is the minimum that makes a tree
value settable and correct; a real tree widget comes with #779.

## Amendment 2026-07-31 (third) — `boolean` is `0`/`1` in `value_num`, on every surface

**Status:** accepted. Closes #791. Implements what this document already specified; changes no
decision.

### There was no decision left to make

The amendment above deferred `boolean` on the grounds that unifying the two encodings carried
"a real decision attached (`0`/`1` vs `"true"`/`"false"`)". That reading was wrong. This
document had answered it twice, in the original text:

- the `asset_field_value` typed-columns comment: `number/boolean -> value_num`
- the field-type primitives table: `boolean | value_num | 0/1 (so we can index numerically)`

So `boolean` was never an open question. It was a **drift repair**, identical in kind to
`tree`'s — and deferring it on the belief that it was a design call is worth recording, because
the belief was formed by reading the *code* (a clean two-against-two split, each side internally
coherent) without checking it against the *specification*. A tie among implementations looks
like an unmade decision. It usually is not.

### The state that shipped

| site | stored `boolean` as | agreed with the ADR |
|---|---|---|
| `metadata/handler.go` — asset write | `value_num` `0`/`1`, rejecting anything else | yes |
| `seed/runner.go` — asset seed | `value_num` `0`/`1` | yes |
| `metadata/collection_handler.go` — collection write | `value_text` `"true"`/`"false"` | no |
| `metadata/collection_handler.go` — collection create-seed | `value_text` | no |
| `metadata/collection_handler.go` — collection validator | required `value_text` | no |
| `web/.../PostHost.svelte` — asset display | read `value_text` | no |
| `web/.../FieldValueInput.svelte` — collection editor | wrote `value_text` | no |
| `web/.../upload/UploadFileRow.svelte` — upload modal | wrote `value_text` | no |
| `web/lib/fieldOptions.ts` — the frontend's column table | `value_text` | no |
| `openapi.yaml` — `CollectionFieldValueWrite` prose | documented the divergence as intended | no |

Ten sites, not the four the split appeared to have. Two right, eight wrong — and the shape of
the failure was worse than "renders blank" in one place: because the asset write endpoint has
*always* required `value_num`, **the upload modal's boolean checkbox produced a rejected request
every time it was used.** A user setting a boolean during upload got a failed field write, not a
blank one. That path had never been exercised, so nobody found out.

### Decision — `0`/`1` in `value_num`, and the range is enforced

Every writer stores the number `1` or `0` in `value_num`. Any other number is rejected: `400` on
the asset path, `422` on the collection path (the two paths' error contracts differ; see the
tree amendment). `"true"` in `value_text` is rejected rather than stored, because storing it
puts a value in a column no reader consults — the bug itself.

`NULL` remains distinct from `0`. "Not set" and "set to false" are different states and every
display distinguishes them: nothing renders for the former, "No" for the latter. This is the one
part of the encoding that is easy to lose accidentally, since `0` is falsy in both Go and
TypeScript, so it is asserted directly on both sides.

No migration: no `boolean` field definition has ever existed, so no row anywhere holds a boolean
value in either encoding.

### The pin now covers ENCODING, not just column

`tree`'s pin compared each writer's chosen *column* against a table. That is half the invariant,
and `boolean` is the half it missed — the two writers each picked a defensible column and still
disagreed, because **agreeing on a column says nothing about what goes in it**. Two writers can
both pick `value_num` and disagree about `1` versus `1.0`; both pick `value_text` and disagree
about `"true"` versus `"1"`. Column agreement is necessary and not sufficient.

`TestWritersAgreeOnColumnAndEncoding` therefore drives the asset and collection writers with
byte-identical input and compares their **rendered stored values** to each other. No table sits
between them to be updated on both sides at once and hide the drift — the same failure mode as a
doc comment reworded along with the bug it described. The seeder's pin
(`internal/seed/valuecolumn_test.go`) gained the same treatment: it is the writer that actually
*translates* (JSON `true` becomes `1`), so "which column" was always the smaller half of what
could go wrong there. On the frontend, `encodeBoolean` / `decodeBoolean` in `fieldOptions.ts` are
the single definition every boolean surface imports, with the `null`-vs-`false` distinction
covered by tests.

`collectionValueColumnOverride` — the map that recorded this divergence as deliberate — is
**deleted**, not emptied. It held exactly one entry, this one. A mechanism for registering a
deliberate divergence is an invitation to register one, and the lesson of both #778 and #791 is
that a divergence which is merely *documented* is a divergence that *ships*. There is now
nowhere to record an exemption; a writer that disagrees fails.

### Where the field types stand

All eleven primitives now agree across all four writers and every display surface, and the two
that had never been exercised end-to-end (`tree`, `boolean`) both have an integration test that
writes through the API and reads back from the columns.

The "never instantiated, therefore never exercised" condition still applies to `longtext`,
`rich_text`, `date`, `datetime` and `reference` — they *agree*, but agreement was verified by
reading, not by driving them. The seeder's RFC3339-only `parseTime` (noted in the tree
amendment) remains the one known concrete instance and remains open.

## Amendment 2026-08-10 (second) — a field definition may declare itself a VIEW onto a column (#822, PR #1007)

`assets.title` and `assets.description` were real columns **and** shipped field definitions, with
nothing expressing that they are the same thing. Two independent stores for one concept, free to
drift the moment anything wrote a field value.

**Decision: the column stays the storage; the field declares itself a view onto it.**
`field_definition.mirrors_column` (migration 00044, CHECK-constrained) names the column. Rejected:
deleting the duplicate definitions (extraction must be able to target `title` through the field
system — IPTC `ObjectName` maps to it — and it would drop them from search config and display
groups), and promoting the columns into fields (93 Go references to `.Title`; far too hot).

**Enforcement is in the DATABASE, not in Go.** Three plpgsql accessors resolve the declared
identifier with `format('%I')`, so no Go or query-layer code names `title` or `description` — the
CHECK constraint is the only enumeration, and widening it is a migration rather than a sweep. Two
triggers reject any `asset_field_value` / `asset_field_value_history` row whose field declares a
mirror, and a third refuses to *declare* a mirror over a field that already holds values. **A path
that has not learned to route fails loudly rather than quietly writing a second copy** — that
covers the seed loader, `psql`, imports and untaught Go alike, which a Go-side branch would not.

### ⛔ The authorisation half, which the issue did not anticipate

Making a field a view onto a column **merges two different permission planes**, and they were not
equivalent. The field plane admitted any authenticated caller; the column plane requires owner,
team-scoped `assets.admin`, or global. Left alone, `PUT /assets/{id}/fields/{title_id}` would have
let **every signed-in account retitle every asset on the instance** — an authorisation regression
arriving as a side effect of a data-model tidy-up.

**The rule: a mirrored write must satisfy the underlying column's gate, and the field's own
`write_capability` composes on top.** It now has one home in
`visibility.AssetMutationCaps.MayMutateOwned`; `assets.canMutateAsset` is a thin adapter holding no
logic of its own, because `metadata` cannot import `assets` (a real cycle) and two statements of an
authorisation rule is the same defect this ADR is about, one plane over.

Corollaries: a `required` mirrored field cannot be blanked, so the field plane cannot reach a state
`PATCH /assets/{id}` forbids; and no history rows are written for mirrored fields, because a
per-field trail covering only edits made through *this* endpoint would lie by omission.

**Generalisable** — when one concept gains a second write path, the gates on both paths must be
reconciled explicitly. The permissive one wins by default, and that default is a security bug.

## Amendment 2026-08-10 — a card-display flag is a display hint, and display hints FEDERATE

#552 adds a per-field flag for "show this at-a-glance on the card". Two questions came with it,
and both are now settled rather than left for the sprint to invent.

**1. It is a display HINT, under this ADR's existing rule.** The schema block above annotates
`display_order` / `display_group` as *"Display hints — UI may use; do not gate logic on these."*
The card flag joins them. Nothing may branch on it for access, filtering, or correctness — a
client that ignores it entirely must still be correct, merely plainer.

**2. It TRAVELS in the federated envelope.** Not a preference — ADR 0083 already decides the
criterion. `display_group` and `display_order` are **in** the envelope (settled during #738), and
the exclusion rule there is that a property is left out "because it names something that exists
only on the sender". A card-display flag names nothing sender-specific: it describes the field,
not the server. So it goes **in**, and the envelope list in both this ADR and 0083 gains it when
#552 lands.

The consequence worth stating plainly: **a peer's fields render the way that peer meant them to.**
A field its owner marked as at-a-glance shows at-a-glance here too. That is the point of shipping
the hint rather than re-deriving presentation locally.

### The counterweight the operator set (2026-08-10): seamless, but never anonymous

> *"if they are sharing, federation should work seamlessly, but be distinct enough to know it's
> from another server"*

Both halves bind, and they pull against each other by design:

- **Seamless** — federated content is not second-class. It uses the same card, the same viewer,
  the same hints. No degraded rendering, no "remote" fallback layout.
- **Distinct** — the *origin* must remain legible. A user must never mistake another server's
  content for something this instance vouches for. `origin_server_id` already exists on the
  relevant tables (baseline schema), and `federation/RestrictedShareBanner.svelte` is the closest
  existing treatment; the card surface has no provenance affordance yet.

**This constrains #552 and #557 both**, and the constraint is easy to get backwards: seamlessness
is about *layout and capability*, provenance is about *attribution*. Making remote content look
different is the wrong reading; making it look identical **and unattributed** is the other wrong
reading. Whatever the card does, it must answer "whose is this?" without answering it in a way
that makes remote work feel lesser.

## Amendment 2026-08-02 — a vocabulary can be open, and open means the WRITE POLICY differs

**PR #846 / issue #830 (part A of #789).** The 2026-07-30 amendment gave options a lifecycle
and #824/#841 closed the write path around it: a value naming a term the field does not offer
is refused with a 422, always. That is the right rule for `country` and the wrong rule for
`keywords` — the field whose vocabulary is *supposed* to grow from the material. This amendment
records the sanctioned way past the gate.

### The flag, not a twelfth type

`field_definition.open_vocabulary boolean NOT NULL DEFAULT false` (migration 00028). An open
vocabulary is a `multi_select` in storage, rendering, search and federation; it differs only in
what a write does with an unknown term. Modelling that as a new field type would have put a
twelfth arm in every type-switch in the codebase to express a difference none of those switches
care about. The column is legal on every type and **honoured only for `multi_select`**; the
narrowing lives in one Go function (`openVocabularyApplies`) rather than a CHECK constraint, so
opening `select` later is a decision, not a migration.

### What a write does on an open field

Each incoming term is matched against the vocabulary — **slug or label, case-insensitive,
whitespace-trimmed, full depth, archived excluded** (the same matching the extraction resolver
already used, now the rule on both paths). A match stores the existing canonical slug: "Character"
and "character" are one term, which is the owner's stated requirement. A true miss **creates** the
term — slugified input as `value`, trimmed original as `label` — and stores its slug. Lifecycle
still applies: openness is about terms the field does not *have*, not terms it has *retired*, so
choosing a deprecated term afresh is refused exactly as on a closed field, and a mint whose slug
would collide with an archived term's slug is refused rather than resurrecting or shadowing it.

### Atomicity — the part that made this an amendment

Term creation rewrites the whole options document, the same last-write-wins hazard the
2026-07-30 amendment recorded for the admin editor — except here it would fire on an ordinary
value save. The creation therefore runs **inside the value-write transaction** with
`SELECT … FOR UPDATE` on the field row, re-resolving against the locked document rather than the
caller's LRU copy (which can be one write old — resolving against a stale copy is how a term gets
minted twice). Two concurrent writes minting different terms both survive; this is pinned by a
two-goroutine test against a real database. The flag itself is also read from the locked row, so
a cached `true` that an admin has since turned off cannot mint. The residual race — the admin
editor's whole-document PATCH does not take this lock — remains the 2026-07-30 amendment's known
gap; closing it means moving the editor onto the same lock.

### Extraction can finally write the field

The applier gains a multi-value path (`value_options` end-to-end): IPTC 2:25's comma-joined set
is split, each term matched-or-minted through the *same* creation routine as the API path, with
`set_by` naming the extractor (`iptc`). Idempotence is set-equality on canonical slugs, order-
insensitive, so backfills converge. `keywords ← iptc_keywords` is wired in migration 00028 itself
— wiring is data, and the migration that makes a wiring safe is the one that turns it on (00025's
precedent, whose own comment had recorded exactly this gap). Two rules stay deliberately strict:
extraction into a *closed* multi_select records a failure row per unknown term rather than
minting, and **field defaults stay closed even on open fields** — a defaults editor silently
growing a vocabulary is surprising in a way a value write is not.

Extraction writes now also append to the value history — the `FieldValueWriter` contract had
always *claimed* history-in-one-tx and the implementation had never written any, so every
extraction-set value carried a blank audit trail. The comment now matches the code because the
code was fixed, not the comment.

### Addendum 2026-08-02 — the picker previews with the server's own matching (#831/#851)

The entry UX ships a client-side `resolveTerm`/`slugify` (web/src/lib/fieldOptions.ts) that
mirrors the server's resolver: same slug-or-label case/whitespace-insensitive match, same
slugify, same archived-slug-collision refusal. This is a deliberate two-implementation
invariant of the same class as the writers-agree pin above — **what the combobox previews
("create «Neon Skyline» → neon-skyline") must equal what the server stores** — and any change
to the server's matching or slugify must change the client mirror in the same PR. The client
deliberately emits the RAW text for a term being created, never a pre-slugified form: the
server mints the label from what it receives, and sending the slug would name the keyword
`macro-detail` instead of "Macro Detail".

### Addendum 2026-08-02 (second) — tree editing ships; depth stays deliberately uncapped (#779/#853)

The tree editor implements this ADR's amended model with no new structural decisions beyond
one worth recording: **tree depth is uncapped on both sides.** The server never enforced a
maximum and the editor does not invent one — a client-only cap would make a legal catalogue
uneditable, which is the same expressed-vs-obtained trap this document keeps warning about.
Reparent and relabel confirm the model's promise in practice: values address terms by
tree-wide-unique slug, so moving or renaming a term never touches a stored value. The
editor-save-vs-mint race is now a DETECTED conflict (the mint path bumps `updated_at`, so a
stale editor baseline 409s) — narrowing the 2026-07-30 amendment's last-write-wins gap to
editor-vs-editor only.

## Amendment 2026-09-01 — a field can say what a value must look like, and who may write one (#1173)

**Status:** accepted. Migration `00064`. Two columns on `field_definition`: `read_only boolean NOT
NULL DEFAULT false` and `regexp_filter text NULL`.

Thirty columns described what a field IS and none of them described what a value of it may BE.
An operator whose `shot_code` values all read `AAA_0010` had no way to say so, and one whose
`pipeline_id` is written only by extraction had no way to stop a person overwriting it. Both gaps
are the same gap wearing two hats: the field definition is where an operator's intent lives, and
this pair is the intent that was unsayable.

### `read_only` refuses PEOPLE, and the seam is the call site

This is the decision, and it is the one that is easy to get backwards. `read_only` does not freeze
the value. Upload defaults, the metadata-extraction pipeline and the mirrored-column filler all
keep writing, because a field an operator marks read-only is normally one they mean the SYSTEM to
own. What it refuses is a human write.

**The exemption is not a flag and cannot be requested.** Four handlers enforce the rule, and all
four begin with `auth.IdentityFromContext` and answer 401 without an identity:
`SetAssetFieldValue`, `ClearAssetFieldValue`, `SetCollectionFieldValue`,
`ClearCollectionFieldValue`. The three writers that skip it are different Go functions with no
OpenAPI operation and no route at all: `ApplyAssetDefaults`, the extraction adapter's
`WriteAssetFieldValue`, and `mirrorFill`. Reaching an exempt writer means being one, so there is
nothing for a client to claim.

This is why there is deliberately **no caller-selectable `initial` flag**, and why `set_by` is not
the mechanism either. A boolean a caller can send is a boolean a caller can lie about, and `set_by`
is server-assigned provenance: evidence of which writer ran, read after the fact, never consulted
to decide whether a write is allowed.

**The consequence, stated so it is not later mistaken for a bug: a stored value may fail a rule a
person would be held to.** That is what "human input validation" means, and the alternative is
worse in both directions. Applying these rules to system writers would make an operator's
formatting preference able to break extraction; applying them retroactively would make a settings
change into a data migration.

### The asset and collection first-write distinction

They differ, and the difference is structural rather than a preference.

- **ASSET: refused immediately, including where the field holds no value.** There is no human
  first-write seam to protect. `POST /assets` writes no `asset_field_value` rows at all:
  `AssetCreate.metadata` is `additionalProperties: true` and lands in the `assets.metadata` JSONB
  column. So "let the first human value through" would be a rule with nothing to attach to.
- **COLLECTION: the create body MAY seed an initial value; every later write is refused.**
  `CollectionCreate.field_values` seeds values inside the create transaction, which is a real
  first-write seam. Refusing it would make a read-only collection field permanently empty unless
  extraction happened to own it, and collections have no extraction pipeline.

The collection seed gate runs **pre-transaction**, beside the existing required-field check and for
the same reason recorded there: a refusal must not leave a half-created collection behind. It
therefore cannot live inside `SeedCollectionFieldValueInTx`, which by then is one statement away
from a written collection row.

`required` is unchanged, and no `required + read_only` refusal is added. The two are already
coherent: asset `required` is write and clear validation rather than presence at creation, and
collection `required` is presence at creation, which the seed the flag permits satisfies.

### Mirrored fields are excluded from BOTH, and partial enforcement was the tempting wrong answer

`title` and `description` declare `mirrors_column` and are views onto columns of `assets`. Those
columns carry a SECOND human write plane: `POST /assets` sets the title, `PATCH /assets/{id}`
mutates both. Enforcing either setting on the metadata plane alone would let one plane reach a
state the other calls invalid, which is precisely the divergence the 2026-08-10 amendment exists to
prevent, one rule over.

The alternative considered and rejected was teaching the asset create and update paths to obey the
field's settings. That is a real feature and a much larger one: it would put a field-definition
lookup in front of every asset write, and it would make `PATCH /assets/{id}` fail for reasons the
caller cannot see in its own schema. Neither setting is worth that, so both are refused at
configuration time instead.

The exclusion is a CHECK constraint rather than a Go rule, following the 2026-08-10 amendment's own
argument: a path that has not learned the rule fails loudly instead of quietly storing a setting
half the writers obey. The handler refuses first with a sentence, so an operator sees a 400 rather
than a 500, which is the division of labour the card-display gate already has.

The unset state stays legal on a mirrored field, and so does the clear. Only the non-default
`read_only` and a non-empty pattern are refused.

### `regexp_filter`: the supported types, and why `rich_text` is not one

**SUPPORTED: `text` and `longtext`.** Both store the operator's own words verbatim in `value_text`.

Everything else refuses a non-empty pattern, and `rich_text` is the member worth arguing, because
it shares the storage column and looks like it belongs. It does not, and the reason is what the
column HOLDS. `richtext.SanitizeValueText` runs before every write, so a stored value is
policy-clean markup, not typed text. A real one looks like this:

```
<p>Cleared for <strong>internal</strong> use.</p>
```

A pattern would be matched against tags. `^[A-Z]` fails on every rich-text value ever stored, and
two values reading identically to a person carry different markup. It is the same objection that
excludes `number`, `boolean`, `date` and `datetime`: the stored form is not the form the rule is
written about. `select`, `multi_select` and `tree` already have a stronger constraint, their
vocabulary. `reference` holds a UUID.

Which types honour a pattern lives in ONE Go function, `regexpFilterApplies`, rather than in a
CHECK constraint. That follows the `open_vocabulary` precedent in the 2026-08-02 amendment and for
the same reason: widening the list later should be a decision rather than a migration.

### Whole-value semantics, and why the server does the anchoring

Go's `regexp` is RE2. A value matches when it matches `\A(?:` plus the operator's pattern plus
`)\z`, assembled at match time.

Operators are **not** asked to write the anchors themselves, and neither of the two reasons is
cosmetic:

- `^` and `$` are LINE anchors as soon as a pattern turns on `(?m)`. A hand-written
  `^[A-Z]{3}_[0-9]{4}$` would happily accept a two-line value whose second line is anything at
  all. `\A` and `\z` are unaffected by `(?m)`, so whole-value semantics survive a multiline
  pattern.
- Anchors bind tighter than a top-level alternation, so `^a|b$` means "starts with `a`, or ends
  with `b`". The non-capturing group is what makes `a|b` mean "the whole value is `a`, or the whole
  value is `b`", which is what the person who wrote it meant.

RE2 has no backtracking and runs in time linear in the input, which is what makes accepting a
free-text pattern from an operator safe. There is no length or complexity cap, and none is needed.

### NULL is the one "no constraint", and the pattern is never trimmed

`regexp_filter` is `text NULL` with no default, and a CHECK refuses the empty string. This is
`edit_tab`'s reasoning from migration `00058` applied to a second column: if `""` were storable,
"this field has no pattern" would have two representations and every reader would have to know
both. Removal therefore travels as an explicit `clear_regexp_filter: true` on
`FieldDefinitionUpdate`, mutually exclusive with `regexp_filter`, exactly as `clear_edit_tab` and
`clear_default` do.

The PATCH contract, in full:

| body | result |
|---|---|
| neither property | unchanged, because PATCH is partial |
| `regexp_filter` non-empty | configured, after the checks above |
| `regexp_filter: ""` | 400, naming `clear_regexp_filter` as the way to remove one |
| `clear_regexp_filter: true` | SQL NULL |
| both | 400 |

**One deliberate divergence from `edit_tab`, and copying that precedent wholesale would have been a
data bug.** `edit_tab` TRIMS before its blank check, because a tab named `" "` is a tab nobody can
navigate to. A pattern is not a label. Whitespace inside one is meaningful, and under the
whole-value semantics above `\A(?:   )\z` legitimately matches exactly three spaces. So only the
GENUINELY EMPTY string is refused here, a whitespace-only pattern is a valid configuration, and
nothing trims or rewrites what the operator wrote.

`clear_regexp_filter: true` is accepted on EVERY field, including mirrored ones and unsupported
types. The two restrictions above are about a configured pattern; a setting must always be
reachable in the direction of off, or a field configured wrongly by an import or a script becomes
unrepairable through the API.

### What refuses what

Configuration-time refusals are 400 at `PATCH /fields/{id}`: a non-default `read_only` or a
non-empty pattern on a mirrored field, a non-empty pattern on an unsupported type, a pattern that
does not compile, a blank pattern, and the two clear properties together.

Value-time refusals are 422 carrying `FieldValueUnprocessable`, the body the asset and collection
writers already share so the two cannot describe one refusal differently. Two new reasons:
`field_read_only` and `pattern_mismatch`. Deliberately not 403 for the first: no capability grants
it and no grant would lift it, so a permission code would send an operator hunting for a role that
does not exist. The thing to change is the field's configuration.

## Amendment 2026-09-02 — `required` means something on the WRITE path, and a field value is its own concurrency unit (#1389, #1119)

**Status:** accepted. No migration. No new column: the token this rests on, `set_at`, has been
on both value tables since the baseline and has always advanced per row.

An operator could set `required` on a field, and on the ordinary value-write paths nothing
enforced it, on either subject kind. The package held exactly two `fieldRow.Required` checks and
both were inside the MIRRORED helpers, so the flag reached `title` and `description` and nothing
else. `SetAssetFieldValue`, `ClearAssetFieldValue`, `SetCollectionFieldValue` and
`ClearCollectionFieldValue` contained zero between them. An empty write was accepted and the value
could be deleted outright.

It stayed invisible for two sprints for a specific reason: `title` is required AND mirrored, so
every reproduction written against the obvious field passed.

### R1 and R2 are different rules, and merging them breaks the product

**R1, the later-write rule.** A later HUMAN `Set` may not write an EMPTY value into a required
field, and a later HUMAN `Clear` of one is refused. It applies to the four ordinary field-value
handlers, on assets and collections alike, and it answers 422 `FieldValueUnprocessable` with the
new reason `field_required` — the same body the read-only and pattern refusals already share, so
the two subject kinds cannot describe one refusal differently.

**R2, the create-time rule.** Collection CREATE separately requires values for required collection
fields and answers 422 `RequiredCollectionFieldMissing`. Unchanged, and deliberately NOT widened.

**Asset creation keeps no completeness gate at all.** `POST /assets` writes no
`asset_field_value` rows — `AssetCreate.metadata` lands in the `assets` JSONB column — so the
two-action guarantee holds: drop a file, press publish, nothing required.

### The write matrix, and why the collection seed is exempt

| writer | class | R1 |
|---|---|---|
| `SetAssetFieldValue` / `ClearAssetFieldValue` | human edit / removal | enforced |
| `SetCollectionFieldValue` / `ClearCollectionFieldValue` | human edit / removal | enforced |
| `SeedCollectionFieldValueInTx` | human INITIAL, from the create body | exempt |
| `ApplyAssetDefaults` | system | exempt |
| the extraction adapter's `WriteAssetFieldValue` | system | exempt |
| `mirrorFill` | system | exempt |

The seed is a HUMAN write on three counts — its own doc says required-field validation is the
caller's job and already happened pre-transaction, `ValidateCollectionSeedValues` pattern-checks it
because `regexp_filter` validates human input, and the 2026-09-01 amendment above carves it out
because "the create body MAY seed an initial value; every later write is refused". Its exemption
comes from the R1/R2 BOUNDARY, not from provenance: it is the create body, which is R2's business,
and R2 already demands a value be there.

The three system exemptions are STRUCTURAL, exactly as `read_only`'s are. Those call sites are
different Go functions with no OpenAPI operation and no route, which is why
`AssetFieldValueWrite.set_by` has no `default` or `mirror` member. There is deliberately no
caller-supplied `initial` or `system` bypass, for the reason the read-only amendment gives: a
boolean a caller can send is a boolean a caller can lie about.

### What EMPTY means, per type, and why `rich_text` is not a trim

| type | empty |
|---|---|
| `text`, `longtext`, `select`, `tree` | `value_text` NULL, or whitespace-only |
| `rich_text` | SEMANTIC emptiness — see below |
| `multi_select` | `value_options` NULL or zero-length |
| `number`, `boolean` | `value_num` NULL |
| `date`, `datetime` | `value_date` NULL |
| `reference` | `value_ref` NULL |

**FALSE is a real boolean value.** `value_num = 0` is a deliberate "no" and is never empty; only a
NULL is. A rule written as a truthiness test would delete every one of them.

**`rich_text` is measured, not assumed.** The mirrored helper's `strings.TrimSpace` test is
text-shaped, and the stored form of a rich-text value is sanitised HTML. Nothing in
`richtext.Sanitize` removes empty elements, and against the shipped policy these all survive it
unchanged:

```
"<p></p>"  "<p><br></p>"  "<p>   </p>"  "<br>"  "<ul><li></li></ul>"  "<blockquote></blockquote>"
```

`<p>&nbsp;</p>` survives with the entity decoded to a literal U+00A0, which is why the predicate is
written against the CHARACTER and never against the entity string. So a TrimSpace implementation
accepts a required rich-text value that renders as nothing at all, and the field then reads blank
while the server considers it filled.

The rule is therefore the server-authoritative TWIN of the display rule the frontend already
ships: `web/src/lib/fieldDisplay.ts`'s `htmlToPlainText`, whose output feeds the field count and
the "is this set" test. ONE rule, in `app/internal/richtext` beside `Sanitize` — the package that
already decides what a rich-text value IS, and callable from outside `metadata` because the batch
work in 20c needs it too.

### The per-field concurrency boundary

**The token is the VALUE ROW'S OWN `set_at`.** Never `assets.updated_at` and never the
collection's. Two people editing two different fields of one record are not in conflict, and a
subject-level token would make them so on every busy record. Both upserts already write
`set_at = NOW()` on INSERT and inside `ON CONFLICT DO UPDATE`, and both response schemas already
carry `set_at` as required, so the token a client needs was already in its hands.

Three states on a write:

- `if_unchanged_since` — guarded write against an EXISTING row.
- `if_absent: true` — guarded FIRST write, against absence.
- neither — UNGUARDED last-write-wins, **unchanged**, and not a legacy accident to be tightened
  later: the upload flush depends on it and so does every non-edit-surface caller.

The two are mutually exclusive and sending both is a 400. `if_unchanged_since` on a row that does
not exist is a **409, not an insert**: a timestamp is a claim that a particular version is still
there, and silently resurrecting a value somebody cleared is the refused update wearing a disguise.
A `Clear` carries the guard as a QUERY PARAMETER, because a DELETE has no body; it has no
`if_absent` companion, since "remove it only if it is not there" has nothing to remove.

The 409 body is `AssetFieldValueConflict` / `CollectionFieldValueConflict`, with `current`
**required and nullable** so the key is ALWAYS present. `present: false` carries `current: null`
and no fabricated `set_at`; an omitted key would be indistinguishable from a server that forgot to
send one, and the client could not tell "removed" from "unknown".

### The ATOMICITY guarantee, stated because the wrong answer is the easy one

For every guarded mutation, **the precondition and the mutation are ONE STATEMENT**. A handler-side
read, a comparison, and then the existing unconditional upsert or delete DOES NOT SATISFY THIS, and
it is the path of least resistance.

The reason is the isolation level. All four handlers open their transaction with
`pgx.TxOptions{}` — empty options, so READ COMMITTED, where a non-locking `SELECT` takes no lock at
all. The gap between such a read and the write that follows it fits an entire competing request,
and every single-threaded test passes anyway. Measured: the read-compare-write variant passes the
whole sequential guard matrix and answers `200 / 200` for two overlapping guarded Sets and
`204 / 204` for two overlapping guarded Clears.

So the guard is the WHERE clause of the statement that mutates — `UPDATE … WHERE set_at = $token`,
`DELETE … WHERE set_at = $token`, and `INSERT … ON CONFLICT DO NOTHING` for the first-write arm,
where the unique index is the precondition and no read participates. At READ COMMITTED a statement
that meets a row another transaction is writing blocks and then re-evaluates its own WHERE against
the version that transaction committed, so a second contender guarding on the same token cannot
match after the first lands. A zero-row result IS the conflict, which is why each of these is
`:one` where both deletes were previously `:exec` and surfaced no affected-row count.

### Mirrored fields are EXCLUDED from the per-field contract

`title` and `description` are views onto columns of `assets`. There is no `asset_field_value` row
to carry a `set_at` — `AssetFieldValue.set_at` for a mirrored read is the asset's `updated_at`
wearing the field shape — so a per-field token would guard a column against a timestamp from a
different plane, on a value `PATCH /assets/{id}` can change without this endpoint ever seeing it.
Both guards are refused with 400 naming the asset plane, which already has
`AssetUpdate.if_unchanged_since` for exactly this. Unguarded mirrored writes are unchanged, and so
are their own `required` refusals: those are the asset plane's rule, and R1 must not reach them or
the two planes start describing one refusal differently.

### The consequence for editors

The `required` flag now has a meaning a person meets, which means the surfaces have to be able to
meet it. `/assets/{id}/edit` renders field values for the first time, and the emptying interaction
every typed control already had now becomes a CLEAR on the wire rather than a typed write with its
value member omitted — which is what it was, and which both validators refused, so removing an
optional value was impossible from any surface in the product.

`boolean` was the one control that could not represent unset, and worse, could not DISPLAY the
difference: a checkbox rendered an absent value and a stored `false` identically and always emitted
1 or 0. It is a three-state select now, so emptying it is the same gesture as emptying a `select`
and no Clear button had to be invented for one type.

## Amendment 2026-09-03 — a field can say WHEN it appears, and hiding it destroys nothing (#1173, #1119)

**Status:** accepted. Migration `00065`. One column on `field_definition`:
`display_condition jsonb NULL`, with a shape CHECK.

Thirty-two columns described what a field is, what a value of it may be, and where it sits on a
form. None of them described **when the field should be offered at all**. An operator whose
`commission_deadline` only means anything on a `work_type` of `Commission` could show it to
everybody always, or not create it.

The full decision, including the parser contract, the operator and type matrix, the whole-condition
fail-open rule, the configuration refusal set, the cycle-invariant atomicity boundary, Policy B for
tabs, and the import specification, is **ADR 0099**. What follows is the part that belongs to this
document: what the column is on the model, and what it does not do to a value.

### It is a display hint, and it joins the ones already here

`display_condition` sits with `display_order`, `display_group`, `show_on_card`,
`show_in_advanced_search`, `show_on_upload` and `edit_tab`. A client that ignores it is still
correct. Nothing about access, filtering, indexing or write validity depends on it, and a hidden
field can still be written through `PUT /assets/{id}/fields/{field_id}` exactly as before.

It is **update-only**, the same shape the other six have: `display_condition` and
`clear_display_condition` on `FieldDefinitionUpdate`, neither on `FieldDefinitionCreate`. A field is
created and then configured, and a create body cannot reference a graph that does not exist yet.

`NULL` is the canonical unset and the CHECK makes it the only one, refusing `[]`, `{}`, `""` and JSON
`null` alike. This is `edit_tab`'s reasoning (00058) and `regexp_filter`'s (00064) applied to a third
column: one representation of "no constraint", so no reader has to know two.

### Value preservation, stated as a property of the model

**A condition never destroys a value.** This is the half of ADR 0099 that is really about the
metadata model, so it is restated here rather than referenced.

- Hiding a field emits **no Set, no Clear, and no empty row**. The stored value is untouched.
- Revealing it restores the persisted value **byte for byte**.
- An unsaved draft in a hidden field survives, reappears on reveal, and is **not submitted while
  hidden**.
- Archiving a controller does **not** rewrite or clear a stored `display_condition`, and restoring
  the controller resumes ordinary evaluation. Configuration records what an operator decided;
  runtime status is a fact about today, and one must not overwrite the other.

### No new completeness gate

**A `required` field hidden by a condition creates no new completeness or save gate**, on asset edit,
collection edit or `/create`.

This follows from the 2026-09-02 amendment above and does not modify it. R1 is a rule about a WRITE,
enforced by the four field-value handlers, and it still refuses an API clear of a required field
whether or not any form happens to be drawing the control. R2, collection create-time completeness,
is unchanged. **Asset creation still requires nothing**, so the two-action guarantee holds: drop a
file, press publish.

The tempting alternative, "a hidden required field is satisfied", and its opposite, "a hidden
required field blocks the save", are both wrong for the same reason: they would make a display hint
decide whether a write is allowed. Composition and validity are different planes, and this column
lives entirely in the first one.

### One consequence for the read path

Building conditional visibility required the composition read path to be fixed first, because
evaluating a condition over field values the caller may not read turns form composition into an
oracle over protected metadata. `GET /assets/{id}/fields` had **no** per-field read check and now
filters by effective, server-derived readability; `GET /collections/{id}/fields` moves to the same
shared helper; and both subject kinds gain a `field-composition` read that reports readability and
carries **no values at all**. ADR 0099 section 5 is the decision; it is noted here because it changes
what a caller receives from two endpoints this document defines.
