-- name: CreateAsset :one
INSERT INTO assets (
    title, description, asset_type, owner_user_ref, status,
    file_hash, file_extension, file_size_bytes, metadata, origin_server_id,
    state_id, processing_status, thumbhash
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
    $11, $12, $13
)
RETURNING id, title, description, asset_type, owner_user_ref, status,
          file_hash, file_extension, file_size_bytes, metadata,
          origin_server_id, state_id, processing_status, thumbhash,
          created_at, updated_at;

-- name: GetAsset :one
SELECT id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at
FROM assets
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetAssetByOwnerHash :one
-- Phase 1.18.A-2 follow-up A — per-user dedup lookup. Returns the
-- live asset row matching (owner_user_ref, file_hash) when present;
-- pgx.ErrNoRows otherwise. The partial unique index from migration
-- 00016 guarantees at most one row matches. Caller uses this both
-- (a) pre-insert to short-circuit duplicate uploads and (b) post-
-- insert to recover the existing asset id when a concurrent
-- upload won the unique-constraint race.
SELECT id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at
FROM assets
WHERE owner_user_ref = $1 AND file_hash = $2 AND deleted_at IS NULL;

-- name: UpdateAsset :one
-- Partial update via COALESCE: any field passed as NULL keeps its
-- current value. Tag changes go through a separate set of queries.
UPDATE assets SET
    title       = COALESCE(sqlc.narg('title'),       title),
    description = COALESCE(sqlc.narg('description'), description),
    status      = COALESCE(sqlc.narg('status'),      status),
    metadata    = COALESCE(sqlc.narg('metadata'),    metadata),
    updated_at  = NOW()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING id, title, description, asset_type, owner_user_ref, status,
          file_hash, file_extension, file_size_bytes, metadata,
          origin_server_id, state_id, processing_status, thumbhash,
          created_at, updated_at;

-- name: MergeAssetMetadata :exec
-- Shallow-merge an incoming JSONB blob into the existing metadata
-- column. Preview workers (audio / video / 3D) use this to stamp
-- their own namespace ({"audio": {...}}, {"video": {...}}) without
-- stomping previously-written keys.
UPDATE assets
SET metadata   = COALESCE(metadata, '{}'::jsonb) || sqlc.arg('metadata')::jsonb,
    updated_at = NOW()
WHERE id = sqlc.arg('id') AND deleted_at IS NULL;

-- name: SoftDeleteAsset :exec
UPDATE assets
SET deleted_at = NOW(), deleted_reason = $2, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListAssetsPage :many
-- NOT THE ENFORCEMENT PATH. This query applies no visibility predicate,
-- and nothing in production calls it: asset browse goes through
-- ListAssetsPageGated (assets/list_page.go), which splices
-- visibility.Predicate — sqlc's static SQL cannot take a runtime
-- fragment (#429). It is retained because its generated row shape stays
-- in sync with the schema, and because the parity test uses it as the
-- oracle proving the hand-built query returns byte-identical rows for
-- authenticated callers. Do not call it from handler code.
-- Cursor pagination: rows newer than the cursor timestamp, plus tie-
-- breaker on id. Filters are OR'd with NULL-checks so a single query
-- covers all the optional-filter combinations.
--
-- `q` is an optional plain-text search query that runs against the
-- search_text TSVECTOR column (maintained by the Phase 1.9 metadata
-- trigger across asset title/description/tags + field values). Backed
-- by the assets_search_text_gin index. Phase 1.12 will replace this
-- with the proper search DSL (ADR 0010), but for the browse page MVP
-- a plain tsquery match is enough.
SELECT id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at, deleted_at, deleted_reason
FROM assets
WHERE (sqlc.narg('include_deleted')::BOOLEAN IS TRUE OR deleted_at IS NULL)
  AND (sqlc.narg('owner_user_ref')::BIGINT IS NULL OR owner_user_ref = sqlc.narg('owner_user_ref')::BIGINT)
  AND (sqlc.narg('asset_type')::BIGINT  IS NULL OR asset_type  = sqlc.narg('asset_type')::BIGINT)
  AND (sqlc.narg('status')::TEXT           IS NULL OR status          = sqlc.narg('status')::TEXT)
  AND (sqlc.narg('q')::TEXT                IS NULL
       OR search_text @@ plainto_tsquery('english', sqlc.narg('q')::TEXT))
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND id < sqlc.narg('cursor_id')::UUID))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: ListAssetsByTagPage :many
-- Same paginated list but constrained to a single tag. Separate
-- query because the join breaks the COALESCE pattern.
SELECT a.id, a.title, a.description, a.asset_type, a.owner_user_ref, a.status,
       a.file_hash, a.file_extension, a.file_size_bytes, a.metadata,
       a.origin_server_id, a.state_id, a.processing_status, a.thumbhash,
       a.created_at, a.updated_at
