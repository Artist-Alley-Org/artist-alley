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

	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
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

	// previewLadder reports the operator's CONFIGURED preview variant
	// keys, cached (#591). nil-safe: nil means no ladder, so
	// ladder_available is false and the rail keeps using `col`.
	previewLadder sysconfig.PreviewLadderReader
}

// SetPreviewLadder installs the cached configured-ladder reader (#591).
func (h *Handler) SetPreviewLadder(r sysconfig.PreviewLadderReader) { h.previewLadder = r }

// Ladder returns the configured preview variant keys, or nil when the
// reader is not wired — the conservative answer (ladder_available false).
func (h *Handler) Ladder(ctx context.Context) []string {
	if h.previewLadder == nil {
		return nil
	}
	return h.previewLadder(ctx)
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

	// Scope is the placement's AUDIENCE (#1104, closing the question
	// #1088 raised). Empty means ScopeOrg, which is what every write
	// produced before this field existed, so an old client's behaviour
	// is unchanged.
	//
	// Admissible: ScopeOrg and ScopePublic.
	//
	// ScopeTeam is NOT writable here and that is deliberate rather than
	// pending. A team placement needs a team_id — featured_items_team_
	// scope_check binds the two — and nothing on the write path names a
	// team, so accepting the scope would produce a row Postgres
	// rejects. Team placements would need their own input shape and
	// their own gate (an operator featuring content INTO one studio is
	// a different act from featuring it to the install), and no reader
	// consumes scope='team' today. It stays unwritable until one does.
	//
	// # The gate on ScopePublic
	//
	// system.admin — stated explicitly because it is worth knowing that
	// it is not a NARROWER gate than the one on org. POST /admin/
	// featured is system.admin in its entirety (http.go), and the
	// capability set holds nothing between "may curate" and "may curate
	// for anonymous visitors": featured.read is a READ cap (#356) and
	// there is no featured.write. So v1 gates `public` on the same cap
	// that gates the endpoint, and the distinction lives in the API
	// being explicit about the audience rather than defaulting to the
	// bigger one. If a narrower curation cap is ever introduced, `org`
	// is the write that drops to it and `public` is the write that
	// stays on system.admin — that is the split this comment exists to
	// preserve.
	Scope string
}

// List returns the curation list in display order (position asc, then
// created_at). Each row resolves its subject's display title plus, for
// asset subjects, thumbnail hints.
func (h *Handler) List(ctx context.Context) ([]ListFeaturedItemsRow, error) {
	return New(h.Pool).ListFeaturedItems(ctx, h.Ladder(ctx))
}

// ErrScopeNotWritable is returned by Add for an audience the write path
// does not accept. Mapped to HTTP 400.
var ErrScopeNotWritable = errors.New("featured: scope must be org or public")

// Add appends (or inserts at Position) a subject. A duplicate subject
// AT THE SAME AUDIENCE surfaces as ErrAlreadyFeatured; the same subject
// at another scope is a distinct placement and is allowed.
func (h *Handler) Add(ctx context.Context, in AddInput) (FeaturedItem, error) {
	// The admissible-audience check lives here rather than only in the
	// HTTP layer so a non-HTTP caller (the seed, a future job) cannot
	// route around it and hand Postgres a 23514 that surfaces as a 500.
	// Same argument the subject_kind check makes in http.go: the
	// constraint is the backstop, this is the contract.
	scope := in.Scope
	if scope == "" {
		scope = ScopeOrg
	}
	if scope != ScopeOrg && scope != ScopePublic {
		return FeaturedItem{}, ErrScopeNotWritable
	}
	row, err := New(h.Pool).InsertFeaturedItem(ctx, InsertFeaturedItemParams{
		SubjectKind:      in.SubjectKind,
		SubjectID:        pgtype.UUID{Bytes: in.SubjectID, Valid: true},
		Position:         in.Position,
		CreatedByUserRef: in.CreatedBy,
		Scope:            &scope,
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
