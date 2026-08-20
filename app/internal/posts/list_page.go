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
	Visibility []string
	Q          *string
	Tag        *string
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
//     be the union it should always have been)
//   - q: plain-text TSVECTOR search across post search_text
//   - tag: single-tag filter (intersects with q if both given)
//   - feed_follower_ref: restrict to what the given ref follows
//     (?feed=following) — authors OR teams OR tags. Three EXISTS,
//     hitting the user_follows PK, the team_follows PK and the
//     tag_follows PK respectively (#1123).
//   - team_id: restrict to one team's posts (#684)
//   - liked_by: restrict to posts a given ref has liked (#1106)
//   - kind: restrict to posts whose COVER asset is of a given kind —
//     the browse footer's type filter (#1166). See kindFilterSQL, and
//     note that its EXISTS carries the field-plane readability rule for
//     the cover, so a cover this caller cannot see matches no kind.
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
// id to join on, and why matching is exact rather than case-folded.
//
// `liked_by` is the newest member of that list and the one whose
// narrowing property is load-bearing rather than incidental: the likes
// table is written by a THIRD PARTY (whoever liked the post), so if this
// conjunct were ORed with the rule instead of ANDed, anybody could put a
// post into anybody's view by liking it. It is a conjunct, so a liked
// post the viewer cannot read is simply not on the page — see
// LikedByUserRef above.
//
// Placeholder discipline (ADR 0063): the builder binds $1–$11, the
// mature and kind conjuncts bind above that when present, the rule's
// fragment owns everything above them, and its args are appended LAST.
func (h *Handler) ListPostsPageGated(
	ctx context.Context,
	id *auth.Identity,
	p ListPostsPageParams,
) ([]ListPostsPageRow, error) {
	args := []any{
		p.IncludeDeleted,  // $1
		p.AuthorUserRef,   // $2
		p.Visibility,      // $3
		p.Q,               // $4
		p.Tag,             // $5
		p.FeedFollowerRef, // $6
		p.CursorPostedAt,  // $7
		p.CursorID,        // $8
		p.RowLimit,        // $9
		p.TeamID,          // $10
		p.LikedByUserRef,  // $11
	}
	// $12 — the caller's own ref, for the mature owner exemption. Bound
	// BEFORE readRuleSQL is asked for its offset, so the rule's own
	// placeholders start above it (ADR 0063's discipline: whoever binds
	// last counts what is already there). A bound placeholder rather
	// than an inlined literal, unlike the facet aggregators: this is the
	// hottest query in the app and a per-user query TEXT would defeat
	// statement caching for a value that changes nothing about the plan.
	//
	// ⚠️ BOUND ONLY WHEN THE FRAGMENT REFERENCES IT. MatureFilterSQL
	// returns the EMPTY STRING for a qualified viewer and for an admin —
	// no conjunct at all, so Postgres never sees a filter on the common
	// path. Binding $12 unconditionally therefore sent a parameter no
	// statement mentioned, and Postgres cannot infer a type for one:
	// `could not determine data type of parameter $12` (42P18), on every
	// request by a QUALIFIED reader. The unqualified path — the one a
	// single-arm test exercises — worked perfectly, which is why this
	// reached the suite instead of the keyboard.
	matureFrag := visibility.MatureFilterSQL(
		"", visibility.MatureOwnerColPost, "$12", p.Mature, p.MatureAdmin)
	if matureFrag != "" {
		args = append(args, matureOwnerArg(id))
	}

	// The `?kind=` conjunct (#1166). Bound AFTER the mature fragment
	// because that one names `$12` literally, and BEFORE readRuleSQL is
	// asked for its offset — same discipline, whoever binds last counts
	// what is already there.
	var kindFrag string
	if p.KindsRequested {
		frag, kindArgs := kindFilterSQL(id, viewkind.Compile(p.Kinds), len(args))
		kindFrag = frag
		args = append(args, kindArgs...)
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
  AND ($3::TEXT[] IS NULL OR visibility = ANY($3::TEXT[]))
  AND ($4::TEXT IS NULL OR search_text @@ plainto_tsquery('english', $4::TEXT))
  AND ($5::TEXT IS NULL
       OR EXISTS (SELECT 1 FROM post_tags pt
                    WHERE pt.post_id = posts.id AND pt.tag = $5::TEXT))
  AND ($6::BIGINT IS NULL
       OR EXISTS (SELECT 1 FROM user_follows ff
                    WHERE ff.follower_user_ref = $6::BIGINT
                      AND ff.followee_user_ref = posts.author_user_ref)
       OR EXISTS (SELECT 1 FROM team_follows tf
                    WHERE tf.user_ref = $6::BIGINT
                      AND tf.team_id = posts.team_id)
       OR EXISTS (SELECT 1 FROM tag_follows gf
                    JOIN post_tags pgt ON pgt.tag = gf.tag
                    WHERE gf.user_ref = $6::BIGINT
                      AND pgt.post_id = posts.id))
  AND ($10::UUID IS NULL OR team_id = $10::UUID)
  AND ($11::BIGINT IS NULL
       OR EXISTS (SELECT 1 FROM likes lk
                    WHERE lk.target_kind = 'post'
                      AND lk.target_id = posts.id
                      AND lk.user_ref = $11::BIGINT))
  AND ` + order.keysetSQL("posted_at", "id", 7, 8))
	b.WriteString(draftFrag)
	b.WriteString(kindFrag)
	b.WriteString(matureFrag)
	b.WriteString(ruleFrag)
	b.WriteString(`
` + order.orderBySQL("posted_at", "id") + `
LIMIT $9::INTEGER`)

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
func (h *Handler) ListPostsByAssetGated(
	ctx context.Context,
	id *auth.Identity,
	assetID uuid.UUID,
	mature visibility.MatureViewer,
	matureAdmin bool,
) ([]pgtype.UUID, error) {
	args := []any{assetID} // $1
	// $2 only when the conjunct is present — see ListPostsPageGated.
	matureFrag := visibility.MatureFilterSQL(
		"p", visibility.MatureOwnerColPost, "$2", mature, matureAdmin)
	if matureFrag != "" {
		args = append(args, matureOwnerArg(id))
	}
	ruleFrag, ruleArgs, err := readRuleSQL(ctx, id, "p", len(args))
	if err != nil {
		return nil, err
	}
	args = append(args, ruleArgs...)

	sql := `SELECT p.id
FROM posts p
WHERE p.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM post_assets pa
                WHERE pa.post_id = p.id AND pa.asset_id = $1::UUID)` +
		matureFrag + ruleFrag + `
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
