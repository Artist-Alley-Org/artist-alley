---
id: "0008"
title: Storage architecture — content-addressed, pluggable backends
status: accepted
date: 2026-05-24
area: infrastructure
phases: 
  - "1.4"
  - "1.4.E"
supersedes: []
related: 
  - "0007"
  - "0075"
tags:
  - infrastructure
  - ai
  - 3d
excerpt: >-
  artist-alley needs to store and serve game-art binaries: still images, multi-gigabyte videos, 3D models, audio, document previews, and any derived renditions (thumbnails, HLS segments, decimated 3D LODs). The Phase 1.4 work depends on getting this layer right; every later feat…
---
## Context

artist-alley needs to store and serve game-art binaries: still images,
multi-gigabyte videos, 3D models, audio, document previews, and any
derived renditions (thumbnails, HLS segments, decimated 3D LODs).
The Phase 1.4 work depends on getting this layer right; every later
feature — upload UX, browse, video review, archive tiering,
federation — sits on top.

A typical DAM stores files at scrambled paths inside a filestore,
keyed by sequential resource IDs and encoding the variant size in
the filename. That layout has real problems for our goals:

- Sequential numeric refs collide across federated peers (ADR 0007).
- Filename-encoded variants make adding new sizes invasive.
- Same bytes uploaded twice = two copies on disk (no dedup).
- Path scrambling is security-through-obscurity, not real access
  control.
- The single-filesystem assumption couples bytes and code; moving to
  S3 requires migrating both.
- Video (HLS segments), 3D (multi-file formats), and large originals
  fit poorly into one file per resource.

We need a layer that:

1. Survives federation (ADR 0007) — bytes are universally
   identifiable across peers.
2. Lets the same install run on local FS, S3-compatible object stores
   (MinIO, AWS, R2, Backblaze B2), and future backends like GCS
   without code changes outside the backend package.
3. Dedups bytes when the same content is uploaded multiple times.
4. Models variants/renditions as first-class so HLS segments, 3D
   LODs, and per-size image previews don't pollute filenames.
5. Supports range reads (video scrub) and signed-URL handoffs
   (offload bandwidth from the app server).
6. Supports resumable uploads (multi-gigabyte uploads must survive
   flaky connections).

## Decision

Three pillars: **content addressing**, a **pluggable Go interface**
to swap backends, and a **three-table relational index** that
separates bytes from variants from references.

### 1. Content addressing

Every blob is keyed by the **lowercase hex sha256 of its contents**.
The hash IS the storage key on every backend:

```
ab/cd/<full hex sha256>/original
ab/cd/<full hex sha256>/preview_2048.webp
ab/cd/<full hex sha256>/thumb_512.webp
ab/cd/<full hex sha256>/hls/index.m3u8
ab/cd/<full hex sha256>/hls/seg00001.ts
```

The two-level `ab/cd/` prefix (first two and next two hex chars)
avoids huge flat directories on filesystem backends. S3 backends use
the same layout for consistency, even though S3 doesn't need it for
performance.

**What this unlocks:**

- **Dedup**. Same texture twice = one set of bytes, two pin rows.
- **Federation integrity**. Peers can negotiate transfers by hash and
  verify any bytes they receive. This is the federation primitive.
