// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/vector"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
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

	// previewLadder reports the operator's CONFIGURED preview variant
	// keys (#591), read per query from the cached sysconfig reader.
	// Installed by boot via SetPreviewLadder; nil in tests and in any
	// boot wire that predates the #850 card payload.
	//
	// Nil means "unknown", which LadderSatisfiedSQL resolves to false,
	// so a card falls back to the single `col` rung — the direction
	// that costs a responsive srcset rather than a wall of 404s.
	previewLadder sysconfig.PreviewLadderReader
}

// NewEngine constructs an Engine bound to the shared pool.
func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{Pool: pool}
}

// SetPreviewLadder installs the cached configured-ladder reader (#591).
// Same post-construction setter the assets / posts / collections /
// featured handlers take, wired from the ONE reader built at boot so all
// five agree on what the ladder is.
func (e *Engine) SetPreviewLadder(r sysconfig.PreviewLadderReader) { e.previewLadder = r }

// ladder returns the configured preview variant keys, or nil.
func (e *Engine) ladder(ctx context.Context) []string {
	if e.previewLadder == nil {
		return nil
	}
	return e.previewLadder(ctx)
}

// Run executes the query. Emits per-entity queries in parallel,
// normalises scores, merges, orders, cuts to the page, computes
// the next cursor, and stamps total counts.
func (e *Engine) Run(ctx context.Context, q Query) (QueryResult, error) {
	// Empty text is allowed when the caller supplied a
	// SimilarityHint — the hybrid path can rank purely on vector
	// similarity. Pure-BM25 empty queries stay rejected.
	if q.Text == "" && q.SimilarityHint == "" {
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
		// Baseline: HybridScore mirrors NormalisedScore for
		// non-hybrid queries; the hybrid merge below overrides it
		// with the weighted sum when a SimilarityHint is present.
		rawHits[i].HybridScore = rawHits[i].NormalisedScore
	}

	// Phase 1.16.B-3 — hybrid ranking. When a SimilarityHint is
	// present AND assets are in the requested types, run a kNN
	// pass against asset_embedding_d768 and merge results into
	// the BM25 hit set.
	if q.SimilarityHint != "" && containsHitType(types, HitTypeAsset) {
		if err := e.applyHybrid(ctx, q, &rawHits); err != nil {
			return QueryResult{}, fmt.Errorf("search: hybrid merge: %w", err)
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

// containsHitType is a tiny lookup helper used by the hybrid gate.
func containsHitType(types []HitType, want HitType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// applyHybrid runs a kNN pass against asset_embedding_d768 and
// merges the results into rawHits per the hybrid ranking formula:
//
//	hybrid = (1 - w) * bm25_normalised + w * cosine_similarity
//
// Assets already in the BM25 hit set gain a VectorScore + updated
// HybridScore; assets found ONLY by the vector pass are appended
// with zero BM25 score.
//
// Hits scoring below q.SimilarityThreshold are dropped (only in
// hybrid + pure-vector modes; a pure-BM25 query bypasses).
//
// This function mutates the slice via the pointer so the caller
// can re-sort against the updated HybridScore field afterwards.
func (e *Engine) applyHybrid(ctx context.Context, q Query, hits *[]Hit) error {
	// Weight defaults: pure-vector when text is empty, mid when
	// both text + hint present, honour caller override otherwise.
	w := q.HybridWeight
	if w <= 0 {
		if q.Text == "" {
			w = 1.0
		} else {
			w = DefaultHybridWeight
		}
	}
	if w > 1 {
		w = 1
	}
	threshold := q.SimilarityThreshold

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	anchor := vector.Anchor{
		Raw:      q.SimilarityHint,
		Provider: q.SimilarityHintProvider,
		Model:    q.SimilarityHintModel,
		Modality: q.SimilarityHintModality,
	}
	vecHits, err := vector.Query(ctx, e.Pool, anchor, visibility.NewCaller(q.CallerUserRef), threshold, limit*VectorOverfetchMultiplier)
	if err != nil {
		return err
	}

	// Merge into the existing hit set. Assets in both sets get
	// updated in-place; vector-only assets require a fresh row
	// lookup for the Hit projection (title, description, etc.).
	idx := make(map[[16]byte]int, len(*hits))
	for i, h := range *hits {
		if h.Type != HitTypeAsset {
			continue
		}
		idx[h.ID] = i
	}
	newRows := make([]Hit, 0, 8)
	for _, vh := range vecHits {
		if pos, ok := idx[vh.AssetID]; ok {
			(*hits)[pos].VectorScore = vh.Similarity
			(*hits)[pos].HybridScore = (1-w)*(*hits)[pos].NormalisedScore + w*vh.Similarity
			continue
		}
		newRows = append(newRows, Hit{
			Type:            HitTypeAsset,
			ID:              vh.AssetID,
			VectorScore:     vh.Similarity,
			HybridScore:     w * vh.Similarity,
			NormalisedScore: w * vh.Similarity, // cursor sort uses this
		})
	}

	// Enrich vector-only hits with their asset row projection so
	// the response body carries titles + timestamps.
	if len(newRows) > 0 {
		if err := e.enrichAssetHits(ctx, q, newRows); err != nil {
			return err
		}
		*hits = append(*hits, newRows...)
	}

	// For BM25-hits WITHOUT vector match (VectorScore=0), rescale
	// HybridScore = (1-w) * NormalisedScore so BM25-only hits
	// don't dominate a pure-vector query. NormalisedScore mirrors
	// this so the sort + cursor comparison happen on one field.
	for i := range *hits {
		if (*hits)[i].Type != HitTypeAsset {
			continue
		}
		if (*hits)[i].VectorScore == 0 {
			(*hits)[i].HybridScore = (1 - w) * (*hits)[i].NormalisedScore
		}
		// Threshold filter fires only in hybrid + pure-vector
		// modes (skip when weight = 0 — that's pure-BM25).
		(*hits)[i].NormalisedScore = (*hits)[i].HybridScore
	}
	if threshold > 0 && w > 0 {
		filtered := (*hits)[:0]
		for _, h := range *hits {
			if h.Type == HitTypeAsset && h.HybridScore < threshold {
				continue
			}
			filtered = append(filtered, h)
		}
		*hits = filtered
	}
	return nil
}

// enrichAssetHits fills in the Title / Summary / timestamps for
// asset hits that arrived via the vector path (no BM25 row scan).
// One IN-clause query keeps the round-trip bounded regardless of
// how many vector-only hits arrived.
func (e *Engine) enrichAssetHits(ctx context.Context, q Query, hits []Hit) error {
	if len(hits) == 0 {
		return nil
	}
	ids := make([][16]byte, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	// The readability columns ride along in the same pass (#899). This
	// query has no visibility predicate of its own — it trusts
	// vector.Query to have pre-filtered the ids — so before #899 there
	// was NOTHING here between the assets table and the caller's JSON.
	// That was sound only as long as no future caller handed it
	// unfiltered ids; the per-row decision below no longer depends on
	// that promise.
	caller, caps := callerOf(q)
	mut := mutCapsOf(q)
	rows, err := e.Pool.Query(ctx, `
		SELECT id, title, description, owner_user_ref, origin_server_id,
		       thumbhash, created_at, updated_at,
		       `+visibility.FieldsColumnsSQL("assets", "$2")+`,
		       `+assetCardColumnsSQL("assets", "$3")+`
		  FROM assets
		 WHERE id = ANY($1::UUID[])
		   AND deleted_at IS NULL
	`, ids, callerRefOf(q), e.ladder(ctx))
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[[16]byte]Hit, len(hits))
	for rows.Next() {
		var (
			id        uuid.UUID
			title     string
			descr     string
			owner     *int64
			origin    *uuid.UUID
			thumb     []byte
			created   time.Time
			updated   time.Time
			fr        visibility.FieldsRow
			ownerName string
			card      assetCardRow
		)
		dest := append([]any{
			&id, &title, &descr, &owner, &origin, &thumb, &created, &updated,
			&fr.Sensitivity, &fr.Status, &fr.ProcessingStatus, &fr.OwnerUserRef,
			&fr.TeamID, &fr.IsTeamMember, &ownerName,
		}, card.scanDest()...)
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		fr.ApplyMutationCaps(mut)
		if !visibility.FieldsReadable(fr, caller, caps) {
			// The projection carries nothing but the marker and the
			// owner's name — note the thumbhash in particular is a
			// blurred picture of the content, not a neutral hint. The
			// #850 card payload is on the other branch for the same
			// reason.
			byID[id] = Hit{Restricted: true, OwnerDisplayName: ownerName}
			continue
		}
		// PreviewReadable, not `true` (#939): a mutation holder reaches
		// this branch on the field plane and must still get no blur and
		// no availability flags.
		extra := assetCardExtra(card, thumb, visibility.PreviewReadable(fr, caller, caps))
		byID[id] = Hit{
			Title:          title,
			Summary:        truncate(descr, 240),
			OwnerUserRef:   owner,
			OriginServerID: origin,
			CreatedAt:      created,
			UpdatedAt:      updated,
			ExtraJSON:      extra,
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, h := range hits {
		enriched, ok := byID[h.ID]
		if !ok {
			continue
		}
		if enriched.Restricted {
			// Replace the WHOLE hit rather than copying the readable
			// fields across — the scores are carried over explicitly by
			// withheldHit and nothing else is.
			hits[i] = withheldHit(hits[i], enriched.OwnerDisplayName)
			continue
		}
		hits[i].Title = enriched.Title
		hits[i].Summary = enriched.Summary
		hits[i].OwnerUserRef = enriched.OwnerUserRef
		hits[i].OriginServerID = enriched.OriginServerID
		hits[i].CreatedAt = enriched.CreatedAt
		hits[i].UpdatedAt = enriched.UpdatedAt
		hits[i].ExtraJSON = enriched.ExtraJSON
	}
	return nil
}

// DefaultHybridWeight is the fallback when the caller doesn't
// specify one AND the query has both text + hint. 0.5 balances
// BM25 relevance against vector proximity.
const DefaultHybridWeight = 0.5

// VectorOverfetchMultiplier bounds how many vector candidates we
// pull relative to the requested page limit. Larger = more
// headroom for the BM25 merge; smaller = less DB work per query.
const VectorOverfetchMultiplier = 5

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
	caller, caps := callerOf(q)
	mut := mutCapsOf(q)
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, 0, err
	}
	// $1=query, $2=limit, $3=caller ref, $4=configured ladder,
	// predicate args start at $5.
	visFrag, visArgs := pred.ToSQL("", 4)

	sqlHits := `
		SELECT id, title, description, owner_user_ref, origin_server_id,
		       thumbhash, created_at, updated_at,
		       ts_rank_cd(search_text, plainto_tsquery('english', $1)) AS score,
		       ` + visibility.FieldsColumnsSQL("assets", "$3") + `,
		       ` + assetCardColumnsSQL("assets", "$4") + `
		  FROM assets
		 WHERE search_text @@ plainto_tsquery('english', $1)` + visFrag + `
		 ORDER BY score DESC, id DESC
		 LIMIT $2
	`
	sqlCount := `
		SELECT COUNT(*)::BIGINT FROM (
			SELECT 1 FROM assets
			 WHERE search_text @@ plainto_tsquery('english', $1)
			   -- $3 is the caller ref and $4 the configured preview
			   -- ladder, read only by the hits query's readability and
			   -- card columns. Both are REFERENCED here (as tautologies)
			   -- rather than dropped, because pgx rejects a statement
			   -- bound with more args than it names and the alternative —
			   -- renumbering the shared predicate fragment per statement —
			   -- is exactly the off-by-one ADR 0063's placeholder
			   -- discipline exists to avoid.
			   AND ($3::BIGINT IS NULL OR TRUE)
			   AND ($4::TEXT[] IS NULL OR TRUE)` + visFrag + `
			 LIMIT $2
		) x
	`
	// Compose args: $1=query text, $2=limit, $3=caller ref, then
	// visibility args. The COUNT does not read $3 but still binds it, so
	// the shared predicate fragment's placeholder indexes line up in
	// both statements (ADR 0063 placeholder discipline).
	//
	// total_count deliberately counts rows the caller cannot open. ADR
	// 0064 keeps those rows in the result set as placeholders, so a
	// total that excluded them would disagree with the array beside it
	// and would make the count itself a readability oracle — "the total
	// dropped by one, so that row is restricted". Existence is already
	// disclosed by decision 1; the count discloses nothing further.
	ladder := e.ladder(ctx)
	hitsArgs := append([]any{q.Text, limit, callerRefOf(q), ladder}, visArgs...)
	countArgs := append([]any{q.Text, TotalCountCap + 1, callerRefOf(q), ladder}, visArgs...)
	rows, err := e.Pool.Query(ctx, sqlHits, hitsArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var (
			id        uuid.UUID
			title     string
			descr     string
			owner     *int64
			origin    *uuid.UUID
			thumb     []byte
			created   time.Time
			updated   time.Time
			score     float64
			fr        visibility.FieldsRow
			ownerName string
			card      assetCardRow
		)
		dest := append([]any{
			&id, &title, &descr, &owner, &origin, &thumb, &created, &updated, &score,
			&fr.Sensitivity, &fr.Status, &fr.ProcessingStatus, &fr.OwnerUserRef,
			&fr.TeamID, &fr.IsTeamMember, &ownerName,
		}, card.scanDest()...)
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		fr.ApplyMutationCaps(mut)
		// #899 — the row STAYS (ADR 0064 gates content, not rows), but
		// an asset the caller cannot open hands over none of its
		// columns. The predicate above decided whether the row is
		// listed; this decides what the listing says.
		//
		// #850 widened what a readable hit carries. It changed nothing
		// here on purpose: the card payload is built on the far side of
		// this branch, so a wider `extra` cannot reach a caller who
		// fails the gate.
		if !visibility.FieldsReadable(fr, caller, caps) {
			hits = append(hits, withheldHit(Hit{
				Type: HitTypeAsset, ID: id, RawScore: score,
			}, ownerName))
			continue
		}
		// PreviewReadable, not `true` (#939) — see enrichAssetHits.
		extra := assetCardExtra(card, thumb, visibility.PreviewReadable(fr, caller, caps))
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
// Anonymous callers get the public floor (`deleted_at IS NULL AND
// visibility = 'public'`), not the always-false predicate this comment
// used to claim — that stopped being true when #445/#448 opened the
// anonymous read paths.
//
// #650 — this used to select `c.featured` and emit it as a hit extra.
// #456 (ADR 0065) dropped that column: featuring is a PLACEMENT row in
// featured_items carrying an audience scope (public | org | team), not
// a property of the collection. The select was never updated, so every
// authenticated search 500'd with 42703 from the moment #456 landed.
//
// The flag is not re-derived. A single boolean cannot say WHICH
// audience a placement is for, so reintroducing one here would rebuild
// exactly the column-shaped concept ADR 0065 removed — and no client
// reads it (the /search page types `extra` as an opaque bag and never
// touches `featured`). A per-row EXISTS against featured_items would
// therefore buy a lossy field, a join, and a second source of truth for
// zero consumers. If a card ever needs to badge featured-ness, the
// correct shape is a scoped placement lookup — the pattern
// ListCollectionsPage already uses for its ?featured= filter — added
// then, against a real consumer.
func (e *Engine) runCollections(ctx context.Context, q Query, limit int) ([]Hit, int, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityCollection, visibility.NewCaller(q.CallerUserRef))
	if err != nil {
		return nil, 0, err
	}
	visFrag, visArgs := pred.ToSQL("c", 2) // $1=query, $2=limit index reserved for hits query

	sqlHits := `
		SELECT c.id, c.name, c.description, c.owner_user_ref, c.origin_server_id,
		       c.created_at, c.updated_at, c.visibility,
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
			id      uuid.UUID
			name    string
			descr   string
			owner   int64
			origin  *uuid.UUID
			created time.Time
			updated time.Time
			vis     string
			score   float64
		)
		if err := rows.Scan(&id, &name, &descr, &owner, &origin, &created, &updated, &vis, &score); err != nil {
			return nil, 0, err
		}
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
			// #850 — the one field CollectionCard reads that the hit did
			// not already carry. Safe to ship unconditionally: the
			// predicate above already decided this caller may see the
			// collection, and its visibility tier is the reason why.
			ExtraJSON: collectionCardExtra(vis),
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
//
// #873 — WithPostCaps is what makes this the same rule the browse feed
// runs. Without it the predicate renders its `private` disjunct as
// FALSE, and a moderator searching for a private post they can open from
// the feed gets nothing back.
func (e *Engine) runPosts(ctx context.Context, q Query, limit int) ([]Hit, int, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityPost,
		visibility.NewCaller(q.CallerUserRef), visibility.WithPostCaps(q.PostCaps))
	if err != nil {
		return nil, 0, err
	}
	visFrag, visArgs := pred.ToSQL("", 2)

	// #850 — the card fields. `cover_asset_id` alone was never enough to
	// render a tile: a post with no explicit cover shows its FIRST member,
	// which is the same resolution order PostCard applies client-side and
	// posts.enrichPreview applies server-side. Resolved here so the
	// frontend never has to ask a second endpoint what a hit looks like.
	//
	// member_count is the true size of the set (what the multi-asset badge
	// counts), not the length of the one-member array the payload ships.
	// Soft-deleted members are excluded from both, matching
	// ListPostAssets.
	const coverAssetExpr = `COALESCE(posts.cover_asset_id, (
		    SELECT pa.asset_id FROM post_assets pa
		      JOIN assets pm ON pm.id = pa.asset_id AND pm.deleted_at IS NULL
		     WHERE pa.post_id = posts.id
		     ORDER BY pa.sort_order ASC, pa.added_at ASC
		     LIMIT 1))`
	sqlHits := `
		SELECT id, title, description, author_user_ref, origin_server_id,
		       ` + coverAssetExpr + ` AS cover_asset_id, created_at, updated_at,
		       like_count, comment_count,
		       (SELECT COUNT(*) FROM post_assets pa
		          JOIN assets pm ON pm.id = pa.asset_id AND pm.deleted_at IS NULL
		         WHERE pa.post_id = posts.id) AS member_count,
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
	// Collected across the page so the cover assets resolve in ONE round
	// trip, not one per hit.
	covers := make([]*uuid.UUID, 0, limit)
	counts := make([][3]int64, 0, limit)
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
			likes    int64
			comments int64
			members  int64
			score    float64
		)
		if err := rows.Scan(&id, &title, &descr, &author, &origin, &cover, &created, &updated,
			&likes, &comments, &members, &score); err != nil {
			return nil, 0, err
		}
		covers = append(covers, cover)
		counts = append(counts, [3]int64{likes, comments, members})
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
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	// The cover assets, per caller (#850). A post the caller may read can
	// still bundle an asset they may not — the cover then arrives as the
	// #883 placeholder and PostCard states the restriction rather than
	// degrading to a generic no-preview plate.
	coverIDs := make([]uuid.UUID, 0, len(covers))
	seen := make(map[uuid.UUID]struct{}, len(covers))
	for _, c := range covers {
		if c == nil {
			continue
		}
		if _, dup := seen[*c]; dup {
			continue
		}
		seen[*c] = struct{}{}
		coverIDs = append(coverIDs, *c)
	}
	coverCards, err := e.loadPostCovers(ctx, q, coverIDs)
	if err != nil {
		return nil, 0, err
	}
	for i := range hits {
		var member *postCardMember
		if covers[i] != nil {
			// Fail CLOSED on a cover whose asset row did not come back
			// (deleted between the two queries): a member with no
			// readability answer is a member the caller does not get.
			if m, ok := coverCards[*covers[i]]; ok {
				member = m
			} else {
				member = &postCardMember{AssetID: *covers[i], Readable: false}
			}
		}
		hits[i].ExtraJSON = postCardExtra(covers[i], member,
			counts[i][0], counts[i][1], counts[i][2])
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
