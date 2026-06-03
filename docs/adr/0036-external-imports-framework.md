---
id: "0036"
title: External imports framework — multi-source, UI-managed asset ingestion
status: accepted
date: 2026-05-30
area: architecture
phases:
  - "1.43"
supersedes: []
related:
  - "0008"
  - "0011"
  - "0012"
  - "0021"
  - "0029"
  - "0033"
  - "0034"
tags:
  - architecture
  - imports
  - infrastructure
  - integrations
  - extensibility
excerpt: >-
  A first-class subsystem for pulling assets out of external systems —
  network shares, S3 buckets, git, Perforce, ShotGrid, OneDrive,
  SharePoint, Google Drive — through a single connector contract,
  with UI-managed config, per-item lifecycle tracking, explicit
  conflict resolution, and ingest-or-reference storage modes.
---
## Context

Today the only way assets enter Artist Alley is through the upload
pipeline — a human dragging files into the modal. That works for the
"I just made this thing" flow. It does not work for the much larger
class of asset that is **already somewhere else** and somebody else
owns the canonical copy:

- The studio's NAS or SMB share that thirty artists save to.
- An S3 / R2 / B2 / MinIO bucket the build farm writes outputs into.
- A Perforce depot where everything *actually* lives.
- A ShotGrid (Flow Production Tracking) site whose Asset / Version /
  PublishedFile entities are the project source-of-truth.
- A SharePoint document library, a OneDrive sync, a Google Drive
  team-folder where producers and reviewers drop reference.
- A git LFS repo carrying authored 3D and texture work.
- A one-off zip-on-a-URL someone wants un-archived into a collection.

Asking users to re-upload these is wrong for three reasons. It
duplicates the source of truth and immediately divides reality.
It loses the source-side mtime / version / revision / entity-link
history. And the moment someone updates the original on the source,
Artist Alley silently goes stale until somebody re-uploads.

ResourceSpace solves this with `staticsync` — `pages/tools/staticsync.php`
+ a chunk of `include/resource_functions.php`. Its design carries
genuine lessons:

- **Path encodes metadata.** `$staticsync_mapfolders` lets the operator
  declare "the second path component is the project field," so a
  folder structure becomes structured tags without manual entry.
- **Cheap change detection.** `filemtime()` against
  `resource.file_modified`; full hash only as a duplicate-block fallback.
- **Lifecycle with reversibility.** Missing-from-source moves a
  resource to `$staticsync_deleted_state` (archived, not gone);
  `$staticsync_revive_state` flips it back if the file reappears.
- **Two storage modes.** `$staticsync_ingest=true` copies bytes into
  the RS filestore; `false` leaves the bytes on the source and
  references them remotely. The toggle is exactly the tradeoff
  between storage cost and source-availability coupling.
- **Per-source process lock.** `set_process_lock("staticsync")` keeps
  concurrent runs from colliding.

And carries genuine pain points we should fix on purpose:

- **Single filesystem source per install.** A studio with NAS *and*
  S3 *and* ShotGrid has to either pick one or run three RS instances.
- **CLI-only.** Operators edit `config.php` to add a source; there is
  no admin surface to view, change, pause, or audit a sync.
- **Every run scans the whole tree.** No change tokens, no
  webhooks, no event-driven mode — even when the source supports it.
- **No per-item review.** A conflict (user edited the field in the
  UI, source then reasserted a different EXIF value) silently
  resolves whichever way the code happens to be ordered. No queue,
  no UI, no audit beyond `resource_log`.
- **No conflict policy choice.** "Source wins" is the only option;
  studios that want "user edits are sacred, log the source delta
  and stop" have no knob.
- **Hard-coded importer identity.** All resources show
  `created_by = $staticsync_userref` (default `1`). No attribution
  to the source, no per-run provenance beyond the timestamp.

The relevant lessons from the rest of Artist Alley's design:

- ADR 0008 (storage) — the storage abstraction can already point at
  remote backends; "reference mode" composes naturally.
- ADR 0011 (asset entity) — every imported file is an Asset; the
  external system is provenance, not a separate entity model.
- ADR 0012 (metadata model) — `set_by` already enumerates
  `'manual' / 'exif' / 'iptc' / 'xmp' / 'api' / 'import' / 'computed'`.
  Imports fit `'import'` and we extend the provenance to name *which*
  import source.
