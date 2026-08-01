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
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value
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
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value
FROM field_definition
WHERE status = 'active'
  AND subject_kind = 'asset'
  AND (cardinality(applies_to) = 0 OR sqlc.arg('rt')::BIGINT = ANY(applies_to))
ORDER BY display_group, display_order, code;

-- name: GetFieldDefinitionByID :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value
FROM field_definition WHERE id = $1;

-- name: GetFieldDefinitionByCode :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value
FROM field_definition WHERE code = $1;

-- name: CreateFieldDefinition :one
INSERT INTO field_definition (
    code, label, description, type, options, required, searchable,
    applies_to, read_capability, write_capability,
    display_order, display_group, source, status,
    created_by_user_ref, updated_by_user_ref, subject_kind, default_value
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$15,$16,$17)
RETURNING id, code, label, description, type, options, required, searchable,
          applies_to, read_capability, write_capability,
          display_order, display_group, source, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref,
          subject_kind, extraction_source, extraction_mode, default_value;

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
    source                    = COALESCE(sqlc.narg('source'),                    source),
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
          display_order, display_group, source, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref,
          subject_kind, extraction_source, extraction_mode, default_value;

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
          display_order, display_group, source, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref,
          subject_kind, extraction_source, extraction_mode, default_value;

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
SELECT v.field_id, v.value_text, v.value_num, v.value_date, v.value_options, v.value_ref,
       v.set_by, v.set_at, v.set_by_user_ref,
       f.code, f.label, f.type, f.status, f.options
FROM asset_field_value v
JOIN field_definition f ON f.id = v.field_id
WHERE v.asset_id = $1
ORDER BY f.display_group, f.display_order, f.code;

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
SELECT collection_id, field_id, value_text, value_num, value_date,
       value_options, value_ref, set_by, set_at, set_by_user_ref
FROM collection_field_value
WHERE collection_id = $1;

-- name: GetCollectionFieldValue :one
SELECT collection_id, field_id, value_text, value_num, value_date,
       value_options, value_ref, set_by, set_at, set_by_user_ref
FROM collection_field_value
WHERE collection_id = $1 AND field_id = $2;

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
SELECT f.id, f.code, f.type, f.options, f.default_value,
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
