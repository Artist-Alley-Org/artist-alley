// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// The post read rule — ONE expression, for every surface that reads posts
// ---------------------------------------------------------------------------
//
// #873. This rule used to live in `posts` as an unexported readRule, and
// [Predicate.ToSQL]'s EntityPost branch carried a coarser second version
// of it (`public OR author`). Browse composed the rich one; search,
// facets and suggest composed the coarse one. The result was not an
// error anyone could see — an org-only post, or a followers-only post
// you follow the author of, simply did not exist as far as /search was
// concerned. No 403, no empty state, just absence, with the facet counts
// and the autocomplete wrong in the same direction.
//
// So the rule lives here now, where ADR 0063 says the read rule lives,
// and `posts` composes it through [Filter] like every other caller. The
// old file's argument for keeping it out of this package was that the
// post rule needs the follow graph and the caller's capabilities, and
// that threading a capability checker through Filter would move all of
// its splice sites. Half of that is answered by [PostCaps] — a resolved
// two-state value carried on the Predicate, exactly the shape #899 used
// for [ContentCaps], which no other entity's branch reads and therefore
// moves no other splice site. The follow graph was never a problem: it
// is an EXISTS against user_follows in the same statement, not a Go
// lookup.
//
// The rule is SQL-only, and deliberately has no Go twin. Every surface
// that asks it — the browse list, the by-asset lookup, the single-item
// gate, search, facets, suggest — asks it of Postgres about a set of
// rows, so there is no per-row Go step for a second expression to live
// in. That is the difference from [ContentReadable], which needed
// [ContentReadableSQL] as a twin (and TestContentReadableSQL_MatchesGo
// to pin the pair) precisely because its primary form IS Go.

// PostsAdmin is the moderator capability for posts: it opens the
// `private` tier on every read path. `posts` re-exports it as
// CapPostsAdmin so the string has one definition.
const PostsAdmin = "posts.admin"

// PostCaps is a [CapabilityChecker] resolved down to the only question
// the post read rule asks it: may this caller read everyone's private
// posts. Same reasoning as [ContentCaps] — a closure cannot cross the
// search engine's per-caller CACHE KEY, and a caller who LOSES
// posts.admin must not keep being served the wider cached result for
// the rest of the TTL.
//
// One boolean rather than a capability set is not a shortcut: the rule
// consults exactly `posts.admin` and `system.admin`, folded together
// because they grant the same thing here, so a third code appearing
// would have to be a deliberate edit in [ResolvePostCaps].
type PostCaps struct {
	// SeesAllPrivate is posts.admin OR system.admin.
	SeesAllPrivate bool
}

// ResolvePostCaps evaluates a CapabilityChecker down to the struct. A
// nil checker (anonymous) resolves to the zero value, which admits
// nothing.
func ResolvePostCaps(caps CapabilityChecker) PostCaps {
	if caps == nil {
		return PostCaps{}
	}
	return PostCaps{SeesAllPrivate: caps(PostsAdmin) || caps(SystemAdmin)}
}

// CacheKey is the stable string a per-caller cache must fold in
// alongside the user ref, for the same reason [ContentCaps.CacheKey]
// exists: two requests carrying the same ref no longer produce the same
// result set.
func (c PostCaps) CacheKey() string {
	if c.SeesAllPrivate {
		return "1"
	}
	return "0"
}

// WithPostCaps attaches the caller's resolved post capabilities to the
// Predicate. Read by the EntityPost branch and by nothing else — passing
// it for another entity type is harmless and inert, in the same way a
// Caller's ref is inert on a branch that does not compare it.
//
// Omitting it is the anonymous-equivalent default (no capabilities), so
// a caller that forgets it gets the NARROWER answer. That direction is
// the safe one: the failure mode is a moderator missing a private post
// from their search results, not a stranger finding one.
func WithPostCaps(c PostCaps) Option {
	return func(p *Predicate) { p.postCaps = c }
}

