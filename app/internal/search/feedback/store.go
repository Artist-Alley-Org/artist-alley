package feedback

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps the search_feedback table. Hand-written SQL (not sqlc)
// so the ON CONFLICT DO UPDATE path is readable + the aggregation
// queries can join against `assets` without extending sqlc's model
// surface. Mirrors reindex.Store + visualbackfill.Store shape.
type Store struct {
	Pool *pgxpool.Pool
}

// NewStore constructs a Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// Upsert inserts a new feedback row OR flips the direction on an
// existing one (via ON CONFLICT DO UPDATE against the unique
// (user_ref, hit_asset_id, query_hash) constraint). Returns the row
// id + resolved direction + a Flipped flag telling the caller
// whether the pre-existing row was updated (true) or a fresh row
// was inserted (false).
func (s *Store) Upsert(ctx context.Context, p SubmitParams) (SubmitResult, error) {
	queryHash := HashDSL(p.DSL)
	var ipHashRef *string
	if p.IPHash != "" {
		ipHashRef = &p.IPHash
	}
	var (
		id        uuid.UUID
		direction string
		inserted  bool
	)
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO search_feedback
		    (query_hash, dsl_query, hit_asset_id, hit_position,
		     direction, user_ref, ip_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_ref, hit_asset_id, query_hash) DO UPDATE
		    SET direction   = EXCLUDED.direction,
		        hit_position = EXCLUDED.hit_position,
		        ip_hash     = EXCLUDED.ip_hash,
		        feedback_at = NOW()
		RETURNING id, direction, (xmax = 0) AS inserted
	`,
		queryHash,
		p.DSL,
		p.HitAssetID,
		p.HitPosition,
		string(p.Direction),
		p.UserRef,
		ipHashRef,
	).Scan(&id, &direction, &inserted)
	if err != nil {
		return SubmitResult{}, fmt.Errorf("feedback.Upsert: %w", err)
	}
	return SubmitResult{
		ID:        id,
		Direction: Direction(direction),
		Flipped:   !inserted,
	}, nil
}

// CountUserSince returns the number of feedback rows for a user with
// feedback_at > cutoff. Used by the rate-limit check; hits the
// (user_ref, feedback_at DESC) index for the daily-cap query.
func (s *Store) CountUserSince(ctx context.Context, userRef int64, cutoff time.Time) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		  FROM search_feedback
		 WHERE user_ref = $1 AND feedback_at > $2
	`, userRef, cutoff).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteOwn removes a feedback row IFF the caller owns it. Returns
