// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package posts

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/viewkind"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// The two post list queries, with the read rule spliced in (#660).
//
// Why these are hand-built SQL rather than the sqlc queries they replace
// (ListPostsPage / ListPostsByAsset, both deleted from queries.sql):
// readRuleSQL returns a runtime fragment and sqlc queries are static
// strings with fixed placeholders — the same reason every other splice
// site of visibility.Predicate is hand-built (see assets/list_page.go).
//
// The sqlc versions are DELETED rather than left unused. A second,
// ungated expression of "which posts does this list return" sitting in
// queries.sql is exactly what produced #660, #657 and #650; leaving one
// behind for a future caller to pick up would rebuild the defect.

// ListPostsPageParams carries the caller-supplied filters for one feed
// page. Visibility is a NARROWING filter only: it selects among the
// tiers the caller's identity already admits (the read rule), and can never
// widen them. `?visibility=private` therefore means "the private posts I
// may read" — my own, plus everyone's for a moderator — not "everyone's
// private posts", which is what it used to mean.
type ListPostsPageParams struct {
	// IncludeDeleted is superadmin-only and enforced as such by the
	// handler. It waives the soft-delete conjunct and nothing else; the
	// read rule still applies in full.
	IncludeDeleted *bool
	AuthorUserRef  *int64
	// Visibility is a SET of tiers, not one tier (#1193). Nil means no
	// display filter at all — every tier the read rule admits.
	//
	// It was a single `*string` while the only two answers the handler
	// ever produced were "one named tier" and "the org-only default",
	// and that shape is exactly what made the default wrong: the
	// signed-in default could not be spelled as anything but ONE tier,
	// so it was org-only, so a member's browse wall excluded every
	// public post on the instance. A set makes the union expressible,
	// which is what the default now is.
	//
	// Still narrowing, unchanged in direction: the rule is ANDed on
	// after this, so naming five tiers here selects among the ones the
	// caller could already read rather than adding any.
	//
	// An EMPTY NON-NIL slice is not produced by any caller and is not
	// given the "selects nothing" meaning `Kinds` has — the SQL below
	// treats it as `= ANY('{}')`, which matches no row, so it degrades
	// to an empty page rather than to the whole feed. The distinction
	// matters for Kinds because a user typo reaches it; no query
	// parameter reaches this one empty.
	//
	// ⚠️ SINCE #1251 SLICE 2 THAT DEGRADATION IS EXPLICIT RATHER THAN
	// INCIDENTAL. The tiers now compose through facet.FacetVisibility,
	// and a facet selection with zero terms means "no constraint" — the
	// OPPOSITE of `= ANY('{}')`. So the empty-non-nil case is answered by
	// feedFiltersSelectNothing before the selection is built, which is
	// where `Kinds`' identical distinction is answered too. The behaviour
	// is unchanged; what changed is that keeping it is now a line
	// somebody has to delete rather than a property of the operator.
	Visibility []string
	Q          *string
	// Tags is the tag filter (#1123), and since #1251 slice 2 it is a
	// SET rather than one value.
	//
	// ⛔ ITS PLURAL MEANING IS **AND**: two tags select posts carrying
	// EVERY one of them. That is not a choice made here — it is the
	// meaning `tag:a tag:b` has always had in the DSL, and `tag` is the
	// only conjunctive dimension in the shared grammar because it is the
	// only multi-valued one (facet.FacetType.conjunctive). Composing the
	// feed through that grammar makes the two surfaces answer the same
	// question the same way, which is the whole of ADR 0093 decision 3.
	//
	// ⚠️ THE PLURAL CASE IS NEW TO THIS SURFACE. It was `Tag *string`,
	// so the feed could not express two tags at all; there is no
	// inherited behaviour to preserve and therefore no inherited
	// behaviour to check the new one against. It is asserted with
	// arithmetic instead — `both < min(a, b)` strictly, never
	// `both > 0` — because the union is the answer a membership
	// assertion cannot tell from the intersection, and it is the answer
	// #1165 and #1242 each found shipped.
	//
	// EMPTY MEANS NO FILTER, unambiguously: the handler drops blank
	// values, so unlike `Visibility` and `Kinds` there is no spelling
	// that asks for a tag and names none.
	//
	// Matching is EXACT and NOT case-folded, on the tag STRING — see
	// migration 00050 for why there is no id to join on.
	Tags []string
	// FeedFollowerRef is ?feed=following, and "following" is the UNION
	// of the two follow graphs (#1048): posts by an author this ref
	// follows, OR posts belonging to a team it follows. It had only the
	// author arm, which made the filter return nothing at all for the
	// very common account that follows studios and no people — and made
	// it structurally unreachable for a read-only account, since
	// following a *user* is a write such an account is refused while
	// following a team is not.
	//
	// The union is what the page's own vocabulary promises: the rail
	// above the grid lists the teams you follow and the only filter
	// offered says "Following". It is still one NARROWING conjunct —
	// see the header note above — so widening it from one graph to two
	// widens the SELECTION, never the read rule, which is ANDed on
	// after it and consults neither table.
	FeedFollowerRef *int64
	// TeamID scopes the page to one team's posts (#684). NARROWING ONLY:
	// it is a plain conjunct beside the read rule, never a disjunct with
	// it, so a team page shows the caller exactly the subset of that
	// team's posts browse would already have shown them. Membership of
	// the team grants nothing here — the read rule never consults
	// team_id (see visibility/post_rule.go), and this filter must not
	// become the place that starts.
	TeamID pgtype.UUID
	// LikedByUserRef scopes the page to posts THIS ref has liked
	// (#1106) — the profile's Likes tab, and the reason it is a
	// parameter on the feed rather than an endpoint of its own.
	//
	// A likes listing is a DERIVED listing: the rows are chosen by one
	// user's actions and read by another, so the whole question is whose
	// read rule decides what is in it. Making it a conjunct on the feed
	// query answers that structurally — the rule below is the VIEWER's,
	// ANDed on after this, exactly as it is for every other filter here.
	// A liked post the viewer may not read is therefore absent from the
	// page, not withheld from it and not counted in it, and it cannot
	// become otherwise without changing the feed for everyone.
	//
	// The alternative was a standalone "things user X liked" endpoint,
	// which would have needed its own copy of the read rule, its own
	// pagination and its own payload — three chances to disagree with
	// the feed about what a post IS, for a tab that renders the same
	// card. NARROWING like every other filter here (see the header):
	// liking a post you cannot read does not make it readable by you or
	// by anyone else.
	LikedByUserRef *int64
	// Draft restricts the page to UNPUBLISHED posts — the author's own
	// drafts listing (`GET /posts?draft=true`, ADR 0091 decision 7).
	//
	// It is the ONE filter here that changes which PREDICATE the page
	// is built from rather than merely narrowing within it: false (the
	// default, and every other surface) composes the read rule with its
	// publication conjunct intact, so no combination of the other
	// filters can produce a draft. True waives that conjunct and adds
	// the complement as a plain conjunct, so the page is drafts and
	// nothing but drafts.
	//
	// It is still NARROWING in the sense that matters. Waiving the
	// publication conjunct does not widen authorization: the read rule
	// itself holds a draft to its author and to a posts.admin holder
	// whichever way this is set (visibility.postReadableExpr), so a
	// stranger asking for drafts gets an empty page rather than
	// somebody's unfinished work.
	Draft bool
	// Kinds is the browse footer's type filter (#1166): the set of
	// cover-asset kinds the page is restricted to, already parsed off
	// `?kind=`. Nil means no filter; a NON-NIL EMPTY SLICE means the
	// caller named only kinds nothing can be, which selects nothing —
	// see KindsRequested for why the distinction is a separate field
	// rather than a length check.
	Kinds []viewkind.Kind
	// KindsRequested says the caller supplied `?kind=` at all.
	//
	// It exists because `len(Kinds) == 0` is ambiguous and the two
	// readings differ by the whole feed. `?kind=nonsense` parses to an
	// empty selection and must return an empty page; an absent
	// parameter must return everything. Collapsing them would make a
	// typo in the query string WIDEN the result — the one direction a
	// narrowing filter is never allowed to move.
	KindsRequested bool
	// AI is the browse footer's "Hide AI-made work" toggle (#1251
	// slice 3, ADR 0094 fourth amendment), already validated off `?ai=`
	// into one of facet.AIPure / facet.AINotPure. EMPTY MEANS NO FILTER.
	//
	// ⛔ IT NEEDS NO "REQUESTED BUT EMPTY" COMPANION, unlike `Kinds` and
	// `Visibility` beside it, and the reason is that the handler REFUSES
	// the state that would need one. Those two have a spelling that asks
	// for a filter and names nothing it can be (`?kind=nonsense`, an
	// empty tier set), which must select nothing rather than everything.
	// `?ai=` has a CLOSED two-value vocabulary validated at the handler
	// by facet.FacetType.CanonicalValue, so an unrecognised value is a
	// 400 and never arrives here as an ambiguous emptiness. The
	// distinction is not stylistic: it is why feedFiltersSelectNothing
	// says nothing about this field.
	//
	// ⚠️ THE VALUE IS `not_pure`, NOT `pure`, FOR THE HIDE CONTROL, and
	// the two are not each other's negation by accident — they PARTITION
	// the corpus, so ORing both terms would mean "no constraint" rather
	// than "nothing". The predicate reads `posts.ai_pure` (migration
	// 00061), the FILTERING fact, and never `posts.ai_provenance`, the
	// LABELLING one: the latter propagates on ANY member, so keying the
	// exclusion on it would drop exactly the MIXED posts the owner's
	// ruling protects.
	//
	// ⛔ A FILTER, NEVER A GATE (ADR 0094 §4). Like every other field
	// here it is a NARROWING conjunct beside the read rule, and unlike
	// the read rule it is only ever applied when a caller asks: nothing
	// is withheld on this axis from anybody who did not send `?ai=`.
	AI             string
	CursorPostedAt pgtype.Timestamptz
	CursorID       pgtype.UUID
	RowLimit       int32
	// Ascending flips the feed to oldest-first (?dir=asc). It moves the
	// ORDER BY and the keyset predicate TOGETHER — see feedOrder.
	Ascending bool
	// Mature is the caller's resolved mature-content qualification
	// (#1116, ADR 0090 §2/§3). ZERO VALUE DISQUALIFIES, which is the
	// property that makes a caller who forgets to set it fail closed.
	//
	// ⚠️ THIS IS A ROW-PLANE CONJUNCT, NOT A PRESENTATION FILTER, and
	// the distinction decides which of two existing mechanisms it joins.
	//
	// #921's `applyHideRestricted` is the other one, and it is the wrong
	// home for this. That filter runs in Go over an already-enriched
	// page, subtracting posts whose members were all redacted — a
	// PRESENTATION preference layered on top of a rule that already
	// returned the row, and it says so ("deliberately incapable of
	// disagreeing with the rule because it does not know the rule").
	// Mature is not a preference about how a returned row is drawn; it
	// is a rule about whether the row is returned. Putting it there
	// would have made it (a) defeatable by `show_restricted`, which is
	// an unrelated toggle, and (b) absent from ListSharedWithMeGated
	// and ListPostsByAssetGated, which never call it.
	//
	// So it lives HERE, beside `ruleFrag`, where every sibling query
	// composes it too and where a disqualified viewer's mature post is
	// never in the result set to begin with.
	Mature visibility.MatureViewer
	// MatureAdmin waives the mature axis for a `system.admin` caller,
	// who has to be able to moderate what the instance switch hid (ADR
	// 0090 §2). It waives NOTHING ELSE — the read rule above still
	// applies in full, so this is not an `IncludeDeleted`-style
	// superuser flag and must not grow into one.
	MatureAdmin bool
	// ExcludeMature is the browse footer's Mature row (#1292, ADR 0090's
	// 2026-08-26 amendment), off `?mature=not_mature`. It is layer 3,
	// the VIEW: "I have consented, but not in these results, right now".
	//
	// ⛔ IT IS NOT A FOURTH CONJUNCT OF THE GATE, and reading it as one
	// is the way this ships wrong. The gate above decides whether the
	// caller may be shown mature rows at all. This subtracts from what
	// survives that decision, so it is only ever reachable for a caller
	// the gate already said yes to, it can only remove rows, and there
	// is no value of it that adds one. False means no filter, which is
	// what every caller from before this parameter existed sends.
	//
	// ⚠️ IT IS NOT WAIVED FOR AN ADMIN, and `MatureAdmin` beside it is.
	// That waiver exists so an operator's switch cannot hide what a
	// moderator has to look at; this is the moderator's own request
	// about their own feed, and honouring the gate's exemption here
	// would mean a control that visibly refuses to do the one thing it
	// says it does.
	ExcludeMature bool
}

