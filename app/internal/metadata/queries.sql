-- ---------------------------------------------------------------------------
-- field_definition — admin-managed schema for what fields exist
-- ---------------------------------------------------------------------------

-- name: ListFieldDefinitions :many
-- Returns field defs ordered by group + display_order. Caller can
-- post-filter by applies_to in the handler since GIN array membership
-- doesn't compose well with our other filters.
--
-- Status semantics (#528): an explicit `status` is an equality filter
-- (so `status=archived` still surfaces soft-deleted fields for anyone
-- who opts in). With no `status`, archived fields are EXCLUDED — they're
-- tombstones, and editors that just want "the live schema" (e.g. the
-- collection edit modal) must not render them. Active + deprecated stay
-- visible by default; deprecated fields can still hold values on
-- existing rows, so hiding them would drop live data from the editor.
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value,
       open_vocabulary, mirrors_column, show_on_card
FROM field_definition
WHERE (
        CASE WHEN sqlc.narg('status')::TEXT IS NULL
             THEN status <> 'archived'
             ELSE status = sqlc.narg('status')::TEXT
        END
      )
  AND (sqlc.narg('subject_kind')::TEXT IS NULL OR subject_kind = sqlc.narg('subject_kind')::TEXT)
ORDER BY display_group, display_order, code;

-- name: ListFieldDefinitionsForAssetType :many
-- Like ListFieldDefinitions but only fields whose applies_to is
-- empty (applies to all) OR contains the given asset_type ref.
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value,
       open_vocabulary, mirrors_column, show_on_card
FROM field_definition
WHERE status = 'active'
  AND subject_kind = 'asset'
  AND (cardinality(applies_to) = 0 OR sqlc.arg('rt')::BIGINT = ANY(applies_to))
ORDER BY display_group, display_order, code;

-- name: GetFieldDefinitionByID :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value,
       open_vocabulary, mirrors_column, show_on_card
FROM field_definition WHERE id = $1;

-- name: GetFieldDefinitionByCode :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value,
       open_vocabulary, mirrors_column, show_on_card
FROM field_definition WHERE code = $1;

-- name: CreateFieldDefinition :one
INSERT INTO field_definition (
    code, label, description, type, options, required, searchable,
    applies_to, read_capability, write_capability,
    display_order, display_group, status,
    created_by_user_ref, updated_by_user_ref, subject_kind, default_value,
    open_vocabulary
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$14,$15,$16,$17)
RETURNING id, code, label, description, type, options, required, searchable,
          applies_to, read_capability, write_capability,
          display_order, display_group, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref,
          subject_kind, extraction_source, extraction_mode, default_value,
          open_vocabulary, mirrors_column, show_on_card;

-- name: UpdateFieldDefinition :one
-- COALESCE pattern: NULL args keep current value. `applies_to` is a
-- non-nullable column, so it always replaces when the caller
-- intends to update it; the handler decides whether to pass NULL or
-- an explicit empty array.
UPDATE field_definition SET
    label                     = COALESCE(sqlc.narg('label'),                     label),
    description               = COALESCE(sqlc.narg('description'),               description),
    options                   = COALESCE(sqlc.narg('options'),                   options),
    required                  = COALESCE(sqlc.narg('required'),                  required),
    searchable                = COALESCE(sqlc.narg('searchable'),                searchable),
    applies_to                = COALESCE(sqlc.narg('applies_to'),                applies_to),
    read_capability           = COALESCE(sqlc.narg('read_capability'),           read_capability),
    write_capability          = COALESCE(sqlc.narg('write_capability'),          write_capability),
    display_order             = COALESCE(sqlc.narg('display_order'),             display_order),
    display_group             = COALESCE(sqlc.narg('display_group'),             display_group),
    open_vocabulary           = COALESCE(sqlc.narg('open_vocabulary'),           open_vocabulary),
    show_on_card              = COALESCE(sqlc.narg('show_on_card'),              show_on_card),
    status                    = COALESCE(sqlc.narg('status'),                    status),
    deprecated_replacement_id = COALESCE(sqlc.narg('deprecated_replacement_id'), deprecated_replacement_id),
    -- default_value needs a CLEAR path, which COALESCE cannot express:
    -- passing NULL means "leave it alone" everywhere else in this
    -- statement, so "remove the default" would be unsayable. The
    -- explicit boolean makes removal a deliberate act rather than an
    -- ambiguity in the absence of a value.
    default_value             = CASE WHEN sqlc.arg('clear_default')::BOOLEAN THEN NULL
                                     ELSE COALESCE(sqlc.narg('default_value'), default_value) END,
    updated_at                = NOW(),
    updated_by_user_ref       = sqlc.narg('updated_by_user_ref')
WHERE id = sqlc.arg('id')
RETURNING id, code, label, description, type, options, required, searchable,
          applies_to, read_capability, write_capability,
          display_order, display_group, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref,
          subject_kind, extraction_source, extraction_mode, default_value,
          open_vocabulary, mirrors_column, show_on_card;

-- name: ArchiveFieldDefinition :exec
-- Soft-archive — keeps the row and any historic values so audit
-- trails stay readable. Use a UPDATE that bumps status; we never
-- DELETE field defs in normal flow.
UPDATE field_definition
   SET status = 'archived', updated_at = NOW(), updated_by_user_ref = $2
 WHERE id = $1 AND status <> 'archived';

-- name: SetFieldExtractionConfig :one
-- Phase 1.18.A-2. Wires (or unwires) the metadata-extraction
-- pipeline against one field. source='' clears the wiring;
-- mode='' is normalised to skip_if_set by the caller.
UPDATE field_definition
   SET extraction_source = $2,
       extraction_mode   = $3,
       updated_at        = NOW(),
       updated_by_user_ref = $4
 WHERE id = $1
RETURNING id, code, label, description, type, options, required, searchable,
          applies_to, read_capability, write_capability,
          display_order, display_group, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref,
          subject_kind, extraction_source, extraction_mode, default_value,
          open_vocabulary, mirrors_column, show_on_card;

-- name: LockFieldDefinitionVocabulary :one
-- Reads the live options document under a ROW LOCK, for the
-- accept-and-create path on an open vocabulary (#830).
--
-- FOR UPDATE is the whole point. Adding a term rewrites the WHOLE
-- options document, so two concurrent writes each minting a different
-- new term would both read the pre-write document and the second
-- UPDATE would discard the first's term — the last-write-wins gap #737
-- records for the admin options editor, except here it happens on an
-- ordinary value save that no operator would think of as an edit to the
-- field. The lock serialises the read-modify-write; the loser re-reads
-- the winner's document inside its own transaction and appends to it.
--
-- Reads the row rather than trusting the caller's copy for a second
-- reason: the field-by-id LRU can hand back an options document that is
-- one write old, and resolving against a stale document is how a term
-- gets minted twice.
SELECT options, type, open_vocabulary
  FROM field_definition
 WHERE id = $1
   FOR UPDATE;

-- name: SetFieldDefinitionOptions :exec
-- Writes back an options document that gained terms. Deliberately
-- narrow — it touches options and nothing else — so it cannot be
-- mistaken for the admin editor's whole-definition update.
UPDATE field_definition
   SET options = $2, updated_at = NOW()
 WHERE id = $1;

-- ---------------------------------------------------------------------------
-- asset_field_value — the actual values
-- ---------------------------------------------------------------------------

-- name: ListAssetFieldValues :many
-- All field values for an asset, joined with the field def so the
-- handler can normalise the typed columns to a single JSON shape
-- per field type. Filtered to active fields (deprecated ones still
-- return so the UI can show "this value was set on a deprecated
-- field; please re-enter").
-- f.options rides along so the handler can resolve a stored select
-- slug to its label and lifecycle without a second query: the join to
-- field_definition is already here for the code/label/type, so the
-- column is free.
--
-- The LEFT JOIN to assets does the same job for `reference` values
-- (#817): value_ref stores a bare UUID, so without it every reference
-- field renders as a raw id. One join, no extra round trip, no N+1 —
-- and because it is a LEFT join, a value that does not resolve simply
-- yields NULLs and the handler omits resolved_reference.
--
-- `r.deleted_at IS NULL` IS THE VISIBILITY RULE, not an incidental
-- tidy-up. It is exactly visibility.Predicate for (EntityAsset,
-- authenticated) — see ADR 0063/0064: a title is row-plane metadata,
-- and for an authenticated caller the asset row predicate is
-- soft-delete and nothing else. The anonymous branch of that predicate
-- (status/sensitivity/processing) is deliberately NOT reproduced here
-- because it is unreachable: GetAssetFields 401s a nil identity, so no
-- anonymous caller ever executes this query. reference_value_e2e_test.go
-- asserts both halves of that claim, so if the authenticated predicate
-- ever tightens (#210's sensitivity rule) or this endpoint is ever
-- opened to anonymous callers, the test fails and points here.
-- `f.display_group` / `f.display_order` ride along because a MIRRORED
-- field's value never appears in this result — the guard trigger from
-- migration 00044 refuses it a row in asset_field_value at all — so
-- ListAssetMirroredValues supplies those rows separately and the
-- handler merges the two lists. A merge needs the sort keys, and the
-- ORDER BY below could not hand them over.
SELECT v.field_id, v.value_text, v.value_num, v.value_date, v.value_options, v.value_ref,
       v.set_by, v.set_at, v.set_by_user_ref,
       f.code, f.label, f.type, f.status, f.options,
       f.display_group, f.display_order,
       r.id AS ref_asset_id,
       -- COALESCE rather than a bare r.title: sqlc infers a LEFT-joined
       -- NOT NULL column as non-nullable and would scan a NULL into a
       -- string. Presence is carried by ref_asset_id.Valid instead,
       -- which is unambiguous — and an empty title is a real state
       -- (assets.title DEFAULT '') that the client renders as the id.
       COALESCE(r.title, '')::TEXT AS ref_asset_title
FROM asset_field_value v
JOIN field_definition f ON f.id = v.field_id
LEFT JOIN assets r ON r.id = v.value_ref AND r.deleted_at IS NULL
WHERE v.asset_id = $1
ORDER BY f.display_group, f.display_order, f.code;

-- name: GetReferencedAsset :one
-- The resolve-one counterpart of ListAssetFieldValues' LEFT JOIN, for
-- the upsert path — which returns a single AssetFieldValue and has no
-- join to ride along on.
--
-- It exists so SetAssetFieldValue's 200 body carries the same
-- resolved_reference the list path does. #775 is the precedent and the
-- warning: buildAssetValue was created because one consumer resolved
-- and another printed the slug, and shipping a DTO field that only one
-- of two endpoints ever populates rebuilds that exact asymmetry.
--
-- Same visibility rule as the join, for the same reason (see above).
SELECT a.id, a.title
FROM assets a
WHERE a.id = $1 AND a.deleted_at IS NULL;

-- name: GetAssetFieldValue :one
SELECT v.asset_id, v.field_id, v.value_text, v.value_num, v.value_date,
       v.value_options, v.value_ref, v.set_by, v.set_at, v.set_by_user_ref,
       f.code, f.type
FROM asset_field_value v
JOIN field_definition f ON f.id = v.field_id
WHERE v.asset_id = $1 AND v.field_id = $2;

-- name: UpsertAssetFieldValue :one
-- INSERT-or-UPDATE. Exactly one of the value_* columns is populated
-- per field type; the handler clears the others.
INSERT INTO asset_field_value (
    asset_id, field_id,
    value_text, value_num, value_date, value_options, value_ref,
    set_by, set_at, set_by_user_ref
) VALUES (
    $1, $2,
    sqlc.narg('value_text'),
    sqlc.narg('value_num'),
    sqlc.narg('value_date'),
    sqlc.narg('value_options'),
    sqlc.narg('value_ref'),
    sqlc.arg('set_by'),
    NOW(),
    sqlc.narg('set_by_user_ref')
)
ON CONFLICT (asset_id, field_id) DO UPDATE SET
    value_text      = EXCLUDED.value_text,
    value_num       = EXCLUDED.value_num,
    value_date      = EXCLUDED.value_date,
    value_options   = EXCLUDED.value_options,
    value_ref       = EXCLUDED.value_ref,
    set_by          = EXCLUDED.set_by,
    set_at          = NOW(),
    set_by_user_ref = EXCLUDED.set_by_user_ref
RETURNING asset_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

-- name: DeleteAssetFieldValue :exec
DELETE FROM asset_field_value WHERE asset_id = $1 AND field_id = $2;

-- ---------------------------------------------------------------------------
-- MIRRORED fields — a definition that declares `mirrors_column` is a VIEW
-- onto that column of `assets`, not storage of its own (#822, migration
-- 00044).
--
-- Every query below obtains the column from `field_definition.mirrors_column`
-- and hands it to the accessor functions the migration installs. NOTHING in
-- this file, or in Go, names `title` or `description`: the CHECK constraint on
-- the column is the only enumeration of what is mirrorable, so widening the
-- set is a migration and not a sweep through the query layer.
-- ---------------------------------------------------------------------------

-- name: ListAssetMirroredValues :many
-- The mirrored half of ListAssetFieldValues. Those rows cannot come from
-- that query because they do not exist in asset_field_value and never
-- will — the guard trigger refuses them — so they are projected from the
-- column here and merged by the handler on (display_group, display_order,
-- code), the same order the stored half arrives in.
--
-- An EMPTY column yields no row, which is the same contract the stored
-- half honours: a field nobody has set has no entry, and the mirrorable
-- columns default to the empty string, which is how they say "unset".
-- Without this a mirrored field would appear on every asset in the
-- catalogue carrying a blank value.
--
-- Archived fields are excluded. The stored half returns them (a value on
-- an archived field is still data an editor must be able to see and
-- clear); a mirrored field has nothing of its own to strand, so an
-- archived one is simply off.
--
-- The soft-delete rule is asset_mirror_read's, so it is stated once — see
-- ListAssetFieldValues' note on why row-plane visibility for an
-- authenticated caller reduces to deleted_at IS NULL.
SELECT f.id AS field_id,
       public.asset_mirror_read(a.id, f.mirrors_column) AS value_text,
       f.code, f.label, f.type, f.status, f.options,
       f.display_group, f.display_order,
       a.updated_at AS set_at
  FROM field_definition f
  CROSS JOIN assets a
 WHERE a.id = $1
   AND a.deleted_at IS NULL
   AND f.mirrors_column IS NOT NULL
   AND f.subject_kind = 'asset'
   AND f.status <> 'archived'
   AND coalesce(public.asset_mirror_read(a.id, f.mirrors_column), '') <> ''
 ORDER BY f.display_group, f.display_order, f.code;

-- name: ReadAssetMirroredValue :one
-- One mirrored field's value, for the extraction pipeline's skip_if_set
-- probe. Empty means unset, and an absent or soft-deleted asset reads
-- empty too: both answers are "there is nothing here to preserve", which
-- is the only question the probe asks.
SELECT coalesce(public.asset_mirror_read(sqlc.arg('asset_id'), sqlc.arg('mirrors_column')), '')::TEXT AS value;

-- name: GetAssetMirrorSubject :one
-- The authorisation probe for a mirrored write. A mirrored field's
-- payload is an `assets` column, so the gate that binds is the COLUMN's
-- (owner / team-scoped assets.admin / global), not the field plane's
-- "any authenticated caller". Deliberately the same projection
-- assets.GetAssetMutationSubject reads, for the same reason: the gate
-- needs a nullable owner alongside team_id and must answer for a row the
-- caller may not be entitled to read.
SELECT owner_user_ref, team_id
  FROM assets
 WHERE id = $1 AND deleted_at IS NULL;

-- name: GetFieldMirrorColumn :one
-- Just the declaration, for a caller that holds a field id and no row —
-- the extraction writer adapter, which is handed a bare field id by the
-- applier.
SELECT mirrors_column FROM field_definition WHERE id = $1;

-- ---------------------------------------------------------------------------
-- asset_field_value_history — append-only audit
-- ---------------------------------------------------------------------------

-- name: AppendAssetFieldValueHistory :exec
INSERT INTO asset_field_value_history (
    asset_id, field_id, old_value, new_value, set_by, changed_by_user_ref
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListAssetFieldValueHistory :many
-- Newest-first. Bounded by limit; callers paginate via cursor on
-- changed_at if/when this gets big.
SELECT id, asset_id, field_id, old_value, new_value,
       changed_at, changed_by_user_ref, set_by
FROM asset_field_value_history
WHERE asset_id = $1
  AND (sqlc.narg('field_id')::UUID IS NULL OR field_id = sqlc.narg('field_id')::UUID)
ORDER BY changed_at DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- ---------------------------------------------------------------------------
-- collection_field_value — typed-value storage for collection metadata
-- (Phase 1.9.B). Mirrors the asset_field_value pattern one-for-one;
-- the value path is identical so debuggers see one shape, not two.
-- ---------------------------------------------------------------------------

-- name: ListCollectionFieldValues :many
-- The collection sibling of ListAssetFieldValues (#840). It carried NO
-- join at all until now, so collection metadata rendered raw slugs and
-- bare reference UUIDs where the asset path rendered labels and linked
-- titles. The two joins here close that gap and cost nothing extra:
--   - JOIN field_definition brings code/label/type AND f.options, so the
--     handler resolves a stored `select` slug to its label without a
--     second query (exactly what ListAssetFieldValues does).
--   - LEFT JOIN assets resolves a `reference` value's title (#817).
--     `r.deleted_at IS NULL` IS the authenticated-asset visibility rule
--     (ADR 0063/0064), the SAME predicate ListAssetFieldValues' join
--     hard-codes — see that query and
--     TestCollectionReferenceJoinMatchesAuthenticatedAssetPredicate. A
--     target that does not resolve yields NULLs and the handler omits
--     resolved_reference, degrading to the bare id (#839).
SELECT v.collection_id, v.field_id, v.value_text, v.value_num, v.value_date,
       v.value_options, v.value_ref, v.set_by, v.set_at, v.set_by_user_ref,
       f.code, f.label, f.type, f.options,
       r.id AS ref_asset_id,
       COALESCE(r.title, '')::TEXT AS ref_asset_title
FROM collection_field_value v
JOIN field_definition f ON f.id = v.field_id
LEFT JOIN assets r ON r.id = v.value_ref AND r.deleted_at IS NULL
WHERE v.collection_id = $1
ORDER BY f.display_group, f.display_order, f.code;

-- name: GetCollectionFieldValue :one
-- Same joins as ListCollectionFieldValues, for the single-row read the
-- write path snapshots against — code/label/type/options and the
-- reference title all resolve here too (#840).
SELECT v.collection_id, v.field_id, v.value_text, v.value_num, v.value_date,
       v.value_options, v.value_ref, v.set_by, v.set_at, v.set_by_user_ref,
       f.code, f.label, f.type, f.options,
       r.id AS ref_asset_id,
       COALESCE(r.title, '')::TEXT AS ref_asset_title
FROM collection_field_value v
JOIN field_definition f ON f.id = v.field_id
LEFT JOIN assets r ON r.id = v.value_ref AND r.deleted_at IS NULL
WHERE v.collection_id = $1 AND v.field_id = $2;

-- name: UpsertCollectionFieldValue :one
-- Cache invalidation MUST follow at handler layer.
-- History row MUST follow in the same transaction.
INSERT INTO collection_field_value (
    collection_id, field_id,
    value_text, value_num, value_date, value_options, value_ref,
    set_by, set_at, set_by_user_ref
) VALUES (
    sqlc.arg('collection_id'),  sqlc.arg('field_id'),
    sqlc.narg('value_text'),    sqlc.narg('value_num'),
    sqlc.narg('value_date'),    sqlc.narg('value_options'),
    sqlc.narg('value_ref'),
    sqlc.arg('set_by'),         NOW(),
    sqlc.narg('set_by_user_ref')
)
ON CONFLICT (collection_id, field_id) DO UPDATE SET
    value_text      = EXCLUDED.value_text,
    value_num       = EXCLUDED.value_num,
    value_date      = EXCLUDED.value_date,
    value_options   = EXCLUDED.value_options,
    value_ref       = EXCLUDED.value_ref,
    set_by          = EXCLUDED.set_by,
    set_at          = NOW(),
    set_by_user_ref = EXCLUDED.set_by_user_ref
RETURNING collection_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

-- name: DeleteCollectionFieldValue :exec
DELETE FROM collection_field_value
WHERE collection_id = $1 AND field_id = $2;

-- ---------------------------------------------------------------------------
-- collection_field_value_history — append-only audit (Phase 1.9.B).
-- Same JSONB pair shape as asset_field_value_history; handler is
-- responsible for serialising values before writing.
-- ---------------------------------------------------------------------------

-- name: AppendCollectionFieldValueHistory :exec
INSERT INTO collection_field_value_history (
    collection_id, field_id, old_value, new_value, set_by, changed_by_user_ref
) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListCollectionFieldValueHistory :many
SELECT id, collection_id, field_id, old_value, new_value,
       changed_at, changed_by_user_ref, set_by
FROM collection_field_value_history
WHERE collection_id = $1
  AND (sqlc.narg('field_id')::UUID IS NULL OR field_id = sqlc.narg('field_id')::UUID)
ORDER BY changed_at DESC
LIMIT sqlc.arg('row_limit')::INTEGER;

-- ---------------------------------------------------------------------------
-- Required collection-field probe — used by collections.Create to
-- gate creation when operators have required collection fields.
-- ---------------------------------------------------------------------------

-- name: ListRequiredCollectionFields :many
SELECT id, code, label, type
FROM field_definition
WHERE subject_kind = 'collection'
  AND status = 'active'
  AND required = TRUE
ORDER BY display_order ASC, label ASC;

-- ---------------------------------------------------------------------------
-- Upload defaults (#793) — ADR 0081 §3
-- ---------------------------------------------------------------------------

-- name: ListAssetDefaultCandidates :many
-- Every asset field that carries a default OR has one overridden by a
-- team the uploader belongs to, for the asset type being created.
--
-- The LEFT JOIN is what makes both halves of the precedence chain
-- readable in ONE query: a field with only a field default comes back
-- once with a NULL override, a field a team has overridden comes back
-- with the override alongside, and a field whose ONLY default is a team
-- override still comes back (the WHERE accepts either side). Emitting
-- one row per matching override rather than picking a winner in SQL is
-- deliberate — Go can then see that two of the uploader's teams both
-- override the same field, which is the case that has no correct answer
-- and must not be silently resolved by an ORDER BY.
--
-- applies_to is honoured here: a field that does not apply to this asset
-- type has no business defaulting onto it.
--
-- f.mirrors_column rides along because a default on a MIRRORED field
-- (#822) fills the COLUMN, not asset_field_value — the guard trigger
-- refuses the latter — and this pass has to know which it is holding
-- before it writes.
SELECT f.id, f.code, f.type, f.options, f.default_value, f.mirrors_column,
       o.team_id, o.default_value AS override_value
FROM field_definition f
LEFT JOIN field_default_override o
       ON o.field_id = f.id
      AND o.team_id = ANY(sqlc.arg('team_ids')::UUID[])
WHERE f.subject_kind = 'asset'
  AND f.status = 'active'
  AND (cardinality(f.applies_to) = 0 OR sqlc.arg('rt')::BIGINT = ANY(f.applies_to))
  AND (f.default_value IS NOT NULL OR o.default_value IS NOT NULL)
ORDER BY f.code;

-- name: ListDefaultTeamsForUser :many
-- The uploader's DIRECT team memberships. Not the closure: an ancestor
-- team is a permission scope, not a place someone uploads to, and
-- expanding it would make a parent team's override apply to uploads it
-- never sees.
SELECT t.id, t.name
FROM team_memberships m
JOIN teams t ON t.id = m.team_id
WHERE m.user_ref = $1
  AND t.deleted_at IS NULL
ORDER BY t.name, t.id;

-- name: GetDefaultUserDisplay :one
-- Display name for the `uploading_user` context value. fullname when
-- the user set one, username otherwise — the same fallback every other
-- surface shows, so a default matches what the byline says.
SELECT COALESCE(NULLIF(fullname, ''), username, '')::TEXT AS display
FROM "user" WHERE ref = $1;

-- name: ListFieldDefaultOverrides :many
SELECT o.field_id, o.team_id, t.slug AS team_slug, t.name AS team_name,
       o.default_value, o.created_at, o.updated_at, o.updated_by_user_ref
FROM field_default_override o
JOIN teams t ON t.id = o.team_id
WHERE o.field_id = $1 AND t.deleted_at IS NULL
ORDER BY t.name, t.id;

-- name: UpsertFieldDefaultOverride :one
INSERT INTO field_default_override (field_id, team_id, default_value, updated_by_user_ref)
VALUES ($1, $2, $3, $4)
ON CONFLICT (field_id, team_id) DO UPDATE
   SET default_value = EXCLUDED.default_value,
       updated_at = NOW(),
       updated_by_user_ref = EXCLUDED.updated_by_user_ref
RETURNING field_id, team_id, default_value, created_at, updated_at, updated_by_user_ref;

-- name: DeleteFieldDefaultOverride :execrows
DELETE FROM field_default_override WHERE field_id = $1 AND team_id = $2;

-- name: GetTeamForDefaultOverride :one
SELECT id, slug, name FROM teams WHERE id = $1 AND deleted_at IS NULL;

-- name: InsertAssetFieldValueIfAbsent :execrows
-- The defaults writer. ON CONFLICT DO NOTHING makes "a default never
-- overwrites a value that is already set" a property of the STATEMENT
-- rather than of the order the caller happens to do things in. The
-- caller today runs at asset creation, where by construction there is
-- nothing to overwrite; encoding the rule here means a future caller
-- (a re-apply action, a backfill) cannot reintroduce the clobber by
-- being placed differently in the sequence.
--
-- set_by is hard-wired to 'default' — this statement exists only for
-- the defaults path, and a provenance the applier can recognise is the
-- whole reason it can tell "a default is sitting here" from "a human
-- typed this".
INSERT INTO asset_field_value (
    asset_id, field_id,
    value_text, value_num, value_date, value_options, value_ref,
    set_by, set_at
) VALUES (
    $1, $2,
    sqlc.narg('value_text'),
    sqlc.narg('value_num'),
    sqlc.narg('value_date'),
    sqlc.narg('value_options'),
    sqlc.narg('value_ref'),
    'default', NOW()
)
ON CONFLICT (asset_id, field_id) DO NOTHING;

-- name: ListCardFieldValues :many
-- The at-a-glance field values for a PAGE of assets (#552).
--
-- It lives HERE, not in `assets`, since #1133. The collection member
-- grid renders the same tiles from the same flag and had never rendered
-- one, because /collections/{id}/resources builds its rows in a package
-- that cannot import `assets` (assets -> posts -> collections is an
-- import cycle) and so had no way to reach this. Restating it there
-- would have been a second projection of one display rule — the #665
-- defect shape. `metadata` is where the rule it feeds already lives
-- (DisplayValue, ADR 0012), and both surfaces now reach it through
-- metadata.CardFieldsForAssets.
--
-- One query for the whole page, not one per asset. The card is a browse
-- surface: an N+1 here is 50 round trips per scroll, and the existing
-- per-row ListAssetTags call is the precedent NOT to follow.
--
-- Both halves of a field's storage are covered, and the caller cannot tell
-- them apart — which is the point of #822's mirror. An ordinary field's
-- value comes from asset_field_value; a MIRRORED field's comes from the
-- column it declares, because it has no row here and the guard trigger
-- guarantees it never will.
--
-- The flag is a DISPLAY HINT and nothing here treats it otherwise: it
-- SELECTS which fields are candidates and takes no part in deciding which
-- assets or which values a caller may see. Row visibility was already
-- decided by the caller's own gated page query — ListAssetsPageGated for
-- browse, ListCollectionResourcesPageGated for the member grid — before
-- this runs, and only readable rows are passed in; a withheld
-- placeholder is dropped by id before the call. Gated fields cannot reach the card at all — the CHECK
-- constraint in migration 00045 refuses `show_on_card` on a field carrying
-- a read_capability, so this query needs no capability argument and cannot
-- acquire one by accident.
--
-- Typed columns come back raw rather than formatted: metadata.DisplayValue
-- resolves a vocabulary slug to its label, and doing that in SQL would be a
-- second implementation of a rule ADR 0012 keeps in one place.
SELECT a.id AS asset_id,
       f.id AS field_id,
       f.code,
       f.label,
       f.type,
       f.options,
       f.display_group,
       f.display_order,
       -- coalesced to '' rather than left nullable: the two states this
       -- query can produce for "nothing here" — no asset_field_value row and
       -- an empty mirrored column — are one answer to the card, which drops
       -- the entry either way. (It also keeps sqlc from typing a CASE with
       -- NULL branches as interface{}.)
       coalesce(
           CASE WHEN f.mirrors_column IS NOT NULL
                THEN public.asset_mirror_read(a.id, f.mirrors_column)
                ELSE v.value_text
           END, '')::TEXT AS value_text,
       v.value_num,
       v.value_date,
       v.value_options
  FROM assets a
  CROSS JOIN field_definition f
  LEFT JOIN asset_field_value v ON v.asset_id = a.id AND v.field_id = f.id
 WHERE a.id = ANY(sqlc.arg('asset_ids')::UUID[])
   AND a.deleted_at IS NULL
   AND f.show_on_card
   AND f.subject_kind = 'asset'
   AND f.status <> 'archived'
   AND (cardinality(f.applies_to) = 0 OR a.asset_type = ANY(f.applies_to))
 ORDER BY a.id, f.display_group, f.display_order, f.code;
