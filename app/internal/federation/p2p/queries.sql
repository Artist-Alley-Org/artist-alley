-- name: ListPeerSuggestions :many
-- Admin "suggested peers" feed. Joined against federation_peers at
-- the registry layer to filter out URLs we already federate with —
-- the table doesn't carry that join because peers can come and go
-- and the filter changes over time.
SELECT id, source_peer_id, suggested_url, suggested_display_name,
       suggested_public_key, suggested_fingerprint, cached_at
FROM federation_peer_suggestions
ORDER BY cached_at DESC, id DESC
LIMIT $1;

-- name: ListPeerSuggestionsBySource :many
-- Per-source-peer feed used by the cache loader.
SELECT id, source_peer_id, suggested_url, suggested_display_name,
       suggested_public_key, suggested_fingerprint, cached_at
FROM federation_peer_suggestions
WHERE source_peer_id = $1
ORDER BY cached_at DESC;

-- name: UpsertPeerSuggestion :exec
-- Refresh worker calls this per item returned by a source's
-- /federation/peers/visible. UPSERT by (source, url) so re-fetching
-- doesn't duplicate.
INSERT INTO federation_peer_suggestions (
    source_peer_id, suggested_url, suggested_display_name,
    suggested_public_key, suggested_fingerprint
)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (source_peer_id, suggested_url) DO UPDATE
SET suggested_display_name = EXCLUDED.suggested_display_name,
    suggested_public_key   = EXCLUDED.suggested_public_key,
    suggested_fingerprint  = EXCLUDED.suggested_fingerprint,
    cached_at              = NOW();

-- name: DeleteSuggestionsBySourceNotIn :exec
-- After a successful refresh, drop suggestions the source has
-- silently de-listed (mirrors the directory-entries refresh
-- pattern). $2 is a JSONB array of URLs to KEEP.
DELETE FROM federation_peer_suggestions
WHERE source_peer_id = $1
  AND suggested_url NOT IN (
      SELECT jsonb_array_elements_text(sqlc.arg('keep_urls')::JSONB)
  );

-- name: ClearAllSuggestionsBySource :exec
-- Used when a refresh attempt fails — we DON'T clear, but provided
-- here for the "remove from cache" admin action.
DELETE FROM federation_peer_suggestions
WHERE source_peer_id = $1;