// feedOrder is the (posted_at, id) keyset in one direction, expressed
// once (#868).
//
// The ORDER BY and the cursor comparison are the SAME fact stated
// twice, and the whole bug class this closes is what happens when only
// one of them moves. The feed's toggle used to send `?dir=asc` to a
// server that declared no such parameter, so nothing moved at all and
// "Oldest" quietly rendered newest. The obvious half-fix — flip the
// ORDER BY, leave the predicate — is strictly worse than the bug it
// replaces: `posted_at < cursor` walking an ascending scan asks for
// rows BEHIND the ones just returned, so page 2 re-serves page 1's
// window and everything past it is unreachable.
//
// So the direction is a single struct with a single constructor, and
// both strings come out of it. There is no way to hold one without the
// other.
type feedOrder struct {
	// cmp is the strict inequality that advances the scan: `<` walking
	// down, `>` walking up.
	cmp string
	// dir is the SQL keyword for both ORDER BY terms.
	dir string
}

func newFeedOrder(ascending bool) feedOrder {
	if ascending {
		return feedOrder{cmp: ">", dir: "ASC"}
	}
	return feedOrder{cmp: "<", dir: "DESC"}
}

// keysetSQL renders the "strictly past the cursor" predicate for the
// (posted_at, id) key, with the timestamp at $tsN and the tiebreak id
// at $idN. NULL cursor means "first page", which admits every row.
//
// The id tiebreak carries the same comparison as the timestamp on
// purpose: it is the low-order digit of one composite key, not a
// separate ordering. Pinning it to `<` while the timestamp flipped
// would make posts that share a posted_at (the seeded case, and any
// bulk import) the only ones that paginate wrongly — a defect that
// hides completely behind timestamps that happen to be distinct.
func (o feedOrder) keysetSQL(col, idCol string, tsN, idN int) string {
	return fmt.Sprintf(
		`($%d::TIMESTAMPTZ IS NULL
       OR %s %s $%d::TIMESTAMPTZ
       OR (%s = $%d::TIMESTAMPTZ AND %s %s $%d::UUID))`,
		tsN, col, o.cmp, tsN, col, tsN, idCol, o.cmp, idN,
	)
}

