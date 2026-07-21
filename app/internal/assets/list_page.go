// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ListAssetsPageGated is the asset browse query with the visibility
// predicate applied (#429).
//
// Why this is hand-built SQL rather than the sqlc query it replaces:
// visibility.Predicate.ToSQL returns a runtime fragment, and sqlc
// queries are static strings with fixed placeholders. A fragment can
// only be spliced into SQL assembled at runtime — which is why every
// one of the predicate's other splice sites is hand-built too. This was
// the last content read path the single enforcement point could not
// reach.
//
// The SELECT list, filters, ordering and cursor semantics are carried
// over from the sqlc ListAssetsPage unchanged; the only behavioural
// difference is that visibility is now decided by the predicate instead
// of by an inline deleted_at clause.
//
// Placeholder discipline (ADR 0063): every placeholder this builder
// emits is <= argOffset, the predicate's own fragment owns everything
// above it, and predicate args are appended LAST. LIMIT is bound before
// the fragment is rendered even though it reads later in the statement
// — the invariant is an index bound, not textual order.
type ListAssetsPageGatedParams struct {
	// IncludeDeleted is superadmin-only and is enforced as such by the
	// caller (assets.Handler). It waives ONLY the soft-delete dimension
	// of the predicate — never publication, sensitivity or processing
	// state. See visibility.IncludeSoftDeleted.
	IncludeDeleted  *bool
	OwnerUserRef    *int64
	AssetType       *int64
	Status          *string
	Q               *string
	CursorCreatedAt pgtype.Timestamptz
	CursorID        pgtype.UUID
	RowLimit        int32
}

// listAssetsPageColumns mirrors the sqlc query's SELECT list exactly.
// Order matters: rows scan positionally.
const listAssetsPageColumns = `id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at, deleted_at, deleted_reason`

// ListAssetsPageGated runs the browse query for one caller.
func ListAssetsPageGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	p ListAssetsPageGatedParams,
) ([]ListAssetsPageRow, error) {
	// Bind the caller-supplied filters first, so their indexes are
	// stable and the predicate's fragment can start above them.
	args := []any{
		p.OwnerUserRef,    // $1
		p.AssetType,       // $2
		p.Status,          // $3
		p.Q,               // $4
		p.CursorCreatedAt, // $5
		p.CursorID,        // $6
		p.RowLimit,        // $7
	}

	var opts []visibility.Option
	if p.IncludeDeleted != nil && *p.IncludeDeleted {
		// Superadmin escape hatch. Narrow by construction: the option
		// waives the soft-delete conjunct and nothing else, so an
		// authorization rule can never be skipped by setting a flag.
		opts = append(opts, visibility.IncludeSoftDeleted())
	}
	pred, err := visibility.Filter(ctx, visibility.EntityAsset, caller, opts...)
	if err != nil {
		return nil, fmt.Errorf("assets: visibility filter: %w", err)
	}
	visFrag, visArgs := pred.ToSQL("", len(args))
	args = append(args, visArgs...) // predicate args LAST

	// The deleted_at decision now lives entirely in the predicate —
	// there is deliberately no inline soft-delete clause here, so the
	// rule has exactly one expression on this path.
	var b strings.Builder
	b.WriteString(`SELECT ` + listAssetsPageColumns + `
FROM assets
WHERE ($1::BIGINT IS NULL OR owner_user_ref = $1::BIGINT)
  AND ($2::BIGINT IS NULL OR asset_type = $2::BIGINT)
  AND ($3::TEXT IS NULL OR status = $3::TEXT)
  AND ($4::TEXT IS NULL OR search_text @@ plainto_tsquery('english', $4::TEXT))
  AND ($5::TIMESTAMPTZ IS NULL
       OR created_at < $5::TIMESTAMPTZ
       OR (created_at = $5::TIMESTAMPTZ AND id < $6::UUID))`)
	b.WriteString(visFrag)
	b.WriteString(`
ORDER BY created_at DESC, id DESC
LIMIT $7::INTEGER`)

	rows, err := pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("assets: list page: %w", err)
	}
	defer rows.Close()

	var out []ListAssetsPageRow
	for rows.Next() {
		var i ListAssetsPageRow
		if err := rows.Scan(
			&i.ID, &i.Title, &i.Description, &i.AssetType, &i.OwnerUserRef, &i.Status,
			&i.FileHash, &i.FileExtension, &i.FileSizeBytes, &i.Metadata,
			&i.OriginServerID, &i.StateID, &i.ProcessingStatus, &i.Thumbhash,
			&i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason,
		); err != nil {
			return nil, fmt.Errorf("assets: list page scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assets: list page rows: %w", err)
	}
	return out, nil
}
