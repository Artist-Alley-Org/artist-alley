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
    field_set_id                UUID         NULL,                    -- for bundling (export/import)

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
    --   text/longtext/rich_text/select(single)/tree(path) -> value_text
    --   number/boolean                                    -> value_num
    --   date/datetime                                     -> value_date
    --   multi_select                                      -> value_options
    --   reference                                         -> value_ref
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
| `tree` | value_text | path-string like "NA/US/CA"; unlimited depth |
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
- `field_set_id` groups related fields into an export/import unit.
  Operators publish a `field_set` JSON to share with peers; peers
  import to adopt identical field schemas.
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

GET    /field_sets                      — list bundles
POST   /field_sets/import               — load a field-set JSON (federation peer share)
GET    /field_sets/{id}/export          — export for peer adoption

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
- Federation-aware from day one via stable codes + field-set
  bundles.

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

## Open questions

- Whether asset edit endpoints (`PATCH /assets/{id}`) should also
  accept inline `field_values` in the body, or always require the
  explicit `/assets/{id}/fields/{field_id}` PUT. Convenience vs.
  consistency; default to convenient + delegate to the field PUT
  handler internally.
