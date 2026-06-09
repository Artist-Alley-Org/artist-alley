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
