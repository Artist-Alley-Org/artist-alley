-- ---------------------------------------------------------------------------
-- field_definition — admin-managed schema for what fields exist
-- ---------------------------------------------------------------------------

-- name: ListFieldDefinitions :many
-- Returns active field defs ordered by group + display_order. Caller
-- can post-filter by applies_to in the handler since GIN array
-- membership doesn't compose well with our other filters.
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, field_set_id, read_capability, write_capability,
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref
FROM field_definition
WHERE (sqlc.narg('status')::TEXT IS NULL OR status = sqlc.narg('status')::TEXT)
ORDER BY display_group, display_order, code;

-- name: ListFieldDefinitionsForResourceType :many
-- Like ListFieldDefinitions but only fields whose applies_to is
-- empty (applies to all) OR contains the given resource_type ref.
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, field_set_id, read_capability, write_capability,
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref
FROM field_definition
WHERE status = 'active'
  AND (cardinality(applies_to) = 0 OR sqlc.arg('rt')::BIGINT = ANY(applies_to))
ORDER BY display_group, display_order, code;

-- name: GetFieldDefinitionByID :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, field_set_id, read_capability, write_capability,
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref
FROM field_definition WHERE id = $1;

-- name: GetFieldDefinitionByCode :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, field_set_id, read_capability, write_capability,
       display_order, display_group, source, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref
FROM field_definition WHERE code = $1;

-- name: CreateFieldDefinition :one
INSERT INTO field_definition (
    code, label, description, type, options, required, searchable,
    applies_to, field_set_id, read_capability, write_capability,
    display_order, display_group, source, status,
    created_by_user_ref, updated_by_user_ref
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
RETURNING id, code, label, description, type, options, required, searchable,
          applies_to, field_set_id, read_capability, write_capability,
          display_order, display_group, source, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref;

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
    field_set_id              = COALESCE(sqlc.narg('field_set_id'),              field_set_id),
    read_capability           = COALESCE(sqlc.narg('read_capability'),           read_capability),
    write_capability          = COALESCE(sqlc.narg('write_capability'),          write_capability),
    display_order             = COALESCE(sqlc.narg('display_order'),             display_order),
    display_group             = COALESCE(sqlc.narg('display_group'),             display_group),
    source                    = COALESCE(sqlc.narg('source'),                    source),
    status                    = COALESCE(sqlc.narg('status'),                    status),
    deprecated_replacement_id = COALESCE(sqlc.narg('deprecated_replacement_id'), deprecated_replacement_id),
    updated_at                = NOW(),
    updated_by_user_ref       = sqlc.narg('updated_by_user_ref')
WHERE id = sqlc.arg('id')
RETURNING id, code, label, description, type, options, required, searchable,
          applies_to, field_set_id, read_capability, write_capability,
          display_order, display_group, source, status,
          deprecated_replacement_id, origin_server_id,
          created_at, updated_at, created_by_user_ref, updated_by_user_ref;

-- name: ArchiveFieldDefinition :exec
-- Soft-archive — keeps the row and any historic values so audit
-- trails stay readable. Use a UPDATE that bumps status; we never
-- DELETE field defs in normal flow.
UPDATE field_definition
   SET status = 'archived', updated_at = NOW(), updated_by_user_ref = $2
 WHERE id = $1 AND status <> 'archived';

-- ---------------------------------------------------------------------------
-- asset_field_value — the actual values
-- ---------------------------------------------------------------------------

-- name: ListAssetFieldValues :many
-- All field values for an asset, joined with the field def so the
-- handler can normalise the typed columns to a single JSON shape
-- per field type. Filtered to active fields (deprecated ones still
-- return so the UI can show "this value was set on a deprecated
-- field; please re-enter").
SELECT v.field_id, v.value_text, v.value_num, v.value_date, v.value_options, v.value_ref,
       v.set_by, v.set_at, v.set_by_user_ref,
       f.code, f.label, f.type, f.status
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