// orderBySQL renders the matching ORDER BY clause.
func (o feedOrder) orderBySQL(col, idCol string) string {
	return fmt.Sprintf("ORDER BY %s %s, %s %s", col, o.dir, idCol, o.dir)
}

// ListPostsPageRow mirrors the SELECT list below. Order matters: rows
// scan positionally.
type ListPostsPageRow struct {
	ID                    pgtype.UUID
	AuthorUserRef         int64
	Title                 string
	Description           string
	Visibility            string
	CoverAssetID          pgtype.UUID
	CoverThumbnailAssetID pgtype.UUID
	PostedAt              pgtype.Timestamptz
	LikeCount             int64
	CommentCount          int64
	OriginServerID        pgtype.UUID
	TeamID                pgtype.UUID
	StateID               pgtype.UUID
	CreatedAt             pgtype.Timestamptz
	UpdatedAt             pgtype.Timestamptz
	DeletedAt             pgtype.Timestamptz
	DeletedReason         *string
}

// listPostsPageColumns is the SELECT list, kept identical to the one the
// sqlc query used so ListPostsPageRow keeps scanning positionally.
const listPostsPageColumns = `id, author_user_ref, title, description, visibility, cover_asset_id,
       cover_thumbnail_asset_id, posted_at, like_count, comment_count,
       origin_server_id, team_id, state_id, created_at, updated_at,
       deleted_at, deleted_reason`

