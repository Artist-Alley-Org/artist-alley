// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// SaveAsCollectionMaxHits caps the number of search hits a single
// save-as-collection call materialises into collection_resources
// rows. Protects against runaway 100k-result saves. Above the cap
// the collection is created with the first N hits + Truncated=true
// in the response body.
//
// B-2 note: caps at 100 because the underlying Engine caps a single
// page at MaxLimit=100. A future revision could iterate through
// cursors to reach the brief's target of 1000; deferred to Phase
// 1.16.B-4 where saved-search re-runs need the same execution loop.
const SaveAsCollectionMaxHits = 100

// SaveAsCollectionHandler wraps the save endpoint.
type SaveAsCollectionHandler struct {
	Service *Service
	Pool    *pgxpool.Pool
}

// saveAsCollectionRequest is the JSON body clients POST.
type saveAsCollectionRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Q           string   `json:"q"`
	DSL         string   `json:"dsl"`
	Types       []string `json:"types"`
}

// saveAsCollectionResponse carries the newly created collection ID
// + how many hits were saved.
type saveAsCollectionResponse struct {
	CollectionID string `json:"collection_id"`
	SavedCount   int    `json:"saved_count"`
	Truncated    bool   `json:"truncated"`
}

// ServeHTTP implements http.Handler.
//
// Wire shape:
//
//	POST /search/save-as-collection
//	Content-Type: application/json
//	{"name": "My Cat Photos", "q": "cat", "types": ["asset"]}
//
// Response:
//
//	201 Created
//	{"collection_id": "0192...", "saved_count": 42, "truncated": false}
//
// Auth: caller must be authenticated. Anonymous → 401.
// Visibility: hits are already visibility-gated by the Engine
// before persistence, so a caller can only save what they can see.
func (h *SaveAsCollectionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	id := auth.IdentityFromContext(r.Context())
	if id == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication_required"})
		return
	}

	var req saveAsCollectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_body"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name_required"})
		return
	}
	if req.Q == "" && req.DSL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query_required"})
		return
	}

	// Execute the query. Force types=asset (only assets flow into
	// collection_resources) even if the caller passed a mixed list
	// — matches RS behaviour + the collection semantics.
	types, _ := ParseTypes("asset")
	q := Query{
		Text:          req.Q,
		Types:         types,
		Limit:         SaveAsCollectionMaxHits,
		CallerUserRef: &id.UserRef,
	}
	// Force a fresh, cache-bypassing execution so the operator sees
	// current-truth hits (the /search cache serves 25-hit pages by
	// default; save-as-collection wants all hits up to the cap).
	res, err := h.Service.engine.Run(r.Context(), q)
	if err != nil {
		if errors.Is(err, ErrEmptyQuery) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "query_required"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
		return
	}
	// Trim if we bumped the cap.
	truncated := len(res.Hits) >= SaveAsCollectionMaxHits && res.TotalCount > SaveAsCollectionMaxHits
	if len(res.Hits) > SaveAsCollectionMaxHits {
		res.Hits = res.Hits[:SaveAsCollectionMaxHits]
	}

	// Persist: one INSERT for the collection + a bulk INSERT for
	// collection_resources.
	collID, err := createCollectionWithResults(r.Context(), h.Pool, id.UserRef, req, res)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist_failed"})
		return
	}

	writeJSON(w, http.StatusCreated, saveAsCollectionResponse{
		CollectionID: collID.String(),
		SavedCount:   len(res.Hits),
		Truncated:    truncated,
	})
}

// createCollectionWithResults inserts a collection + its resources
// inside one tx. Rollback on any failure.
func createCollectionWithResults(ctx context.Context, pool *pgxpool.Pool, ownerRef int64, req saveAsCollectionRequest, res QueryResult) (uuid.UUID, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Build the smart_query payload: DSL string if provided; else the
	// raw q= text. Consumed by Phase 1.16.B-4 saved-search re-runs.
	smart := req.DSL
	if smart == "" {
		smart = req.Q
	}

	var collID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO collections (owner_user_ref, name, description, visibility, membership, smart_query)
		VALUES ($1, $2, $3, 'private', 'manual', $4)
		RETURNING id
	`, ownerRef, req.Name, req.Description, smart).Scan(&collID); err != nil {
		return uuid.Nil, fmt.Errorf("insert collection: %w", err)
	}

	// Bulk insert resources with a per-row rank preserving score
	// order. Uses UNNEST-driven inserts to keep the round-trips
	// bounded; for 1000 hits this is one query.
	if len(res.Hits) > 0 {
		ids := make([]pgtype.UUID, 0, len(res.Hits))
		ranks := make([]int32, 0, len(res.Hits))
		for i, h := range res.Hits {
			ids = append(ids, pgtype.UUID{Bytes: h.ID, Valid: true})
			ranks = append(ranks, int32(i))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned)
			SELECT $1, u.rid, u.r, false
			  FROM UNNEST($2::UUID[], $3::INTEGER[]) AS u(rid, r)
			ON CONFLICT DO NOTHING
		`, collID, ids, ranks); err != nil {
			return uuid.Nil, fmt.Errorf("insert resources: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return collID, nil
}
