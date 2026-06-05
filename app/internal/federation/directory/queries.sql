-- name: ListDirectories :many
-- Admin "subscribed directories" list, newest-first.
SELECT id, directory_url, operator_name, operator_public_key,
       operator_fingerprint, operator_contact,
       subscribed_at, subscribed_by_user_ref, enabled,
       last_polled_at, last_poll_status, last_poll_error,
       poll_interval_seconds, notes, created_at, updated_at
FROM federation_directories
ORDER BY subscribed_at DESC;

-- name: GetDirectoryByID :one
SELECT id, directory_url, operator_name, operator_public_key,
       operator_fingerprint, operator_contact,
       subscribed_at, subscribed_by_user_ref, enabled,
       last_polled_at, last_poll_status, last_poll_error,
       poll_interval_seconds, notes, created_at, updated_at
FROM federation_directories
WHERE id = $1;

-- name: GetDirectoryByURL :one
-- Idempotency check + the auth-shaped lookup the polling worker
-- uses.
SELECT id, directory_url, operator_name, operator_public_key,
       operator_fingerprint, operator_contact,
       subscribed_at, subscribed_by_user_ref, enabled,
       last_polled_at, last_poll_status, last_poll_error,
       poll_interval_seconds, notes, created_at, updated_at
FROM federation_directories
WHERE directory_url = $1;

-- name: InsertDirectory :one
INSERT INTO federation_directories (
    directory_url,
    operator_name,
    operator_public_key,
    operator_fingerprint,
    operator_contact,
    subscribed_by_user_ref,
    notes
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, directory_url, operator_name, operator_public_key,
          operator_fingerprint, operator_contact,
          subscribed_at, subscribed_by_user_ref, enabled,
          last_polled_at, last_poll_status, last_poll_error,
          poll_interval_seconds, notes, created_at, updated_at;

-- name: UpdateDirectoryPollOutcome :exec
-- Polling worker writes the outcome of a poll cycle here. Status
-- + error are paired (status='ok' → error=''; failure status →
-- error has details).
UPDATE federation_directories
SET last_polled_at   = NOW(),
    last_poll_status = $2,
    last_poll_error  = $3,
    updated_at       = NOW()
WHERE id = $1;

-- name: SetDirectoryEnabled :one
UPDATE federation_directories
SET enabled    = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING id, directory_url, operator_name, operator_public_key,
          operator_fingerprint, operator_contact,
          subscribed_at, subscribed_by_user_ref, enabled,
          last_polled_at, last_poll_status, last_poll_error,
          poll_interval_seconds, notes, created_at, updated_at;

-- name: DeleteDirectory :exec
-- Unsubscribe. ON DELETE CASCADE on federation_directory_entries
-- drops the cached entries automatically.
DELETE FROM federation_directories WHERE id = $1;

-- name: ListDirectoryEntries :many
-- Browse cached entries for one directory, ordered by recency
-- of verification.
SELECT id, directory_id, instance_url, display_name,
       instance_public_key, fingerprint, region, description,
       tags, verified_at, verified_via, listing_id, cached_at
FROM federation_directory_entries
WHERE directory_id = $1
ORDER BY verified_at DESC, id DESC
LIMIT $2;

-- name: UpsertDirectoryEntry :exec
-- Polling worker calls this per entry returned by a successful
-- poll. UPSERT semantics: re-polling overwrites by
-- (directory_id, instance_url).
INSERT INTO federation_directory_entries (
    directory_id, instance_url, display_name, instance_public_key,
    fingerprint, region, description, tags, verified_at,
    verified_via, listing_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (directory_id, instance_url) DO UPDATE
SET display_name        = EXCLUDED.display_name,
    instance_public_key = EXCLUDED.instance_public_key,
    fingerprint         = EXCLUDED.fingerprint,
    region              = EXCLUDED.region,
    description         = EXCLUDED.description,
    tags                = EXCLUDED.tags,
    verified_at         = EXCLUDED.verified_at,
    verified_via        = EXCLUDED.verified_via,
    listing_id          = EXCLUDED.listing_id,
    cached_at           = NOW();

-- name: DeleteDirectoryEntriesNotIn :exec
-- After a successful poll, any cached entry whose instance_url
-- isn't in the fresh set gets removed (the directory de-listed
-- it). Takes a directory_id + a JSONB array of URLs to KEEP.
DELETE FROM federation_directory_entries
WHERE directory_id = $1
  AND instance_url NOT IN (
      SELECT jsonb_array_elements_text(sqlc.arg('keep_urls')::JSONB)
  );
