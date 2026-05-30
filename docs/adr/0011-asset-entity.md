---
id: "0011"
title: Asset entity — UUID-keyed, fork-not-port from RS's resource
status: accepted
date: 2026-05-25
area: architecture
phases: 
  - "1.4"
  - "1.4.D"
  - "1.8"
supersedes: []
related: 
  - "0007"
  - "0008"
  - "0009"
  - "0010"
tags:
  - architecture
  - ai
  - infrastructure
  - 3d
excerpt: >-
  artist-alley's storage layer (ADR 0008, implemented in Phase 1.4) sits below the user-facing entity. storage_objects deduplicates byte streams by sha256; storage_variants records renditions; storage_pins reference-counts ownership. None of these are the *thing a user uploads,…
---
## Context

artist-alley's storage layer (ADR 0008, implemented in Phase 1.4)
sits below the user-facing entity. `storage_objects` deduplicates
byte streams by sha256; `storage_variants` records renditions;
`storage_pins` reference-counts ownership. None of these are the
*thing a user uploads, titles, tags, and shares*. That layer was
called "resource" in ResourceSpace and is the next port.

Two earlier ADRs frame what we want:

- **ADR 0009 (collection model)** assumes `resource_id UUID` as the
  membership target. Collections can't land before this entity does.
- **ADR 0007 (federation thinking ahead)** locks in UUIDs and
  `origin_server_id` on every entity that might cross peer
  boundaries. RS's `resource.ref BIGINT auto_increment` is the
  opposite of that.

Per memory rule `feedback_pre_mvp_everything_is_volatile.md`, the DB
is throwaway until MVP — so we **fork the schema cleanly** instead of
trying to bend the RS table into a federation-ready shape.

## Decision

### Name: `assets`

The entity is called `assets` (table name) and lives under
`/api/v1/assets`. RS's `resource` table stays in the baseline
migration for PHP coexistence and is gradually retired as PHP pages
move to Go.

This conflicts with the Phase 1.4 endpoint group which currently
uses `/api/v1/assets` for raw byte upload/download. **We rename the
Phase 1.4 endpoints to `/api/v1/storage/objects`** — they were always
the low-level byte plane and the name now reflects that.

### Schema

The new `assets` table carries most of RS's `resource` columns (per
user guidance: keep what we haven't replaced yet, drop later as
porting reveals what's actually used), with three structural
additions: UUID primary key, file-hash link to the storage layer,
and federation-ready origin tracking.

```sql
CREATE TABLE assets (
    id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    title              TEXT         NOT NULL DEFAULT '',
    description        TEXT         NOT NULL DEFAULT '',
    resource_type      BIGINT       NOT NULL REFERENCES resource_type(ref),
    owner_user_ref     BIGINT       NULL,                     -- nullable so system uploads can exist
    status             TEXT         NOT NULL DEFAULT 'active'
                                    CHECK (status IN ('draft','active','archived')),
    file_hash          TEXT         NULL REFERENCES storage_objects(hash) ON DELETE SET NULL,
    file_extension     TEXT         NULL,                     -- 'jpg', 'mp4', 'glb', ...
    file_size_bytes    BIGINT       NULL,                     -- denormalized from storage_objects for fast filter
    -- RS columns we keep until the porting reveals whether they're needed:
    rating             INTEGER      NULL,
    user_rating        REAL         NULL,
    hit_count          BIGINT       NOT NULL DEFAULT 0,
    new_hit_count      BIGINT       NOT NULL DEFAULT 0,
    request_count      BIGINT       NOT NULL DEFAULT 0,
    archive_state      INTEGER      NOT NULL DEFAULT 0,        -- RS "archive" enum; status above is the new layer
    access             INTEGER      NOT NULL DEFAULT 0,        -- RS "access" enum (0 open, 1 restricted, 2 confidential)
    thumb_width        INTEGER      NULL,
    thumb_height       INTEGER      NULL,
    image_red          SMALLINT     NULL,                      -- dominant-colour theming
    image_green        SMALLINT     NULL,
    image_blue         SMALLINT     NULL,
    colour_key         TEXT         NULL,
    geo_lat            DOUBLE PRECISION NULL,
    geo_long           DOUBLE PRECISION NULL,
    country            TEXT         NULL,
    has_image          BOOLEAN      NOT NULL DEFAULT FALSE,
    is_transcoding     BOOLEAN      NOT NULL DEFAULT FALSE,
    -- artist-alley additions:
    metadata           JSONB        NOT NULL DEFAULT '{}'::jsonb,  -- EXIF / extracted / custom fields
    origin_server_id   UUID         NULL,                          -- federation prep (ADR 0007)
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    deleted_at         TIMESTAMPTZ  NULL                            -- soft-delete
);

CREATE INDEX assets_owner_idx       ON assets (owner_user_ref) WHERE deleted_at IS NULL;
CREATE INDEX assets_type_idx        ON assets (resource_type)  WHERE deleted_at IS NULL;
CREATE INDEX assets_status_idx      ON assets (status)         WHERE deleted_at IS NULL;
CREATE INDEX assets_file_hash_idx   ON assets (file_hash)      WHERE file_hash IS NOT NULL;
CREATE INDEX assets_created_at_idx  ON assets (created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX assets_metadata_gin    ON assets USING gin (metadata);

-- Tags as a join table (indexable, queryable; jsonb tags can't be
-- filtered as cheaply at our expected scale).
CREATE TABLE asset_tag (
    asset_id   UUID  NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag        TEXT  NOT NULL,
    added_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (asset_id, tag)
);

CREATE INDEX asset_tag_tag_idx ON asset_tag (tag);
```

