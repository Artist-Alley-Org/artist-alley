-- GitHub #341 — admin-curated featured_items queries.
--
-- Naming convention: InsertX / ListX / DeleteX / UpdateX mirrors the
-- existing requests + users packages.

-- name: ListFeaturedItems :many
-- Ordered curation list for ONE surface. LEFT JOINs both subject tables so a single
-- read resolves the display title (asset.title or collection.name)
-- plus the asset thumbnail hints, without the handler fanning out a
-- per-row lookup. A dangling reference (subject hard-deleted) yields
-- an empty title rather than dropping the row — the operator prunes
-- stale entries by hand.
SELECT f.id, f.subject_kind, f.subject_id, f.position,
       f.created_at, f.created_by_user_ref,
       COALESCE(a.title, c.name, tm.name, '')::text AS title,
       -- The asset whose col variant renders the tile (#625): the
       -- subject itself for an asset, the hero-card fallback for a
       -- collection. NULL when nothing is servable — the client keys on
       -- this, not on subject_kind, because for a collection subject_id
       -- is the COLLECTION and the variant endpoint would 404 on it.
       CASE f.subject_kind
            WHEN 'asset'      THEN a.id
            WHEN 'collection' THEN cover.id
       END::uuid AS cover_asset_id,
       -- '' means "no servable hash" — same convention as the title
       -- COALESCE above; the mapper turns it back into an absent field.
       COALESCE(CASE f.subject_kind
            WHEN 'asset'      THEN a.file_hash
            WHEN 'collection' THEN cover.file_hash
       END, '')::text AS asset_file_hash,
       -- preview_available (#471): a servable col variant exists. This is
       -- the admin curation list, served to operators who read every
       -- tier, so variant existence alone decides it (no per-caller
       -- readability needed here). The cover lateral below already
       -- REQUIRES a servable col, so for a collection its presence is
       -- the answer.
       COALESCE(CASE f.subject_kind
            WHEN 'asset' THEN EXISTS (
                 SELECT 1 FROM storage_variants sv
                  WHERE sv.object_hash = a.file_hash AND sv.variant_key = 'col')
            WHEN 'collection' THEN cover.file_hash IS NOT NULL
       END, false)::boolean AS asset_preview_available,
       -- ladder_available (#591): every CONFIGURED rung exists — for the
       -- asset itself, or for the collection's resolved cover. The rung
       -- list is a parameter, not a literal, because the ladder is
       -- operator-tunable — a hardcoded four-key check would report
       -- false forever on an install that dropped a rung. The
       -- cardinality guard makes an empty (unknown) ladder resolve to
       -- false rather than vacuously true.
       COALESCE((COALESCE(cardinality(sqlc.arg('ladder')::text[]), 0) > 0
            AND COALESCE(a.file_hash, cover.file_hash) IS NOT NULL
            AND (SELECT COUNT(DISTINCT sv.variant_key) FROM storage_variants sv
                  WHERE sv.object_hash = COALESCE(a.file_hash, cover.file_hash)
                    AND sv.variant_key = ANY(sqlc.arg('ladder')::text[]))
                = cardinality(sqlc.arg('ladder')::text[])), false)::boolean AS asset_ladder_available
FROM featured_items f
LEFT JOIN assets a
       ON f.subject_kind = 'asset' AND a.id = f.subject_id
LEFT JOIN collections c
       ON f.subject_kind = 'collection' AND c.id = f.subject_id
-- Team subjects (#1084). Found by adding one and looking at the result:
-- without this join the curation list answered with a row whose title
-- was '' — an operator staring at an unnamed entry they cannot identify
-- and therefore cannot safely remove. Widening subject_kind is not
-- finished until every surface that resolves a subject knows the new
-- kind, and this list is one of them.
--
-- Deliberately title-only, with no tile: a team's picture is admissible
-- only through the render-time TeamHeroes re-check (#982, migration
-- 00047), and resolving it here would mean a SECOND copy of that rule
-- inside a query whose whole design note is that its gates are weaker
-- than the rail's. The operator gets the name, which is what identifies
-- the row; nothing here can leak a picture because nothing here
-- resolves one.
--
-- No deleted_at filter, matching the assets/collections joins above and
-- for the same stated reason: a dangling or tombstoned subject yields a
-- row the operator can see and prune, rather than a placement that
-- vanishes from the only surface that could remove it.
LEFT JOIN teams tm
       ON f.subject_kind = 'team' AND tm.id = f.subject_id
-- Hero-card fallback for collection subjects (#625), ported from the
-- public rail's lateral (#559 / ADR 0027): the cover of the most recent
-- eligible post in the collection.
--
-- DELIBERATELY WEAKER GATES THAN THE RAIL, and the difference is the
-- point. The rail splices the caller's visibility predicate and demands
-- ca.sensitivity = 'public', because it serves anonymous visitors and
-- featuring must never widen access. This endpoint is system.admin-gated
-- and "served to operators who read every tier" (see
-- asset_preview_available above), so both caller-scoped gates are
-- dropped: a team-tier or embargo cover SHOULD render here. What stays:
--   * soft-delete on the post and the cover asset — the rail got these
--     from the predicate it spliced, so dropping the splice must not
--     drop them with it; a deleted asset is not a cover for anyone
--   * a servable col variant — the zero-404 property; a tile must never
--     build a byte URL that cannot be answered
LEFT JOIN LATERAL (
       SELECT ca.id, ca.file_hash
         FROM collection_posts cp
         JOIN posts p   ON p.id = cp.post_id AND p.deleted_at IS NULL
         JOIN assets ca ON ca.id = p.cover_asset_id AND ca.deleted_at IS NULL
        WHERE cp.collection_id = c.id
          AND EXISTS (SELECT 1 FROM storage_variants sv
                       WHERE sv.object_hash = ca.file_hash AND sv.variant_key = 'col')
        ORDER BY p.created_at DESC, p.id DESC
        LIMIT 1
) cover ON true
-- Which SURFACE's curation list this is (#1118). `IS NOT DISTINCT FROM`
-- rather than a pair of branches: passing NULL selects the rail — every
-- row that existed before band_id — and passing a band id selects that
-- band's cards. One query, because the alternative was a second copy of
-- everything above it, and the two would have drifted on the next change
-- to the cover lateral exactly as the rail and the admin list did before
-- #625.
--
-- It is NOT a visibility filter and does not pretend to be one: this
-- endpoint is system.admin-gated (see the note on the cover lateral),
-- and band membership is a surface, not an audience. The band's audience
-- lives on promo_bands.scope and is read by the PUBLIC band query.
WHERE f.band_id IS NOT DISTINCT FROM sqlc.narg('band_id')::uuid
ORDER BY f.position ASC, f.created_at ASC;

-- name: InsertFeaturedItem :one
-- Adds one subject to the curation list. When position is NULL the
-- row appends to the end (max existing position + 1). Scope defaults
-- to 'org' — the admin surface curates the internal list, and a
-- public placement is a deliberate act (ADR 0065). The
-- (subject_kind, subject_id, scope, team_id, band_id) unique constraint
-- makes a duplicate add WITHIN ONE AUDIENCE ON ONE SURFACE a 23505 the
-- handler maps to 409, while still allowing the same subject at another
-- scope or in a band.
--
-- `band_id` NULL is the rail (#1118). The append position is computed
-- WITHIN THE SURFACE — `IS NOT DISTINCT FROM` again — because a global
-- MAX would hand a rail add a position derived from a band's ordering
-- and vice versa. Harmless while ordering is only ever relative, and
-- wrong the moment anything reads a position as a number.
--
-- ⚠️ `scope` on a BAND row is not the band's audience. The band carries
-- it (promo_bands.scope); this column keeps its default for band rows
-- and no reader consults it there. See migration 00053.
INSERT INTO featured_items (subject_kind, subject_id, position, created_by_user_ref, scope, band_id)
VALUES (
    $1,
    $2,
    COALESCE(sqlc.narg('position')::integer,
             (SELECT COALESCE(MAX(position), -1) + 1 FROM featured_items f2
               WHERE f2.band_id IS NOT DISTINCT FROM sqlc.narg('band_id')::uuid)),
    sqlc.narg('created_by_user_ref')::bigint,
    COALESCE(sqlc.narg('scope')::text, 'org'),
    sqlc.narg('band_id')::uuid
)
RETURNING id, subject_kind, subject_id, position, created_at, created_by_user_ref, scope, team_id, band_id;

-- name: DeleteFeaturedItem :execrows
-- Removes one entry by id. Returns rows-affected so the handler can
-- map 0 → 404.
DELETE FROM featured_items WHERE id = $1;

-- name: UpdateFeaturedPosition :execrows
-- Sets the ordering position of one entry. Called once per row inside
-- the reorder transaction. Returns rows-affected for the same 0 → 404
-- mapping.
UPDATE featured_items SET position = $2 WHERE id = $1;

-- ---------------------------------------------------------------------
-- The operator promo band (#1118)
-- ---------------------------------------------------------------------
--
-- The band DEFINITION only — its cards are featured_items rows carrying
-- its id, read through ListFeaturedItems (admin) and the hand-built
-- public band query in band.go (readers). See migration 00053 for why
-- there is no second membership table.
--
-- These are all system.admin surfaces, so none of them gates on
-- audience: `scope` is DATA here, and the only reader that treats it as
-- a predicate is the public one.

-- name: GetPromoBand :one
-- The band the v1 surfaces operate on, enabled or not.
--
-- "The band" is singular by PRODUCT decision, not by schema: the table
-- admits several (ADR 0030's slot inventory is plural) and this picks
-- the one the release renders — earliest feed position, then oldest.
-- `id` is the final tiebreak so the choice is total rather than
-- arbitrary under equal timestamps, which a seeded install can produce.
SELECT id, title, blurb, cta_label, cta_url, enabled, after_page, scope,
       created_at, updated_at, created_by_user_ref
FROM promo_bands
ORDER BY after_page ASC, created_at ASC, id ASC
LIMIT 1;

-- name: InsertPromoBand :one
-- Creates the band. Called only when GetPromoBand found none — the
-- admin surface is an upsert over a singleton, and the decision which
-- of the two writes to run is the handler's, inside one transaction.
INSERT INTO promo_bands (title, blurb, cta_label, cta_url, enabled, after_page, scope, created_by_user_ref)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.narg('created_by_user_ref')::bigint)
RETURNING id, title, blurb, cta_label, cta_url, enabled, after_page, scope,
          created_at, updated_at, created_by_user_ref;

-- name: UpdatePromoBand :one
-- Replaces the band's definition wholesale. NOT a COALESCE-style PATCH:
-- the admin form posts every field it owns on every save, and a partial
-- write would make "clear the blurb" indistinguishable from "leave the
-- blurb alone" — the failure the COALESCE PATCH convention exists to
-- produce deliberately elsewhere and which is wrong here.
UPDATE promo_bands
   SET title = $2, blurb = $3, cta_label = $4, cta_url = $5,
       enabled = $6, after_page = $7, scope = $8, updated_at = now()
 WHERE id = $1
RETURNING id, title, blurb, cta_label, cta_url, enabled, after_page, scope,
          created_at, updated_at, created_by_user_ref;

-- name: DeletePromoBand :execrows
-- Removes the band. Its cards go with it through
-- featured_items_band_id_fkey's ON DELETE CASCADE rather than being
-- orphaned onto the rail — see migration 00053's Down for the same
-- argument. Returns rows-affected for the 0 → 404 mapping.
DELETE FROM promo_bands WHERE id = $1;