FROM assets a
JOIN asset_tag t ON t.asset_id = a.id
WHERE a.deleted_at IS NULL
  AND t.tag = sqlc.arg('tag')::TEXT
  AND (sqlc.narg('owner_user_ref')::BIGINT IS NULL OR a.owner_user_ref = sqlc.narg('owner_user_ref')::BIGINT)
  AND (sqlc.narg('asset_type')::BIGINT  IS NULL OR a.asset_type  = sqlc.narg('asset_type')::BIGINT)
  AND (sqlc.narg('status')::TEXT           IS NULL OR a.status          = sqlc.narg('status')::TEXT)
  AND (sqlc.narg('cursor_created_at')::TIMESTAMPTZ IS NULL
       OR a.created_at < sqlc.narg('cursor_created_at')::TIMESTAMPTZ
       OR (a.created_at = sqlc.narg('cursor_created_at')::TIMESTAMPTZ
           AND a.id < sqlc.narg('cursor_id')::UUID))
ORDER BY a.created_at DESC, a.id DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: ListAssetTags :many
SELECT tag FROM asset_tag WHERE asset_id = $1 ORDER BY tag;

-- name: ListAssetTagsDetailed :many
-- Phase 1.14.B — typed read returning per-tag source/confidence/
-- provenance. Backs the openapi.Asset.tag_details field that the
-- frontend AssetTagBadge consumes. Same ordering as ListAssetTags
-- so the legacy flat `tags` array stays index-aligned with this
-- list (consumers can zip them when needed).
SELECT
    tag,
    source,
    confidence,
    created_by_provider,
    created_by_model
FROM asset_tag
WHERE asset_id = $1
ORDER BY tag;

-- name: AddAssetTag :exec
INSERT INTO asset_tag (asset_id, tag)
VALUES ($1, $2)
ON CONFLICT (asset_id, tag) DO NOTHING;

-- name: RemoveAssetTag :exec
DELETE FROM asset_tag WHERE asset_id = $1 AND tag = $2;

-- name: ReplaceAssetTags :exec
-- Wipes and refills the tag set in one transaction. Called by
-- AssetUpdate when the request body sends a `tags` array.
WITH wipe AS (
    DELETE FROM asset_tag WHERE asset_id = $1
)
INSERT INTO asset_tag (asset_id, tag)
SELECT $1, unnest($2::TEXT[])
ON CONFLICT (asset_id, tag) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Preview-pipeline state transitions (Phase 1.18.A)
-- ---------------------------------------------------------------------------

-- name: MarkAssetProcessing :exec
-- Worker claimed a preview job for this asset. Best-effort — failing
-- here just means the admin queue view shows the asset as pending a
-- moment longer.
UPDATE assets
   SET processing_status     = 'processing',
       processing_started_at = COALESCE(processing_started_at, NOW()),
       processing_attempts   = processing_attempts + 1,
       updated_at            = NOW()
 WHERE id = $1
   AND processing_status <> 'ready';

-- name: MarkAssetReady :exec
-- All configured variants are written; flip to steady state.
UPDATE assets
   SET processing_status      = 'ready',
       processing_finished_at = NOW(),
       processing_error       = NULL,
       updated_at             = NOW()
 WHERE id = $1;

-- name: MarkAssetFailed :exec
-- Variant generation hit a terminal error. The admin UI surfaces
-- processing_error and offers a retry that re-enqueues.
UPDATE assets
   SET processing_status      = 'failed',
       processing_error       = sqlc.arg('processing_error')::TEXT,
       processing_finished_at = NOW(),
       updated_at             = NOW()
 WHERE id = $1;