Notes on the column choices:

- **`status`** is artist-alley's new vocabulary (`draft / active /
  archived`). RS's `archive_state` enum (0..5 with site-specific
  semantics) lives alongside as an integer; mapping happens at the
  PHP/Go boundary until RS pages retire.
- **`file_hash`** ties the asset to the byte plane (`storage_objects`).
  Multiple assets pointing at the same hash is fine and intentional —
  that's the whole reason content-addressed storage exists.
- **`metadata jsonb`** is the extensibility safety valve. EXIF blobs,
  extracted technical metadata, custom-field overrides land here
  rather than as new columns. Indexed via GIN.
- **`tags`** are a join table, not jsonb. Indexable; query patterns
  like "every asset tagged X" stay fast.

### API surface

```
# raw-byte storage (renamed from Phase 1.4's /assets)
POST   /storage/objects                    — upload raw bytes, return hash
GET    /storage/objects/{hash}             — download bytes
GET    /storage/objects/{hash}/variants/{variant}  — download variant

# asset entity (Phase 1.8, this ADR)
POST   /assets                             — create asset (body: title, type, optional file_hash)
GET    /assets                             — list (paginated, filter by owner/type/status/tag)
GET    /assets/{id}                        — fetch one
PATCH  /assets/{id}                        — partial update
DELETE /assets/{id}                        — soft-delete

GET    /assets/{id}/file                   — proxy to /storage/objects/{file_hash}
GET    /assets/{id}/variants/{variant}     — proxy to /storage/objects/{file_hash}/variants/{variant}

POST   /assets/{id}/tags                   — add tags {tags: ["x","y"]}
DELETE /assets/{id}/tags/{tag}             — remove
```

The proxy endpoints (`/assets/{id}/file`, `/assets/{id}/variants/*`)
mean the front-end never has to know an asset's underlying hash —
better encapsulation, and we can swap storage backends without
breaking links.

### Storage pinning

Creating an asset auto-pins the underlying byte blob with
`{subject_type: "asset", subject_id: <asset.id>}`. Deleting the
asset removes the pin; the storage GC sweeper later removes the
bytes if no other pin remains. The existing `user:` pin from upload
gets removed when the asset is created (its work is done).

### Migration from RS data

**Explicit non-goal** for Phase 1.8. The DB is volatile until MVP;
fresh installs start with the new model. If we ever need to import
RS data (and we may, for the maintainer's own RS instances), it
lands as a separate Phase later under `scripts/import-rs-resources.go`.

## Consequences

**Positive:**
- Clean UUID-keyed entity ready for federation, collections, search.
- Storage layer / entity layer naming finally distinguishes the byte
  plane from the user-facing record.
- Soft-delete + JSONB metadata + tag join table cover the
  extensibility cases without re-architecting later.
- RS coexistence preserved — `resource` table untouched; PHP keeps
  rendering through the translator.

**Negative:**
- Breaking rename of Phase 1.4 endpoints (`/assets` → `/storage/objects`).
  Acceptable per volatility rule; will be a single-commit change with
  test-shim updates.
- Two tables (`assets` + RS's `resource`) describe overlapping things
  during the transition. Operators with no RS data installed can
  ignore `resource` entirely; we drop it from the baseline when no PHP
  page on the served route table needs it.

**Deferred:**
- Search / filter DSL (ADR 0010) is what powers `GET /assets?...`
  beyond simple equality filters. Until 0010 lands, list endpoint
  supports only owner / type / status / tag exact-match filters and
  cursor pagination.
- Variant generator (was Phase 1.4.D) finally has somewhere to attach
  — it lives as a background job that produces thumbnails and
  previews for each new asset, writing `storage_variants` rows.
  Scope: ships with Phase 1.8 if simple (image only), else immediately
  after.
- Resource_type as UUID-keyed entity (currently BIGINT FK). Phase
  1.8 keeps the BIGINT FK to avoid a coordinated change; if/when
  resource_type ports to UUIDs, `assets.resource_type` swaps.

## Open questions

- Whether asset versioning belongs in this model (replace the file
  but keep title/tags/comments). Useful for "v2 of this concept piece"
  — but adds complexity. Lean: defer to a follow-up, model versions
  as separate assets linked via `metadata.parent_asset_id` until
  demand is clear.
- Whether `owner_user_ref` should be UUID. The user table itself
  still uses `ref BIGINT` (RS); porting that is its own large phase.
  Keep BIGINT for now, swap when users move to UUIDs.
