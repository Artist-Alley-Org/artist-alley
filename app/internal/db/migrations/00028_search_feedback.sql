-- Phase 1.16.B-5-followup — search-result feedback loop.
--
-- Records thumbs up/down from authenticated users on individual
-- search-result hits. Powers three admin-facing aggregations:
--   1. Queries with most down-votes (candidate ranking bugs).
--   2. Under-ranked hits (thumbs-up from deep positions).
--   3. Per-user audit view for abuse review.
--
-- Design decisions:
--
--   - `query_hash` is the SHA-256 of a canonicalized DSL string
--     (trimmed + lowercased + whitespace-collapsed). Same-semantic
--     variants collapse; full AST canonicalization is out of scope.
--     The raw `dsl_query` is retained alongside so the admin UI can
--     render human-readable queries in the aggregation view.
--   - UNIQUE (user_ref, hit_asset_id, query_hash) enforces vote-
--     flipping: a re-vote against the same (asset, query) pair
--     updates the row's direction via ON CONFLICT DO UPDATE rather
--     than duplicating.
--   - `hit_position` is 1-indexed at time of vote. Ranking changes
--     between vote and admin review don't retroactively invalidate
--     the signal.
--   - `ip_hash` is the same /24 IPv4 or /56 IPv6 subnet hash the
--     1.19.D lockout audits use — records threat class for admin
--     correlation without a per-IP audit log.
--   - Never federates. No origin_server_id column. Feedback is
--     per-instance ranking signal.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE search_feedback (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    query_hash    TEXT NOT NULL,
    dsl_query     TEXT NOT NULL,
    hit_asset_id  UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    hit_position  INTEGER NOT NULL CHECK (hit_position >= 1),
    direction     TEXT NOT NULL CHECK (direction IN ('up','down')),
    user_ref      BIGINT NOT NULL REFERENCES "user"(ref) ON DELETE CASCADE,
    ip_hash       TEXT,
    feedback_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_ref, hit_asset_id, query_hash)
);

-- Aggregation by query_hash (top down-voted queries in a time window).
CREATE INDEX search_feedback_query_hash_idx
    ON search_feedback (query_hash);

-- Aggregation by hit_asset_id (under-ranked hits + admin correlation).
CREATE INDEX search_feedback_hit_asset_idx
    ON search_feedback (hit_asset_id);

-- Time-window scans (last 7d aggregation + per-user daily cap).
CREATE INDEX search_feedback_feedback_at_idx
    ON search_feedback (feedback_at DESC);

-- Per-user daily-cap query hits this: WHERE user_ref = $1 AND
-- feedback_at > NOW() - INTERVAL '24 hours'.
CREATE INDEX search_feedback_user_ref_at_idx
    ON search_feedback (user_ref, feedback_at DESC);
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DROP INDEX IF EXISTS search_feedback_user_ref_at_idx;
DROP INDEX IF EXISTS search_feedback_feedback_at_idx;
DROP INDEX IF EXISTS search_feedback_hit_asset_idx;
DROP INDEX IF EXISTS search_feedback_query_hash_idx;
DROP TABLE IF EXISTS search_feedback;
-- +goose StatementEnd