-- name: ListAssetsForBackfill :many
-- Backfill helper. Returns asset rows whose extension is in the given
-- allowlist and whose processing_status is anything other than the
-- in-flight states. The admin / CLI uses this to enqueue jobs for
-- existing data.
SELECT id, file_hash, file_extension
  FROM assets
 WHERE deleted_at IS NULL
   AND file_hash  IS NOT NULL
   AND file_extension IS NOT NULL
   AND lower(file_extension) = ANY(sqlc.arg('extensions')::TEXT[])
 ORDER BY COALESCE(file_size_bytes, 0) ASC, id ASC
 LIMIT sqlc.arg('row_limit')::INTEGER;

-- name: SetAssetThumbhashIfMissing :exec
-- Only writes when the column is NULL — never overwrites an existing
-- thumbhash so the preview worker can backfill cheaply without
-- racing the synchronous compute in CreateAsset.
UPDATE assets
   SET thumbhash  = $2,
       updated_at = NOW()
 WHERE id = $1
   AND thumbhash IS NULL;

-- name: SetAssetPageCount :exec
-- Idempotent page-count stamp from the metadata pipeline (PDF today;
-- comics + ebooks later). Always overwrites — re-extraction on the
-- same asset should converge to the current truth, not preserve a
-- stale value from an older extractor. Does not touch updated_at
-- because page_count is asset-intrinsic, not an editorial change.
UPDATE assets
   SET page_count = $2
 WHERE id = $1;