- ADR 0021 (platform integrations) — different direction: platforms
  PUSH events at Artist Alley (auth, notifications). Imports PULL
  assets from external systems. Both deserve first-class subsystems.
- ADR 0033 (observability) — `import.*` audit category + `import_*`
  metric family are reserved for this work.
- ADR 0034 (capability add-ons) — sister-pattern for pluggable
  extension surfaces; the connector interface is shaped so a future
  WASM-via-Extism plugin can implement it without core changes.

This ADR commits Artist Alley to one external-imports subsystem that
covers every source through the same data model, admin surface, and
lifecycle.

## Decision

Introduce an **External Imports** subsystem with three primitives —
**Source**, **Run**, **Item** — and a **Connector** interface that
every source kind implements. The subsystem is UI-managed,
job-queue-driven, audit-hooked, and pluggable.

### Connector contract

A connector is a Go type implementing a single small interface. The
shape is designed to fit FS, blob stores, version control, and
SaaS PIM/DAM APIs through the same surface, and to round-trip
through a future WASM plugin host (per ADR 0034) without rework.

```go
type Connector interface {
    // Identity + capability flags so the framework knows what surfaces
    // to render and what calls to make.
    Identity() ConnectorMeta

    // Validate the source config + decrypt credentials into an opaque
    // Session. Sessions are short-lived and not persisted.
    Open(ctx, SourceConfig, Credentials) (Session, error)
    Close(Session) error

    // Walk the source. Returns Items in tree-stable order. Connectors
    // that support change tokens (Drive, Graph) honor opts.SinceToken
    // and return a new token; connectors that do not return Items for
    // the full tree and the framework diffs against import_item.
    List(ctx, Session, ListOpts) (iter.Seq2[Item, error], NextToken, error)

    // Stream bytes for one item. Used at ingest and on cache miss for
    // reference-mode reads.
    Fetch(ctx, Session, ExternalID) (io.ReadCloser, FetchMeta, error)

    // Optional. Connectors that support push (Drive webhooks, Graph
    // subscriptions, ShotGrid event log) register a delivery handler;
    // others return ErrPushUnsupported and rely on cron-driven pulls.
    Subscribe(ctx, Session, EventSink) error
}

type ConnectorMeta struct {
    Kind            string   // "fs" | "s3" | "http" | "git" | "p4" | "shotgrid" | "ms365" | "gdrive" | ...
    DisplayName     string
    Version         string
    SupportsPush    bool
    SupportsDelta   bool     // can return only changes since a token
    SupportsRange   bool     // Fetch can honour byte ranges (matters for reference mode)
    AuthForm        []FormField  // declarative schema for the admin "Add source" form
}

type Item struct {
    ExternalID   string             // source-stable, immutable across runs
    ParentID     string             // empty for tree roots
    Path         []string           // human-readable segments, fed into mapping rules
    Kind         ItemKind           // file | folder | entity-link
    Size         int64              // -1 if unknown
    ContentHash  []byte             // strong hash if cheap; nil if the source doesn't expose one
    ETag         string             // weak validator; used when hash isn't available
    LastModified time.Time
    Metadata     map[string]any     // connector-native; survives into the mapping pipeline
}
```

`ExternalID` is whatever the source vouches for as a permanent
identifier. For FS / S3 / HTTP it is the relative path. For git it is
`blob_sha`. For Perforce it is `depot_path`. For Drive / Graph it is
the immutable item ID. For ShotGrid it is the entity GUID. The
framework treats it as opaque; the connector promises it is stable
across runs.

### Data model

Three new tables, plus a `source_id` + `external_id` pair joined to
`assets` for provenance lookups.

