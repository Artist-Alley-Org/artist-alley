// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"fmt"
	"strings"
)

// ToSQL renders the Predicate as a SQL WHERE-fragment against a
// specific table alias. Returns (fragment, args). fragment always
// begins with " AND (…)" — callers concatenate into an existing
// WHERE clause without any pre-processing.
//
// The argOffset param is the number of $-placeholders the caller
// has ALREADY bound in their query; the first placeholder in
// fragment will be $(argOffset+1). This lets callers compose the
// predicate into any position in their query without rewriting
// existing bindings.
//
// Semantics per entity type (mirrors the effective behaviour of
// each entity's ListXxxPage handler + the search Engine's inline
// filters shipped in 1.16.B-1):
//
//   - Asset: soft-delete gate only. Sensitivity gating (public /
//     team / restricted / embargo) is a follow-up phase — the
//     baseline migration comment at 00001 explicitly documents
//     it isn't enforced by ListAssetsPage today. Matches B-1
//     Engine behaviour.
//   - Collection: caller must be the owner OR hold a live
//     collection_acls grant. Anonymous caller returns "false"
//     literal so the caller's query returns zero rows without a
//     dedicated NULL check.
//   - Post: soft-delete gate + visibility='public' OR author matches
//     caller. Anonymous caller sees public rows only (caller ref
//     is [AnonymousCaller], which cannot equal any real
//     author_user_ref).
func (p Predicate) ToSQL(alias string, argOffset int) (fragment string, args []any) {
	a := strings.TrimSpace(alias)
	if a == "" {
		// No alias — direct column references. Rare but supported
		// for queries against a single un-aliased FROM.
		a = ""
	} else {
		a = a + "."
	}
	switch p.entity {
	case EntityAsset:
		return fmt.Sprintf(" AND (%sdeleted_at IS NULL)", a), nil
	case EntityCollection:
		if p.caller.IsAnonymous {
			// Anonymous callers cannot own or be ACL-granted; short-
			// circuit to always-false so upstream aggregators emit
			// zero rows without an extra branch.
			return " AND (FALSE)", nil
		}
		idx := argOffset + 1
		frag := fmt.Sprintf(
			" AND (%sowner_user_ref = $%d OR EXISTS ("+
				"SELECT 1 FROM collection_acls acl "+
				"WHERE acl.collection_id = %sid "+
				"AND acl.principal_type = 'user' "+
				"AND acl.principal_id = $%d::TEXT "+
				"AND (acl.expires_at IS NULL OR acl.expires_at > NOW())))",
			a, idx, a, idx,
		)
		return frag, []any{p.caller.UserRef}
	case EntityPost:
		idx := argOffset + 1
		frag := fmt.Sprintf(
			" AND (%sdeleted_at IS NULL AND (%svisibility = 'public' OR %sauthor_user_ref = $%d))",
			a, a, a, idx,
		)
		return frag, []any{p.caller.UserRef}
	}
	// Unreachable — Filter constructor validates entity type.
	return " AND (FALSE)", nil
}
