package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// DefaultLimit is the /search page size when the caller doesn't
// specify one. Matches the browse-page default.
const DefaultLimit = 25

// MaxLimit caps caller-supplied limit — the handler rejects any
// value above this. Keeps a single request from monopolising the
// per-user rate-limit budget.
const MaxLimit = 100

// TotalCountCap is the number of exact matches we're willing to
// count. Beyond this the endpoint returns "10,000+" — matches the
// Google/GitHub/Linear pattern. Under the cap the count is exact.
const TotalCountCap = 10000

// Engine executes the unified search. Stateless + concurrency-
// safe; one per process. Constructed by boot; callers hand it a
// prepared Query and get back a QueryResult.
type Engine struct {
	Pool *pgxpool.Pool
}

// NewEngine constructs an Engine bound to the shared pool.
func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{Pool: pool}
}

// Run executes the query. Emits per-entity queries in parallel,
// normalises scores, merges, orders, cuts to the page, computes
// the next cursor, and stamps total counts.
func (e *Engine) Run(ctx context.Context, q Query) (QueryResult, error) {
	if q.Text == "" {
		return QueryResult{}, ErrEmptyQuery
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	types := q.Types
	if len(types) == 0 {
		types = AllHitTypes()
	}

	// The per-entity queries each pull `limit * some multiplier`
	// so the cross-entity merge has enough headroom to sort. A 3x
	// multiplier keeps a single entity from monopolising the page
	// while still bounding the work.
	perEntityLimit := limit * 3
	if perEntityLimit > MaxLimit*3 {
		perEntityLimit = MaxLimit * 3
	}

	rawHits := make([]Hit, 0, perEntityLimit*len(types))
	perTypeCount := make(map[HitType]int, len(types))
	maxScoreByType := make(map[HitType]float64, len(types))

	for _, t := range types {
		hits, total, err := e.runOne(ctx, t, q, perEntityLimit)
		if err != nil {
			return QueryResult{}, fmt.Errorf("search: run %s: %w", t, err)
		}
		perTypeCount[t] = total
		var max float64
		for i := range hits {
			if hits[i].RawScore > max {
				max = hits[i].RawScore
			}
		}
		maxScoreByType[t] = max
		rawHits = append(rawHits, hits...)
	}

	// Normalise scores by per-entity max so cross-entity ordering
	// is on the same [0,1] scale. If an entity had zero hits we
	// leave the max at 0 and skip normalisation for that (empty)
	// group — no divide-by-zero.
	for i := range rawHits {
		mx := maxScoreByType[rawHits[i].Type]
		if mx > 0 {
			rawHits[i].NormalisedScore = rawHits[i].RawScore / mx
		}
	}

	// Order by (normalised_score DESC, id DESC, type DESC) so
	// the cursor's tie-breaker is a total order.
	sort.SliceStable(rawHits, func(i, j int) bool {
		if rawHits[i].NormalisedScore != rawHits[j].NormalisedScore {
			return rawHits[i].NormalisedScore > rawHits[j].NormalisedScore
		}
		if rawHits[i].ID != rawHits[j].ID {
			return rawHits[i].ID.String() > rawHits[j].ID.String()
		}
		return rawHits[i].Type > rawHits[j].Type
	})

	// Apply the cursor cut — drop everything at-or-above the
	// last-page position. The cursor was emitted from the prior
	// call so the comparison is strict-less-than on the tuple.
	if q.Cursor != nil {
		cut := *q.Cursor
		filtered := rawHits[:0]
		for _, h := range rawHits {
			if cursorLess(h, cut) {
				filtered = append(filtered, h)
			}
		}
		rawHits = filtered
	}

	// Cut to the page + compute next cursor from the tail.
	var next *Cursor
	if len(rawHits) > limit {
		tail := rawHits[limit-1]
		next = &Cursor{
			LastScore: tail.NormalisedScore,
			LastID:    tail.ID,
			LastType:  tail.Type,
		}
		rawHits = rawHits[:limit]
	}

	// Total count: sum per-entity totals; cap flag if any entity
	// reported a cap or the sum exceeded the cap.
	totalCount := 0
	capped := false
	for _, c := range perTypeCount {
		if c >= TotalCountCap {
			capped = true
		}
		totalCount += c
	}
	if totalCount >= TotalCountCap {
		capped = true
		totalCount = TotalCountCap
	}

	return QueryResult{
		Hits:             rawHits,
		NextCursor:       next,
		TotalCount:       totalCount,
		TotalCountCapped: capped,
		TypesMatched:     types,
		Facets:           nil, // B-2 placeholder
	}, nil
}

// cursorLess reports whether hit h ordering-comes-after the
// cursor's position (i.e. h should be on a later page than the
// one whose tail was `cut`). Ordering is (score DESC, id DESC,
// type DESC), so "after" is strictly-less-than on the tuple.
func cursorLess(h Hit, cut Cursor) bool {
	if h.NormalisedScore != cut.LastScore {
		return h.NormalisedScore < cut.LastScore
	}
	if h.ID.String() != cut.LastID.String() {
		return h.ID.String() < cut.LastID.String()
	}
	return h.Type < cut.LastType
}

// runOne executes the per-entity ranked query + a count-with-cap
// query, returning the top `limit` hits plus the (capped) total
// count.
func (e *Engine) runOne(ctx context.Context, t HitType, q Query, limit int) ([]Hit, int, error) {
	switch t {
	case HitTypeAsset:
		return e.runAssets(ctx, q, limit)
	case HitTypeCollection:
		return e.runCollections(ctx, q, limit)
	case HitTypePost:
		return e.runPosts(ctx, q, limit)
	}
	return nil, 0, fmt.Errorf("search: unknown hit type %q", t)
}

// ErrEmptyQuery is returned by Run when q.Text is empty. HTTP
// handler maps to 400 {"error": "query_required"}.
var ErrEmptyQuery = errors.New("search: query text is required")

// ---------------------------------------------------------------------------
// Per-entity queries
//
// Each returns:
//   - the top `limit` hits, ranked by ts_rank_cd
//   - the total count of matches (capped at TotalCountCap+1 so the
//     engine can flag the cap)
//
// Visibility gates mirror the existing per-entity list handlers
// (see doc.go).
// ---------------------------------------------------------------------------

// runAssets queries the assets table. Visibility gate composed via
// visibility.Predicate — see the shared package (Phase 1.16.B-2).
// The base search_text @@ predicate stays inline; the visibility
// AND clause is appended by the shared helper.
func (e *Engine) runAssets(ctx context.Context, q Query, limit int) ([]Hit, int, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, visibility.NewCaller(q.CallerUserRef))
	if err != nil {
		return nil, 0, err
	}
	visFrag, visArgs := pred.ToSQL("", 2) // $1=query, next placeholder = $3

	sqlHits := `
		SELECT id, title, description, owner_user_ref, origin_server_id,
		       thumbhash, created_at, updated_at,
		       ts_rank_cd(search_text, plainto_tsquery('english', $1)) AS score
		  FROM assets
		 WHERE search_text @@ plainto_tsquery('english', $1)` + visFrag + `
		 ORDER BY score DESC, id DESC
		 LIMIT $2
	`
	sqlCount := `
		SELECT COUNT(*)::BIGINT FROM (
			SELECT 1 FROM assets
			 WHERE search_text @@ plainto_tsquery('english', $1)` + visFrag + `
			 LIMIT $2
		) x
	`
	// Compose args: $1=query text, $2=limit, then visibility args.
	hitsArgs := append([]any{q.Text, limit}, visArgs...)
	countArgs := append([]any{q.Text, TotalCountCap + 1}, visArgs...)
	rows, err := e.Pool.Query(ctx, sqlHits, hitsArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var (
			id       uuid.UUID
			title    string
			descr    string
			owner    *int64
			origin   *uuid.UUID
			thumb    []byte
			created  time.Time
			updated  time.Time
			score    float64
		)
		if err := rows.Scan(&id, &title, &descr, &owner, &origin, &thumb, &created, &updated, &score); err != nil {
			return nil, 0, err
		}
		extra, _ := json.Marshal(map[string]any{
			"thumbhash_b64": encodeB64(thumb),
		})
		hits = append(hits, Hit{
			Type:           HitTypeAsset,
			ID:             id,
			Title:          title,
			Summary:        truncate(descr, 240),
			OwnerUserRef:   owner,
			OriginServerID: origin,
			CreatedAt:      created,
			UpdatedAt:      updated,
			RawScore:       score,
			ExtraJSON:      extra,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	total, err := e.scalarInt(ctx, sqlCount, countArgs...)
	if err != nil {
		return nil, 0, err
	}
	return hits, total, nil
}

// runCollections queries collections. Visibility gate composed via
// visibility.Predicate — see the shared package (Phase 1.16.B-2).
// Anonymous callers get an always-false predicate → zero hits.
func (e *Engine) runCollections(ctx context.Context, q Query, limit int) ([]Hit, int, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityCollection, visibility.NewCaller(q.CallerUserRef))
	if err != nil {
		return nil, 0, err
	}
	visFrag, visArgs := pred.ToSQL("c", 2) // $1=query, $2=limit index reserved for hits query

	sqlHits := `
		SELECT c.id, c.name, c.description, c.owner_user_ref, c.origin_server_id,
		       c.featured, c.created_at, c.updated_at,
		       ts_rank_cd(c.search_text, plainto_tsquery('english', $1)) AS score
		  FROM collections c
		 WHERE c.search_text @@ plainto_tsquery('english', $1)` + visFrag + `
		 ORDER BY score DESC, id DESC
		 LIMIT $2
	`
	sqlCount := `
		SELECT COUNT(*)::BIGINT FROM (
			SELECT 1 FROM collections c
			 WHERE c.search_text @@ plainto_tsquery('english', $1)` + visFrag + `
			 LIMIT $2
		) x
	`
	hitsArgs := append([]any{q.Text, limit}, visArgs...)
	countArgs := append([]any{q.Text, TotalCountCap + 1}, visArgs...)
	rows, err := e.Pool.Query(ctx, sqlHits, hitsArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var (
			id       uuid.UUID
			name     string
			descr    string
			owner    int64
			origin   *uuid.UUID
			featured bool
			created  time.Time
			updated  time.Time
			score    float64
		)
		if err := rows.Scan(&id, &name, &descr, &owner, &origin, &featured, &created, &updated, &score); err != nil {
			return nil, 0, err
		}
		extra, _ := json.Marshal(map[string]any{"featured": featured})
		hits = append(hits, Hit{
			Type:           HitTypeCollection,
			ID:             id,
			Title:          name,
			Summary:        truncate(descr, 240),
			OwnerUserRef:   &owner,
			OriginServerID: origin,
			CreatedAt:      created,
			UpdatedAt:      updated,
			RawScore:       score,
			ExtraJSON:      extra,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	total, err := e.scalarInt(ctx, sqlCount, countArgs...)
	if err != nil {
		return nil, 0, err
	}
	return hits, total, nil
}

// runPosts queries posts. Visibility gate composed via
// visibility.Predicate — see the shared package (Phase 1.16.B-2).
func (e *Engine) runPosts(ctx context.Context, q Query, limit int) ([]Hit, int, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityPost, visibility.NewCaller(q.CallerUserRef))
	if err != nil {
		return nil, 0, err
	}
	visFrag, visArgs := pred.ToSQL("", 2)

	sqlHits := `
		SELECT id, title, description, author_user_ref, origin_server_id,
		       cover_asset_id, created_at, updated_at,
		       ts_rank_cd(search_text, plainto_tsquery('english', $1)) AS score
		  FROM posts
		 WHERE search_text @@ plainto_tsquery('english', $1)` + visFrag + `
		 ORDER BY score DESC, id DESC
		 LIMIT $2
	`
	sqlCount := `
		SELECT COUNT(*)::BIGINT FROM (
			SELECT 1 FROM posts
			 WHERE search_text @@ plainto_tsquery('english', $1)` + visFrag + `
			 LIMIT $2
		) x
	`
	hitsArgs := append([]any{q.Text, limit}, visArgs...)
	countArgs := append([]any{q.Text, TotalCountCap + 1}, visArgs...)
	rows, err := e.Pool.Query(ctx, sqlHits, hitsArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var (
			id       uuid.UUID
			title    string
			descr    string
			author   *int64
			origin   *uuid.UUID
			cover    *uuid.UUID
			created  time.Time
			updated  time.Time
			score    float64
		)
		if err := rows.Scan(&id, &title, &descr, &author, &origin, &cover, &created, &updated, &score); err != nil {
			return nil, 0, err
		}
		var coverStr any
		if cover != nil {
			coverStr = cover.String()
		}
		extra, _ := json.Marshal(map[string]any{"cover_asset_id": coverStr})
		hits = append(hits, Hit{
			Type:           HitTypePost,
			ID:             id,
			Title:          title,
			Summary:        truncate(descr, 240),
			OwnerUserRef:   author,
			OriginServerID: origin,
			CreatedAt:      created,
			UpdatedAt:      updated,
			RawScore:       score,
			ExtraJSON:      extra,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	total, err := e.scalarInt(ctx, sqlCount, countArgs...)
	if err != nil {
		return nil, 0, err
	}
	return hits, total, nil
}

// scalarInt runs a `SELECT COUNT(*)::BIGINT` and returns the value
// as an int. Uses a fresh QueryRow so the caller doesn't juggle
// row-cursor lifecycles.
func (e *Engine) scalarInt(ctx context.Context, sql string, args ...any) (int, error) {
	var v int64
	if err := e.Pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return int(v), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Trim to n bytes at a rune boundary.
	trimmed := s[:n]
	for i := len(trimmed) - 1; i > 0 && trimmed[i]&0xC0 == 0x80; i-- {
		trimmed = trimmed[:i]
	}
	return trimmed + "…"
}
