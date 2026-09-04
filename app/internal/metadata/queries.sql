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
       open_vocabulary, mirrors_column, show_on_card,
       show_in_advanced_search, show_on_upload, edit_tab,
       read_only, regexp_filter, display_condition
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
--
-- STATUS SEMANTICS ARE THE SAME ONES ListFieldDefinitions CARRIES
-- (#528, #1389). This query pinned `status = 'active'` and had no
-- status parameter at all, which made "which fields are live" answer
-- differently depending on whether an unrelated filter was present —
-- and left the asset edit surface unable to ask for the definitions a
-- record may legitimately still hold values on. Deprecated definitions
-- are not tombstones; archived ones are.
--
-- The active-only narrowing did not disappear, it MOVED to the caller
-- that owns it. A composer offering fields for a NEW value passes
-- status=active, which the upload form already did; an editor passes
-- no status and gets active + deprecated.
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value,
       open_vocabulary, mirrors_column, show_on_card,
       show_in_advanced_search, show_on_upload, edit_tab,
       read_only, regexp_filter, display_condition
FROM field_definition
WHERE (
        CASE WHEN sqlc.narg('status')::TEXT IS NULL
             THEN status <> 'archived'
             ELSE status = sqlc.narg('status')::TEXT
        END
      )
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
       open_vocabulary, mirrors_column, show_on_card,
       show_in_advanced_search, show_on_upload, edit_tab,
       read_only, regexp_filter, display_condition
FROM field_definition WHERE id = $1;

-- name: GetFieldDefinitionByCode :one
SELECT id, code, label, description, type, options, required, searchable,
       applies_to, read_capability, write_capability,
       display_order, display_group, status,
       deprecated_replacement_id, origin_server_id,
       created_at, updated_at, created_by_user_ref, updated_by_user_ref,
       subject_kind, extraction_source, extraction_mode, default_value,
       open_vocabulary, mirrors_column, show_on_card,
       show_in_advanced_search, show_on_upload, edit_tab,
       read_only, regexp_filter, display_condition
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
          open_vocabulary, mirrors_column, show_on_card,
          show_in_advanced_search, show_on_upload, edit_tab,
          read_only, regexp_filter, display_condition;

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
    show_in_advanced_search   = COALESCE(sqlc.narg('show_in_advanced_search'),   show_in_advanced_search),
    show_on_upload            = COALESCE(sqlc.narg('show_on_upload'),            show_on_upload),
    -- `edit_tab` needs a CLEAR path for the same reason `default_value`
    -- does, and it is the only participation flag that does: the other
    -- two are booleans, where "off" is a value COALESCE can carry, while
    -- "this field belongs to no tab" is NULL — indistinguishable from
    -- "leave it alone" everywhere else in this statement. The explicit
    -- boolean makes un-assigning a tab a deliberate act rather than an
    -- ambiguity in the absence of a value.
    edit_tab                  = CASE WHEN sqlc.arg('clear_edit_tab')::BOOLEAN THEN NULL
                                     ELSE COALESCE(sqlc.narg('edit_tab'), edit_tab) END,
    read_only                 = COALESCE(sqlc.narg('read_only'),                 read_only),
    -- `regexp_filter` is the third column needing a CLEAR path, and the
    -- only one of the three that is a PATTERN rather than a label. NULL
    -- is the single canonical "no constraint" (the CHECK refuses ''),
    -- and NULL is also what "leave it alone" looks like to COALESCE —
    -- so removal has to be said out loud, exactly as `edit_tab` and
    -- `default_value` say it. The handler refuses the two together.
    regexp_filter             = CASE WHEN sqlc.arg('clear_regexp_filter')::BOOLEAN THEN NULL
                                     ELSE COALESCE(sqlc.narg('regexp_filter'), regexp_filter) END,
    -- `display_condition` is the FOURTH column needing a CLEAR path, after
    -- `default_value`, `edit_tab` and `regexp_filter`, and for the identical
    -- reason: NULL is "this field is always offered" AND "leave it alone",
    -- so removal has to be said out loud. Migration 00065's CHECK refuses
    -- the empty array, so there is no second spelling of unset to fall back
    -- on and no way to express removal by sending a value.
    --
    -- ⚠️ The condition ARRAY IS REPLACED WHOLE and never merged. A
    -- condition is one predicate, not a bag of independent settings, and
    -- there is deliberately no way to express "add a term": an operator
    -- editing a condition is editing a sentence.
    display_condition         = CASE WHEN sqlc.arg('clear_display_condition')::BOOLEAN THEN NULL
                                     ELSE COALESCE(sqlc.narg('display_condition'), display_condition) END,
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
          open_vocabulary, mirrors_column, show_on_card,
          show_in_advanced_search, show_on_upload, edit_tab,
          read_only, regexp_filter, display_condition;

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
          open_vocabulary, mirrors_column, show_on_card,
          show_in_advanced_search, show_on_upload, edit_tab,
          read_only, regexp_filter, display_condition;

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
-- GUARDED field-value mutation (#1119) — the precondition and the
-- mutation are ONE STATEMENT.
--
-- Every handler here runs BeginTx with EMPTY options, so the isolation
-- level is READ COMMITTED and a plain SELECT takes no lock. A
-- handler-side "read the row, compare set_at, then run the
-- unconditional upsert" would therefore be two statements with a
-- window between them that a competing writer fits through entirely,
-- and it would read as correct in every single-threaded test. It is not
-- correct, and it is the path of least resistance, so it is written
-- down here as the thing these queries exist instead of.
--
-- The guard is the WHERE clause. At READ COMMITTED an UPDATE or DELETE
-- that meets a row another transaction is currently writing BLOCKS,
-- and then re-evaluates its own WHERE against the version that
-- transaction committed (EvalPlanQual). So a second contender guarding
-- on the same `set_at` cannot match after the first one lands: its
-- predicate is re-checked against the new row, `set_at` has advanced,
-- and it affects zero rows. The zero-row result IS the conflict, which
-- is why every one of these is `:one` — sqlc surfaces it as
-- pgx.ErrNoRows, where `:exec` surfaced nothing at all.
--
-- The token is the value row's OWN set_at, never the subject's
-- updated_at: two people editing two different fields of one asset are
-- not in conflict. Both upserts already write set_at = NOW() on INSERT
-- and on UPDATE, so no migration is needed to make the token advance.
-- ---------------------------------------------------------------------------

-- name: UpdateAssetFieldValueIfUnchanged :one
-- Guarded Set against an EXISTING row. Zero rows means either the row
-- is gone or somebody else wrote it; the handler reads the current
-- state afterwards to say which, and stores nothing either way.
--
-- Deliberately an UPDATE and not an upsert: `if_unchanged_since` on a
-- row that does not exist is a 409, not an insert. A timestamp is a
-- claim that a particular version is still there, and resurrecting a
-- value somebody cleared would be the write that was refused wearing a
-- disguise.
UPDATE asset_field_value SET
    value_text      = sqlc.narg('value_text'),
    value_num       = sqlc.narg('value_num'),
    value_date      = sqlc.narg('value_date'),
    value_options   = sqlc.narg('value_options'),
    value_ref       = sqlc.narg('value_ref'),
    set_by          = sqlc.arg('set_by'),
    set_at          = NOW(),
    set_by_user_ref = sqlc.narg('set_by_user_ref')
WHERE asset_id = sqlc.arg('asset_id')
  AND field_id = sqlc.arg('field_id')
  AND set_at   = sqlc.arg('if_unchanged_since')
RETURNING asset_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

-- name: InsertAssetFieldValueWhenAbsent :one
-- (Named apart from the defaults path's InsertAssetFieldValueIfAbsent,
-- which is the same ON CONFLICT DO NOTHING primitive with `set_by`
-- hard-wired to 'default' and no returned row.)
-- Guarded first write. The unique index (asset_id, field_id) IS the
-- precondition, so no read participates at all: a competing inserter
-- waits on the in-progress tuple and then takes DO NOTHING, and
-- exactly one row survives two overlapping attempts.
INSERT INTO asset_field_value (
    asset_id, field_id,
    value_text, value_num, value_date, value_options, value_ref,
    set_by, set_at, set_by_user_ref
) VALUES (
    sqlc.arg('asset_id'), sqlc.arg('field_id'),
    sqlc.narg('value_text'),
    sqlc.narg('value_num'),
    sqlc.narg('value_date'),
    sqlc.narg('value_options'),
    sqlc.narg('value_ref'),
    sqlc.arg('set_by'),
    NOW(),
    sqlc.narg('set_by_user_ref')
)
ON CONFLICT (asset_id, field_id) DO NOTHING
RETURNING asset_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

-- name: DeleteAssetFieldValueIfUnchanged :one
-- Guarded removal. RETURNING carries the row that was deleted, so the
-- history entry is written from what was actually removed rather than
-- from a snapshot taken before the statement ran.
DELETE FROM asset_field_value
WHERE asset_id = sqlc.arg('asset_id')
  AND field_id = sqlc.arg('field_id')
  AND set_at   = sqlc.arg('if_unchanged_since')
RETURNING asset_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

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

-- The collection twins of the three guarded asset statements. See
-- UpdateAssetFieldValueIfUnchanged for why the guard is the WHERE
-- clause rather than a handler-side read, and why each is `:one`.

-- name: UpdateCollectionFieldValueIfUnchanged :one
UPDATE collection_field_value SET
    value_text      = sqlc.narg('value_text'),
    value_num       = sqlc.narg('value_num'),
    value_date      = sqlc.narg('value_date'),
    value_options   = sqlc.narg('value_options'),
    value_ref       = sqlc.narg('value_ref'),
    set_by          = sqlc.arg('set_by'),
    set_at          = NOW(),
    set_by_user_ref = sqlc.narg('set_by_user_ref')
WHERE collection_id = sqlc.arg('collection_id')
  AND field_id      = sqlc.arg('field_id')
  AND set_at        = sqlc.arg('if_unchanged_since')
RETURNING collection_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

-- name: InsertCollectionFieldValueWhenAbsent :one
INSERT INTO collection_field_value (
    collection_id, field_id,
    value_text, value_num, value_date, value_options, value_ref,
    set_by, set_at, set_by_user_ref
) VALUES (
    sqlc.arg('collection_id'), sqlc.arg('field_id'),
    sqlc.narg('value_text'),
    sqlc.narg('value_num'),
    sqlc.narg('value_date'),
    sqlc.narg('value_options'),
    sqlc.narg('value_ref'),
    sqlc.arg('set_by'),
    NOW(),
    sqlc.narg('set_by_user_ref')
)
ON CONFLICT (collection_id, field_id) DO NOTHING
RETURNING collection_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

-- name: DeleteCollectionFieldValueIfUnchanged :one
DELETE FROM collection_field_value
WHERE collection_id = sqlc.arg('collection_id')
  AND field_id      = sqlc.arg('field_id')
  AND set_at        = sqlc.arg('if_unchanged_since')
RETURNING collection_id, field_id, value_text, value_num, value_date,
          value_options, value_ref, set_by, set_at, set_by_user_ref;

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

-- ---------------------------------------------------------------------------
-- Vocabulary curation — merge (ADR 0092 §4, #789)
-- ---------------------------------------------------------------------------

-- name: RewriteAssetValuesForMergedOption :execrows
-- Points every asset value naming @source at @target, and writes the
-- change into the per-value history in the same statement.
--
-- ## Why one statement rather than a loop in Go
--
-- A merge on a real vocabulary touches thousands of rows. Reading them
-- into Go, rewriting each and issuing an UPDATE apiece is thousands of
-- round trips inside one transaction, holding locks for the duration —
-- and the loop would still have to reimplement the array rewrite the
-- expression below does once.
--
-- ## Why the history row is not optional
--
-- A merge is the ONE vocabulary operation that edits records whose
-- owners did not touch them. Skipping the history entry would leave
-- `asset_field_value_history` describing a value the asset no longer
-- holds, and the audit event for the merge itself cannot fill that gap:
-- it says a merge happened, not what any particular asset now says. So
-- the same INSERT the ordinary write path performs runs here, sourced
-- from the CTE's before-and-after — which is also what makes :execrows
-- a truthful count, since one history row is inserted per value
-- rewritten.
--
-- set_by is 'computed': no human typed this value, and the VALUE row's
-- own set_by is deliberately left alone — a keyword that arrived from
-- IPTC is still an IPTC keyword after the term it names is renamed.
--
-- ## The array rewrite
--
-- array_replace alone is wrong when a row already holds BOTH terms:
-- {uk, gb} would become {gb, gb}. The unnest/DISTINCT ON/array_agg
-- sandwich keeps first-occurrence order and drops the duplicate, which
-- is the same shape the write path's slug dedupe produces.
--
-- The `search_text` TSVECTOR needs no help here: the AFTER UPDATE
-- trigger on asset_field_value rebuilds it per row.
WITH affected AS (
    SELECT p.asset_id, p.field_id, p.value_text AS old_text, p.value_options AS old_options
      FROM asset_field_value p
     WHERE p.field_id = @field_id
       AND (p.value_text = @source::text OR @source::text = ANY(p.value_options))
     ORDER BY p.asset_id
       FOR UPDATE
), updated AS (
    UPDATE asset_field_value v
       SET value_text = CASE WHEN v.value_text = @source::text THEN @target::text ELSE v.value_text END,
           value_options = CASE
               WHEN v.value_options IS NULL THEN NULL
               ELSE (
                   SELECT array_agg(d.slug ORDER BY d.ord)
                     FROM (
                         SELECT DISTINCT ON (u.slug) u.slug, u.ord
                           FROM unnest(array_replace(v.value_options, @source::text, @target::text))
                                WITH ORDINALITY AS u(slug, ord)
                          ORDER BY u.slug, u.ord
                     ) d
               )
           END
      FROM affected a
     WHERE v.asset_id = a.asset_id AND v.field_id = a.field_id
    RETURNING v.asset_id, v.field_id, v.value_text AS new_text, v.value_options AS new_options,
              a.old_text, a.old_options
)
INSERT INTO asset_field_value_history
    (asset_id, field_id, old_value, new_value, set_by, changed_by_user_ref)
SELECT u.asset_id, u.field_id,
       jsonb_build_object('type', @field_type::text, 'value',
           CASE WHEN u.old_text IS NOT NULL THEN to_jsonb(u.old_text) ELSE to_jsonb(u.old_options) END),
       jsonb_build_object('type', @field_type::text, 'value',
           CASE WHEN u.new_text IS NOT NULL THEN to_jsonb(u.new_text) ELSE to_jsonb(u.new_options) END),
       'computed', sqlc.narg(actor_user_ref)::bigint
  FROM updated u;

-- name: RewriteCollectionValuesForMergedOption :execrows
-- The collection twin of RewriteAssetValuesForMergedOption. Same
-- rewrite, same history guarantee, on the collection tables — a merge
-- that fixed assets and left collections naming a tombstoned term
-- would have moved the drift rather than removed it.
WITH affected AS (
    SELECT p.collection_id, p.field_id, p.value_text AS old_text, p.value_options AS old_options
      FROM collection_field_value p
     WHERE p.field_id = @field_id
       AND (p.value_text = @source::text OR @source::text = ANY(p.value_options))
     ORDER BY p.collection_id
       FOR UPDATE
), updated AS (
    UPDATE collection_field_value v
       SET value_text = CASE WHEN v.value_text = @source::text THEN @target::text ELSE v.value_text END,
           value_options = CASE
               WHEN v.value_options IS NULL THEN NULL
               ELSE (
                   SELECT array_agg(d.slug ORDER BY d.ord)
                     FROM (
                         SELECT DISTINCT ON (u.slug) u.slug, u.ord
                           FROM unnest(array_replace(v.value_options, @source::text, @target::text))
                                WITH ORDINALITY AS u(slug, ord)
                          ORDER BY u.slug, u.ord
                     ) d
               )
           END
      FROM affected a
     WHERE v.collection_id = a.collection_id AND v.field_id = a.field_id
    RETURNING v.collection_id, v.field_id, v.value_text AS new_text, v.value_options AS new_options,
              a.old_text, a.old_options
)
INSERT INTO collection_field_value_history
    (collection_id, field_id, old_value, new_value, set_by, changed_by_user_ref)
SELECT u.collection_id, u.field_id,
       jsonb_build_object('type', @field_type::text, 'value',
           CASE WHEN u.old_text IS NOT NULL THEN to_jsonb(u.old_text) ELSE to_jsonb(u.old_options) END),
       jsonb_build_object('type', @field_type::text, 'value',
           CASE WHEN u.new_text IS NOT NULL THEN to_jsonb(u.new_text) ELSE to_jsonb(u.new_options) END),
       'computed', sqlc.narg(actor_user_ref)::bigint
  FROM updated u;

-- name: RebuildAssetSearchTextForField :exec
-- Re-derives `assets.search_text` for every asset holding a value of
-- this field (#1016).
--
-- `rebuild_asset_search_text` folds a field's values into the document
-- only while `f.searchable = TRUE AND f.status = 'active'`, and the
-- trigger that calls it fires on writes to asset_field_value — not on
-- writes to field_definition. So flipping `searchable` off changed what
-- the rule SAID and nothing about what was already indexed: the field's
-- values kept answering text queries until something happened to touch
-- each asset. An operator who unticks the box has every reason to
-- believe they excluded the field, which is precisely the lie #1016
-- refused to let a UI toggle ship on top of.
--
-- Runs for a status change too, since `status = 'active'` is the other
-- conjunct of the same WHERE.
SELECT rebuild_asset_search_text(p.asset_id)
  FROM asset_field_value p
 WHERE p.field_id = $1;

-- name: LockFieldDisplayConditionGraph :exec
-- Serialises display-condition writes on ONE subject-kind graph
-- (#1173, #1119, ADR 0099 §8).
--
-- ⛔ THE CYCLE CHECK IS THEATRE WITHOUT THIS, and it is theatre in
-- exactly the case it exists for. A condition names OTHER definitions, so
-- the conditions on one subject kind form a directed GRAPH, and
-- "that graph is acyclic" is not a property of any single row: `A -> B`
-- and `B -> A` are each individually a perfectly valid row, which is why
-- no CHECK, no UNIQUE index and no per-row trigger can express it.
--
-- So two operators, one writing `A -> B` and one writing `B -> A`, each
-- read a graph in which the other's edge is not yet visible. Both
-- validate. Both commit. The graph now holds a 2-cycle that neither write
-- could have created on its own, and every later validation walks it.
--
-- ADVISORY rather than `SELECT ... FOR UPDATE` over the subject kind, for
-- two reasons. The invariant belongs to the WHOLE graph, so there is no
-- single row whose lock expresses it; and taking a row lock on every
-- definition of a subject kind would block unrelated field edits for the
-- duration of a graph walk. LockFieldDefinitionVocabulary above uses FOR
-- UPDATE because there the read-modify-write really is one row's options
-- document. This is the other case.
--
-- Transaction-scoped, so it is released by COMMIT or ROLLBACK and cannot
-- be leaked by an early return. Taken ONLY when a request actually
-- touches display_condition, so ordinary field edits never queue behind
-- it. The first key is a constant naming this lock space; the second is
-- the subject kind, mapped by the caller to a small integer rather than
-- hashed, so the value in pg_locks is readable by a person debugging one.
SELECT pg_advisory_xact_lock(sqlc.arg('lock_space')::INT, sqlc.arg('subject_key')::INT);

-- name: ListFieldDefinitionsForConditionGraph :many
-- EVERY field definition, reduced to what the display-condition
-- validator needs (#1173, ADR 0099 §6).
--
-- Read INSIDE the advisory lock, and read WHOLE rather than edge by edge:
-- a cycle closes on the third write of `A -> B`, `B -> C`, `C -> A`, so a
-- validator that only looked at the immediate edge would accept every one
-- of those three.
--
-- ⚠️ NO subject_kind FILTER, and that is not an oversight. Scoped to one
-- kind, a term naming a field of the OTHER kind is simply absent, and the
-- operator is told "this server does not have that field" — which is
-- false, and which would send them to create a duplicate. Reading both
-- kinds lets the validator answer the question they actually asked:
-- the field exists and describes the wrong sort of record.
--
-- Safe because `code` is UNIQUE across the whole table
-- (field_definition_code_key), so a code names one row and the graph has
-- no ambiguity to resolve. And the two kinds' graphs are genuinely
-- disjoint, because a cross-kind edge is refused at configuration time —
-- which is why the ADVISORY LOCK stays per subject kind even though this
-- read is not. The extra rows inform a REFUSAL and can never contribute
-- an edge, so a concurrent write on the other kind cannot break an
-- invariant here; at worst it changes which of two refusals an operator
-- sees.
--
-- ARCHIVED DEFINITIONS ARE INCLUDED, deliberately. They are excluded from
-- every composition surface but they are NOT excluded from the graph: an
-- archived dependent keeps its stored configuration (ADR 0099 §7), so its
-- edges still exist and a cycle through it is still a cycle. The
-- CONTROLLER-side status rule is a separate check in Go, which refuses an
-- already-archived controller at configuration time while leaving a
-- previously valid condition alone when the controller is archived later.
SELECT id, code, type, subject_kind, status, applies_to, mirrors_column,
       display_condition
  FROM field_definition;

-- name: ListFieldDefinitionsForComposition :many
-- The definitions a composition surface may draw for one subject kind,
-- reduced to the identity plus the read gate (#1173, ADR 0099 §5).
--
-- Backs GET /assets/{id}/field-composition and
-- GET /collections/{id}/field-composition, which report EFFECTIVE,
-- SERVER-DERIVED readability per field and carry no values whatsoever.
-- Selecting `read_capability` and nothing else from the gate side is the
-- point: the caller-facing shape has no member a stored value could be
-- put in, so non-disclosure is structural rather than a rule somebody has
-- to remember.
--
-- ARCHIVED EXCLUDED, matching the status semantics of
-- ListFieldDefinitions with no explicit status (#528): archived
-- definitions are tombstones and never appear on a composition surface,
-- so reporting readability for one would describe a control that is not
-- there. A controller archived after a valid configuration therefore
-- resolves to nothing here, which is exactly what makes the dependent
-- fail open.
SELECT id, code, read_capability
  FROM field_definition
 WHERE subject_kind = $1
   AND status <> 'archived'
 ORDER BY code;

-- name: GetAssetTeamForFieldComposition :one
-- The team an asset belongs to, for the team-scoped half of effective
-- field readability (#1173, ADR 0099 §5).
--
-- `assets.team_id` is NULLABLE and a NULL is not "no scope required": a
-- team-less asset SKIPS the scoped disjunct entirely and is answered by
-- the caller's GLOBAL holding alone, which is the same nullable trap
-- `hasAssetCapability` documents on the assets side. Collections have no
-- team column at all, so they have no counterpart to this query and their
-- readability is the global answer by construction.
SELECT team_id FROM assets WHERE id = $1;

-- ---------------------------------------------------------------------------
-- Batch metadata edit (#1173, #1119, ADR 0019)
-- ---------------------------------------------------------------------------

-- name: ExpandPostsToAssets :many
-- Membership expansion for a batch selection's `post` entries.
--
-- SERVER-SIDE on purpose. A client that expanded posts itself would be
-- sending an asset list the server has to trust, and the whole reason
-- the selection is typed is that the server, not the client, decides
-- what a post means. Soft-deleted posts and soft-deleted assets are
-- excluded here rather than partitioned later: they were never targets,
-- so counting them as `gone` would report an outcome for something the
-- operator never selected.
--
-- DISTINCT because an asset can sit in several selected posts and is
-- one target however many routes reach it. The ORDER BY is the
-- deterministic order the whole contract rests on — preview and apply
-- derive the identical ordered set from it, which is why it is asset id
-- and not sort_order (mutable) or selection order (client-supplied).
SELECT DISTINCT pa.asset_id
  FROM post_assets pa
  JOIN posts p ON p.id = pa.post_id
  JOIN assets a ON a.id = pa.asset_id
 WHERE pa.post_id = ANY(@post_ids::uuid[])
   AND p.deleted_at IS NULL
   AND a.deleted_at IS NULL
 ORDER BY pa.asset_id;

-- name: ListPostsWithMembers :many
-- Which of the selected posts actually hold at least one live member,
-- so the preview can REPORT the ones that hold none rather than
-- silently dropping them. A selected post that contributes no target is
-- a thing the operator should see.
SELECT DISTINCT pa.post_id
  FROM post_assets pa
  JOIN posts p ON p.id = pa.post_id
  JOIN assets a ON a.id = pa.asset_id
 WHERE pa.post_id = ANY(@post_ids::uuid[])
   AND p.deleted_at IS NULL
   AND a.deleted_at IS NULL;

-- name: ListBatchTargetSubjects :many
-- The batch's PREVIEW-side subject probe: owner, team and asset type
-- for every selected target, in the deterministic order.
--
-- Not GetAssetMutationSubject in a loop — that is one round trip per
-- target, and at the 1000-target ceiling the difference is the whole
-- latency budget. Same projection and the same `deleted_at IS NULL`
-- filter, so the two answer the same question about liveness: only a
-- SOFT DELETE removes a subject from the probe. An ARCHIVED asset comes
-- back and is written, because archive is not deletion.
--
-- No row lock. This is the preview, which writes nothing and therefore
-- has nothing to make atomic; the apply takes its own locked read per
-- target (see LockBatchTargetSubject).
SELECT id, owner_user_ref, team_id, asset_type
  FROM assets
 WHERE id = ANY(@asset_ids::uuid[])
   AND deleted_at IS NULL
 ORDER BY id;

-- name: LockBatchTargetSubject :one
-- The batch's APPLY-side subject probe, and the seam that makes the
-- per-target subject invariant atomic (#1173, ADR 0019).
--
-- FOR SHARE, and the row lock is the entire point.
--
-- "Inside the same transaction" is NOT sufficient at READ COMMITTED.
-- The precondition this read establishes lives on `assets` while the
-- mutation it authorises lands on `asset_field_value`, a DIFFERENT
-- table, so 20a's single-statement guarded-update pattern does not
-- transfer: there is no one statement that can both test the owner and
-- write the value. Nor does the FK from asset_field_value.asset_id
-- help. Its implicit lock is FOR KEY SHARE, which conflicts only with
-- FOR UPDATE, while an ownership transfer, a team move and a soft
-- delete all take FOR NO KEY UPDATE — so the FK lets every one of them
-- slip between this read and the write it authorises.
--
-- FOR SHARE DOES conflict with FOR NO KEY UPDATE. Taking it here means
-- the read BLOCKS until any in-flight transfer, team move or soft
-- delete of this asset has committed, then sees the committed truth,
-- and HOLDS the row against a later one until this batch commits. The
-- authority verdict and the write it authorises become one atomic
-- operation, which is what the invariant says they are.
--
-- Read BEFORE the write and inside the transaction, for the reason
-- UpdateAsset states on the assets plane.
SELECT id, owner_user_ref, team_id, asset_type
  FROM assets
 WHERE id = $1
   AND deleted_at IS NULL
 FOR SHARE;

-- name: LockBatchReferenceTarget :one
-- The proposed reference target's liveness, held for the batch.
--
-- Same FOR SHARE mechanism and the same reason, one plane over: THERE
-- IS NO FOREIGN KEY ON `value_ref` (asset_field_value has exactly two,
-- on asset_id and field_id), so nothing in the schema stops the target
-- being soft-deleted midway through a thousand writes that point at it.
-- A pre-batch re-check is not sufficient — it establishes a fact that
-- can stop being true before the last write lands. The lock makes the
-- liveness verdict and every write using it atomic.
--
-- `deleted_at IS NULL` and NEVER `status`: an ARCHIVED asset is a valid
-- reference target, exactly as GetReferencedAsset has it. Archive is not
-- deletion on this plane either.
SELECT id
  FROM assets
 WHERE id = $1
   AND deleted_at IS NULL
 FOR SHARE;

-- name: LockFieldDefinitionForBatch :one
-- The batch-wide definition, configuration and vocabulary seam.
--
-- FOR UPDATE on the field_definition row, taken BEFORE the batch reads
-- anything about the field. Every writer of that row — UpdateField,
-- ArchiveField, the options editor, EnsureOpenVocabularyTerms' own
-- LockFieldDefinitionVocabulary — either takes FOR UPDATE or issues an
-- UPDATE, which takes FOR NO KEY UPDATE, and both conflict with this.
-- So there are EXACTLY TWO valid serial outcomes: the external change
-- wins and the batch reads it and refuses batch-wide with zero writes
-- and no mint, or the batch wins and the ENTIRE batch executes under
-- one validated state with the external change following it. The
-- forbidden third — the first N targets written under the old rules and
-- the rest under the new ones — cannot happen.
--
-- Lock BEFORE the read, not after. A lock taken after would serialise
-- the writes while still letting the batch validate against a
-- definition that has already changed, which is the failure mode
-- display_condition_race_test.go's header names exactly.
SELECT id, code, label, type, subject_kind, applies_to, required, status,
       options, open_vocabulary, mirrors_column, read_only, regexp_filter,
       read_capability, write_capability, display_condition
  FROM field_definition
 WHERE id = $1
 FOR UPDATE;

-- name: ListBatchTargetValues :many
-- Every stored value for one field across the batch's targets, in one
-- round trip. Targets holding no value simply do not come back, and
-- absence is the emptiness the fill_empties mode is about.
SELECT asset_id, value_text, value_num, value_date, value_options, value_ref, set_at
  FROM asset_field_value
 WHERE field_id = $1
   AND asset_id = ANY(@asset_ids::uuid[]);

-- name: InsertBatchPreview :one
-- Mint one preview token's durable binding. `token_hash` and never the
-- token: a database that has never held the bearer secret cannot leak
-- it, which is the same reason session tokens are stored hashed.
INSERT INTO metadata_batch_preview
    (token_hash, caller_user_ref, field_id, mode, would_change, payload, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, created_at, expires_at;

-- name: GetBatchPreviewByTokenHash :one
-- The unlocked read behind apply's steps 1 through 5. It answers for a
-- row belonging to ANY caller on purpose: the caller-binding comparison
-- is made in Go, and it must be made on the SAME code path for a row
-- that exists and one that does not, so that "belongs to somebody else"
-- and "does not exist" cannot be told apart by timing or by shape.
SELECT id, caller_user_ref, field_id, mode, would_change, payload,
       created_at, expires_at, consumed_at
  FROM metadata_batch_preview
 WHERE token_hash = $1;

-- name: ConsumeBatchPreview :one
-- THE SINGLE-USE LATCH, and the reason consumption cannot be lost.
--
-- Run INSIDE the apply transaction, before any field write. The
-- predicate and the mutation are ONE statement, so two concurrent
-- replays of one token cannot both see it unconsumed: the second blocks
-- on the row lock the first took, and when it proceeds it matches zero
-- rows and reports the replay. A handler-side "read, compare, update"
-- would let both through at READ COMMITTED.
--
-- Because it lives in the apply's transaction, a refusal that rolls
-- that transaction back also rolls this back: a pre-write refusal
-- leaves the token spendable, and a committed apply spends it in the
-- same durable outcome as its writes and its audit envelope. There is
-- no third result.
UPDATE metadata_batch_preview
   SET consumed_at = NOW()
 WHERE id = $1
   AND consumed_at IS NULL
RETURNING id, consumed_at;

-- name: PurgeExpiredBatchPreviews :exec
-- Opportunistic sweep from the preview endpoint, so the table stays
-- bounded without a scheduler.
--
-- The cutoff is well past expiry, not at it: a token that has just
-- expired must still be found, so that its own caller gets 409
-- preview_expired rather than the 403 an unattributable credential
-- gets. Past the cutoff the distinction stops being useful — the token
-- is hours dead — and 403 is the right answer for a credential the
-- server can no longer attribute to anybody.
DELETE FROM metadata_batch_preview
 WHERE expires_at < NOW() - INTERVAL '24 hours';
