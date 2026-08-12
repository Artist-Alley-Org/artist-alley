// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package saved

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/search/dsl"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/vector"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// EngineRunner is the narrow slice of search.Engine the executor
// consumes. Kept as an interface so tests can inject a fake without
// spinning up the full Engine stack.
type EngineRunner interface {
	Run(ctx context.Context, q search.Query) (search.QueryResult, error)
}

// Executor runs a saved search's stored DSL through the shared
// search.Engine + vector.Fetcher paths, then hashes the sorted
// asset-ID set for delta detection.
type Executor struct {
	Pool    *pgxpool.Pool
	Engine  EngineRunner
	Fetcher *vector.Fetcher

	// PerRunLimit caps how many hits one execution retrieves.
	// Digest emails degrade with too many rows; the coordinator
	// also runs many rows per tick so unbounded work per row
	// starves peers. Default 100.
	PerRunLimit int
}

// NewExecutor wires the executor with sane defaults.
func NewExecutor(pool *pgxpool.Pool, engine EngineRunner, fetcher *vector.Fetcher) *Executor {
	return &Executor{
		Pool:        pool,
		Engine:      engine,
		Fetcher:     fetcher,
		PerRunLimit: 100,
	}
}

// Run executes one saved search + returns the ordered ID set +
// hash + per-hit metadata. Applies visibility.Filter for the OWNER
// at execution time — the whole invariant this sub-phase is built
// around.
func (e *Executor) Run(ctx context.Context, row Row) (RunResult, error) {
	parsed, err := dsl.Parse(row.DSL)
	if err != nil {
		return RunResult{}, fmt.Errorf("saved.Execute: parse dsl: %w", err)
	}
	compiled, err := dsl.Compile(parsed)
	if err != nil {
		return RunResult{}, fmt.Errorf("saved.Execute: compile dsl: %w", err)
	}

	// Build the Engine.Query with the owner as caller so
	// visibility.Filter gates on the owner's effective view.
	owner := row.OwnerUserRef
	limit := e.PerRunLimit
	if limit <= 0 {
		limit = 100
	}
	query := search.Query{
		Text:          row.DSL, // BM25 path uses the raw text if compiled tsQuery is empty (similar_to-only)
		Types:         []search.HitType{search.HitTypeAsset},
		Limit:         limit,
		CallerUserRef: &owner,
		// #907 — the compiled field:value constraints, through the SAME
		// conversion the interactive handler uses. Without it a saved
		// `tag:foo` would run WIDER than the /search?dsl=tag:foo it was
		// saved from, and the owner would be emailed hits their own
		// search does not return.
		Filters: search.SelectionFromDSL(compiled.Filters, facet.Selection{}),
	}
	if compiled.TSQuery == "" {
		// Pure similar_to or all-filter DSL — nothing for the
		// BM25 path. Empty text keeps the Engine on the vector
		// path exclusively.
		query.Text = ""
	}

	// Resolve similar_to:<uuid> to an anchor embedding via the
	// vector.Fetcher — same path the interactive /search?dsl=
	// handler uses.
	if compiled.SimilarToAssetID != "" && e.Fetcher != nil {
		assetID, perr := uuid.Parse(compiled.SimilarToAssetID)
		if perr == nil {
			// Owner-visibility spot-check on the anchor asset
			// itself — a restricted asset the owner has since
			// lost access to shouldn't leak its neighbourhood.
			visible, err := e.anchorVisibleToOwner(ctx, assetID, owner)
			if err != nil {
				return RunResult{}, fmt.Errorf("saved.Execute: anchor visibility: %w", err)
			}
			if !visible {
				// Owner lost access to the anchor → treat as
				// "no hits" for this run. Delta detection will
				// see the empty set and clear the digest.
				return emptyRunResult(), nil
			}
			anchor, aerr := e.Fetcher.FetchAssetEmbedding(ctx, assetID)
			if aerr != nil {
				if errors.Is(aerr, vector.ErrNotEmbedded) {
					return emptyRunResult(), nil
				}
				return RunResult{}, fmt.Errorf("saved.Execute: fetch anchor: %w", aerr)
			}
			query.SimilarityHint = anchor.Raw
			query.SimilarityHintProvider = anchor.Provider
			query.SimilarityHintModel = anchor.Model
			query.SimilarityHintModality = anchor.Modality
			query.SimilarityHintID = "asset:" + compiled.SimilarToAssetID
			if compiled.HybridWeightSuggestion > 0 {
				query.HybridWeight = compiled.HybridWeightSuggestion
			}
		}
	}

	// Direct Engine.Run — deliberately bypasses search.Service so
	// the query-result cache is skipped. Notifications need
	// real-time state, not a 60-second-stale hit set.
	res, err := e.Engine.Run(ctx, query)
	if err != nil {
		// An empty query (Text=="", no SimilarityHint) is not an
		// error — it just means the saved search's DSL didn't
		// produce anything the Engine could execute. Record as
		// an empty run so the coordinator moves on.
		if errors.Is(err, search.ErrEmptyQuery) {
			return emptyRunResult(), nil
		}
		return RunResult{}, err
	}

	ids := make([]uuid.UUID, 0, len(res.Hits))
	meta := make([]HitMeta, 0, len(res.Hits))
	for _, h := range res.Hits {
		if h.Type != search.HitTypeAsset {
			continue
		}
		ids = append(ids, h.ID)
		meta = append(meta, HitMeta{ID: h.ID, Title: h.Title, Summary: h.Summary})
	}
	sortedIDs, sortedMeta := sortByID(ids, meta)
	return RunResult{
		HitIDs:   sortedIDs,
		Hash:     hashIDs(sortedIDs),
		HitsMeta: sortedMeta,
	}, nil
}

