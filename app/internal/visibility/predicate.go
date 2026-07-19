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
// This is the SINGLE enforcement point for read visibility (#414).
// It is spliced into every hand-built read path — search, facets,
// suggest, saved-search execution, vector kNN — so editing a branch
// here changes behaviour at every one of those call sites at once.
// That is the design (one rule, one place), and it is why the entity ×
// caller matrix in predicate_test.go is a contract test on this
// function rather than a per-endpoint test. See ADR 0063.
//
// Semantics per entity type:
//
//   - Asset, anonymous: soft-delete + status='active' +
//     sensitivity='public' + processing_status='ready'. All four are
//     required: a draft or archived asset is not published, a
//     non-public sensitivity tier is not for strangers, and an asset
//     still processing has no derivatives to serve.
//   - Asset, authenticated: soft-delete ONLY, unchanged. An
//     authenticated non-owner can still list assets of any
//     sensitivity. That gap is deliberate and deferred, NOT an
//     oversight: closing it requires deciding what team / restricted
//     / embargo mean for reads, which is a product decision the
//     operator has not made. `sensitivity` is consumed only by the
//     federation gates today. The plumbing to close it is one branch
//     here. See ADR 0063.
//   - Collection, anonymous: soft-delete + visibility='public'. This
//     replaces a hard FALSE short-circuit that existed because no
//     collection COULD be public before migration 00008 added the
//     tier.
//   - Collection, authenticated: owner OR a live collection_acls
//     grant, unchanged.
//   - Post, anonymous: soft-delete + visibility='public'. The old
//     branch filtered on 'public' while the CHECK constraint forbade
//     that value, so it matched zero rows and only looked like
//     working anonymous support. 00008 makes it real.
//   - Post, authenticated: soft-delete + (public OR author), unchanged.
//
// The anonymous branches bind NO arguments. Callers append the
// returned args last and never hard-code a placeholder after the
// fragment, so a zero-arg fragment composes correctly — the asset
// branch has always returned nil args, so this shape is long-exercised.
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
		if p.caller.IsAnonymous {
			conds := []string{}
			if !p.includeSoftDeleted {
				conds = append(conds, a+"deleted_at IS NULL")
			}
			// These three always apply. IncludeSoftDeleted must never
			// reach them — that narrowness is the point (see the option).
			conds = append(conds,
				a+"status = 'active'",
				a+"sensitivity = 'public'",
				a+"processing_status = 'ready'",
			)
			return " AND (" + strings.Join(conds, " AND ") + ")", nil
		}
		// Authenticated: unchanged. Do not tighten here without
		// deciding the sensitivity rule — every splice site moves.
		if p.includeSoftDeleted {
			// Soft-delete is the whole authenticated rule today, so
			// waiving it leaves nothing to assert. Emitting TRUE rather
			// than "" keeps the "fragment always starts with AND"
			// contract every splice site relies on.
			return " AND (TRUE)", nil
		}
		return fmt.Sprintf(" AND (%sdeleted_at IS NULL)", a), nil
	case EntityCollection:
		if p.caller.IsAnonymous {
			if p.includeSoftDeleted {
				return fmt.Sprintf(" AND (%svisibility = 'public')", a), nil
			}
			return fmt.Sprintf(
				" AND (%sdeleted_at IS NULL AND %svisibility = 'public')", a, a,
			), nil
		}
		// Authenticated collections assert no soft-delete conjunct, so
		// IncludeSoftDeleted has nothing to waive here.
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
		if p.caller.IsAnonymous {
			// No author comparison: an anonymous caller cannot be an
			// author, and binding AnonymousCaller (0) against
			// author_user_ref would be a coincidence waiting to happen
			// if a real ref were ever 0.
			if p.includeSoftDeleted {
				return fmt.Sprintf(" AND (%svisibility = 'public')", a), nil
			}
			return fmt.Sprintf(
				" AND (%sdeleted_at IS NULL AND %svisibility = 'public')", a, a,
			), nil
		}
		idx := argOffset + 1
		if p.includeSoftDeleted {
			return fmt.Sprintf(
				" AND ((%svisibility = 'public' OR %sauthor_user_ref = $%d))", a, a, idx,
			), []any{p.caller.UserRef}
		}
		frag := fmt.Sprintf(
			" AND (%sdeleted_at IS NULL AND (%svisibility = 'public' OR %sauthor_user_ref = $%d))",
			a, a, a, idx,
		)
		return frag, []any{p.caller.UserRef}
	}
	// Unreachable — Filter constructor validates entity type.
	return " AND (FALSE)", nil
}
