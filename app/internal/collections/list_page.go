// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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
// THE INLINE SOFT-DELETE CLAUSE STAYS, and that is a deliberate
// departure from #429 and #438, which both deleted theirs.
//
// Those two could delete it because the predicate expresses soft-delete
// on their branches. It does NOT here. The authenticated
// EntityCollection branch is `owner OR ACL grant` with no deleted_at
// conjunct at all — it is the only authenticated branch in the
// predicate without one (asset and post both assert it). Dropping the
// inline clause on that basis was tried first and the parity test
// caught it immediately: every authenticated caller started seeing
// soft-deleted collections in their browse list, and `include_deleted`
// became meaningless because deleted rows were returned unconditionally.
//
// So this is not a second expression of a rule the predicate already
// states — it is the ONLY expression of a rule the predicate does not
// state for this caller class. The duplication for ANONYMOUS callers
// (whose branch does assert deleted_at IS NULL) is accepted as the
// cheaper half of the trade: a redundant conjunct costs nothing, and
// the alternative is leaking deleted rows to everyone signed in.
//
// The real fix is to give the authenticated collection branch a
// soft-delete conjunct like every other branch has, at which point this
// clause can go. That is a change at the enforcement point affecting
// twelve splice sites, so it belongs in its own PR, not this one.
//
// IncludeDeleted therefore drives BOTH: it waives the inline clause and
// maps onto visibility.IncludeSoftDeleted so the anonymous branch
// agrees. The caller (Handler.ListCollections) has already checked it
// is superadmin-only.
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
	// Featured now resolves through featured_items at scope='org'
	// (ADR 0065). The boolean column is gone; the filter's MEANING is
	// unchanged for callers — "is this featured internally" — which is
	// what the collections hub's Featured tab has always asked.
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
       c.deleted_at, c.deleted_reason`

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
		p.IncludeDeleted,  // $1
		p.OwnerUserRef,    // $2
		p.ExcludeOwner,    // $3
		p.Visibility,      // $4
		p.Featured,        // $5
		p.QName,           // $6
		p.SharedWithUser,  // $7
		p.CursorCreatedAt, // $8
		p.CursorID,        // $9
		p.RowLimit,        // $10
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
WHERE ($1::BOOLEAN IS TRUE OR c.deleted_at IS NULL)
  AND ($2::BIGINT IS NULL OR c.owner_user_ref = $2::BIGINT)
  AND ($3::BIGINT IS NULL OR c.owner_user_ref <> $3::BIGINT)
  AND ($4::TEXT IS NULL OR c.visibility = $4::TEXT)
  AND ($5::BOOLEAN IS NULL OR $5::BOOLEAN = EXISTS (
         SELECT 1 FROM featured_items fi
          WHERE fi.subject_kind = 'collection'
            AND fi.subject_id   = c.id
            AND fi.scope        = 'org'
       ))
  AND ($6::TEXT IS NULL OR c.name ILIKE '%' || $6::TEXT || '%')
  AND ($7::BIGINT IS NULL OR EXISTS (
         SELECT 1 FROM collection_acls a
          WHERE a.collection_id = c.id
            AND a.principal_type = 'user'
            AND a.principal_id   = $7::BIGINT::TEXT
            AND (a.expires_at IS NULL OR a.expires_at > NOW())
       ))
  AND ($8::TIMESTAMPTZ IS NULL
       OR c.created_at < $8::TIMESTAMPTZ
       OR (c.created_at = $8::TIMESTAMPTZ AND c.id < $9::UUID))`)
	b.WriteString(visFrag)
	b.WriteString(`
ORDER BY c.created_at DESC, c.id DESC
LIMIT $10::INTEGER`)

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
			&i.DeletedAt, &i.DeletedReason,
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
