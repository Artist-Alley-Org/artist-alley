// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// ---------------------------------------------------------------------------
// The post read rule — ONE expression, spliced into every read path
// ---------------------------------------------------------------------------
//
// #660: `GET /posts?visibility=private` returned other people's private
// posts to any signed-in caller. The list query took the caller-supplied
// tier and applied it as a bare `visibility = $1` with no author or
// relationship conjunct, while the single-item gate (canReadPost) did the
// real work in Go. Two expressions of one rule; one of them was wrong,
// and the comments on both asserted the other one's behaviour.
//
// So there is now exactly one expression: readRule.sql. It renders the
// WHERE-fragment that decides which posts a caller may read, and it is
// used by
//
//   - ListPostsPageGated  — the feed / browse list
//   - ListPostsByAssetGated — the post-by-asset lookup
//   - Handler.canReadPost — the single-item gate, which runs the SAME
//     fragment as `SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 …)`
//
// The list path and the single-item path can therefore no longer
// disagree: they are the same SQL. That agreement is asserted directly by
// TestListPosts_NeverExceedsSingleItemGate, which enumerates the tiers
// from the database's own CHECK constraint so a tier added later is
// covered without anyone remembering to extend a list.
//
// This mirrors visibility.Predicate.ToSQL + visibility.CanSee (ADR 0063)
// and is deliberately NOT folded into that package: the post rule needs
// the follow graph and the caller's capabilities, and threading a
// capability checker through visibility.Filter would move all twelve of
// its splice sites to answer a question only posts ask. See the note in
// visibility/predicate.go's EntityCollection branch, which declines the
// same widening for the same reason.

// readRule is the caller-side input to the post read predicate: who is
// asking, and what does their identity (not their query string) entitle
// them to.
type readRule struct {
	// anonymous callers get the public tier only. Reachable today only
	// via /posts/by-asset, the one posts route on the public-mode
	// allowlist (auth.publicmode).
	anonymous bool

	// userRef is the authenticated caller's ref. Zero + anonymous=true
	// for unauthenticated requests; never compared against
	// author_user_ref in that case (no real ref is 0, but a coincidence
	// there would be a leak, so the anonymous branch omits the comparison
	// entirely — same reasoning as visibility.Predicate.ToSQL).
	userRef int64

	// seesAllPrivate is the moderator bypass for the `private` tier:
	// posts.admin or system.admin. It matches what canReadPost has
	// always granted on the single-item path.
	seesAllPrivate bool
}

// readRuleFor derives the rule from the request identity. Nil or
// anonymous identity → the anonymous branch.
func readRuleFor(id *auth.Identity) readRule {
	if id == nil || id.IsAnonymous() {
		return readRule{anonymous: true}
	}
	return readRule{
		userRef:        id.UserRef,
		seesAllPrivate: id.Can(CapPostsAdmin) || id.Can(CapSystemAdmin),
	}
}

// sql renders the rule as a SQL WHERE-fragment against `alias` (empty
// string for an un-aliased FROM). The fragment always begins with
// " AND (…)", so callers concatenate it into an existing WHERE clause
// with no pre-processing, and it binds its placeholders starting at
// argOffset+1 — the same contract as visibility.Predicate.ToSQL.
//
// Tier by tier, for an authenticated caller:
//
//   - public        — readable by everyone, including anonymous. No post
//     can be written into this tier through the API
//     (validVisibility rejects it), but migration 00008 admits
//     the value and the shared predicate treats it as
//     world-readable, so the rule agrees rather than being the
//     one place that says otherwise.
//   - org-only      — any authenticated local user. The walled-garden
//     default tier.
//   - followers     — the caller must follow the author. Decided by the
//     user_follows table directly; there is no "treat it as
//     public when the social handler is unwired" degrade any
//     more (that fallback existed for test fixtures and was a
//     leak waiting for a boot-order slip).
//   - private       — the author, plus posts.admin / system.admin.
//   - explicit-share — the author, plus anyone holding a live post_acls
//     grant (below).
//
// On top of the tiers sits ONE more disjunct: an unexpired `post_acls`
// row naming the caller (#667). ADR 0010 L6 is explicit that "ACLs grant
// *additional* access beyond those defaults. They never restrict below
// them", so it is OR'd in at the top level rather than folded into a
// tier: a grant opens a post the caller could not otherwise read, and
// removing it can never close one they could. That is why the disjunct
// is not conditioned on `visibility = 'explicit-share'` — a share on a
// `private` post grants too, and nothing a caller sees today stops being
// visible because this branch was added.
//
// Wiring it here rather than in either caller is the whole point: the
// fragment feeds the list paths AND postReadable, so the list can never
// become wider than GET /posts/{id} the way it did in #660. That
// agreement is what the earlier deferral in this comment was protecting,
// and it is now structural.
//
// The grant disjunct itself is liveGrantSQL below — principal_type,
// expiry, the TEXT cast and the deliberate absence of a `permission`
// filter are all documented there, because the "Shared with me" surface
// (#875) uses the SAME fragment standalone and neither copy may drift.
// role/team principals stay unimplemented on both post_acls and
// collection_acls; see visibility/predicate.go's EntityCollection
// branch, which carries the identical user-only disjunct.
//
// Soft-delete is NOT part of this fragment. It is an orthogonal axis
// owned by each caller: the list path has an admin-only include_deleted
// flag, and GetPost's fetch already filters deleted_at IS NULL. Keeping
// it out means the admin trash view cannot accidentally waive an
// authorization conjunct along with the soft-delete one — the same
// narrowness visibility.IncludeSoftDeleted is built around.
func (r readRule) sql(alias string, argOffset int) (fragment string, args []any) {
	a := strings.TrimSpace(alias)
	if a != "" {
		a += "."
	}
	if r.anonymous {
		return fmt.Sprintf(" AND (%svisibility = 'public')", a), nil
	}
	idx := argOffset + 1
	// Rendered as a literal rather than a bound arg: it is a Go bool
	// turned into a SQL keyword, so there is nothing to inject.
	privateOK := "FALSE"
	if r.seesAllPrivate {
		privateOK = "TRUE"
	}
	frag := fmt.Sprintf(
		" AND (%[1]sauthor_user_ref = $%[2]d"+
			" OR %[1]svisibility IN ('public', 'org-only')"+
			" OR (%[1]svisibility = 'private' AND %[3]s)"+
			" OR (%[1]svisibility = 'followers' AND EXISTS ("+
			"SELECT 1 FROM user_follows f"+
			" WHERE f.follower_user_ref = $%[2]d"+
			" AND f.followee_user_ref = %[1]sauthor_user_ref))"+
			" OR %[4]s)",
		a, idx, privateOK, liveGrantSQL(a, idx),
	)
	return frag, []any{r.userRef}
}