```sql
CREATE TABLE import_source (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                TEXT NOT NULL,
    connector_kind      TEXT NOT NULL,
    config              JSONB NOT NULL,        -- connector-specific (path, bucket, base_url, scope)
    credentials_ref     UUID NULL,             -- soft FK into encrypted credentials store
    schedule            TEXT NULL,             -- cron expression; NULL = manual / push-only
    mapping_rules       JSONB NOT NULL DEFAULT '[]',
    conflict_policy     TEXT NOT NULL DEFAULT 'source_wins'
                        CHECK (conflict_policy IN ('source_wins', 'local_wins', 'review')),
    storage_mode        TEXT NOT NULL DEFAULT 'ingest'
                        CHECK (storage_mode IN ('ingest', 'reference')),
    delete_policy       TEXT NOT NULL DEFAULT 'archive'
                        CHECK (delete_policy IN ('archive', 'hard_delete', 'ignore')),
    revive_on_reappear  BOOLEAN NOT NULL DEFAULT TRUE,
    importer_user_ref   BIGINT NULL,           -- which Artist Alley user owns the imports
    rate_limit_rps      INTEGER NULL,          -- per-source override
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    last_run_id         UUID NULL,
    last_run_at         TIMESTAMPTZ NULL,
    next_run_at         TIMESTAMPTZ NULL,
    state_token         JSONB NULL,            -- connector-managed; survives across runs
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_user_ref BIGINT NULL,
    updated_by_user_ref BIGINT NULL
);

CREATE TABLE import_run (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id       UUID NOT NULL REFERENCES import_source(id) ON DELETE CASCADE,
    trigger         TEXT NOT NULL CHECK (trigger IN ('cron', 'manual', 'push')),
    triggered_by    BIGINT NULL,
    started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ NULL,
    status          TEXT NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'running', 'success', 'partial', 'failed', 'cancelled')),
    counts          JSONB NOT NULL DEFAULT '{}'::jsonb,   -- {created,updated,unchanged,skipped,conflict,deleted,error}
    error_summary   TEXT NULL,
    correlation_id  UUID NOT NULL                          -- joined with audit log entries
);

CREATE TABLE import_item (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id           UUID NOT NULL REFERENCES import_source(id) ON DELETE CASCADE,
    external_id         TEXT NOT NULL,
    asset_id            UUID NULL REFERENCES assets(id) ON DELETE SET NULL,
    last_run_id         UUID NULL REFERENCES import_run(id) ON DELETE SET NULL,
    last_outcome        TEXT NOT NULL
                        CHECK (last_outcome IN ('created','updated','unchanged','skipped','conflict','deleted','error')),
    last_seen_at        TIMESTAMPTZ NOT NULL,
    last_modified_src   TIMESTAMPTZ NULL,
    content_hash        BYTEA NULL,
    etag                TEXT NULL,
    last_error          TEXT NULL,
    review_state        TEXT NOT NULL DEFAULT 'none'
                        CHECK (review_state IN ('none','pending','approved','dismissed')),
    UNIQUE (source_id, external_id)
);

CREATE INDEX import_item_run_idx        ON import_item (last_run_id);
CREATE INDEX import_item_outcome_idx    ON import_item (source_id, last_outcome);
CREATE INDEX import_item_review_idx     ON import_item (source_id, review_state)
    WHERE review_state = 'pending';
```

Idempotency is `(source_id, external_id)`. Every run replays this
join to compute "created vs updated vs unchanged vs deleted" without
the connector having to remember anything beyond its `state_token`.

### Lifecycle

The framework runs a fixed state machine per item:

| Source side                    | Local side                     | Result                                                  |
|---|---|---|
| New                            | No matching `import_item`      | Create asset → write `import_item` (`created`).         |
| Unchanged (hash/etag/mtime ≤)  | `import_item` present          | No-op (`unchanged`).                                    |
| Modified                       | Asset has no user edits        | Per `conflict_policy: source_wins` — overwrite.         |
| Modified                       | Asset has user-edited fields   | Per `conflict_policy` — overwrite / skip / queue review.|
| Renamed (same hash, new path)  | `import_item` matches by hash  | Update `external_id`, leave asset alone (`updated`).    |
| Missing                        | `import_item` present, asset exists | Per `delete_policy` — archive / hard-delete / ignore. |
| Reappears (new ListItem matches a deleted import_item by external_id or hash) | Asset archived | Revive if `revive_on_reappear`.                         |

"User-edited" is a per-field flag: when a user changes a metadata
field through the UI, the field write sets `asset_field_value.user_locked = true`.
Source-wins overwrites do **not** trample user-locked fields — they
update the file content and re-extract EXIF/IPTC/XMP into *non-locked*
fields only. This is the explicit fix for RS staticsync's silent
"EXIF beats user edit" behaviour.

### Storage modes

Two modes per source:

- **`ingest`** — copy bytes into the Artist Alley filestore on
  creation/update. The asset is self-sufficient; deleting the source
  does not break reads. Higher storage cost; required for any source
  the operator doesn't fully control.
