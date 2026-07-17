// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// GitHub #341 — admin-curated featured-content list.
//
// The Handler is the package's domain surface: an ordered list of
// hand-picked spotlight entries a home/browse surface can render as a
// curated rail. Each entry points at an asset or a collection and
// carries an explicit position. The handler is deliberately thin —
// four operations (List / Add / Remove / Reorder) over a single flat
// table, no cache, no jobs, no notifications. Admin-only, tiny result
// set; the ABC (always-be-caching) convention is noted as a follow-up
// rather than applied to a list this small and rarely read.
//
// Kept separate from the per-collection `collections.featured`
// boolean, which is a browse filter flag, not an ordered mixed-kind
// curation surface.

package featured

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Capabilities for the featured curation surface. Reads take the
// dedicated read cap so a read-only auditor role can view the list
// (#356); every write still requires system.admin, which wildcards
// every capability check.
const (
	CapFeaturedRead = "featured.read"
	CapSystemAdmin  = "system.admin"
)

// ErrAlreadyFeatured is returned by Add when the (subject_kind,
// subject_id) unique constraint rejects a duplicate. The HTTP layer
// maps it to 409.
var ErrAlreadyFeatured = errors.New("featured: subject already featured")

// ErrNotFound is returned by Remove when no row matches the id.
// Mapped to HTTP 404.
var ErrNotFound = errors.New("featured: not found")

// Handler is the public surface. Construct via NewHandler at boot.
type Handler struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// NewHandler builds the Handler.
func NewHandler(pool *pgxpool.Pool, logger *slog.Logger) *Handler {
	return &Handler{Pool: pool, Logger: logger}
}

// AddInput is the parameter list for Add. Position nil means append.
type AddInput struct {
	SubjectKind string
	SubjectID   uuid.UUID
	Position    *int32
	CreatedBy   *int64
}

// List returns the curation list in display order (position asc, then
// created_at). Each row resolves its subject's display title plus, for
// asset subjects, thumbnail hints.
func (h *Handler) List(ctx context.Context) ([]ListFeaturedItemsRow, error) {
	return New(h.Pool).ListFeaturedItems(ctx)
}

// Add appends (or inserts at Position) a subject. A duplicate subject
// surfaces as ErrAlreadyFeatured.
func (h *Handler) Add(ctx context.Context, in AddInput) (FeaturedItem, error) {
	row, err := New(h.Pool).InsertFeaturedItem(ctx, InsertFeaturedItemParams{
		SubjectKind:      in.SubjectKind,
		SubjectID:        pgtype.UUID{Bytes: in.SubjectID, Valid: true},
		Position:         in.Position,
		CreatedByUserRef: in.CreatedBy,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return FeaturedItem{}, ErrAlreadyFeatured
		}
		return FeaturedItem{}, fmt.Errorf("featured: insert: %w", err)
	}
	return row, nil
}

// Remove deletes one entry by its featured_items id. Missing rows
// surface as ErrNotFound.
func (h *Handler) Remove(ctx context.Context, id uuid.UUID) error {
	n, err := New(h.Pool).DeleteFeaturedItem(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		return fmt.Errorf("featured: delete: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Reorder assigns each id its 0-based index in the supplied slice as
// its new position, in one transaction. Ids not present are left
// untouched; unknown ids are silently ignored (a concurrent remove
// racing the reorder shouldn't fail the whole batch).
func (h *Handler) Reorder(ctx context.Context, ids []uuid.UUID) error {
	return pgx.BeginFunc(ctx, h.Pool, func(tx pgx.Tx) error {
		q := New(tx)
		for i, id := range ids {
			pos := int32(i)
			if _, err := q.UpdateFeaturedPosition(ctx, UpdateFeaturedPositionParams{
				ID:       pgtype.UUID{Bytes: id, Valid: true},
				Position: pos,
			}); err != nil {
				return fmt.Errorf("featured: update position: %w", err)
			}
		}
		return nil
	})
}