- **Forever-cacheable** — **for the ORIGINAL bytes only.** The hash never
  changes, so `original` under a given hash is genuinely immutable and
  CDN/HTTP caches may treat it as such.
  **Amended 2026-07-26 (#620, PR #623):** as written, this bullet read as
  licensing immutable caching for *any* URL under a stable hash, and the
  codebase took it that way — `middleware/variant_cache.go` shipped
  `Cache-Control: immutable, max-age=31536000` on **asset-addressed**
  routes (`/api/v1/assets/{id}/variants/{key}`) with an ETag derived from
  the URL path, and 304'd without consulting the stored bytes. The result
  was not staleness but permanence: no sequence of requests could return
  updated content.
  **The distinction the bullet was missing:** a *derived variant* under a
  stable hash is exactly what preview regeneration rewrites
  (`RecreateAssetPreview`), and `file_hash` digests the original, so it
  cannot witness that change. Immutability is a property of the original
  bytes, not of everything addressed beneath their hash. Asset-addressed
  byte routes now carry a content-derived validator
  (`sha256(object_hash | variant_key | size | modtime)`) and
  `public, max-age=60, must-revalidate`.
- **Variant ergonomics**. Adding HLS segments to an existing original
  is just new keys under the same hash, no rename.

### 2. Pluggable storage backend

Single Go interface, multiple implementations, swapped via env var.

```go
// app/internal/storage/storage.go
type Backend interface {
    Put(ctx context.Context, hash, variant string, r io.Reader) (*ObjectInfo, error)
    Get(ctx context.Context, hash, variant string) (io.ReadCloser, *ObjectInfo, error)
    GetRange(ctx context.Context, hash, variant string, offset, length int64) (io.ReadCloser, error)
    Delete(ctx context.Context, hash, variant string) error
    PresignGet(ctx context.Context, hash, variant string, ttl time.Duration) (string, error)
    PresignPut(ctx context.Context, hash, variant string, ttl time.Duration) (string, error)
    Stat(ctx context.Context, hash, variant string) (*ObjectInfo, error)
}

type ObjectInfo struct {
    Size        int64
    ContentType string
    ETag        string
    ModifiedAt  time.Time
}
```

Implementations:

- `internal/storage/fs` — local filesystem. Dev default and the
  appropriate backend for single-server installs that don't need
  cloud storage.
- `internal/storage/s3` — any S3-API target (AWS, MinIO, R2,
  Backblaze, Wasabi). Built on the AWS SDK Go v2.
- (later) `internal/storage/gcs` — Google Cloud Storage.
- (later) `internal/storage/azblob` — Azure Blob.

Selected at boot via `AA_STORAGE_BACKEND=fs|s3|...`. Handler code
never references concrete backend types — only the `Backend`
interface.

**Federation benefit:** every server picks its own backend
independently. one game team on S3, another on local FS, Archive on
Glacier — all interoperable because content hashes are universal.

### 3. Object / variant / pin schema

Three tables, each with one job:

```sql
-- One row per UNIQUE blob (deduped on hash).
CREATE TABLE storage_objects (
    hash             TEXT PRIMARY KEY,   -- lowercase hex sha256
    size_bytes       BIGINT NOT NULL,
    content_type     TEXT NOT NULL,
    backend          TEXT NOT NULL,      -- 'fs' / 's3' / 'gcs' (the backend currently holding the bytes)
    backend_bucket   TEXT NULL,
    origin_server_id UUID NULL,          -- federation per ADR 0007; NULL = this server
    gc_eligible_at   TIMESTAMPTZ NULL,   -- set when last pin removed; sweeper deletes after this
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Variants/renditions of an object (preview sizes, HLS segments, 3D LODs).
CREATE TABLE storage_variants (
    object_hash  TEXT NOT NULL REFERENCES storage_objects(hash) ON DELETE CASCADE,
    variant_key  TEXT NOT NULL,           -- 'original' | 'preview_2048' | 'hls/seg00001.ts'
    size_bytes   BIGINT NOT NULL,
    content_type TEXT NOT NULL,
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,  -- duration_ms, dims, codec, bitrate, ...
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (object_hash, variant_key)
);

-- Reference counts. Many resources can pin the same hash (this is
-- where dedup pays off). Bytes stay alive while any pin exists.
CREATE TABLE storage_pins (
    object_hash      TEXT NOT NULL REFERENCES storage_objects(hash) ON DELETE RESTRICT,
    pin_subject_type TEXT NOT NULL,       -- 'resource' | 'avatar' | 'thumbnail' | ...
    pin_subject_id   TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (object_hash, pin_subject_type, pin_subject_id)
);
```

The split is deliberate. `storage_objects` is the bytes. `storage_pins`
is who's keeping them alive. `storage_variants` is what derived forms
have been generated. The user-facing `resource` row is yet another
layer above — it owns the title, tags, permissions, comments. One
resource maps to one or more pins.

**Amended 2026-07-30 (#760):** `storage_variants` carries `updated_at`
alongside `created_at` (migration 00019). The sketch above has only
`created_at`, and the upsert that runs on every render never touched it
after the first write — so the table recorded when a derived form first
appeared and could not say whether it had since been re-derived. That is
the wrong shape for a row whose whole subject is mutable: the same
amendment that #620 forced on the caching bullet applies here. A variant
under a stable object hash is *expected* to be rewritten when the
renderer improves, and a table that cannot witness the rewrite makes
"did the rebuild work?" unanswerable — which is exactly how a preview
control that quietly did nothing survived three releases. `created_at`
keeps its original meaning; `updated_at` moves on every write.

### Lifecycle / dedup semantics

Bytes are **reference-counted, not user-owned**. Walking through:

1. Alice uploads `photo.jpg` (sha256 = `abc123`).
   - `storage_objects(hash='abc123', bytes=…)` created.
   - Alice's `resource:1` created with Alice's metadata.
   - `storage_pins(object='abc123', subject='resource:1')` created.

2. Bob uploads the same `photo.jpg` (sha256 = `abc123`).
   - `storage_objects` NOT recreated (bytes already on disk).
   - Bob's `resource:2` created with **Bob's own** metadata, perms,
     comments.
   - `storage_pins(object='abc123', subject='resource:2')` created.

3. Alice deletes her resource.
   - `resource:1` and its pin are removed.
   - Bob's pin still exists → bytes stay → Bob's resource still works.

4. Bob deletes his resource.
   - Last pin gone → `storage_objects.gc_eligible_at = NOW() + 24h`.
   - A background sweeper deletes the row + backend bytes after the
     grace period.

**Permission checks happen at the resource layer**, never at the
storage layer. The download endpoint authorises against the user's
resource; the backend just sees "give me hash X for caller with token
Y" and either serves bytes or hands back a presigned URL.

**Privacy edge case (deferred):** dedup is observable as a side
channel — a fast 200 with no bytes transferred reveals the server
already had that content. Mitigated for sensitive deployments by
hashing with a per-tenant salt. Not a Phase 1.4 concern; flagged so
we don't forget when multi-tenancy lands.

### Upload flow

1. Client begins a TUS-resumable upload.
2. Server buffers chunks to a temp file and feeds the same bytes
   into `sha256.New()`.
3. On the final chunk, finalise the hash.
4. Check `storage_objects` for that hash.
   - **Hit** → skip backend `Put`; just insert the new pin row.
   - **Miss** → stream temp file to `Backend.Put(hash, "original", …)`;
     insert `storage_objects`; insert pin.
5. Schedule variant-generation jobs (thumbnail, preview, HLS, …).

### Asset-type playbook

| Asset | Object + variants |
|---|---|
| Image (JPG/PNG/etc) | original + preview_2048 + thumb_512 |
| Video | original + HLS index/segments at multiple bitrates + thumbnail frames |
| 3D (glTF/GLB) | original + decimated LOD + thumbnail render. Multi-file 3D (FBX with external textures) bundled as `.zip` at the object level. |
| Annotations / comments | NOT stored as objects. Live as structured rows in Postgres; they reference resources, not bytes. |

### Range reads, presigned URLs, large transfers

- Range support comes from `Backend.GetRange`. HLS scrubbing, partial
  3D loads, and resumable downloads all use it.
- For local FS backends, the handler can `X-Accel-Redirect` to nginx
  so the Go process doesn't proxy the bytes.
- For S3, presigned GET URLs let clients fetch directly from the
  bucket, offloading bandwidth.
- For uploads, presigned PUT URLs (S3 only) allow direct browser-to-S3
  uploads with a checksum check on the server side once the upload
  completes.

## Consequences

**Positive**

- One storage model for every asset type and every backend.
- Dedup at scale — game-art uploads have heavy texture duplication.
- Federation primitive in place: content hashes are universal.
- Variants are first-class; HLS, 3D LODs, future formats fit cleanly.
- Bytes / pins / metadata cleanly separated — each layer has one job.

**Negative**

- Extra metadata in Postgres per upload (object + variant + pin rows).
  Modest cost, easily within Postgres scale.
- GC complexity: pin-counted bytes plus a grace period plus a sweeper.
  Worth it for the safety it gives accidental-delete users.
- Initial migration of any existing filestore is one-shot work
  (Phase 1.4.E) — must hash every existing file.

**Mitigations**

- The 24h GC grace period catches accidental deletes (matches S3
  lifecycle behaviour).
- The sweeper is idempotent; safe to re-run.
- The legacy importer can leave symlinks for PHP compatibility so the
  legacy code continues to find files at the old paths during the
  transition.

## Implementation phasing

| Sub-phase | What ships |
|---|---|
| **1.4.A** | Schema (`storage_objects`, `storage_variants`, `storage_pins`) via goose migration, `Backend` Go interface, `fs` implementation, unit tests. No new HTTP endpoints. |
| **1.4.B** | `s3` implementation using AWS SDK Go v2; tests against MinIO container in docker-compose. |
| **1.4.C** | `POST /api/v1/assets` (TUS-resumable) and `GET /api/v1/assets/{hash}/{variant}` (range-supporting). |
| **1.4.D** | Variant generator: goroutine pool reading from a `storage_variant_jobs` table. First kind: image thumbnails using libvips or pure-Go alternatives. |
| **1.4.E** | Legacy filestore importer: one-shot Go binary that walks `filestore/`, computes sha256 per file, populates the new tables, and optionally writes symlinks at the old paths so the legacy PHP keeps working. |

**MVP for Phase 1.4** = A + C. B (S3) and the importer can land in
follow-up commits without blocking the frontend.

## Federation prep applied (per ADR 0007)

- `storage_objects.origin_server_id UUID NULL` — peer-originated
  objects can be marked when replicated in.
- Hashes are global by definition; federation peers can request and
  verify any byte stream by hash.
- Pins are per-server (each install keeps its own pin set);
  replication boils down to "give me hash X" + "pin X locally to my
  resource Y."

## Alternatives considered

- **Stay on a scrambled-filename layout, modernise behind the
  scenes.** Rejected: keeps the dedup blocker, the federation
  blocker, and the variant-management blocker.
- **One file per resource at a stable path (e.g., `assets/<ref>/...`).**
  Rejected: still no dedup, still no federation primitive, breaks
  the moment we move to S3.
- **Hash-but-no-pins (delete bytes when the resource that uploaded
  them is deleted).** Rejected: defeats dedup — Alice uploading and
  deleting deletes Bob's reference.
- **Hash-everywhere INCLUDING per-resource metadata (Merkle-DAG-style
  IPFS).** Rejected as over-engineered for our scale. The relational
  index is more pragmatic and federation can layer hashes on top later
  if we ever need IPFS-style content distribution.

## Open questions (defer)

1. **Multi-tenancy per-tenant hash salt.** When and if hosted
   multi-tenancy lands, dedup observability becomes a side channel.
   Adding a per-tenant salt to the hash breaks dedup across tenants
   but plugs the leak.
2. **Cold-tier integration.** Eventually we want a "demote to
   Glacier" policy for assets that haven't been read in N months.
   The `backend` column in `storage_objects` is the seam — a sweeper
   can re-Put bytes to a cold backend and update the row.
3. **Variant lifecycle.** Sometimes variants want to be lazy-deleted
   (HLS bitrates the install no longer serves). Tracking
   `last_accessed_at` on variants is the data needed; deferred until
   we have real usage patterns.
