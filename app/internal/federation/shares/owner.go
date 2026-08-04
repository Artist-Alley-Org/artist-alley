// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The single expression of "who owns this shareable object", and
// the two authority questions built on top of it.
//
// Before #893 the owner map lived inline in the boot wiring
// (http/api.go's ownerResolverFor) and answered exactly one
// question — "may this HTTP caller GRANT a share on this object?"
// The transitive container fallback in gate.go needed the same
// notion for a different subject (the share's grantor, who is not
// the caller), which is precisely the duplication epic #665 exists
// to prevent. So the map moved here and both callers derive from
// it:
//
//   - [NewObjectOwnerResolver] — the grant-path adapter boot wires
//     into AdminHandler. Owner == caller.
//   - [Registry.grantorMayShare] — the gate-path check. Owner ==
//     the share row's grantor_user_ref, or the grantor holds
//     system.admin (which is what let them grant on someone else's
//     object in the first place; see AdminHandler.GrantFederationShare).
//
// Ownership resolution FAILS CLOSED. A missing row, a NULL
// owner_user_ref (assets.owner_user_ref is nullable — federated
// mirrors and system-imported rows carry no local owner), or a
// kind with no owner column all return ok=false, and every caller
// reads that as "no authority".

package shares

import (
	"context"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// objectOwnerColumn maps a share object kind to the table +
// column carrying its owning user_ref. The closed switch is what
// makes the string interpolation in [ObjectOwnerRef] safe — no
// caller-supplied text ever reaches the SQL text.
//
// workspace + brand_kit tables don't exist yet; user-kind shares
// are server-internal (the Accept(Follow) path). All three return
// ok=false, so no ownership claim over them ever succeeds.
func objectOwnerColumn(kind federation.ShareObjectKind) (table, column string, ok bool) {
	switch kind {
	case federation.ShareObjectKindPost:
		return "posts", "author_user_ref", true
	case federation.ShareObjectKindCollection:
		return "collections", "owner_user_ref", true
	case federation.ShareObjectKindAsset:
		return "assets", "owner_user_ref", true
	default:
		return "", "", false
	}
}

// ObjectOwnerRef resolves the user_ref owning (kind, objectID).
//
// ok=false means "no resolvable owner" and is NOT an error: the
// row may not exist, the kind may have no owner column, or the
// column may be NULL. Callers treat every ok=false as a denial.
// A genuine DB failure comes back as err.
func ObjectOwnerRef(ctx context.Context, db DBTX, kind federation.ShareObjectKind, objectID uuid.UUID) (int64, bool, error) {
	table, column, ok := objectOwnerColumn(kind)
	if !ok {
		return 0, false, nil
	}
	var ownerRef *int64
	err := db.QueryRow(ctx,
		"SELECT "+column+" FROM "+table+" WHERE id = $1",
		objectID,
	).Scan(&ownerRef)
	if err != nil {
		// No such row — indistinguishable from "not yours" for
		// authority purposes, and the caller maps it to a denial
		// (the grant path renders 403, the gate path renders
		// grantor_not_owner). Never leak the DB error class here;
		// it would tell a peer whether an object exists.
		return 0, false, nil
	}
	if ownerRef == nil {
		return 0, false, nil
	}
	return *ownerRef, true, nil
}

// NewObjectOwnerResolver returns the grant-path [ObjectOwnerResolver]
// boot wires into [AdminHandler]: "may this caller grant a share on
// this object?" answered as owner == caller. system.admin bypasses
// this check at the handler, before the resolver runs.
func NewObjectOwnerResolver(db DBTX) ObjectOwnerResolver {
	return func(ctx context.Context, kind federation.ShareObjectKind, objectID uuid.UUID, caller *auth.Identity) (bool, error) {
		if caller == nil {
			return false, nil
		}
		ownerRef, ok, err := ObjectOwnerRef(ctx, db, kind, objectID)
		if err != nil || !ok {
			return false, err
		}
		return ownerRef == caller.UserRef, nil
	}
}

// grantorAuthority answers the gate-path question for ONE share
// row: could this share's grantor have granted a share on the
// member directly?
//
// memberOwnerRef is the already-resolved owner of the member (one
// lookup per gate call, not one per candidate share). adminSeen
// memoizes the system.admin lookup for the duration of a single
// gate call — the common case never reaches it, since the grantor
// IS the owner.
//
// Deliberately NOT cached beyond the call. See the caching note in
// gate.go: the per-object LRU holds share ROWS, and folding a
// membership- or ownership-derived answer into it would make the
// "{kind}:{uuid}" key ambiguous.
func (r *Registry) grantorAuthority(ctx context.Context, grantorUserRef, memberOwnerRef int64, adminSeen map[int64]bool) (bool, error) {
	if grantorUserRef == memberOwnerRef {
		return true, nil
	}
	// Not the owner. The only other way they could have granted
	// on the member directly is the system.admin bypass the grant
	// handler applies. Resolved through auth's existing
	// role+grant−revoke query so admin means the same thing here
	// as it does everywhere else.
	if seen, ok := adminSeen[grantorUserRef]; ok {
		return seen, nil
	}
	held, err := auth.New(r.Pool).UserHoldsSystemAdmin(ctx, grantorUserRef)
	if err != nil {
		return false, err
	}
	isAdmin := held > 0
	adminSeen[grantorUserRef] = isAdmin
	return isAdmin, nil
}