// ListPostsPageGated runs the feed query for one caller. Cursor
// pagination on the (posted_at, id) keyset, in whichever direction
// `Ascending` selects — both halves of that key move together via
// feedOrder, which is the point of it. Filters:
//   - author_user_ref: limit to posts by a given user
//   - visibility: narrow to a SET of tiers WITHIN what the caller may
//     read (#1193 — it was one tier, which is why the default could not
//     be the union it should always have been). Since #1251 slice 2 the
//     predicate is the shared grammar's `visibility` dimension
//     (facet.FacetVisibility); the tiers OR together and the read rule is
//     still a SEPARATE conjunct ANDed on below, which is the property
//     that keeps this a display filter and not a grant.
//   - q: plain-text TSVECTOR search across post search_text
//   - tag: a SET of tags, ANDed — "carries every one of these". Since
//     #1251 slice 2 the predicate is the shared grammar's `tag`
//     dimension (facet.FacetTag), which is where /search reads it too;
//     the feed used to hold a byte-identical second copy of it. It was
//     a single value before that, so the plural is NEW here — see
//     ListPostsPageParams.Tags.
//   - feed_follower_ref: restrict to what the given ref follows
//     (?feed=following) — authors OR teams OR tags. Three EXISTS,
//     hitting the user_follows PK, the team_follows PK and the
//     tag_follows PK respectively (#1123).
//   - team_id: restrict to one team's posts (#684)
//   - liked_by: restrict to posts a given ref has liked (#1106)
//   - kind: restrict to posts holding a member of a given badge kind —
//     the browse footer's type filter (#1166, widened from the cover to
//     the whole membership by #1190). Since #1251 the predicate is the
//     shared grammar's `kind` dimension (facet.FacetKind); note that its
//     EXISTS carries the field-plane readability rule PER MEMBER, so a
//     member this caller cannot see contributes no kind.
//
// Every one of those NARROWS. The read rule is ANDed onto the result,
// never ORed into it, so no filter here can surface a row the caller
// could not already read — least obviously `team_id`, which looks like
// it ought to mean "the team's space" and does not. It means "the part
// of the team's space this caller can already see".
//
// ⚠️ The tag arm is where that sentence stops being an abstraction.
// A post's author chooses its tags, so `tag_follows` is the one follow
// source whose matching side is written by the party the read rule is
// protecting against: anybody may tag their own restricted post
// `fantasy` and every follower of `fantasy` would then have it in
// their Following feed — IF this disjunction were ORed with the read
// rule. It is not. The three EXISTS are ORed with EACH OTHER inside a
// single conjunct, and `ruleFrag` is appended as a separate ANDed
// conjunct below, so the follow set can only ever remove rows from a
// page the caller could already see. The acceptance test for #1123 is
// exactly that pair: one restricted post carrying a followed tag,
// visible to the caller who may read it and absent for the one who may
// not.
//
// The tag EXISTS joins `post_tags` on the tag STRING because that is
// what the corpus is keyed by — see migration 00050 for why there is no
// id to join on, and why matching is exact rather than case-folded. That
// remains true of the `?tag=` FILTER after #1251 slice 2 moved it into
// facet.dimensionSQL: the shared expression joins the same table on the
// same string with the same `=`. The follow-set EXISTS above is the
// other, separate reader of `post_tags` and stays here, because a follow
// set is SCOPE and not a filter (ADR 0093 decision 2).
//
// `liked_by` is the newest member of that list and the one whose
// narrowing property is load-bearing rather than incidental: the likes
// table is written by a THIRD PARTY (whoever liked the post), so if this
// conjunct were ORed with the rule instead of ANDed, anybody could put a
// post into anybody's view by liking it. It is a conjunct, so a liked
// post the viewer cannot read is simply not on the page — see
// LikedByUserRef above.
//
// Placeholder discipline (ADR 0063): the builder binds $1–$10, the
// filter selection binds above that when present, the rule's fragment
// owns everything above them, and its args are appended LAST.
//
// ⚠️ THE NUMBERING MOVED IN #1251 SLICE 2 and every reference had to move
// with it. `visibility` was $3 and `tag` was $5; both conjuncts are now
// rendered by facet.Selection.SQL, which numbers from whatever is
// already bound, so the two slots are gone and the seven parameters
// above them shifted down by two. The three OTHER readers of a
// hardcoded number — the keyset predicate, `LIMIT`, and the caller ref
// the mature and kind fragments name — are updated below and are the
// whole of what an off-by-one here would break. The capture-based
// comparison in the PR is what proves it did not: the same feed, the
// same ids, the same cursor tokens, before and after.
func (h *Handler) ListPostsPageGated(
	ctx context.Context,
	id *auth.Identity,
	p ListPostsPageParams,
) ([]ListPostsPageRow, error) {
	// ⛔ THE "ASKED FOR, NAMED NOTHING" CASE, ANSWERED BEFORE THE
	// SELECTION IS BUILT — because a facet selection cannot carry it.
	//
	// `?kind=nonsense` and an EMPTY NON-NIL `Visibility` are both a
	// caller naming a set with nothing in it, and both must return an
	// empty page. Zero terms in a facet.Selection means the OPPOSITE —
	// no constraint at all — so folding either into the selection would
	// turn a typo into "show me everything", which is the one direction
	// a narrowing filter may never move.
	//
	// It sits here rather than inside feedFilters because only this scope
	// can answer with a page. See feedFiltersSelectNothing.
	if feedFiltersSelectNothing(p) {
		return nil, nil
	}

	args := []any{
		p.IncludeDeleted,  // $1
		p.AuthorUserRef,   // $2
		p.Q,               // $3
		p.FeedFollowerRef, // $4
		p.CursorPostedAt,  // $5
		p.CursorID,        // $6
		p.RowLimit,        // $7
		p.TeamID,          // $8
		p.LikedByUserRef,  // $9
	}
	// $10 — the caller's own ref, read by the mature owner exemption and
	// by the kind conjunct's per-member field gate. Bound BEFORE
	// readRuleSQL is asked for its offset, so the rule's own placeholders
	// start above it (ADR 0063's discipline: whoever binds last counts
	// what is already there). A bound placeholder rather than an inlined
	// literal, unlike the facet aggregators: this is the hottest query in
	// the app and a per-user query TEXT would defeat statement caching
	// for a value that changes nothing about the plan.
	//
	// ⚠️ IT IS BOUND UNCONDITIONALLY AND REFERENCED UNCONDITIONALLY, and
	// the second half is what makes the first half safe. Postgres cannot
	// infer a type for a parameter no statement mentions —
	// `could not determine data type of parameter $10` (42P18) — and
	// BOTH readers of this value fold to the empty string on some
	// callers: MatureFilterSQL renders nothing for a qualified viewer or
	// an admin, and FieldsReadableSQL renders nothing for system.admin /
	// content.read.all / a global assets.admin.
	//
	// It used to be bound only when the mature fragment referenced it,
	// which was correct while that fragment was its only reader and
	// stopped being correct the moment a second one appeared: "bind it
	// if EITHER of two fragments happens to be non-empty" is a condition
	// that has to be re-derived every time a third arrives, and getting
	// it wrong is a 42P18 on exactly one class of caller — the path a
	// single-arm test does not take. The tautology below is the same one
	// search.runPosts uses on its own caller ref, for the same reason,
	// and Postgres folds it away.
	args = append(args, matureOwnerArg(id)) // $10
	matureFrag := visibility.MatureFilterSQL(
		"", visibility.MatureOwnerColPost, "$10", p.Mature, p.MatureAdmin)

	// ⭐ The VIEW-level mature filter (#1292), which is a SECOND and
	// separate conjunct on the same column. Rendered next to the gate
	// rather than merged with it because they answer different
	// questions and the merge would hide that: see
	// visibility.MatureExcludeSQL, and ExcludeMature's own note above.
	//
	// It takes no placeholder, so it is safe to render conditionally
	// where matureFrag is not (an unreferenced parameter is a 42P18).
	var matureViewFrag string
	if p.ExcludeMature {
		matureViewFrag = visibility.MatureExcludeSQL("")
	}

	// ⭐ THE FEED'S FILTERS, COMPOSED THROUGH THE SHARED GRAMMAR — ADR
	// 0093 decision 1, "filtered browse does not get a parallel filter
	// implementation", and decision 3, "a filter is defined once".
	//
	// FOUR dimensions and ONE call. `kind` crossed in slice 1 (#1251);
	// `visibility` and `tag` crossed in slice 2; `ai` crosses in slice 3
	// and is the payoff — the dimension arrived complete from #1242 and
	// wiring it here cost one `sel.With`, because the composition was
	// already built. They are rendered together rather than one fragment
	// each because the grammar already knows how dimensions compose —
	// within a dimension by its own conjunctive rule, across dimensions
	// by AND — and asking it four times would be four chances to get the
	// joining wrong in the one place it is already correct.
	//
	// What crosses the seam is the FILTER and only the filter. Every
	// scoping parameter above — author, team, follow set, liked-by — is
	// still bound and applied here, because scope selects the corpus
	// rather than narrowing within it (decision 2), and so are this
	// query's ordering and its keyset cursor. The result is the same
	// posts in the same order on the same pages; what changed is that
	// "which rows carry this tag", "which rows are in this tier", "which
	// rows are this kind" and "which rows are purely AI" are each stated
	// once, in facet.dimensionSQL, where /search reads them too. The tag
	// arm in particular was a byte-identical SECOND COPY of that
	// expression.
	//
	// ⛔ THE READ RULE IS NOT IN HERE AND MUST NOT BE. `ruleFrag` below
	// is a separate conjunct, ANDed on after this fragment, and that
	// ordering is the whole reason `?visibility=` is a display filter
	// rather than a grant: naming five tiers selects among the ones the
	// caller could already read. A tier that reached the rule would be an
	// authorization bypass wearing a filter's clothes — see
	// facet.FacetVisibility.
	//
	// The "asked for, named nothing" case was answered at the top of this
	// function, where it can return a page. See feedFiltersSelectNothing.
	var filterFrag string
	if sel := feedFilters(p); !sel.Empty() {
		frag, selArgs, satisfiable := sel.SQL(
			visibility.EntityPost, "posts", len(args), feedRenderContext(id, "$10"))
		if !satisfiable {
			// ⚠️ REACHABLE, and the note that stood here said it was
			// not ("unreachable for these three dimensions today").
			// That was true of the half it was looking at and false of
			// the whole: a post IS satisfiable for all four dimensions,
			// so the ENTITY arm never fires — but [facet.Selection.SQL]
			// re-validates every VALUE through
			// facet.FacetType.CanonicalValue and returns the same false
			// for a value outside a closed vocabulary. `?visibility=junk`
			// takes exactly this branch today and answers an empty page,
			// which is the documented behaviour (see
			// [ListPostsPageParams.Visibility]) arriving through here
			// rather than through the `= ANY('{}')` the note assumed.
			//
			// `?ai=` cannot reach it: its value is validated at the
			// handler and an unknown one is a 400 (#1251 slice 3), which
			// is the whole reason that parameter needs no companion flag.
			//
			// Honoured rather than assumed either way, which is what
			// runPosts and the aggregators do with the same answer.
			// "Unsupported" means zero rows, never "no constraint".
			return nil, nil
		}
		filterFrag = frag
		args = append(args, selArgs...)
	}

	// The drafts page (ADR 0091 decision 7). Two halves that must move
	// together: waive the read rule's publication conjunct, and add its
	// complement so the page cannot also return published rows. Neither
	// half alone is a listing anybody asked for — the first is browse
	// with drafts mixed in.
	var ruleOpts []visibility.Option
	var draftFrag string
	if p.Draft {
		ruleOpts = append(ruleOpts, visibility.IncludeDrafts())
		draftFrag = "\n  AND " + visibility.PostUnpublishedSQL("")
	}

	ruleFrag, ruleArgs, err := readRuleSQL(ctx, id, "posts", len(args), ruleOpts...)
	if err != nil {
		return nil, err
	}
	args = append(args, ruleArgs...)

	order := newFeedOrder(p.Ascending)

	var b strings.Builder
	b.WriteString(`SELECT ` + listPostsPageColumns + `
FROM posts
WHERE ($1::BOOLEAN IS TRUE OR deleted_at IS NULL)
  AND ($2::BIGINT IS NULL OR author_user_ref = $2::BIGINT)
  AND ($3::TEXT IS NULL OR search_text @@ plainto_tsquery('english', $3::TEXT))
  AND ($4::BIGINT IS NULL
       OR EXISTS (SELECT 1 FROM user_follows ff
                    WHERE ff.follower_user_ref = $4::BIGINT
                      AND ff.followee_user_ref = posts.author_user_ref)
       OR EXISTS (SELECT 1 FROM team_follows tf
                    WHERE tf.user_ref = $4::BIGINT
                      AND tf.team_id = posts.team_id)
       OR EXISTS (SELECT 1 FROM tag_follows gf
                    JOIN post_tags pgt ON pgt.tag = gf.tag
                    WHERE gf.user_ref = $4::BIGINT
                      AND pgt.post_id = posts.id))
  AND ($8::UUID IS NULL OR team_id = $8::UUID)
  AND ($9::BIGINT IS NULL
       OR EXISTS (SELECT 1 FROM likes lk
                    WHERE lk.target_kind = 'post'
                      AND lk.target_id = posts.id
                      AND lk.user_ref = $9::BIGINT))
  AND ($10::BIGINT IS NULL OR TRUE)
  AND ` + order.keysetSQL("posted_at", "id", 5, 6))
	b.WriteString(draftFrag)
	b.WriteString(filterFrag)
	b.WriteString(matureFrag)
	b.WriteString(matureViewFrag)
	b.WriteString(ruleFrag)
	b.WriteString(`
` + order.orderBySQL("posted_at", "id") + `
LIMIT $7::INTEGER`)

	rows, err := h.Pool.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("posts: list page: %w", err)
	}
	defer rows.Close()

	var out []ListPostsPageRow
	for rows.Next() {
		var i ListPostsPageRow
		if err := rows.Scan(
			&i.ID, &i.AuthorUserRef, &i.Title, &i.Description, &i.Visibility,
			&i.CoverAssetID, &i.CoverThumbnailAssetID, &i.PostedAt,
			&i.LikeCount, &i.CommentCount, &i.OriginServerID, &i.TeamID,
			&i.StateID, &i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason,
		); err != nil {
			return nil, fmt.Errorf("posts: list page scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: list page rows: %w", err)
	}
	return out, nil
}

