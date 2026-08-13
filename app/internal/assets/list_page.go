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
	Tag *string
	// TeamID scopes the page to one team's assets (#684). NARROWING
	// ONLY, and it is worth being blunt about why, because this is the
	// filter most likely to be mistaken for an access grant: it is a
	// plain conjunct ANDed beside the visibility predicate, never a
	// disjunct ORed into it. Asking for a team's page does not make the
	// caller a member of it. A `restricted` asset owned by this team is
	// still a placeholder for a non-member here — the same placeholder
	// browse already renders — because the predicate and the field
	// plane decide that, and neither one reads this field.
	TeamID          pgtype.UUID
	CursorCreatedAt pgtype.Timestamptz
	CursorID        pgtype.UUID
	RowLimit        int32
	// Ladder is the operator's CONFIGURED preview variant keys (#591),
	// supplied by the handler from the cached sysconfig reader. Empty
	// means "unknown", which LadderSatisfiedSQL resolves to false — the
	// client then falls back to the single `col` rung.
	Ladder []string
	// MutationCaps is the caller's resolved `assets.admin` scope
	// (#939). It widens the FIELD plane only — a holder sees the
	// titles of restricted assets in their team and still gets no
	// picture and no bytes. The zero value denies, so omitting it
	// fails closed.
	MutationCaps visibility.AssetMutationCaps
}

// listAssetsPageColumns mirrors the sqlc query's SELECT list exactly.
// Order matters: rows scan positionally.
const listAssetsPageColumns = `id, title, description, asset_type, owner_user_ref, status,
       file_hash, file_extension, file_size_bytes, metadata,
       origin_server_id, state_id, processing_status, thumbhash,
       created_at, updated_at, deleted_at, deleted_reason, team_id`

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
	// ScrubAvailable: a `sprites.vtt` cue file exists AND the caller
	// passes the content plane (#835). Same 0064 contract and the same
	// readability decision again — this is the card's hover-scrub gate,
	// which used to be inferred from the file extension.
	ScrubAvailable bool
	// PixelWidth / PixelHeight: the recorded source dimensions, joined in
	// the same pass (#640). Gated on Readable like every other column —
	// they used to be exempt on the reasoning that they are metadata
	// about a row the caller can already see, the same plane as
	// file_size_bytes. #899 retired that reasoning for the whole row:
	// file_size_bytes was a leak too, and a source resolution is a fact
	// about a file you cannot open. Nil when the install has never
	// measured this asset; see the pixeldims package.
	PixelWidth  *int32
	PixelHeight *int32

	// Readable is visibility.FieldsReadable for this row and this
	// caller (#899) — the ONE decision the three availability flags and
	// the placeholder are all derived from, so they can never disagree
	// on a restricted asset. False means the handler must replace every
	// asset column with the placeholder.
	Readable bool
	// OwnerDisplayName is the asset owner's display name (or username),
	// empty when unresolvable. The only asset-derived value the
	// placeholder is permitted to carry.
	OwnerDisplayName string
}