// anchorVisibleToOwner runs the shared visibility.Predicate
// against the anchor asset ID with the owner as caller. Returns
// (visible, err).
func (e *Executor) anchorVisibleToOwner(ctx context.Context, assetID uuid.UUID, ownerRef int64) (bool, error) {
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, visibility.Caller{UserRef: ownerRef, IsAnonymous: false})
	if err != nil {
		return false, err
	}
	frag, args := pred.ToSQL("", 1)
	var exists bool
	sql := "SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1" + frag + ")"
	err = e.Pool.QueryRow(ctx, sql, append([]any{assetID}, args...)...).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// emptyRunResult returns a zero-hit RunResult with a deterministic
// hash for the empty set. Persisting the empty hash lets delta
// detection distinguish "never run" (LastResultHash IS NULL) from
// "ran but got nothing" (hash of empty set).
func emptyRunResult() RunResult {
	return RunResult{
		HitIDs: nil,
		Hash:   hashIDs(nil),
	}
}

// hashIDs returns the sha256 hex of the joined ID set. Empty input
// hashes to the empty-string sha; distinct from any populated set.
func hashIDs(ids []uuid.UUID) string {
	h := sha256.New()
	for i, id := range ids {
		if i > 0 {
			_, _ = h.Write([]byte{','})
		}
		_, _ = h.Write([]byte(id.String()))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sortByID sorts the ID slice ascending + carries meta along so
// the two stay index-aligned. Necessary because the Engine's
// ordering is by score, but our hash needs to be
// order-independent (so identical hit sets in different orders
// produce identical hashes).
func sortByID(ids []uuid.UUID, meta []HitMeta) ([]uuid.UUID, []HitMeta) {
	if len(ids) == 0 {
		return ids, meta
	}
	type pair struct {
		id   uuid.UUID
		meta HitMeta
	}
	pairs := make([]pair, len(ids))
	for i, id := range ids {
		pairs[i] = pair{id: id, meta: meta[i]}
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].id.String() < pairs[j].id.String()
	})
	sIDs := make([]uuid.UUID, len(pairs))
	sMeta := make([]HitMeta, len(pairs))
	for i, p := range pairs {
		sIDs[i] = p.id
		sMeta[i] = p.meta
	}
	return sIDs, sMeta
}