// ListSharedWithMeGated returns one page of "posts somebody explicitly
// shared with me" (#875): the posts on which this caller holds a live
// post_acls grant. Newest-first on the same (posted_at, id) cursor key
// as the feed.
//
// The predicate is visibility.PostLiveGrantSQL — the SAME fragment the
// post read rule ORs in as its ACL disjunct, not a second copy of it.
// It moved to the shared package with the rest of the rule (#873) and is
// exported for this one caller. That is the whole design
// of this surface: "shared with me" and "a grant lets me read this" must
// be the same question, so a post can never appear here that
// GET /posts/{id} then refuses, and expiry/revocation drop an item off
// this page for free rather than because someone remembered to repeat
// the `expires_at` clause.
//
// No further read-rule conjunct is applied, and that is not an omission.
// ADR 0010 L6: a grant is purely additive, so a live grant alone is
// sufficient authorization to read the post at any tier — the rule's own
// disjunct sits at the top level for exactly that reason. AND-ing the
// full rule in here would be a no-op on every row this query can return.
//
// Soft-delete is the caller's axis as always: a deleted post is not
// shared content, and unlike the feed this surface has no admin
// trash-view mode, so it is filtered unconditionally.
//
// An anonymous caller cannot reach this (the handler 401s first) and
// would have no principal to match anyway.
//
// ⚠️ THE MATURE AXIS *IS* APPLIED HERE, and it is the one conjunct the
// paragraph above does not cover (#1116, ADR 0090 §1).
//
// "A grant is purely additive, so a live grant alone is sufficient
// authorization" is a true statement about CLEARANCE, and it is exactly
// why no further read-rule conjunct belongs here. It says nothing about
// RATING. ADR 0090's whole claim is that the two are independent axes —
// `sensitivity` answers who is ALLOWED, `mature` answers who has OPTED
// IN — so somebody granting me access to a post tells me they chose to
// let me read it, and tells me nothing about whether I consented to be
// shown adult content. Reading the additivity of the first axis as
// consent on the second is precisely the conflation the ADR exists to
// prevent, and it would make "share it with them" a way to route mature
// content to a reader who switched it off.
//
// The reader loses nothing they cannot recover: the grant survives
// untouched, and the item reappears the moment they opt in. Nothing is
// revoked — it is not offered.
func (h *Handler) ListSharedWithMeGated(
	ctx context.Context,
	userRef int64,
	cursorPostedAt pgtype.Timestamptz,
	cursorID pgtype.UUID,
	rowLimit int32,
	mature visibility.MatureViewer,
	matureAdmin bool,
) ([]ListPostsPageRow, error) {
	args := []any{
		cursorPostedAt, // $1
		cursorID,       // $2
		rowLimit,       // $3
		userRef,        // $4 — consumed by PostLiveGrantSQL
	}
	// $5 only when the conjunct is present — see ListPostsPageGated's
	// note: an unreferenced parameter is a 42P18, not a no-op.
	matureFrag := visibility.MatureFilterSQL(
		"", visibility.MatureOwnerColPost, "$5", mature, matureAdmin)
	if matureFrag != "" {
		args = append(args, userRef)
	}

	sql := `SELECT ` + listPostsPageColumns + `
FROM posts
WHERE deleted_at IS NULL
  AND ($1::TIMESTAMPTZ IS NULL
       OR posted_at < $1::TIMESTAMPTZ
       OR (posted_at = $1::TIMESTAMPTZ AND id < $2::UUID))
  AND ` + visibility.PostLiveGrantSQL("", 4) + matureFrag + `
ORDER BY posted_at DESC, id DESC
LIMIT $3::INTEGER`

	rows, err := h.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("posts: shared with me: %w", err)
	}
	defer rows.Close()

	var out []ListPostsPageRow
	for rows.Next() {
		var i ListPostsPageRow
		if err := rows.Scan(
			&i.ID, &i.AuthorUserRef, &i.Title, &i.Description, &i.Visibility,
			&i.CoverAssetID, &i.CoverThumbnailAssetID, &i.PostedAt,
			&i.LikeCount, &i.CommentCount, &i.OriginServerID, &i.TeamID,
			&i.StateID, &i.CreatedAt, &i.UpdatedAt, &i.DeletedAt, &i.DeletedReason,
		); err != nil {
			return nil, fmt.Errorf("posts: shared with me scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: shared with me rows: %w", err)
	}
	return out, nil
}