- **`reference`** — leave bytes on the source. The asset row carries
  `import_source_id` + `external_id`; `storage.Open(asset)` dispatches
  through the connector's `Fetch`. Lower storage cost; reads break if
  the source disappears or rate-limits. Suitable for internal P4 /
  SharePoint / NAS where the source IS the storage. Variants (thumbs,
  previews) are still generated locally and cached in the filestore.

The storage abstraction (ADR 0008) gains one new backend kind,
`import-ref`, that delegates byte fetches through the connector pool.

### Mapping rules — path-as-metadata

Inherited from RS `$staticsync_mapfolders` and made JSON-shaped + per-source:

```json
[
  {
    "match": "regex",
    "pattern": "^projects/(?P<project>[^/]+)/(?P<artist>[^/]+)/(?P<kind>concept|model|texture)/",
    "extract": [
      { "target": "field:project_code", "value": "{{project}}" },
      { "target": "field:artist",       "value": "{{artist}}" },
      { "target": "asset_type",         "value": "{{kind}}" },
      { "target": "collection",         "value": "Projects / {{project}}" }
    ]
  },
  {
    "match": "prefix",
    "pattern": "_archive/",
    "extract": [
      { "target": "archive_state", "value": "archived" }
    ]
  }
]
```

Rules run top-to-bottom; first match wins. Mapping fires on item
**creation only** — on update the source content is refreshed but
mapped fields are not retramped. (Matches RS, fixes the user-edit
preservation problem on resync.)

### Scheduling and concurrency

- Cron-driven schedule per source (`schedule` column). Scheduler is a
  goroutine in the Go binary; wakes once a minute and enqueues
  `import.run` jobs for any source whose `next_run_at` has passed.
- Manual `POST /admin/imports/sources/{id}/run` queues an immediate
  run.
- Push (webhook / Graph subscription / Drive notification) routes
  through a connector-specific HTTP endpoint and enqueues a
  partial-update run scoped to the affected items.
- Per-source advisory lock — only one run per source at a time.
  Concurrent runs across different sources are bounded by the global
  import worker pool (default: 3; admin-tunable).
- Per-connector rate limits (`rate_limit_rps`) protect external
  services from runaway sync.

### Credentials

Credentials live in the existing encrypted credentials table (shared
with ADR 0021 platform integrations, the future webhook delivery
worker, and the cloud-bridge add-on host). Connectors receive an
opaque `Credentials` blob from the broker; the broker decrypts in
memory and zeroes after `Open`. No connector sees raw secrets at rest
and no connector writes them.

### Admin surface

`/admin/imports` ships with three views:

- **Sources list** — name / connector kind / schedule / last run /
  status / actions. Add-source flow uses the connector's `AuthForm`
  metadata to render the field set declaratively (no per-connector UI
  code).
- **Source detail** — config form, mapping rules editor, run history
  (paginated), the conflict review queue, and a "Sync now" button.
- **Item viewer** — per-item detail with the source path / external
  ID / mapped fields / asset link / outcome / error / review actions.

The review queue (`import_item.review_state = 'pending'`) is the
critical UI: every `conflict_policy: review` resolution lands here
with a side-by-side diff (source values vs local values) and
explicit Accept / Reject buttons that write to the audit log.

### Audit, metrics, federation

- Audit category `import.*` per ADR 0033 — `import.source.created`,
  `import.run.started`, `import.run.completed`, `import.item.created`,
  `import.item.conflict`, `import.item.reviewed`, etc.
- Metrics under `import_` prefix per Phase 1.41 — `import_runs_total`,
  `import_items_processed_total{outcome}`, `import_run_duration_seconds`,
  `import_bytes_transferred_total{mode}`, `import_errors_total{kind}`.
- Federation-aware: a future `aa-peer` connector kind reuses the
  exact same shape to seed a follower instance from a leader.

### Connector kinds and rollout

Phase **1.43.A — Framework**: connector contract, data model, scheduler,
admin surface, audit / metric hooks. No connectors yet — the framework
ships with one in-process test connector for integration testing only.

Phase **1.43.B — Foundation connectors** (FS / S3 / HTTP / git): four
connectors that share ~80% implementation (walk a tree, hash files,
emit `Item`s):

- **`fs`** — local mount (NFS / SMB / bind-mount). Direct RS staticsync
  replacement.
