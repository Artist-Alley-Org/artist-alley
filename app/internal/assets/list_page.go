// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
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
	IncludeDeleted *bool
	OwnerUserRef   *int64
	AssetType      *int64
	Status         *string
	Q              *string
	// Tag constrains the page to assets carrying one exact tag (#657).
	// It lives HERE, as one more optional filter on the gated query,
	// rather than in a separate by-tag query: the by-tag branch used to
	// be its own static sqlc statement, and being separate is precisely
	// how it ended up without the visibility predicate, without the
	// ladder and without the preview flags. One query, one set of rules.
	Tag             *string
	CursorCreatedAt pgtype.Timestamptz
	CursorID        pgtype.UUID
	RowLimit        int32
	// Ladder is the operator's CONFIGURED preview variant keys (#591),
	// supplied by the handler from the cached sysconfig reader. Empty
	// means "unknown", which LadderSatisfiedSQL resolves to false — the
	// client then falls back to the single `col` rung.
	Ladder []string
}

// listAssetsPageColumns mirrors the sqlc query's SELECT list exactly.
// Order matters: rows scan positionally.
const listAssetsPageColumns = `id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at, deleted_at, deleted_reason`

// ListAssetsPageGatedRow is a browse row plus the derived
// preview_available flag (#471). Embeds the sqlc row so callers keep
// scanning the same columns positionally via .ListAssetsPageRow.
type ListAssetsPageGatedRow struct {
	ListAssetsPageRow
	// PreviewAvailable: a servable `col` variant exists AND the caller
	// passes the content plane — computed here so the client renders a
	// thumbnail only when true, never firing a byte request that 404s.
	PreviewAvailable bool
	// LadderAvailable: EVERY variant in the configured ladder exists AND
	// the caller passes the content plane (#591). Same 0064 contract as
	// PreviewAvailable and derived from the SAME readability decision,
	// so the two can never disagree for a restricted asset.
	LadderAvailable bool
	// PixelWidth / PixelHeight: the recorded source dimensions, joined in
	// the same pass (#640). NOT gated on readability — they are metadata
	// about a row the caller can already see, the same plane as
	// file_size_bytes, and the client needs them to reserve the tile's
	// height before any bytes are requested. Nil when the install has
	// never measured this asset; see the pixeldims package.
	PixelWidth  *int32
	PixelHeight *int32
}

// ListAssetsPageGated runs the browse query for one caller. `caps` is
// the caller's capability checker (nil for anonymous), consulted only to
// short-circuit preview_available for SystemAdmin / content.read.all —
// it does NOT affect which rows are returned (that stays the predicate's
// job).
func ListAssetsPageGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	caps visibility.CapabilityChecker,
	p ListAssetsPageGatedParams,
) ([]ListAssetsPageGatedRow, error) {
	// Bind the caller-supplied filters first, so their indexes are
	// stable and the predicate's fragment can start above them. $8 is the
	// caller ref, used only by the team-membership EXISTS in the SELECT
	// (below) — it must sit within the builder's own placeholders so the
	// predicate fragment starts above it (ADR 0063 discipline).
	args := []any{
		p.OwnerUserRef,    // $1
		p.AssetType,       // $2
		p.Status,          // $3
		p.Q,               // $4
		p.CursorCreatedAt, // $5
		p.CursorID,        // $6
		p.RowLimit,        // $7
		caller.UserRef,    // $8 — anonymous carries ref 0, matching no membership
		p.Ladder,          // $9 — configured preview ladder (#591)
		p.Tag,             // $10 — optional single-tag filter (#657)
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
	// Two derived columns join preview_available's inputs in the SAME
	// pass — no per-asset round-trips on this browse hot path (#471):
	//   has_col_variant     — a servable `col` thumbnail exists
	//   has_full_ladder     — every CONFIGURED rung exists (#591)
	//   caller_is_team_member — caller belongs to a team-tier asset's team
	// Readability is then decided in-Go per row (visibility.ContentReadable)
	// from sensitivity + owner + that membership boolean + caps.
	b.WriteString(`SELECT ` + listAssetsPageColumns + `,
       sensitivity,
       ` + pixeldims.SelectColumnsSQL("assets.id") + `,
       (file_hash IS NOT NULL AND EXISTS (
            SELECT 1 FROM storage_variants sv
             WHERE sv.object_hash = assets.file_hash AND sv.variant_key = 'col')) AS has_col_variant,
       ` + sysconfig.LadderSatisfiedSQL("assets.file_hash", "$9") + ` AS has_full_ladder,
       (team_id IS NOT NULL AND EXISTS (
            SELECT 1 FROM team_memberships tm
             WHERE tm.team_id = assets.team_id AND tm.user_ref = $8::BIGINT)) AS caller_is_team_member
FROM assets
WHERE ($1::BIGINT IS NULL OR owner_user_ref = $1::BIGINT)
  AND ($2::BIGINT IS NULL OR asset_type = $2::BIGINT)
  AND ($3::TEXT IS NULL OR status = $3::TEXT)
  AND ($4::TEXT IS NULL OR search_text @@ plainto_tsquery('english', $4::TEXT))
  AND ($10::TEXT IS NULL
       OR EXISTS (SELECT 1 FROM asset_tag t
                   WHERE t.asset_id = assets.id AND t.tag = $10::TEXT))
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

	var out []ListAssetsPageGatedRow
	for rows.Next() {
		var i ListAssetsPageRow
		var (
			sensitivity        string
			pixelWidth         *int32
			pixelHeight        *int32
			hasColVariant      bool
			hasFullLadder      bool
			callerIsTeamMember bool
		)
		if err := rows.Scan(
			&i.ID, &i.Title, &i.Description, &i.AssetType, &i.OwnerUserRef, &i.Status,
			&i.FileHash, &i.FileExtension, &i.FileSizeBytes, &i.Metadata,
			&i.OriginServerID, &i.StateID, &i.ProcessingStatus, &i.Thumbhash,
			&i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason,
			&sensitivity, &pixelWidth, &pixelHeight,
			&hasColVariant, &hasFullLadder, &callerIsTeamMember,
		); err != nil {
			return nil, fmt.Errorf("assets: list page scan: %w", err)
		}
		readable := visibility.ContentReadable(sensitivity, i.OwnerUserRef, caller, caps, callerIsTeamMember)
		row := ListAssetsPageGatedRow{
			ListAssetsPageRow: i,
			PreviewAvailable:  hasColVariant && readable,
			LadderAvailable:   hasFullLadder && readable,
		}
		// A pair or neither — never a half-populated one the client has
		// to re-validate before dividing.
		if pixeldims.Sane(pixelWidth, pixelHeight) {
			row.PixelWidth, row.PixelHeight = pixelWidth, pixelHeight
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("assets: list page rows: %w", err)
	}
	return out, nil
}
