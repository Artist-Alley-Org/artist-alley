// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package aiedit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LineageStore wraps the sqlc Queries with typed in/out and
// pgtype.UUID ↔ uuid.UUID conversions so callers don't repeat them.
// Same shape as the other domain stores in this repo
// (storage.Service, ai/mcp_registry.Registry, …).
type LineageStore struct {
	pool *pgxpool.Pool
	q    *Queries
}

// NewLineageStore wires the store against the global pool. Pool may
// be nil for tests that inject a stub *Queries directly via the
// internal-test seam; production wires through here.
func NewLineageStore(pool *pgxpool.Pool) *LineageStore {
	return &LineageStore{pool: pool, q: New(pool)}
}

// Lineage is the typed in-Go shape of a creative_lineage row. The
// sqlc-generated [CreativeLineage] uses pgtype.UUID + raw JSON;
// callers across the codebase want google/uuid.UUID + map[string]any
// for ergonomics.
type Lineage struct {
	DerivativeAssetID  uuid.UUID
	SourceAssetID      uuid.UUID
	GenerationMetadata map[string]any
	CreatedAt          time.Time
}

// InsertParams is the typed input shape for [LineageStore.Insert].
type InsertParams struct {
	DerivativeAssetID  uuid.UUID
	SourceAssetID      uuid.UUID
	GenerationMetadata map[string]any
}

// Insert records the derivative→source link. The handler / job
// caller persists this AFTER the derivative asset row has been
// committed (the FK on derivative_asset_id requires the asset to
// exist).
//
// Insert is NOT idempotent — derivative_asset_id is the PK, so a
// duplicate call surfaces a unique-violation. Callers that need
// "create-or-replace" semantics should DELETE first; the v1 use
// case (one job → one derivative → one lineage row) doesn't need
// that.
func (s *LineageStore) Insert(ctx context.Context, p InsertParams) (Lineage, error) {
	metaJSON, err := json.Marshal(p.GenerationMetadata)
	if err != nil {
		return Lineage{}, fmt.Errorf("aiedit.lineage.insert: marshal metadata: %w", err)
	}
	row, err := s.q.InsertCreativeLineage(ctx, InsertCreativeLineageParams{
		DerivativeAssetID:  pgtype.UUID{Bytes: p.DerivativeAssetID, Valid: true},
		SourceAssetID:      pgtype.UUID{Bytes: p.SourceAssetID, Valid: true},
		GenerationMetadata: metaJSON,
	})
	if err != nil {
		return Lineage{}, fmt.Errorf("aiedit.lineage.insert: %w", err)
	}
	return modelToLineage(row)
}

// GetByDerivative returns the row keyed by the derivative asset
// id, or [ErrLineageNotFound] if none exists.
func (s *LineageStore) GetByDerivative(ctx context.Context, derivativeID uuid.UUID) (Lineage, error) {
	row, err := s.q.GetCreativeLineageByDerivative(ctx, pgtype.UUID{Bytes: derivativeID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Lineage{}, ErrLineageNotFound
		}
		return Lineage{}, fmt.Errorf("aiedit.lineage.get_by_derivative: %w", err)
	}
	return modelToLineage(row)
}

// ListBySource returns every derivative spawned from the source,
// newest first. Empty slice when there are none — never a sentinel.
func (s *LineageStore) ListBySource(ctx context.Context, sourceID uuid.UUID) ([]Lineage, error) {
	rows, err := s.q.ListCreativeLineageBySource(ctx, pgtype.UUID{Bytes: sourceID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("aiedit.lineage.list_by_source: %w", err)
	}
	out := make([]Lineage, 0, len(rows))
	for _, row := range rows {
		l, err := modelToLineage(row)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// ErrLineageNotFound is returned by [LineageStore.GetByDerivative]
// when no lineage row exists. The asset detail view checks this to
// decide between "AI-generated, show provenance" vs "uploaded, show
// upload metadata".
var ErrLineageNotFound = errors.New("aiedit: lineage row not found")

// modelToLineage converts the sqlc-generated row into the
// google/uuid + map[string]any shape callers prefer.
func modelToLineage(row CreativeLineage) (Lineage, error) {
	out := Lineage{
		DerivativeAssetID: row.DerivativeAssetID.Bytes,
		SourceAssetID:     row.SourceAssetID.Bytes,
		CreatedAt:         row.CreatedAt.Time,
	}
	if len(row.GenerationMetadata) > 0 {
		var meta map[string]any
		if err := json.Unmarshal(row.GenerationMetadata, &meta); err != nil {
			return Lineage{}, fmt.Errorf("aiedit.lineage: unmarshal metadata: %w", err)
		}
		out.GenerationMetadata = meta
	}
	return out, nil
}