// liveGrantSQL renders "an unexpired post_acls row names this user" as
// a bare EXISTS, against `qualifier` (an already-dotted table alias, or
// empty) and the placeholder holding the caller's user ref.
//
// It is factored out for ONE reason: two callers need the identical
// predicate and this file's whole point is that there is one expression
// of it (#660, #665). The read rule ORs it in as the ACL disjunct;
// ListSharedWithMeGated uses it standalone as "the posts shared with
// me". Restating the same four conditions in the second query is how
// the two drift — the "Shared with me" page listing a post the read
// rule refuses, or (worse) still listing an expired grant because only
// one of the two copies got the expiry clause.
//
// So expiry lives here, not at either call site: `expires_at IS NULL OR
// > NOW()`, evaluated by Postgres in the same statement that decides
// visibility. Same for `principal_type = 'user'` — role and team are
// Layer 5, unimplemented on both ACL surfaces.
//
// The cast is `$n::BIGINT::TEXT`, in that order, and the BIGINT half is
// load-bearing rather than decorative. principal_id is TEXT and a user
// ref is a bigint, so the bound value has to be cast at the comparison;
// but Postgres infers a parameter's type from its context, and a bare
// `$n::TEXT` tells it the parameter IS text — at which point pgx is
// asked to encode an int64 as text and fails with "cannot find encode
// plan". The read rule never hit that because the same placeholder is
// also compared against author_user_ref, which pins it to bigint; the
// standalone caller has no such conjunct. Naming the input type here
// makes the fragment mean the same thing wherever it is spliced, which
// is the entire point of it being a fragment.
//
// `permission` is deliberately unfiltered — the column's CHECK admits
// read/write/admin and all three imply read.
func liveGrantSQL(qualifier string, argIdx int) string {
	return fmt.Sprintf(
		"EXISTS ("+
			"SELECT 1 FROM post_acls acl"+
			" WHERE acl.post_id = %[1]sid"+
			" AND acl.principal_type = 'user'"+
			" AND acl.principal_id = $%[2]d::BIGINT::TEXT"+
			" AND (acl.expires_at IS NULL OR acl.expires_at > NOW()))",
		qualifier, argIdx,
	)
}

// postReadable reports whether the caller may read one post, by id. It
// runs the rule's own fragment as an EXISTS probe, so the single-item
// answer is produced by the same SQL the list path filters with —
// agreement by construction rather than by two implementations kept in
// step by hand.
//
// Returns (false, nil) both for "row hidden" and "row missing": callers
// map both to their own 403/404 choice, and the two collapsing here
// keeps this helper enumeration-safe by default.
//
// Soft-deleted rows are excluded — a deleted post is not readable
// content on either path.
func (h *Handler) postReadable(ctx context.Context, id *auth.Identity, postID uuid.UUID) (bool, error) {
	frag, args := readRuleFor(id).sql("", 1)
	sql := "SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL" + frag + ")"
	var ok bool
	if err := h.Pool.QueryRow(ctx, sql, append([]any{postID}, args...)...).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("posts: read gate: %w", err)
	}
	return ok, nil
}