// ---------------------------------------------------------------------------
// Publication — the OTHER question, kept apart from the read rule
// ---------------------------------------------------------------------------
//
// #1161 / ADR 0091 decision 7. A post is a DRAFT until its author
// publishes it, and a draft appears on no shared surface: not browse,
// not search, not a collection, not a feed, not the federation outbox,
// and not its own author's feed either.
//
// That is a DIFFERENT question from the read rule below, and the two
// are rendered separately on purpose:
//
//   - "may this caller read this post"  — postReadableExpr. Identity,
//     tiers, the follow graph, live grants.
//   - "does this post belong on a shared listing at all" —
//     postPublishedExpr. Nothing to do with the caller.
//
// The second is an orthogonal axis owned by [Predicate.ToSQL], exactly
// like the soft-delete conjunct beside it, and waivable by exactly one
// narrow option ([IncludeDrafts]) for the two callers that must see a
// draft: the single-item gate, so an author can open the thing they are
// writing, and the author's own drafts listing. Every other splice site
// gets it without asking, which is the property that matters — a
// surface added next year is published-only unless somebody
// deliberately says otherwise.
//
// Waiving it does NOT open a draft to strangers. The read rule below
// carries its own draft conjunct that no option waives, so with
// [IncludeDrafts] set a draft is still readable by its author and by a
// posts.admin holder alone.
//
// ⚠️ THIS IS THE ONE PLACE A WORKFLOW STATE DECIDES PUBLICATION.
// ADR 0091's first amendment is sharp about not conflating the two —
// a workflow state answers "where is this in its production process",
// published answers "has its author put it in front of people" — and
// the second amendment then RESOLVES that for posts specifically: the
// `post` domain has exactly two states, `wip` and `published`, and
// they ARE draft and published. An ASSET's workflow state is the other
// thing, and no asset read path may be written this way.
//
// It is also why this is a predicate over one named state rather than
// a read of `workflow_states.visible_by_default`. That flag is
// workflow CONFIGURATION — an admin's answer to "which states show up
// in default searches" — and hanging publication off it would make
// editing a config row publish other people's drafts. Both post states
// carry visible_by_default = TRUE today, so it does not even encode
// the distinction; wiring it would have meant changing the data to fit
// a flag whose only reader would have been this expression.

// PostDraftStateCode + PostPublishedStateCode name the two states of
// the `post` workflow domain (migration 00001, made reachable by
// 00059). Exported so callers that must MOVE a post between them —
// the publish / unpublish handlers — resolve the same two rows this
// rule reads, rather than a second spelling of them.
const (
	PostWorkflowDomain     = "post"
	PostDraftStateCode     = "wip"
	PostPublishedStateCode = "published"
)

// postPublishedExpr renders "this post is published" against
// `qualifier` (an already-dotted alias, or ""). Binds no placeholders.
//
// FAIL-CLOSED, and the direction is load-bearing. It asks whether the
// state IS `published`, not whether it is NOT `wip`. The two spellings
// agree on every row whose state is one of the two and disagree on
// every row whose state is unknown — NULL, or some third state a
// future domain edit introduces — and the safe answer to "I cannot
// tell whether this is published" is to withhold it. `state_id`'s FK
// is ON DELETE SET NULL, so deleting a workflow state row is exactly
// the event that produces unknown states in bulk: under the other
// spelling, dropping the `wip` row would publish every draft on the
// instance at once, silently. Under this one it hides them, which is
// wrong in the direction somebody notices.
//
// Migration 00059 backfilled every pre-existing post to `published`
// precisely so that fail-closed costs nothing on real data.
//
// The state id arrives as a scalar sub-select rather than a bound
// parameter. `workflow_states` has a UNIQUE (domain, code) index, so
// it is one index probe hoisted to an InitPlan and evaluated once per
// statement — while binding it would have meant resolving the UUID in
// Go, which every splice site here reaches WITHOUT a database handle
// (the search engine builds predicates inside a per-caller cache key).
// It also cannot drift: there is no Go-side copy of the id to go
// stale if an install reseeds its state machine.
// PostUnpublishedSQL is the complement — "this post is NOT published"
// — as a bare SQL boolean against `qualifier`. Binds no placeholders.
//
// Exported for ONE caller: the drafts listing (`GET /posts?draft=true`),
// which pairs it with [IncludeDrafts] so the page returns drafts and
// only drafts. It is here rather than spelled out there so that the
// listing and the rule name the same state row the same way, and a
// domain that grew a third state could not have the listing and the
// gate disagree about which side of the line it falls on.
//
// `IS DISTINCT FROM` rather than `<>`, and that is not decoration: a
// row with no state at all is UNKNOWN under `<>`, so it would be
// excluded from the published listing (correct — fail-closed) AND from
// this one, leaving it in nobody's view. It belongs in the author's
// drafts page, which is where they can see something is wrong with it.
func PostUnpublishedSQL(qualifier string) string {
	return fmt.Sprintf(
		"%sstate_id IS DISTINCT FROM (SELECT id FROM workflow_states"+
			" WHERE domain = '%s' AND code = '%s')",
		qualifier, PostWorkflowDomain, PostPublishedStateCode,
	)
}

func postPublishedExpr(qualifier string) string {
	return fmt.Sprintf(
		"%sstate_id = (SELECT id FROM workflow_states"+
			" WHERE domain = '%s' AND code = '%s')",
		qualifier, PostWorkflowDomain, PostPublishedStateCode,
	)
}