- **`s3`** — S3-compatible bucket (AWS, R2, B2, Wasabi, GCS via S3
  compat, MinIO). Configured endpoint + bucket + prefix.
- **`http`** — single-archive URL (zip / tar / OCI artifact). Fetches,
  extracts, walks. "Drop this folder once" workflows.
- **`git`** — clone or pull-on-schedule of a regular or LFS repo.
  Commits are runs; each commit boundary is a potential delta.

Subsequent phases ship one external service per phase because each
brings its own SDK, auth flow, rate-limit profile, and quirks:

- Phase **1.43.C — ShotGrid (Flow Production Tracking)**. Entity-link
  items (`Asset`, `Version`, `PublishedFile`) plus attached media.
  Event-log push.
- Phase **1.43.D — Perforce**. Wire protocol via `p4golang`; per-revision
  history in `import_item`.
- Phase **1.43.E — Microsoft 365 (OneDrive + SharePoint)**. Graph API,
  delegated or app permissions, Graph subscriptions for push.
- Phase **1.43.F — Google Drive**. Drive v3 API + change tokens +
  webhook channels for push.

Order of 1.43.C / D / E / F is demand-driven — whichever audience
arrives first wins the next slot.

## Consequences

### Positive

- One conceptual model covers filesystem, blob stores, version
  control, and SaaS sources. Operators learn `Source → Run → Item`
  once.
- UI-managed configuration replaces RS's `config.php`-only model —
  non-engineers can wire up new sources and see what they did.
- Per-item review queue makes conflicts visible and auditable rather
  than silently dropped — directly addresses the RS pain point.
- Storage mode (ingest vs reference) lets operators trade storage
  cost against source-availability coupling — preserves the strongest
  RS feature while making the choice per-source instead of per-install.
- Connector interface is small and round-trippable through a WASM
  plugin host — ADR 0034 plugin path stays open.
- `external_id` plus `source_id` plus the run/item ledger is a real
  provenance record. Federation reuses it; migration tools reuse it;
  audit reuses it.
- Push support where the source offers it cuts the "scan everything
  every hour" pattern for the sources that bleed money on it (Drive,
  Graph).

### Negative

- The framework itself is a phase of work before any connector is
  useful end-to-end. There is no "ship one connector and pretend it's
  generic" shortcut without painting ourselves into corners later.
- Each external service has its own SDK, auth flow, rate-limit
  profile, and idiosyncrasy cliff. ShotGrid + Graph + Drive each carry
  weeks of integration work that doesn't reduce to "implement the
  interface."
- Reference mode introduces a "asset bytes live somewhere else" path
  that touches the storage abstraction, the variants pipeline, every
  asset-fetch endpoint, and the federation story. The implementation
  is real even though the user-visible shape is small.
- Conflict resolution at scale is genuinely hard. The `review` policy
  adds admin UI surface that has to be good or operators will set
  every source to `source_wins` and reintroduce RS's silent-trample
  problem.
- Credentials at rest for many external services is a security
  surface area expansion. The encrypted credentials table is the only
  load-bearing piece, and it stays load-bearing for ADR 0021 and the
  cloud-bridge add-on path too — so the investment is shared, but the
  blast radius if it goes wrong is now larger.

## Alternatives considered

- **Port `staticsync` to Go as-is.** One filesystem source, CLI-only,
  no UI. Smallest possible shippable thing. Rejected because it does
  not address the multi-source ask, has no UI, no conflict
  resolution, and would have to be replaced wholesale once the second
  source kind arrives.

- **Per-source bespoke admin pages.** Skip the framework; ship
  "Connect ShotGrid", "Connect Drive", etc. as one-off admin pages
  each with their own data model. Faster v1 for any single source.
  Rejected because the matrix expands quadratically — every new
  source kind touches audit / scheduling / error handling / review
  again, and there is no shared mental model for operators.

- **Generic webhook receiver.** Expose `POST /webhook/import` and let
  every source POST a payload. Loose coupling. Rejected because
  (a) most sources don't push and we'd still need pull; (b) the
  source-side glue is bespoke per integration anyway; (c) we lose
  the central state ledger that makes the review queue and revive
  flow work.

- **External data-integration tool (Apache Camel, Airbyte,
  Singer.io).** Use an existing platform. Rejected: heavy
  operational dependency, conflicts with the single-binary deploy
  commitment (ADR 0006), and these tools are general-purpose — they
  do not know about Artist Alley assets, metadata fields, or
  conflict policies. We'd still write Artist Alley-side glue and
  carry the operator's burden of running another moving part.