// ErrNotFound if the row doesn't exist OR belongs to another user
// (enumeration-safe conflation).
func (s *Store) DeleteOwn(ctx context.Context, id uuid.UUID, userRef int64) error {
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM search_feedback
		 WHERE id = $1 AND user_ref = $2
	`, id, userRef)
	if err != nil {
		return fmt.Errorf("feedback.DeleteOwn: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TopQueriesByDownvote returns the top-N query_hash values by
// down-vote count within the aggregation window. Anonymized —
// no user_ref surfaces. DSL query is taken from the most recent
// row's dsl_query column (there may be minor formatting variance
// between rows sharing a query_hash; the most recent is a
// reasonable "canonical" for display).
func (s *Store) TopQueriesByDownvote(ctx context.Context, window time.Duration, limit int32) ([]TopQueryRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `
		WITH totals AS (
		    SELECT query_hash,
		           COUNT(*)                                            AS total_votes,
		           COUNT(*) FILTER (WHERE direction = 'down')          AS down_votes
		      FROM search_feedback
		     WHERE feedback_at > NOW() - $1::INTERVAL
		     GROUP BY query_hash
		),
		latest_dsl AS (
		    SELECT DISTINCT ON (query_hash) query_hash, dsl_query
		      FROM search_feedback
		     WHERE feedback_at > NOW() - $1::INTERVAL
		  ORDER BY query_hash, feedback_at DESC
		)
		SELECT t.query_hash,
		       l.dsl_query,
		       t.total_votes,
		       t.down_votes,
		       (t.down_votes::REAL / NULLIF(t.total_votes, 0)) AS down_pct
		  FROM totals t
		  JOIN latest_dsl l USING (query_hash)
		 WHERE t.down_votes > 0
		 ORDER BY t.down_votes DESC, t.total_votes DESC
		 LIMIT $2
	`, intervalString(window), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TopQueryRow, 0, limit)
	for rows.Next() {
		var r TopQueryRow
		var pct *float32
		if err := rows.Scan(&r.QueryHash, &r.DSLQuery, &r.TotalVotes, &r.DownVotes, &pct); err != nil {
			return nil, err
		}
		if pct != nil {
			r.DownVotePct = *pct
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnderRankedHits returns thumbs-up rows where the reported position
// suggests the ranker buried a good result — hits average position
// > 5 across all voters, restricted to the aggregation window.
// Joins assets to pull the human-readable title for the admin view.
func (s *Store) UnderRankedHits(ctx context.Context, window time.Duration, minPosition int32, limit int32) ([]UnderRankedHitRow, error) {
	if limit <= 0 {
		limit = 20
	}
	if minPosition <= 0 {
		minPosition = 5
	}
	rows, err := s.Pool.Query(ctx, `
		WITH grouped AS (
		    SELECT sf.hit_asset_id,
		           sf.query_hash,
		           AVG(sf.hit_position)::DOUBLE PRECISION AS avg_pos,
		           COUNT(*)                                AS up_votes
		      FROM search_feedback sf
		     WHERE sf.feedback_at > NOW() - $1::INTERVAL
		       AND sf.direction = 'up'
		     GROUP BY sf.hit_asset_id, sf.query_hash
		    HAVING AVG(sf.hit_position) > $2
		),
		latest_dsl AS (
		    SELECT DISTINCT ON (query_hash) query_hash, dsl_query
		      FROM search_feedback
		     WHERE feedback_at > NOW() - $1::INTERVAL
		  ORDER BY query_hash, feedback_at DESC
		)
		SELECT g.hit_asset_id,
		       g.query_hash,
		       l.dsl_query,
		       g.avg_pos,
		       g.up_votes,
		       COALESCE(a.title, '')
		  FROM grouped g
		  JOIN latest_dsl l USING (query_hash)
		  LEFT JOIN assets a ON a.id = g.hit_asset_id
		 ORDER BY g.avg_pos DESC, g.up_votes DESC
		 LIMIT $3
	`, intervalString(window), float64(minPosition), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UnderRankedHitRow, 0, limit)
	for rows.Next() {
		var r UnderRankedHitRow
		if err := rows.Scan(&r.HitAssetID, &r.QueryHash, &r.DSLQuery, &r.AvgPos, &r.UpVotes, &r.AssetTitle); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListForUser returns the abuse-review per-user feedback log, most-
// recent first. Joins assets for asset title display.
func (s *Store) ListForUser(ctx context.Context, userRef int64, limit int32) ([]PerUserRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT sf.id, sf.query_hash, sf.dsl_query, sf.hit_asset_id,
		       sf.hit_position, sf.direction, sf.user_ref, sf.ip_hash,
		       sf.feedback_at, COALESCE(a.title, '')
		  FROM search_feedback sf
		  LEFT JOIN assets a ON a.id = sf.hit_asset_id
		 WHERE sf.user_ref = $1
		 ORDER BY sf.feedback_at DESC
		 LIMIT $2
	`, userRef, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]PerUserRow, 0, limit)
	for rows.Next() {
		var r PerUserRow
		var directionStr string
		if err := rows.Scan(
			&r.ID, &r.QueryHash, &r.DSLQuery, &r.HitAssetID,
			&r.HitPosition, &directionStr, &r.UserRef, &r.IPHash,
			&r.FeedbackAt, &r.AssetTitle,
		); err != nil {
			return nil, err
		}
		r.Direction = Direction(directionStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveVoters returns the count of DISTINCT user_ref values with
// at least one feedback row in the last `window`. Used by the
// /admin/search/health gauge callback.
func (s *Store) ActiveVoters(ctx context.Context, window time.Duration) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT user_ref)
		  FROM search_feedback
		 WHERE feedback_at > NOW() - $1::INTERVAL
	`, intervalString(window)).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}

// intervalString formats a Go duration as a PostgreSQL interval
// literal. Postgres accepts "N seconds" fine; keeps type
// compatibility with the query's ::INTERVAL cast.
func intervalString(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