-- name: AddAssetCompanion :one
-- Attach a companion blob to an asset under a given relative path.
-- Companion bytes live in storage_objects (deduped by hash); this
-- row is metadata that maps "this asset, this path → that blob".
-- Unique (asset_id, companion_path) — re-uploading the same path
-- replaces the row at the handler level (delete + insert under one
-- transaction).
INSERT INTO asset_companions (
    asset_id, companion_path, object_hash, content_type, size_bytes
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING id, asset_id, companion_path, object_hash,
          content_type, size_bytes, created_at;

-- name: ListAssetCompanions :many
-- All companions attached to one asset, ordered by path so the
-- viewer's LoadingManager can iterate deterministically.
SELECT id, asset_id, companion_path, object_hash,
       content_type, size_bytes, created_at
  FROM asset_companions
 WHERE asset_id = $1
 ORDER BY companion_path ASC;

-- name: GetAssetCompanionByPath :one
-- Resolve one companion by its declared relative path. Used by the
-- GET /assets/{id}/companions/{path} download endpoint.
SELECT id, asset_id, companion_path, object_hash,
       content_type, size_bytes, created_at
  FROM asset_companions
 WHERE asset_id = $1 AND companion_path = $2;

-- name: GetAssetCompanion :one
-- Resolve a companion by its row id — used by the delete handler so
-- the asset id can be cross-checked against the URL and the pin can
-- be tied back to the companion id.
SELECT id, asset_id, companion_path, object_hash,
       content_type, size_bytes, created_at
  FROM asset_companions
 WHERE id = $1;

-- name: DeleteAssetCompanion :exec
-- Remove a companion by id. Caller is responsible for unpinning the
-- storage object so the GC can claim the bytes back when no other
-- pin remains.
DELETE FROM asset_companions
 WHERE id = $1;

-- ─── Asset alternates (Phase 9 + paint-track Phase 13+) ─────────
-- Variants of the asset itself (palette swap, transcode, thumbnail,
-- authored). Mirrors the companions shape but adds a label, a kind
-- tag, and free-form per-kind JSONB metadata.

-- name: AddAssetAlternate :one
INSERT INTO asset_alternates (
    asset_id, label, kind, object_hash, content_type, size_bytes,
    origin_server_id, created_by_user_ref, metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING id, asset_id, label, kind, object_hash, content_type,
          size_bytes, origin_server_id, created_by_user_ref, created_at,
          metadata;

-- name: ListAssetAlternates :many
-- All alternates for one asset, ordered by created_at desc so the
-- most recent variant lands at the top of the UI list.
SELECT id, asset_id, label, kind, object_hash, content_type,
       size_bytes, origin_server_id, created_by_user_ref, created_at, metadata
  FROM asset_alternates
 WHERE asset_id = $1
 ORDER BY created_at DESC;

-- name: GetAssetAlternateByLabel :one
-- Resolve one alternate by label — used by the upload path to
-- replace an existing variant with the same label.
SELECT id, asset_id, label, kind, object_hash, content_type,
       size_bytes, origin_server_id, created_by_user_ref, created_at, metadata
  FROM asset_alternates
 WHERE asset_id = $1 AND label = $2;

-- name: GetAssetAlternate :one
SELECT id, asset_id, label, kind, object_hash, content_type,
       size_bytes, origin_server_id, created_by_user_ref, created_at, metadata
  FROM asset_alternates
 WHERE id = $1;

-- name: DeleteAssetAlternate :exec
-- Remove an alternate by id. Caller unpins the underlying blob.
DELETE FROM asset_alternates
 WHERE id = $1;

-- name: GetAssetSensitivity :one
-- Phase 1.22.I-i — single-column probe used by the federation
-- inbox dispatcher's receiver-side encryption policy gate
-- (SensitivityLookup callback). Returns the intrinsic
-- sensitivity tier; pgx.ErrNoRows when the asset doesn't exist
-- locally (gate treats as pass-through).
SELECT sensitivity
  FROM assets
 WHERE id = $1;

-- name: SetAssetSensitivity :exec
-- Phase 1.22.I-i — admin / owner action. Does NOT retroactively
-- affect outstanding federation_shares (those resolve sensitivity
-- at dispatch time via GetAssetSensitivity, so the change DOES
-- propagate to in-flight emissions — see 00014 migration's
-- design comment for the chosen tradeoff vs. copy-at-grant).
UPDATE assets
   SET sensitivity = $2,
       updated_at = NOW()
 WHERE id = $1;

-- ---------------------------------------------------------------------------
-- Phase 1.14.A-bridge — AI bridge queries.
--
-- AssetLookup + TagWriter implementations on assets.Handler consume
-- these. Mirrored on the file_hash + thumbhash columns since the
-- assets schema doesn't carry separate primary_image / primary_audio
-- references (that was an audit-first finding vs the brief's
-- assumption).
-- ---------------------------------------------------------------------------

-- name: GetAssetForAIBridge :one
-- Read-side projection for the ai.AssetLookup bridge. Returns the
-- minimal column set the AI handlers need; the asset row stays
-- untouched.
--
-- existing_tags is the JSON-aggregated list of (tag, source) pairs
-- so the AI handler can pass operator-set tags as prompt context
-- (don't re-suggest those) without a second round-trip.
SELECT
    a.id,
    a.asset_type,
    a.sensitivity,
    a.team_id,
    a.title,
    a.file_hash,
    a.has_image,
    COALESCE(
        (SELECT json_agg(json_build_object('tag', t.tag, 'source', t.source))
           FROM asset_tag t
          WHERE t.asset_id = a.id),
        '[]'::json
    )::TEXT AS existing_tags_json
FROM assets a
WHERE a.id = $1 AND a.deleted_at IS NULL;

-- name: DeleteAITagsForAsset :exec
-- Idempotent: removes every AI-source tag for one asset. Called by
-- SetAITagsForAsset inside the same tx as the fresh inserts so the
-- merge is atomic.
DELETE FROM asset_tag
 WHERE asset_id = $1 AND source = 'ai';

-- name: InsertAITagForAsset :exec
-- Per-tag insert (one row per AI-generated tag). SetAITagsForAsset
-- loops over the AI output + inserts each. Single-row inserts are
-- fine — typical AI returns < 30 tags per asset.
INSERT INTO asset_tag
    (asset_id, tag, source, confidence, created_by_provider, created_by_model)
VALUES ($1, $2, 'ai', sqlc.narg('confidence')::REAL, sqlc.narg('provider')::TEXT, sqlc.narg('model')::TEXT);

-- name: AssetExistsForAI :one
-- Cheap existence probe for the bridge. Returns true when the
-- asset exists + isn't soft-deleted.
SELECT EXISTS (
    SELECT 1 FROM assets WHERE id = $1 AND deleted_at IS NULL
)::BOOLEAN AS exists;

-- Phase 1.14.B embedding queries live under
-- app/internal/ai/embeddings/queries.sql so the writer + reader code
-- can package alongside them. assets package keeps focus on asset
-- CRUD + the bridge read/write surface for AI tags.
