// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AdminHandler owns the admin-side reads + dismiss writes for the
// extraction_failure queue. The job handler writes rows via
// FailureWriter; this handler is the operator's mirror surface.
type AdminHandler struct {
	pool *pgxpool.Pool
	q    *Queries
}

// NewAdminHandler wires the queue admin handler. The pool is held
// directly for the count query (sqlc-generated COUNT path returns
// int64 cleanly) + the dismiss exec.
func NewAdminHandler(pool *pgxpool.Pool) *AdminHandler {
	return &AdminHandler{pool: pool, q: New(pool)}
}

// ListFailuresFilter mirrors the listMetadataExtractionFailures
// query-param shape. Empty pointer fields mean "no filter".
type ListFailuresFilter struct {
	ErrorKind *string
	Format    *string
	Limit     int32 // capped 1-200; default 50 enforced by caller
	Offset    int32
}

// FailureRow is the admin-side projection of one extraction_failure
// row, plain Go types (no pgtype) so the HTTP layer can marshal
// directly.
type FailureRow struct {
	ID          uuid.UUID
	AssetID     uuid.UUID
	Format      string
	ErrorKind   string
	Message     string
	FieldKey    string
	RawValue    []byte // raw JSONB bytes; HTTP layer unmarshals to any
	OccurredAt  time.Time
	DismissedAt *time.Time
}

// ListFailures returns the filtered + paginated page along with the
// TOTAL pending count under the same filter (used for the admin nav
// badge so the UI gets one round-trip per render).
func (h *AdminHandler) ListFailures(ctx context.Context, f ListFailuresFilter) ([]FailureRow, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Limit > 200 {
		f.Limit = 200
	}

	rows, err := h.q.ListPendingExtractionFailures(ctx, ListPendingExtractionFailuresParams{
		Limit:     f.Limit,
		Offset:    f.Offset,
		ErrorKind: f.ErrorKind,
		Format:    f.Format,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("metadata.AdminHandler.ListFailures: list: %w", err)
	}

	out := make([]FailureRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, failureRowFromDB(r))
	}

	total, err := h.countFiltered(ctx, f)
	if err != nil {
		return nil, 0, fmt.Errorf("metadata.AdminHandler.ListFailures: count: %w", err)
	}
	return out, total, nil
}

// countFiltered runs the same WHERE clause as the list query but
// returns the COUNT. Kept inline (rather than another sqlc query)
// because the filter shape is small + identical to the list.
func (h *AdminHandler) countFiltered(ctx context.Context, f ListFailuresFilter) (int64, error) {
	row := h.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM extraction_failure
		 WHERE dismissed_at IS NULL
		   AND ($1::TEXT IS NULL OR error_kind = $1::TEXT)
		   AND ($2::TEXT IS NULL OR format     = $2::TEXT)
	`, f.ErrorKind, f.Format)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ErrFailureNotFound is returned by DismissFailure when no row
// with the given id exists at all (even a dismissed one). The HTTP
// layer maps this to 404. An already-dismissed row is NOT an error
// — the dismiss is idempotent.
var ErrFailureNotFound = errors.New("metadata: extraction failure not found")

// DismissFailure soft-dismisses one row. Idempotent — if the row
// is already dismissed we still return nil. ErrFailureNotFound when
// the row id never existed.
func (h *AdminHandler) DismissFailure(ctx context.Context, id uuid.UUID) error {
	var exists bool
	err := h.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM extraction_failure WHERE id = $1)
	`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("metadata.AdminHandler.DismissFailure: probe: %w", err)
	}
	if !exists {
		return ErrFailureNotFound
	}
	pgID := pgtype.UUID{Bytes: id, Valid: true}
	if err := h.q.DismissExtractionFailure(ctx, pgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already-dismissed; idempotent OK
		}
		return fmt.Errorf("metadata.AdminHandler.DismissFailure: %w", err)
	}
	return nil
}

func failureRowFromDB(r ExtractionFailure) FailureRow {
	out := FailureRow{
		Format:    r.Format,
		ErrorKind: r.ErrorKind,
		Message:   r.Message,
		FieldKey:  r.FieldKey,
		RawValue:  r.RawValue,
	}
	if r.ID.Valid {
		out.ID = uuid.UUID(r.ID.Bytes)
	}
	if r.AssetID.Valid {
		out.AssetID = uuid.UUID(r.AssetID.Bytes)
	}
	if r.OccurredAt.Valid {
		out.OccurredAt = r.OccurredAt.Time
	}
	if r.DismissedAt.Valid {
		t := r.DismissedAt.Time
		out.DismissedAt = &t
	}
	return out
}