// ListAssetsPageGated runs the browse query for one caller. `caps` is
// the caller's capability checker (nil for anonymous).
//
// `caps` short-circuits preview_available for SystemAdmin /
// content.read.all, and — since #902 — it also reaches the `?q=` text
// match, which is readability-gated. That is the one way capabilities
// affect WHICH ROWS come back, and it is confined to the `?q=` branch:
// with no query text the predicate is still the whole row rule, so an
// unfiltered browse lists exactly what it always did, placeholders
// included. See the qMatch fragment below.
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
		p.TeamID,          // $11 — optional single-team filter (#684)
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

	// #902 — the `?q=` text match is READABILITY-GATED, and it is gated
	// by the same visibility.AssetSearchMatchSQL /search composes, not by
	// a second conjunct written here.
	//
	// This is the surface a search-only fix misses. `@@` decides in SQL
	// whether the row comes back, so before this a caller could type a
	// phrase from a restricted asset's withheld title into browse and
	// watch the page go from empty to one placeholder — then recover the
	// rest of the title token by token, through the field plane #899
	// closed on the payload.
	//
	// It gates ONLY the `?q=` branch, deliberately: with no query text
	// the conjunct short-circuits at `$4 IS NULL` and every row the
	// predicate returned is still listed, placeholder and all, which is
	// what ADR 0064 requires of browse.
	//
	// MutationCaps rides along because a team-scoped assets.admin holder
	// is owed the FIELDS of the assets they administer (#939) — the same
	// caps this function already hands the per-row FieldsReadable below,
	// so the rows that match and the rows that render agree.
	qMatch := visibility.AssetSearchMatchSQL(
		"assets", `plainto_tsquery('english', $4::TEXT)`, "$8",
		caller, visibility.ResolveContentCaps(caps), p.MutationCaps)

	// The deleted_at decision now lives entirely in the predicate —
	// there is deliberately no inline soft-delete clause here, so the
	// rule has exactly one expression on this path.
	var b strings.Builder
	// Derived columns join the readability inputs and preview_available's
	// inputs in the SAME pass — no per-asset round-trips on this browse
	// hot path (#471):
	//   visibility.FieldsColumnsSQL — sensitivity, status,
	//       processing_status, owner_user_ref, is_team_member and the
	//       owner's display name, as ONE fragment so this query cannot
	//       select four of the five and silently decide the fifth (#899)
	//   has_col_variant     — a servable `col` thumbnail exists
	//   has_full_ladder     — every CONFIGURED rung exists (#591)
	//   has_scrub_variant   — a `sprites.vtt` hover-scrub cue file exists (#835)
	// Readability is then decided in-Go per row
	// (visibility.FieldsReadable) from those columns + caps.
	b.WriteString(`SELECT ` + listAssetsPageColumns + `,
       ` + visibility.FieldsColumnsSQL("assets", "$8") + `,
       ` + pixeldims.SelectColumnsSQL("assets.id") + `,
       (file_hash IS NOT NULL AND EXISTS (
            SELECT 1 FROM storage_variants sv
             WHERE sv.object_hash = assets.file_hash AND sv.variant_key = 'col')) AS has_col_variant,
       ` + sysconfig.LadderSatisfiedSQL("assets.file_hash", "$9") + ` AS has_full_ladder,
       (file_hash IS NOT NULL AND EXISTS (
            SELECT 1 FROM storage_variants sv
             WHERE sv.object_hash = assets.file_hash AND sv.variant_key = 'sprites.vtt')) AS has_scrub_variant
FROM assets
WHERE ($1::BIGINT IS NULL OR owner_user_ref = $1::BIGINT)
  AND ($2::BIGINT IS NULL OR asset_type = $2::BIGINT)
  AND ($3::TEXT IS NULL OR status = $3::TEXT)
  AND ($4::TEXT IS NULL OR ` + qMatch + `)
  AND ($10::TEXT IS NULL
       OR EXISTS (SELECT 1 FROM asset_tag t
                   WHERE t.asset_id = assets.id AND t.tag = $10::TEXT))
  AND ($11::UUID IS NULL OR team_id = $11::UUID)
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
			fr              visibility.FieldsRow
			ownerName       string
			pixelWidth      *int32
			pixelHeight     *int32
			hasColVariant   bool
			hasFullLadder   bool
			hasScrubVariant bool
		)
		if err := rows.Scan(
			&i.ID, &i.Title, &i.Description, &i.AssetType, &i.OwnerUserRef, &i.Status,
			&i.FileHash, &i.FileExtension, &i.FileSizeBytes, &i.Metadata,
			&i.OriginServerID, &i.StateID, &i.ProcessingStatus, &i.Thumbhash,
			&i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason, &i.TeamID,
			&fr.Sensitivity, &fr.Status, &fr.ProcessingStatus, &fr.OwnerUserRef,
			&fr.TeamID, &fr.IsTeamMember, &ownerName,
			&pixelWidth, &pixelHeight,
			&hasColVariant, &hasFullLadder, &hasScrubVariant,
		); err != nil {
			return nil, fmt.Errorf("assets: list page scan: %w", err)
		}
		fr.ApplyMutationCaps(p.MutationCaps)
		// ONE readability decision feeds the three availability flags AND
		// the #899 field withholding, for the same reason the post
		// preview enrich does it that way: a true ladder flag on gated
		// bytes is a 403 the client walks straight into, and a false
		// withholding decision on a gated row is the leak #899 closes.
		//
		// FieldsReadable, not ContentReadable: this is the metadata
		// plane, and the two differ on a caller who passes the tier but
		// not the row's workflow state.
		readable := visibility.FieldsReadable(fr, caller, caps)
		// #939 — the three flags AND the thumbhash follow the PICTURE
		// plane, which a mutation capability does not confer (ADR 0064:
		// "a thumbhash IS a blur"). The field withholding follows
		// `readable`. One row, two decisions, both from the same pass.
		picture := visibility.PreviewReadable(fr, caller, caps)
		if !picture {
			i.Thumbhash = nil
		}
		row := ListAssetsPageGatedRow{
			ListAssetsPageRow: i,
			PreviewAvailable:  hasColVariant && picture,
			LadderAvailable:   hasFullLadder && picture,
			ScrubAvailable:    hasScrubVariant && picture,
			Readable:          readable,
			OwnerDisplayName:  ownerName,
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