- **Plugin-only — defer the entire subsystem to WASM/Extism
  plugins.** Rejected because the framework is the load-bearing part
  (data model, lifecycle, UI). Plugins are how *new* connector kinds
  arrive *after* the framework exists; they are not a substitute for
  the framework.

## Implementation

Phases land sequentially, each as its own milestone with an epic
issue. Sub-phase A is gating for everything that follows.

- **1.43.A — Framework**
  - `import_source` + `import_run` + `import_item` migrations.
  - Connector interface in `internal/imports` + in-process test
    connector for unit tests.
  - Scheduler + worker pool integrated into the existing job queue
    (Phase 1.15.A).
  - Per-source lock + per-connector rate limit infrastructure.
  - Credentials broker wiring against the encrypted credentials
    store.
  - `asset_field_value.user_locked` column + UI flagging on user
    edits.
  - Admin `/admin/imports` Source list + detail + run history pages.
  - Item review queue surface.
  - Audit (`import.*`) + metrics (`import_*`) emission.
  - OpenAPI surface: `GET/POST /admin/imports/sources`,
    `GET/POST /admin/imports/sources/{id}/run`,
    `GET /admin/imports/sources/{id}/runs`,
    `GET /admin/imports/items?source_id=&review_state=`,
    `POST /admin/imports/items/{id}/review`.

- **1.43.B — Foundation connectors (FS / S3 / HTTP / git)**
  - `fs` connector + integration test against a tmpfs fixture.
  - `s3` connector + integration test against MinIO in CI
    (`docker compose --profile storage-s3 up`).
  - `http` connector + integration test against a fixture zip.
  - `git` connector + integration test against a local bare repo.
  - Connector-specific admin form schemas.
  - End-to-end documentation: "import a directory", "import a
    bucket", "import a zip", "import a git repo".

- **1.43.C — ShotGrid connector**
  - REST API client (vetted SG SDK or focused in-tree client).
  - Entity → Item mapping (`Asset` / `Version` / `PublishedFile` +
    attachments).
  - Event-log push subscription where available.
  - Per-project scope configuration.
  - Docs + sample mapping rules for studio conventions.

- **1.43.D — Perforce connector**
  - P4 wire-protocol client (vetted in-tree implementation).
  - Depot-subset scope configuration.
  - Per-revision history captured in `import_item`.
  - Reference-mode-first (depots are usually huge; ingest mode is
    opt-in per source).

- **1.43.E — Microsoft 365 (OneDrive + SharePoint)**
  - Microsoft Graph API client.
  - Delegated + application permission flows.
  - Drive item → Item mapping; single connector covers OneDrive
    Personal, OneDrive for Business, and SharePoint document
    libraries (same Drive shape).
  - Graph change subscriptions for push.

- **1.43.F — Google Drive**
  - Drive v3 API client.
  - OAuth 2.0 with delegated permissions.
  - Change tokens for incremental sync.
  - Push channels (`watch`) for near-real-time updates.

## References

- ResourceSpace `staticsync` — `pages/tools/staticsync.php`,
  the resource-creation entry points in `include/resource_functions.php`,
  the `staticsync_*` config keys in `include/config.default.php`,
  the pre-processing pipeline in `include/image_processing.php`, and
  the test in `tests/test_list/001240_staticsync.php`. Studied as
  the closest prior art; lessons inherited (path-as-metadata, two
  storage modes, delete/revive lifecycle, per-source lock) and pain
  points fixed on purpose (single source, CLI-only, no review queue,
  silent overwrites, hard-coded importer identity).
- ADR 0008 — Storage architecture. Gains the `import-ref` backend
  for reference-mode reads.
- ADR 0011 — Asset entity. The target of every import.
- ADR 0012 — Metadata model. `set_by = 'import'` provenance + the
  new `user_locked` flag on field values.
- ADR 0021 — Platform integrations. Mirror direction (push from
  external systems); shares the credentials store.
- ADR 0033 — Observability. Reserves `import.*` audit and `import_*`
  metric prefixes.
- ADR 0034 — Capability add-ons. Sister-pattern for pluggable
  extension surfaces; the connector interface is shaped so a future
  WASM plugin host can implement it.