// postReadableExpr renders the post read rule as a bare SQL boolean
// expression (no leading " AND ", no outer parens) against `qualifier`
// — an already-dotted table alias, or "". It binds at most one
// placeholder, $argIdx, holding the caller's user ref.
//
// [Predicate.ToSQL] wraps it; nothing else calls it. The soft-delete
// conjunct is NOT part of it, deliberately: soft-delete is an orthogonal
// axis that ToSQL owns and [IncludeSoftDeleted] waives, and keeping it
// out of here means the admin trash view cannot ever waive an
// authorization disjunct along with the soft-delete one. That failure
// would be silent, which is why the two are separated structurally
// rather than by a comment asking the next caller to be careful.
//
// Tier by tier, for an authenticated caller:
//
//   - public        — readable by everyone, including anonymous. Writable
//     as of #1176: posts.validVisibility used to refuse the
//     tier, so this branch served rows only the seeder or a
//     direct INSERT could produce, and an instance in public
//     mode had nothing to show an anonymous visitor. The
//     column (migration 00008), this rule, and the write gate
//     now agree.
//   - org-only      — any authenticated local user. The walled-garden
//     default tier.
//   - followers     — the caller must follow the author, decided by
//     user_follows in this same statement. There is no "treat
//     it as public when the social graph is unwired" degrade.
//   - private       — the author, plus posts.admin / system.admin
//     (PostCaps.SeesAllPrivate).
//   - explicit-share — the author, plus anyone holding a live post_acls
//     grant (below).
//
// On top of the tiers sits ONE more disjunct: an unexpired post_acls row
// naming the caller (#667). ADR 0010 L6 is explicit that ACLs grant
// *additional* access beyond the defaults and never restrict below them,
// so it is OR'd in at the top level rather than folded into a tier — a
// share on a `private` post grants too, and nothing a caller sees today
// stops being visible because this branch exists. That is also why it is
// not conditioned on `visibility = 'explicit-share'`.
//
// The anonymous branch omits the author comparison entirely rather than
// binding the AnonymousCaller sentinel against author_user_ref: no real
// ref is 0, but a coincidence there would be a leak. Same reasoning as
// the other two entity branches.
func postReadableExpr(qualifier string, argIdx int, caller Caller, caps PostCaps) (expr string, args []any) {
	if caller.IsAnonymous {
		// A stranger never reads a draft, whatever its tier says. A
		// `public` draft is a post its author has not put in front of
		// anyone yet; `public` describes who may read it ONCE
		// published, and reading the tier as consent to publish would
		// make the compose form's visibility control double as a
		// publish button.
		return fmt.Sprintf(
			"%s AND %svisibility = 'public'",
			postPublishedExpr(qualifier), qualifier,
		), nil
	}
	// Rendered as a literal rather than a bound arg: it is a Go bool
	// turned into a SQL keyword, so there is nothing to inject, and a
	// constant lets Postgres drop the disjunct outright.
	privateOK := "FALSE"
	if caps.SeesAllPrivate {
		privateOK = "TRUE"
	}
	// The DRAFT conjunct, and it is a conjunct rather than another
	// disjunct beside the tiers for the reason above: an unpublished
	// post is not readable by whoever its tier would admit, it is
	// readable by its author and by a moderator. So it NARROWS
	// whatever the tier disjunction below decides, and no option
	// waives it — [IncludeDrafts] waives the surface conjunct in
	// [Predicate.ToSQL], never this one. That separation is what lets
	// the author's drafts listing exist without the listing becoming
	// the hole.
	//
	// posts.admin is here for the same reason it opens the `private`
	// tier: it is the instance-moderator claim, and a moderator who
	// can read every private post but not the draft of a reported one
	// has a gap nobody chose.
	draftOK := fmt.Sprintf(
		"(%s OR %sauthor_user_ref = $%d)",
		postPublishedExpr(qualifier), qualifier, argIdx,
	)
	if caps.SeesAllPrivate {
		draftOK = "TRUE"
	}
	return fmt.Sprintf(
		"%[5]s AND ("+
			"%[1]sauthor_user_ref = $%[2]d"+
			" OR %[1]svisibility IN ('public', 'org-only')"+
			" OR (%[1]svisibility = 'private' AND %[3]s)"+
			" OR (%[1]svisibility = 'followers' AND EXISTS ("+
			"SELECT 1 FROM user_follows f"+
			" WHERE f.follower_user_ref = $%[2]d"+
			" AND f.followee_user_ref = %[1]sauthor_user_ref))"+
			" OR %[4]s)",
		qualifier, argIdx, privateOK, PostLiveGrantSQL(qualifier, argIdx), draftOK,
	), []any{caller.UserRef}
}

