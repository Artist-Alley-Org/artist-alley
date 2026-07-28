// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.14.B — /assets/{id}/similar HTTP handler.
//
// Reads the anchor asset's embedding row (per the configured default
// model in system_config.ai.embedding.default_model), runs a kNN
// over the per-dim sibling table, then re-fetches the matched asset
// rows so the response carries the standard Asset projection.
//
// # Why the per-call sysconfig read
//
// The default model can change without a restart (operator edits
// system_config.ai.embedding.default_model via the admin surface).
// Caching it here would mean a stale embedding lookup after such
// an edit. Per-call read is cheap (cache-backed at the sysconfig
// layer in 1.14.A) and keeps the model selection live.

package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// defaultSimilarLimit when the request omits it. Mirrors
// system_config.ai.search.similar_default_limit (12) but hard-coded
// here so a missing config row doesn't bubble as a 500.
const defaultSimilarLimit = 12

// maxSimilarLimit caps the response size regardless of operator
// override. Matches the openapi.yaml schema cap.
const maxSimilarLimit = 50

// ListSimilarAssets implements the GET /assets/{id}/similar endpoint.
//
// Response shape:
//   - results: ranked neighbours by ascending cosine distance, anchor excluded
//   - anchor_has_embedding: false when no embedding exists yet — UI
//     uses this to disambiguate "no neighbours" from "embedding pending"
func (h *Handler) ListSimilarAssets(ctx context.Context, req openapi.ListSimilarAssetsRequestObject) (openapi.ListSimilarAssetsResponseObject, error) {
	if h.similarReader == nil {
		// Boot-wire bug or test scaffold; return an empty payload
		// rather than 500 — the frontend gracefully renders
		// "embedding pending".
		return openapi.ListSimilarAssets200JSONResponse(openapi.SimilarAssets{
			Results:            []openapi.SimilarAsset{},
			AnchorHasEmbedding: false,
		}), nil
	}

	anchorID := uuid.UUID(req.Id)
	caller := callerFromContext(ctx)

	// #661 — the anchor must be VISIBLE to this caller, not merely
	// present. The check here used to be a bare existence probe with no
	// predicate, so any asset id worked as an anchor: a draft, archived,
	// still-processing or `restricted` asset on a public-mode install
	// confirmed its own existence to an anonymous caller and then seeded
	// a neighbour list. CanSee obtains the same rule GetAsset uses (ADR
	// 0063) rather than restating it, so the two cannot disagree — and
	// 404 rather than 403 keeps this from confirming a hidden id.
	visible, err := visibility.CanSee(ctx, h.Pool, visibility.EntityAsset, caller, anchorID)
	if err != nil {
		return nil, fmt.Errorf("ListSimilarAssets: anchor visibility: %w", err)
	}
	if !visible {
		return openapi.ListSimilarAssets404JSONResponse{}, nil
	}

	limit := defaultSimilarLimit
	if req.Params.Limit != nil && *req.Params.Limit > 0 {
		limit = int(*req.Params.Limit)
		if limit > maxSimilarLimit {
			limit = maxSimilarLimit
		}
	}

	// Default-model + modality come from system_config; the writer
	// + admin UI keep these in sync. Provider is hard-coded "router"
	// to match what the EmbedHandler persists (see jobs/handlers.go).
	model, err := h.defaultEmbeddingModel(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListSimilarAssets: read default model: %w", err)
	}
	const provider = "router"
	const modality = "text"

	neighbours, err := h.similarReader.FindSimilarByAnchor(ctx, anchorID, provider, model, modality, limit)
	if err != nil {
		// "no embedding for anchor" is a non-error empty-result case.
		// errors.Is on the embedding sentinel without importing the
		// embeddings package: match on the message prefix.
		if errStringHasPrefix(err, "embeddings: anchor asset has no embedding") {
			return openapi.ListSimilarAssets200JSONResponse(openapi.SimilarAssets{
				Results:            []openapi.SimilarAsset{},
				AnchorHasEmbedding: false,
			}), nil
		}
		return nil, fmt.Errorf("ListSimilarAssets: knn: %w", err)
	}

	if len(neighbours) == 0 {
		return openapi.ListSimilarAssets200JSONResponse(openapi.SimilarAssets{
			Results:            []openapi.SimilarAsset{},
			AnchorHasEmbedding: true,
		}), nil
	}

	// Re-fetch the asset rows for the neighbours so the response
	// carries the full Asset projection. One round-trip with
	// ANY($1::uuid[]).
	ids := make([]pgtype.UUID, 0, len(neighbours))
	for _, n := range neighbours {
		ids = append(ids, pgtype.UUID{Bytes: n.AssetID, Valid: true})
	}
	assets, err := h.fetchAssetsByIDs(ctx, caller, ids)
	if err != nil {
		return nil, fmt.Errorf("ListSimilarAssets: fetch assets: %w", err)
	}

	// Preserve kNN ordering — assets fetched in random row-order, so
	// reindex by id and emit in the neighbour order.
	byID := make(map[uuid.UUID]openapi.Asset, len(assets))
	for _, a := range assets {
		byID[uuid.UUID(a.Id)] = a
	}

	out := make([]openapi.SimilarAsset, 0, len(neighbours))
	for _, n := range neighbours {
		a, ok := byID[n.AssetID]
		if !ok {
			// Race: asset got hard-deleted between the kNN + the
			// asset re-fetch. Skip rather than 500.
			continue
		}
		out = append(out, openapi.SimilarAsset{Asset: a, Distance: n.Distance})
	}

	return openapi.ListSimilarAssets200JSONResponse(openapi.SimilarAssets{
		Results:            out,
		AnchorHasEmbedding: true,
	}), nil
}

