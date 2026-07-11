-- name: UpsertRemoteActor :one
-- The inbound dispatch handler upserts on every inbound activity
-- from a remote actor so display info (display_name, avatar_url)
-- refreshes naturally. ON CONFLICT updates display fields +
-- bumps last_seen_at; first_seen_at stays at its original
-- insertion timestamp.
INSERT INTO federation_remote_actors (
    actor_uri, peer_id, display_name, avatar_url
)
VALUES ($1, $2, $3, $4)
ON CONFLICT (actor_uri) DO UPDATE
SET display_name = EXCLUDED.display_name,
    avatar_url   = EXCLUDED.avatar_url,
    peer_id      = EXCLUDED.peer_id,
    last_seen_at = NOW(),
    updated_at   = NOW()
RETURNING actor_uri, peer_id, display_name, avatar_url,
          first_seen_at, last_seen_at, updated_at;

-- name: GetRemoteActor :one
SELECT actor_uri, peer_id, display_name, avatar_url,
       first_seen_at, last_seen_at, updated_at
FROM federation_remote_actors
WHERE actor_uri = $1;

-- name: ListRemoteActorsByPeer :many
-- Admin per-peer view. Most-recent-active first.
SELECT actor_uri, peer_id, display_name, avatar_url,
       first_seen_at, last_seen_at, updated_at
FROM federation_remote_actors
WHERE peer_id = $1
ORDER BY last_seen_at DESC
LIMIT $2;

-- --- Phase 1.22.I-c — remote-actor encryption-key cache --------------

-- name: SetRemoteActorEncryptionKey :execrows
-- Writes the encryption-key block on an EXISTING remote_actor
-- row. UpsertRemoteActor (the display-info upsert) is the
-- prerequisite — call it first on every inbound activity so the
-- row exists. Returns rowcount; 0 means the actor URI wasn't in
-- the table (caller surfaces as ErrNoActor).
--
-- Callers that want change-detection for the audit event read
-- GetRemoteActorEncryptionKey first + compare the result. The
-- atomic CHECK constraint on the table (migration 00008) means
-- all three columns move together — we can't partial-write.
UPDATE federation_remote_actors
   SET encryption_public_key            = $2,
       encryption_public_key_version    = $3,
       encryption_public_key_updated_at = NOW()
 WHERE actor_uri = $1;

-- name: GetRemoteActorEncryptionKey :one
-- Reader path used by I-e outbox encryption + the I-c
-- Handler.GetEncryptionKey cache miss. Returns the actor's
-- current encryption_public_key block when present; pgx.ErrNoRows
-- when the actor row doesn't exist OR the row exists but the key
-- column is NULL. Callers needing to distinguish those cases run
-- GetRemoteActor in tandem.
SELECT encryption_public_key,
       encryption_public_key_version,
       encryption_public_key_updated_at
  FROM federation_remote_actors
 WHERE actor_uri = $1
   AND encryption_public_key IS NOT NULL;

-- name: CountRemoteActorsMissingEncryptionKey :one
-- Operator observability. Backed by the
-- federation_remote_actors_missing_encryption_key_idx partial
-- index so the count is cheap regardless of total actor volume.
SELECT count(*) FROM federation_remote_actors
 WHERE encryption_public_key IS NULL;