// postsByAssetBodySQL builds the FROM + WHERE both by-asset queries run,
// and it is ONE expression on purpose (#1237).
//
// [Handler.ListPostsByAssetGated] returns a bounded page; [Handler.CountPostsByAssetGated]
// counts the whole readable set. `GET /assets/{id}/posts` SUBTRACTS one
// from a total, so the moment the two disagree about which posts are
// readable the difference stops meaning "withheld" and starts meaning
// "withheld, plus however much the two rules drifted". Two copies of a
// read rule that are then subtracted from each other is the #665 defect
// with an arithmetic amplifier bolted on.
//
// Returns the clause and its args; `$1` is always the asset id.
func (h *Handler) postsByAssetBodySQL(
	ctx context.Context,
	id *auth.Identity,
	assetID uuid.UUID,
	mature visibility.MatureViewer,
	matureAdmin bool,
) (string, []any, error) {
	args := []any{assetID} // $1
	// $2 only when the conjunct is present — see ListPostsPageGated.
	matureFrag := visibility.MatureFilterSQL(
		"p", visibility.MatureOwnerColPost, "$2", mature, matureAdmin)
	if matureFrag != "" {
		args = append(args, matureOwnerArg(id))
	}
	ruleFrag, ruleArgs, err := readRuleSQL(ctx, id, "p", len(args))
	if err != nil {
		return "", nil, err
	}
	args = append(args, ruleArgs...)

	return `
FROM posts p
WHERE p.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM post_assets pa
                WHERE pa.post_id = p.id AND pa.asset_id = $1::UUID)` +
		matureFrag + ruleFrag, args, nil
}

