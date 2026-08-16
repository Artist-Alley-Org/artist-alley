-- name: CreateAsset :one
INSERT INTO assets (
    title, description, asset_type, owner_user_ref, status,
    file_hash, file_extension, file_size_bytes, metadata, origin_server_id,
    state_id, processing_status, thumbhash, team_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
    $11, $12, $13, $14
)
RETURNING id, title, description, asset_type, owner_user_ref, status,
          file_hash, file_extension, file_size_bytes, metadata,
          origin_server_id, state_id, processing_status, thumbhash,
          created_at, updated_at, team_id;

-- name: GetAsset :one
-- Pixel dimensions are deliberately NOT selected here (#640). sqlc types
-- a scalar subquery as NOT NULL — it has no way to infer otherwise — so
-- an asset with no recorded dimensions would scan NULL into an int32 and
-- fail at runtime on the detail endpoint. The projection therefore lives
-- in pixeldims.SelectColumnsSQL, spliced into the hand-built queries that
-- need it, and the detail path fetches it alongside its variant probe.
SELECT id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at, team_id
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
          created_at, updated_at, team_id;

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
-- deleted_by_user_ref records WHO removed the row, because #931's
-- restore rule turns on it: you may undo your own delete, and an
-- admin's delete is undone by that admin or by system.admin. NULL is
-- the honest answer for a system-scheduled retention delete, and it
-- fails closed — nobody self-restores a row whose deleter is unknown.
UPDATE assets
SET deleted_at = NOW(), deleted_reason = $2, deleted_by_user_ref = $3, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetAssetMutationSubject :one
-- The authorisation probe behind UpdateAsset / DeleteAsset. Deliberately
-- NOT GetAsset: the mutation gate needs a nullable `owner_user_ref`
-- alongside `team_id` (for the team-scoped `assets.admin` disjunct), and
-- it must answer for a row the caller may not be entitled to read at
-- all — a gate that borrowed the read projection would be one edit away
-- from inheriting the read rule's filters. (`team_id` is now on the read
-- projection too, since #953 made it settable and therefore something a
-- client has to be able to observe; that does not merge the two.)
-- `status` comes along because the publication boundary
-- in UpdateAsset compares against the current value, and `updated_at`
-- because the optimistic-concurrency check needs the same row — one
-- read, so the gate and the conflict check can never disagree about
-- which version of the row they looked at.
SELECT owner_user_ref, team_id, status, updated_at
  FROM assets
 WHERE id = $1 AND deleted_at IS NULL;

-- name: GetAssetDeletedBy :one
-- Who soft-deleted this asset. Errors with pgx.ErrNoRows when the row
-- is live or absent, which is the same answer the restore path gives
-- those two cases anyway.
SELECT deleted_by_user_ref
  FROM assets
 WHERE id = $1 AND deleted_at IS NOT NULL;

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
--
-- #902 deliberately did NOT gate this `q` clause on readability, and it
-- is the one asset text match left ungated. It is not reachable: this
-- query has NO caller and NO production caller — it exists only as the
-- oracle TestListAssetsPage_AuthenticatedParity compares the hand-built
-- browse query against, and a static sqlc statement cannot splice a
-- runtime visibility fragment (which is why the browse query stopped
-- being sqlc in the first place, see list_page.go). Gating it would also
-- destroy its value as an oracle, because an oracle that applies the
-- rule under test cannot detect the rule being wrong. Do NOT promote
-- this to handler code: it has no visibility rule of any kind.
SELECT id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at, deleted_at, deleted_reason, team_id
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

-- ListAssetsByTagPage was DELETED by #657. It was the by-tag half of
-- the browse: a second static query whose whole WHERE clause was
-- `a.deleted_at IS NULL`, so `?tag=` served draft, archived and
-- restricted rows to anonymous callers that plain `/assets` correctly
-- withheld — and reported preview_available/ladder_available as false
-- for every row (#612). The tag filter is now one more optional
-- conjunct on ListAssetsPageGated (assets/list_page.go), where the
-- visibility predicate already lives. Do not reintroduce it: a filter
-- with its own query is a filter with its own rules.

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

-- name: SetAssetThumbhash :exec
-- Overwrites unconditionally. The ONLY caller is a forced re-render
-- (#760), where the operator has said the stored preview is wrong: the
-- thumbhash is a 30-byte blur of those same wrong pixels, and leaving it
-- makes a corrected card fade up from the stale image it just replaced.
-- Every other writer must keep using SetAssetThumbhashIfMissing, whose
-- NULL guard is what stops the worker racing CreateAsset's synchronous
-- compute and what makes the backfill safe to re-run.
UPDATE assets
   SET thumbhash  = $2,
       updated_at = NOW()
 WHERE id = $1;

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
    -- file_extension, not has_image (#579). has_image is DEFAULT false
    -- with no writer, so the MimeType hint it fed was never set for any
    -- asset and the AI handler never learned an asset was an image. The
    -- extension is real data and yields a real MIME rather than a
    -- wildcard.
    a.file_extension,
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

-- ListCardFieldValues MOVED to internal/metadata/queries.sql (#1133).
-- The card-field projection is not an assets-package concern: the
-- collection member grid renders the same tiles from the same flag, and
-- `collections` cannot import this package (assets -> posts ->
-- collections is a cycle). It lives beside metadata.DisplayValue, which
-- is the rule it feeds, and both surfaces call metadata.CardFieldsForAssets.

-- name: ListAssetOrigins :many
-- Which peer an asset came from, for the card's provenance affordance
-- (#552). Local rows (origin_server_id IS NULL) are simply absent from the
-- result, so "no row" reads as "ours" without a sentinel.
--
-- Batched with the page for the same reason ListCardFieldValues is. The
-- join is to federation_peers.display_name, the name an operator gave the
-- peer at handshake — never the raw UUID, which answers "whose is this?"
-- with a number.
SELECT a.id AS asset_id, p.id AS peer_id, p.display_name, p.instance_url
  FROM assets a
  JOIN federation_peers p ON p.id = a.origin_server_id
 WHERE a.id = ANY(sqlc.arg('asset_ids')::UUID[])
   AND a.deleted_at IS NULL;