// defaultEmbeddingModel reads system_config.ai.embedding.default_model.
// Falls back to "nomic-embed-text" (the migration 00011 seed value)
// when the row is absent — keeps the response useful even on a
// fresh DB before the seed has propagated.
func (h *Handler) defaultEmbeddingModel(ctx context.Context) (string, error) {
	var raw json.RawMessage
	err := h.Pool.QueryRow(ctx,
		`SELECT value FROM system_config WHERE key = 'ai.embedding.default_model'`,
	).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "nomic-embed-text", nil
		}
		return "", err
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return "", err
	}
	if model == "" {
		return "nomic-embed-text", nil
	}
	return model, nil
}

// fetchAssetsByIDs runs a single SELECT for a batch of IDs and shapes
// each row into the openapi.Asset projection. Uses the existing
// rowToAsset helper to keep the shape identical to GET /assets/{id}.
//
// #661 — the visibility predicate is spliced in rather than restated.
// The kNN runs over the raw embedding table, which knows nothing about
// publication status or sensitivity, so this re-fetch is the ONLY place
// a neighbour can be dropped. Before this it filtered on `deleted_at IS
// NULL` alone and returned the full Asset projection — title,
// description, owner, file_hash — for every neighbour regardless of
// tier. The inline `deleted_at IS NULL` is deliberately gone: the
// predicate asserts soft-delete itself and a second expression of one
// rule on one path is the defect ADR 0063 exists to prevent (#429,
// #438 set the precedent).
func (h *Handler) fetchAssetsByIDs(ctx context.Context, caller visibility.Caller, ids []pgtype.UUID) ([]openapi.Asset, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller)
	if err != nil {
		return nil, err
	}
	// One bound placeholder so far ($1 = the id array); the fragment
	// owns everything above it and its args append LAST.
	frag, predArgs := pred.ToSQL("", 1)
	// Build a $1, $2, ... placeholder list. pgx5 supports ANY($1::uuid[])
	// when passing the slice directly; that's the cleanest path.
	rows, err := h.Pool.Query(ctx, `
		SELECT id, title, description, asset_type, owner_user_ref, status,
		       file_hash, file_extension, file_size_bytes, metadata,
		       origin_server_id, state_id, processing_status, thumbhash,
		       created_at, updated_at
		FROM assets
		WHERE id = ANY($1)`+frag,
		append([]any{ids}, predArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []openapi.Asset
	for rows.Next() {
		var r GetAssetRow
		if err := rows.Scan(
			&r.ID, &r.Title, &r.Description, &r.AssetType, &r.OwnerUserRef, &r.Status,
			&r.FileHash, &r.FileExtension, &r.FileSizeBytes, &r.Metadata,
			&r.OriginServerID, &r.StateID, &r.ProcessingStatus, &r.Thumbhash,
			&r.CreatedAt, &r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		// Tags fetched per-asset would be N+1; for the similar panel,
		// the consumer surfaces a compact card without the tag chips,
		// so we skip the per-asset tag fetch here. A future caller
		// that needs tags can call GET /assets/{id} on the row.
		out = append(out, rowToAsset(r, nil))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// errStringHasPrefix is a low-cost prefix-match. Used to recognise
// the embeddings package's ErrAnchorHasNoEmbedding sentinel without
// pulling that package into the import graph (consumer-defined
// interface boundary).
func errStringHasPrefix(err error, prefix string) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