// ListPostsByAssetGated returns the ids of posts the caller may read
// whose members include the given asset (#478 slice-2, ADR 0070).
// Bounded (no cursor) — an asset lands in few posts, and the client only
// needs "redirect if one, list if several". Newest first.
//
// This used to hand-build its own tier list ({'public'} for anonymous,
// {'public','org-only'} for authenticated), which agreed with the
// single-item gate for those two tiers and silently disagreed for the
// three relationship tiers — an author could not find their OWN private
// post by asset. It now obtains the rule instead of restating it (#665),
// which both closes that gap and removes the second expression.
// The mature axis is ANDed on for the same reason it is on the feed
// (#1116): this is a DISCOVERY surface — "which posts contain this
// thing" — and answering it for a disqualified viewer would name mature
// posts by id, which is the first half of finding them. That the asset
// gate upstream would probably have refused already is not the
// argument; a predicate that depends on another endpoint's behaviour is
// a predicate whose correctness moves when that endpoint does.
//
// ⚠️ THE `LIMIT 200` IS A TRUNCATION, NOT A GATE. A caller of this
// function cannot tell a 200-row answer from "there are exactly 200".
// That distinction was invisible while the only caller wanted "one or
// several", and became a wrong number the moment `GET /assets/{id}/posts`
// subtracted this length from a total — see CountPostsByAssetGated.
func (h *Handler) ListPostsByAssetGated(
	ctx context.Context,
	id *auth.Identity,
	assetID uuid.UUID,
	mature visibility.MatureViewer,
	matureAdmin bool,
) ([]pgtype.UUID, error) {
	body, args, err := h.postsByAssetBodySQL(ctx, id, assetID, mature, matureAdmin)
	if err != nil {
		return nil, err
	}

	sql := `SELECT p.id` + body + `
ORDER BY p.posted_at DESC, p.id DESC
LIMIT 200`

	rows, err := h.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("posts: by asset: %w", err)
	}
	defer rows.Close()

	var out []pgtype.UUID
	for rows.Next() {
		var pid pgtype.UUID
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("posts: by asset scan: %w", err)
		}
		out = append(out, pid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("posts: by asset rows: %w", err)
	}
	return out, nil
}

