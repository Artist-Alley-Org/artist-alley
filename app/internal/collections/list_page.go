// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/featured"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ListCollectionsPageGated is the collection browse query with the
// visibility predicate applied (#449).
//
// WHAT WAS WRONG: the sqlc ListCollectionsPage this replaces applied no
// visibility rule at all. Its WHERE clause was `deleted_at IS NULL`
// plus a row of OPTIONAL narg filters, so a caller who set none of them
// — which is what `GET /collections` does by default — got the entire
// table. Anonymous callers enumerated every collection on the install,
// private ones included, with names and their `visibility` values. The
// handler comment claimed "the predicate decides which rows they see";
// nothing on the path ever consulted it.
//
// This is the third conversion of this shape, after assets.ListAssetsPageGated
// (#429) and collections.ListCollectionResourcesPageGated (#438), and
// for the same reason: Predicate.ToSQL returns a runtime fragment while
// sqlc queries are static strings with fixed placeholders, so the
// fragment can only be spliced into SQL assembled at runtime.
//
// The narg filters, the (created_at DESC, id DESC) cursor and the
// SELECT list are carried over UNCHANGED. Those are product behaviour —
// the hub's mine/shared/featured tabs are built out of them — and are a
// separate axis from visibility. The predicate narrows what the caller
// may see; the filters narrow what they asked for. Both apply, and
// conflating them is how a "shared with me" tab quietly becomes an
// authorization decision.
//
// NO inline soft-delete clause — the predicate owns it, like #429 and
// #438. #449 kept an inline `deleted_at IS NULL` here because the
// authenticated EntityCollection branch lacked a soft-delete conjunct
// and this was the only expression of that rule for signed-in callers.
// #451 gave that branch the conjunct (matching asset and post), so the
// inline clause became a genuine second expression of a rule the
// predicate now states everywhere — exactly the ADR 0063 defect — and
// is removed. IncludeDeleted now drives ONLY the predicate's
// IncludeSoftDeleted option; the caller (Handler.ListCollections) has
// already checked it is superadmin-only.
//
// Placeholder discipline (ADR 0063): every placeholder this builder
// emits is <= argOffset, the predicate's fragment owns everything above
// it, and predicate args are appended LAST. LIMIT binds before the
// fragment renders even though it reads later in the statement — the
// invariant is an index bound, not textual order.
type ListCollectionsPageGatedParams struct {
	// IncludeDeleted is superadmin-only and is enforced as such by the
	// caller. It waives ONLY the soft-delete dimension of the
	// predicate — never visibility or ownership.
	IncludeDeleted *bool
	OwnerUserRef   *int64
	ExcludeOwner   *int64
	Visibility     *string
	// Featured now resolves through featured_items (ADR 0065). The
	// boolean column is gone; the filter's MEANING is "is this
	// collection featured to THIS viewer", which is what the hub's
	// Featured tab has always meant to ask.
	//
	// It asked `scope = 'org'` until #1104, which is why the tab was
	// empty on every install whose placements came from the seed (all
	// `public`). The audience now comes from featured.ScopeVisibleSQL,
	// the same expression the browse rail splices — one rule, two
	// surfaces, per #1063.
	Featured        *bool
	QName           *string
	SharedWithUser  *int64
	CursorCreatedAt pgtype.Timestamptz
	CursorID        pgtype.UUID
	RowLimit        int32
}

// listCollectionsPageColumns mirrors the sqlc query's SELECT list
// exactly. Order matters: rows scan positionally into Collection.
const listCollectionsPageColumns = `c.id, c.owner_user_ref, c.name, c.description, c.visibility, c.membership,
       c.expires_at, c.purpose, c.origin_server_id,
       c.created_at, c.updated_at, c.search_text, c.smart_query,
       c.deleted_at, c.deleted_reason, c.deleted_by_user_ref, c.cover_asset_id`

// ListCollectionsPageGated runs the browse query for one caller.
func ListCollectionsPageGated(
	ctx context.Context,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	p ListCollectionsPageGatedParams,
) ([]Collection, error) {
	// Bind the caller-supplied filters first, so their indexes are
	// stable and the predicate's fragment can start above them.
	args := []any{
		p.OwnerUserRef,    // $1
		p.ExcludeOwner,    // $2
		p.Visibility,      // $3
		p.Featured,        // $4
		p.QName,           // $5
		p.SharedWithUser,  // $6
		p.CursorCreatedAt, // $7
		p.CursorID,        // $8
		p.RowLimit,        // $9
	}

	var opts []visibility.Option
	if p.IncludeDeleted != nil && *p.IncludeDeleted {
		opts = append(opts, visibility.IncludeSoftDeleted())
	}
	pred, err := visibility.Filter(ctx, visibility.EntityCollection, caller, opts...)
	if err != nil {
		return nil, fmt.Errorf("collections: visibility filter: %w", err)
	}
	visFrag, visArgs := pred.ToSQL("c", len(args))
	args = append(args, visArgs...) // predicate args LAST

	var b strings.Builder
	b.WriteString(`SELECT ` + listCollectionsPageColumns + `
FROM collections c
WHERE ($1::BIGINT IS NULL OR c.owner_user_ref = $1::BIGINT)
  AND ($2::BIGINT IS NULL OR c.owner_user_ref <> $2::BIGINT)
  AND ($3::TEXT IS NULL OR c.visibility = $3::TEXT)
  AND ($4::BOOLEAN IS NULL OR $4::BOOLEAN = EXISTS (
         SELECT 1 FROM featured_items fi
          WHERE fi.subject_kind = 'collection'
            AND fi.subject_id   = c.id
            AND ` + featured.ScopeVisibleSQL("fi", caller) + `
       ))
  AND ($5::TEXT IS NULL OR c.name ILIKE '%' || $5::TEXT || '%')
  AND ($6::BIGINT IS NULL OR EXISTS (
         SELECT 1 FROM collection_acls a
          WHERE a.collection_id = c.id
            AND a.principal_type = 'user'
            AND a.principal_id   = $6::BIGINT::TEXT
            AND (a.expires_at IS NULL OR a.expires_at > NOW())
       ))
  AND ($7::TIMESTAMPTZ IS NULL
       OR c.created_at < $7::TIMESTAMPTZ
       OR (c.created_at = $7::TIMESTAMPTZ AND c.id < $8::UUID))`)
	b.WriteString(visFrag)
	b.WriteString(`
ORDER BY c.created_at DESC, c.id DESC
LIMIT $9::INTEGER`)

	rows, err := pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("collections: list page: %w", err)
	}
	defer rows.Close()

	var out []Collection
	for rows.Next() {
		var i Collection
		if err := rows.Scan(
			&i.ID, &i.OwnerUserRef, &i.Name, &i.Description, &i.Visibility, &i.Membership,
			&i.ExpiresAt, &i.Purpose, &i.OriginServerID,
			&i.CreatedAt, &i.UpdatedAt, &i.SearchText, &i.SmartQuery,
			&i.DeletedAt, &i.DeletedReason, &i.DeletedByUserRef, &i.CoverAssetID,
		); err != nil {
			return nil, fmt.Errorf("collections: list page scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collections: list page rows: %w", err)
	}
	return out, nil
}
