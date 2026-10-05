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
//   - Collection, authenticated: soft-delete + (public OR owner OR a
//     live collection_acls grant). The public disjunct keeps the
//     invariant that authenticated callers see at least what anonymous
//     ones do; the soft-delete conjunct spans the whole predicate (#451)
//     so an owner's soft-deleted collections stay out of browse lists,
//     matching the asset and post branches.
//   - Post, anonymous: soft-delete + PUBLISHED + visibility='public'.
//     The old branch filtered on 'public' while the CHECK constraint
//     forbade that value, so it matched zero rows and only looked like
//     working anonymous support. 00008 makes it real. Publication is a
//     separate conjunct as of #1161 — a draft is not a stranger's to
//     read at any tier — and the anonymous read rule carries it too,
//     so [IncludeDrafts] cannot open one to an unauthenticated caller.
//   - Post, authenticated: soft-delete + PUBLISHED + the full post read rule —
//     author OR public/org-only OR private-with-posts.admin OR
//     followers-you-follow OR a live post_acls grant. This branch used
//     to read `public OR author` while the browse list composed the
//     rich rule from posts' own copy, so search, facets and suggest
//     silently dropped every org-only and followers post the caller
//     could read anywhere else (#873). One expression now, in
//     post_rule.go, spliced by both. The `private` disjunct needs the
//     caller's capabilities: pass [WithPostCaps]. The PUBLISHED
//     conjunct is what keeps a draft off every shared surface (ADR
//     0091 decision 7); [IncludeDrafts] waives it for the single-item
//     gate and the author's own drafts listing, and for nothing else.
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
			//
			// Rendered from `anonymousAssetConditions` (#1209) rather
			// than written out here, because Go code now has to answer
			// the same question about a scanned row
			// (visibility.AnonymouslyVisible) and two transcriptions of
			// one security rule is the defect ADR 0070's amendment
			// exists to stop.
			conds = append(conds, anonymousAssetSQL(a)...)
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
		// Authenticated: owner OR a live ACL grant OR the collection is
		// public.
		//
		// The public disjunct is the fix for the inversion this branch
		// used to produce. Without it the authenticated rule was
		// owner-or-ACL and nothing else, which meant signing in REMOVED
		// access to public content: an anonymous caller got the
		// collection, and the same person authenticated got 404. Every
		// signed-in non-owner was affected, system.admin included.
		//
		// It stayed latent because nothing exercised it — GetCollection
		// carried no visibility check until #439, and anonymous callers
		// never reached these paths until #437/#439/#442 opened them.
		// #445 made it observable by opening the anonymous side.
		//
		// The invariant this restores: an authenticated caller sees AT
		// LEAST what an anonymous one sees. EntityPost below had exactly
		// this shape (`visibility = 'public' OR author`) when that was
		// written; collections were the outlier. The post branch has
		// since grown the full read rule (#873) and keeps the same
		// invariant — `public` is still one of its disjuncts.
		//
		// Soft-delete conjoins the WHOLE predicate, not just the public
		// disjunct (#451). #448 originally scoped deleted_at into the
		// public disjunct only, on the reasoning that an owner may still
		// see their own soft-deleted collection — but that made this the
		// one authenticated branch out of step with EntityAsset and
		// EntityPost, both of which exclude soft-deleted rows across the
		// entire predicate. A soft-deleted collection belongs in the
		// trash view, not a browse list, for its owner exactly as for
		// anyone else. So the shape now mirrors EntityPost below:
		//   deleted_at IS NULL AND (public OR owner OR ACL)
		//
		// NO system.admin bypass here, deliberately — and the reason is
		// now a product one only. The mechanical objection this comment
		// used to carry ("threading a capability checker through Filter
		// would move every splice site") was answered by #873: a
		// capability resolved to a value and carried as an Option moves
		// nothing, because only the branch that reads it changes. See
		// [WithPostCaps]. What is still unanswered is the product
		// question — whether an admin may browse OTHER people's PRIVATE
		// collections — and nobody has asked it. An admin sees every
		// public collection plus their own plus anything ACL'd to them,
		// the same floor as every other authenticated caller. If admins
		// need more, that is an explicit, narrow option, enforced by the
		// caller (cf. IncludeSoftDeleted, #429) — not a silent bypass.
		//
		// includeSoftDeleted (superadmin escape hatch) waives ONLY the
		// soft-delete conjunct, never the visibility disjunction — same
		// as EntityPost's includeSoftDeleted path.
		//
		// Placeholder note: literal-only disjuncts, so the branch binds
		// exactly ONE arg at argOffset+1. No splice site moves.
		idx := argOffset + 1
		visible := fmt.Sprintf(
			"%svisibility = 'public' OR %sowner_user_ref = $%d OR EXISTS ("+
				"SELECT 1 FROM collection_acls acl "+
				"WHERE acl.collection_id = %sid "+
				"AND acl.principal_type = 'user' "+
				"AND acl.principal_id = $%d::TEXT "+
				"AND (acl.expires_at IS NULL OR acl.expires_at > NOW()))",
			a, a, idx, a, idx,
		)
		if p.includeSoftDeleted {
			return fmt.Sprintf(" AND ((%s))", visible), []any{p.caller.UserRef}
		}
		return fmt.Sprintf(" AND (%sdeleted_at IS NULL AND (%s))", a, visible), []any{p.caller.UserRef}
	case EntityPost:
		// The whole rule lives in postReadableExpr (post_rule.go) —
		// author, the five tiers, the follow graph and live ACL grants
		// — because `posts` splices the SAME expression and #873 was
		// what happened when it did not: browse composed the rich rule
		// and search composed `public OR author`, so an org-only post
		// you could read while browsing did not exist in search.
		//
		// Soft-delete stays HERE, outside the expression, so
		// IncludeSoftDeleted can waive it and nothing else. The admin
		// trash view must not be able to shed an authorization disjunct
		// along with the soft-delete conjunct, and that failure would be
		// silent.
		//
		// PUBLICATION is a THIRD conjunct, and it sits out here for the
		// same reason soft-delete does: it is an axis of its own, it is
		// not about the caller, and exactly one narrow option waives it
		// ([IncludeDrafts]). Held inside the read rule it could not be
		// waived for the single-item gate without also waiving an
		// authorization disjunct — which is the failure ADR 0063 keeps
		// structurally impossible rather than asking reviewers to
		// notice. See postPublishedExpr for why it is fail-closed and
		// why it is not `visible_by_default`.
		expr, args := postReadableExpr(a, argOffset+1, p.caller, p.postCaps)
		conds := make([]string, 0, 3)
		if !p.includeSoftDeleted {
			conds = append(conds, a+"deleted_at IS NULL")
		}
		if !p.includeDrafts {
			conds = append(conds, postPublishedExpr(a))
		}
		conds = append(conds, "("+expr+")")
		return " AND (" + strings.Join(conds, " AND ") + ")", args
	}
	// Unreachable — Filter constructor validates entity type.
	return " AND (FALSE)", nil
}