// CountPostsByAssetGated counts every post the caller may read whose
// members include the given asset — the same set ListPostsByAssetGated
// pages, UNBOUNDED (#1237).
//
// # Why this exists rather than len(the list)
//
// `GET /assets/{id}/posts` answers with the readable posts plus
// `withheld_count`, "how many further posts contain this asset that you
// may not read". It derived that as `total − len(items)`, and `items`
// comes off a `LIMIT 200`. So an asset in 250 posts the caller may read
// in FULL reported 50 withheld: the arithmetic counted the truncation as
// a restriction, and the UI copy built on it ("also used in 50 posts you
// cannot see") was simply false — the caller could see all 250.
//
// It is the wrong direction for a disclosure count to fail in, too. The
// number is meant to be the smallest honest answer (the handler floors
// it at zero for the same reason), and truncation pushed it UP.
//
// So the endpoint subtracts this count instead, and the truncation goes
// back to being a page size: the list still stops at 200, but nothing
// reads a restriction into where it stopped.
//
// The mature axis rides the shared clause exactly as the list's does, so
// a mature post a disqualified viewer cannot be shown is excluded here
// too and therefore lands in the withheld remainder. That is deliberate
// and is ADR 0090 §1: the total this is subtracted FROM is mature-blind,
// so an owner still learns the true number of places their file is used.
func (h *Handler) CountPostsByAssetGated(
	ctx context.Context,
	id *auth.Identity,
	assetID uuid.UUID,
	mature visibility.MatureViewer,
	matureAdmin bool,
) (int64, error) {
	body, args, err := h.postsByAssetBodySQL(ctx, id, assetID, mature, matureAdmin)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := h.Pool.QueryRow(ctx, `SELECT count(*)::BIGINT`+body, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("posts: by asset count: %w", err)
	}
	return n, nil
}