// PostLiveGrantSQL renders "an unexpired post_acls row names this user"
// as a bare EXISTS, against `qualifier` (an already-dotted table alias,
// or "") and the placeholder holding the caller's user ref.
//
// It is exported for ONE caller: posts.ListSharedWithMeGated, the
// "Shared with me" surface (#875), which asks exactly this question
// standalone. Restating the same four conditions there is how the two
// drift — that page listing a post the read rule refuses, or (worse)
// still listing an expired grant because only one of the two copies got
// the expiry clause. Everything else must reach the rule through
// [Filter]; a second caller splicing this alone is almost certainly
// re-implementing the read rule by hand.
//
// So expiry lives here, not at either call site: `expires_at IS NULL OR
// > NOW()`, evaluated by Postgres in the same statement that decides
// visibility. Same for `principal_type = 'user'` — role and team are
// ADR 0010 Layer 5, unimplemented on both post_acls and collection_acls.
//
// The cast is `$n::BIGINT::TEXT`, in that order, and the BIGINT half is
// load-bearing rather than decorative. principal_id is TEXT and a user
// ref is a bigint, so the bound value has to be cast at the comparison;
// but Postgres infers a parameter's type from its context, and a bare
// `$n::TEXT` tells it the parameter IS text — at which point pgx is
// asked to encode an int64 as text and fails with "cannot find encode
// plan". Inside the read rule that never bites, because the same
// placeholder is also compared against author_user_ref and that pins it
// to bigint; the standalone caller has no such conjunct (#874, #879).
// Naming the input type here makes the fragment mean the same thing
// wherever it is spliced, which is the entire point of it being one
// fragment.
//
// `permission` is deliberately unfiltered — the column's CHECK admits
// read/write/admin and all three imply read.
func PostLiveGrantSQL(qualifier string, argIdx int) string {
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

// PostReadable answers "may this caller read THIS post", by id — the
// single-item form of the rule above.
//
// # Why it is here and not in `posts`
//
// It WAS in `posts`, as the unexported postReadable, and it was the only
// Go-callable form of the rule. #882's second half needs the same
// question asked from `collections` — may this caller put someone else's
// post into their own collection — and a package-private helper cannot
// be asked. The two ways out were to export it from `posts` (which makes
// a container package import the entity package for one boolean) or to
// restate it, and restating a security rule is the defect epic #665
// exists to remove: the ADD path would have started agreeing with
// GET /posts/{id} and drifted the first time a tier moved.
//
// So it sits beside the expression it probes, exactly like
// [CanSeeAssetContent] sits beside the two planes it composes (#922). There
// is still only ONE expression of the post read rule —
// [postReadableExpr] — and this runs it as an EXISTS against one id, so
// the single-item answer cannot diverge from what the feed lists. That
// agreement is the #660 property, and it is structural rather than
// maintained by hand.
//
// # What it answers, and what a caller must do with the answer
//
// Returns (false, nil) BOTH for "the post is hidden from you" and for
// "there is no such post". Collapsing the two is what keeps callers
// enumeration-safe by default: whatever response you give, give the same
// one to both, or the endpoint becomes a UUID-existence probe. Soft-
// deleted posts are excluded — a deleted post is not readable content
// on any path — because the option that would waive that conjunct
// ([IncludeSoftDeleted]) is deliberately not passed.
//
// A DB error is propagated rather than folded into "no". A read gate
// that answers "denied" on a transport blip is indistinguishable from a
// permissions bug to whoever hits it.
func PostReadable(
	ctx context.Context,
	pool Pool,
	caller Caller,
	caps PostCaps,
	postID uuid.UUID,
) (bool, error) {
	// IncludeDrafts, deliberately: this is the SINGLE-ITEM gate, and an
	// author opening the draft they are writing is the whole point of
	// the draft state. It waives only the shared-surface conjunct —
	// postReadableExpr's own draft conjunct still holds a draft to its
	// author and posts.admin, so this does not make one readable by
	// anybody a listing would have refused.
	pred, err := Filter(ctx, EntityPost, caller, WithPostCaps(caps), IncludeDrafts())
	if err != nil {
		return false, fmt.Errorf("visibility: post read gate: %w", err)
	}
	// ToSQL's EntityPost branch renders `deleted_at IS NULL AND (rule)`
	// and binds its own placeholders from argOffset+1; $1 is the id.
	frag, args := pred.ToSQL("", 1)
	sql := "SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1" + frag + ")"

	var ok bool
	if err := pool.QueryRow(ctx, sql, append([]any{postID}, args...)...).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("visibility: post read gate: %w", err)
	}
	return ok, nil
}
