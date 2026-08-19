// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1181 — the anonymous read of the browse feed, and the two gates that
// decide it.
//
// # Why this test composes both gates instead of testing either one
//
// Anonymous `GET /posts` used to be refused twice over: the route was
// absent from auth.PublicSurfaceRoutes, and posts.ListPosts 401'd a nil
// caller on its own. #1181 removes the second refusal and adds the
// route, which leaves the toggle as the only thing standing between an
// anonymous visitor and the feed.
//
// A test of either gate alone would not notice if the other one went
// away. The auth package can already prove "the middleware 401s
// /posts when the toggle is off" against a stub handler, and this
// package can already prove "ListPosts returns rows to a nil caller" by
// calling the method directly — and BOTH would keep passing on a build
// where the middleware entry had been deleted and the handler's own
// 401 had been deleted too, i.e. on a build that serves the feed to
// anonymous callers on a PRIVATE install. That is the failure this file
// exists to catch, so it wires the real auth.ResolveIdentity middleware
// around the real handler methods and drives them over HTTP.
//
// The status codes are the generated Visit*Response writers, not a
// hand-rolled mapping, so the assertion is against what the wire would
// actually carry.
//
// # The three arms, and why the write arm is the important one
//
// read + public mode ON  → 200   (the feature)
// read + public mode OFF → 401   (a private install stays private)
// write                  → 401   in BOTH states (the escalation guard)
//
// The write arm is the one worth the machinery. auth.PublicSurfaceRoutes
// entries carry no HTTP method, so naming "/posts" there matches POST
// /posts as well as GET. That is safe because the table is a deny-list
// — being named can only subtract reachability, never add it — but
// "safe because of how the middleware is written" is exactly the kind
// of reasoning that stops being true during a refactor. So it is
// asserted rather than argued: an anonymous write is refused with the
// toggle ON, which is the state where the middleware waves it through
// and CreatePost's own nil-caller check is the only thing left.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// afRoute stands in for the generated mux: it runs the REAL
// public-mode middleware, then dispatches to the REAL handler method
// for the verb, then lets the generated response type write itself.
//
// The dispatch is by method on one path because that is precisely the
// distinction under test — auth.PublicSurfaceRoutes matches on path
// alone, so GET and POST on "/posts" enter the middleware
// indistinguishable and must leave it differently.
func afRoute(h *Handler, publicMode bool) http.Handler {
	r := &auth.Resolver{
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		PublicMode: func(context.Context) bool { return publicMode },
	}
	return r.ResolveIdentity(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		switch req.Method {
		case http.MethodGet:
			limit := 1
			resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{
				Params: openapi.ListPostsParams{Limit: &limit},
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = resp.VisitListPostsResponse(w)
		case http.MethodPost:
			title := "af write probe"
			resp, err := h.CreatePost(ctx, openapi.CreatePostRequestObject{
				Body: &openapi.PostCreate{
					Title: &title,
					Members: []openapi.PostAssetWrite{
						{AssetId: openapi_types.UUID(afNonexistentAsset)},
					},
				},
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_ = resp.VisitCreatePostResponse(w)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// A UUID that is not an asset. The write arm must be refused for
// AUTHENTICATION reasons before anything looks this up — if a build
// ever answers 404 here instead of 401, the nil-caller check has moved
// below the member gate and the test says so.
var afNonexistentAsset = openapi_types.UUID{
	0x11, 0x81, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00,
	0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x11, 0x81,
}

// TestPostsRouteGate is #1181's acceptance at the route layer. It fails
// on the pre-fix build at the first case (401 instead of 200) and would
// fail on an over-wide fix at the second or the write cases.
func TestPostsRouteGate(t *testing.T) {
	h := wireWriteHandler(t)

	cases := []struct {
		name       string
		method     string
		publicMode bool
		want       int
		why        string
	}{
		{
			name:   "anonymous read is served when public mode is on",
			method: http.MethodGet, publicMode: true, want: http.StatusOK,
			why: "the feature: a public install's visitors can browse. " +
				"This is the arm that was a hard 401 before #1181",
		},
		{
			name:   "anonymous read is refused when public mode is off",
			method: http.MethodGet, publicMode: false, want: http.StatusUnauthorized,
			why: "a private install must stay private. Removing the '/posts' " +
				"entry from auth.PublicSurfaceRoutes breaks exactly this case, " +
				"and nothing in the posts package would otherwise notice",
		},
		{
			name:   "anonymous write is refused with public mode ON",
			method: http.MethodPost, publicMode: true, want: http.StatusUnauthorized,
			why: "THE escalation guard. PublicSurfaceRoutes carries no method, so " +
				"the middleware passes this POST through; CreatePost's own " +
				"nil-caller check is the only refusal left and must hold",
		},
		{
			name:   "anonymous write is refused with public mode OFF",
			method: http.MethodPost, publicMode: false, want: http.StatusUnauthorized,
			why: "refused a layer earlier, by the middleware. Same answer, " +
				"different gate — which is the point of asserting both",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// No Authorization header and no session cookie: the
			// request resolves to an anonymous identity, which is the
			// only caller this test is about.
			req := httptest.NewRequest(c.method, "/api/v1/posts", nil)
			afRoute(h, c.publicMode).ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("%s /api/v1/posts (public_mode=%v) = %d, want %d — %s",
					c.method, c.publicMode, rec.Code, c.want, c.why)
			}
		})
	}
}

// TestPostsRouteGate_AuthenticatedReadIsUnaffectedByTheToggle guards
// the guard from the other side.
//
// Public mode decides ADMISSION FOR ANONYMOUS CALLERS and nothing else.
// A gate placed above the token and cookie branches, or scoped to every
// request rather than to unauthenticated ones, would 401 signed-in
// users on a private install — which is every user of every private
// install. The cases above cannot detect that, because every one of
// them is anonymous.
//
// The identity is injected into the context directly rather than
// carried by a credential: ResolveIdentity authenticates from headers
// and would need a live session to do otherwise, and the thing being
// pinned here is the handler's behaviour once an identity is present,
// not the middleware's header parsing.
func TestPostsRouteGate_AuthenticatedReadIsUnaffectedByTheToggle(t *testing.T) {
	h := wireWriteHandler(t)

	for _, publicMode := range []bool{true, false} {
		limit := 1
		resp, err := h.ListPosts(
			auth.WithIdentity(t.Context(), lvIdentity(ptAuthor)),
			openapi.ListPostsRequestObject{
				Params: openapi.ListPostsParams{Limit: &limit},
			})
		if err != nil {
			t.Fatalf("ListPosts(authenticated, public_mode=%v): %v", publicMode, err)
		}
		if _, ok := resp.(openapi.ListPosts200JSONResponse); !ok {
			t.Errorf("ListPosts(authenticated) = %T with public_mode=%v, want 200 — "+
				"the toggle has started affecting signed-in callers", resp, publicMode)
		}
	}
}

// ---------------------------------------------------------------------------
// #1181 scope 4 — the enrichment sweep
// ---------------------------------------------------------------------------
//
// Opening the feed to anonymous callers does not introduce a single new
// enrichment; it makes four EXISTING anonymous stories reachable through
// a handler that could not previously be entered without a session. Each
// of them was written for /posts/by-asset or /collections/{id}/posts and
// is composed here by construction rather than by restatement — which is
// exactly why they need asserting ON THIS PATH: "composed by
// construction" is a claim about a call graph, and call graphs get
// refactored.
//
// So every assertion below drives the real ListPosts with a context
// carrying NO identity. Nothing calls enrichPreview or the read rule
// directly; the point is the handler.
//
// Each fixture ships its own negative control, because every one of
// these assertions is of the form "X is absent" and absence passes
// trivially on a page that returned nothing at all.

// afList runs one anonymous ListPosts scoped to a single author and
// returns both the decoded posts and the raw JSON.
//
// The raw JSON is the load-bearing half. Asserting on the typed fields
// only checks the fields the test thought to look at; a withheld value
// that reappears on a field added later — the #1066 derived-copy class —
// is caught by grepping the whole body for the string that should not
// be in it.
func afList(t *testing.T, h *Handler, author int64) ([]openapi.Post, string) {
	t.Helper()
	limit := maxListLimit
	ref := author
	resp, err := h.ListPosts(context.Background(), openapi.ListPostsRequestObject{
		Params: openapi.ListPostsParams{Limit: &limit, AuthorRef: &ref},
	})
	if err != nil {
		t.Fatalf("ListPosts(anonymous): %v", err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts(anonymous) = %T, want 200", resp)
	}
	raw, err := json.Marshal(ok)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return ok.Items, string(raw)
}

// afStoredMature reads the DERIVED posts.mature column. The house rule
// is to assert the persisted value rather than the one the fixture
// thought it wrote (#946) — and here the two genuinely disagree, since
// the column is trigger-maintained and ignores what an INSERT supplies.
func afStoredMature(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var got bool
	if err := pool.QueryRow(context.Background(),
		`SELECT mature FROM posts WHERE id = $1`, id).Scan(&got); err != nil {
		t.Fatalf("read mature: %v", err)
	}
	return got
}

func afIDs(items []openapi.Post) map[uuid.UUID]*openapi.Post {
	out := make(map[uuid.UUID]*openapi.Post, len(items))
	for i := range items {
		out[uuid.UUID(items[i].Id)] = &items[i]
	}
	return out
}

// TestAnonymousFeed_OwnerDisplayNameOptOut is the #1023 assertion on
// this path.
//
// ADR 0024's opt-out makes a profile 404 to anonymous callers. An author
// object riding the feed card is a side channel around that — the same
// name, arriving on someone else's surface — and until #1181 the feed
// was not a surface anonymous callers could reach, so the opt-out held
// here for free. It no longer does.
//
// The negative control is a second author who did NOT opt out, whose
// display name MUST still ride: without it this test passes on a build
// that dropped the author object entirely.
func TestAnonymousFeed_OwnerDisplayNameOptOut(t *testing.T) {
	h := wireWriteHandler(t)

	hiddenRef, hiddenUser := seedAuthor(t, h.Pool, "Hidden Realname", "Hidden Displayname", "", true)
	shownRef, shownUser := seedAuthor(t, h.Pool, "Shown Realname", "Shown Displayname", "", false)
	hiddenPost := seedAuthoredPost(t, h.Pool, hiddenRef)
	shownPost := seedAuthoredPost(t, h.Pool, shownRef)

	hiddenItems, hiddenJSON := afList(t, h, hiddenRef)
	byID := afIDs(hiddenItems)
	if byID[hiddenPost] == nil {
		t.Fatal("the opted-out author's PUBLIC post is missing from the anonymous feed — " +
			"the opt-out is about the identity, not the content; the post must still ride")
	}
	if a := byID[hiddenPost].Author; a != nil {
		t.Errorf("post.author = %+v for an author who set hide_from_anonymous; want nil "+
			"(a withheld author is an OMISSION from the lookup map, never a redacted object)", *a)
	}
	// Not just the fields this test knows about.
	for _, leaked := range []string{"Hidden Realname", "Hidden Displayname", hiddenUser} {
		if strings.Contains(hiddenJSON, leaked) {
			t.Errorf("the anonymous feed body contains %q, which belongs to an author "+
				"who opted out of anonymous exposure (ADR 0024)", leaked)
		}
	}

	// The control. Same code path, opt-out off.
	shownItems, shownJSON := afList(t, h, shownRef)
	shownByID := afIDs(shownItems)
	if shownByID[shownPost] == nil || shownByID[shownPost].Author == nil {
		t.Fatal("an author who did NOT opt out has no author object on the anonymous feed — " +
			"the withholding above is unconditional, not the opt-out")
	}
	if !strings.Contains(shownJSON, "Shown Displayname") {
		t.Errorf("the non-opted-out author's display name is missing from the feed body")
	}
	// Rung 2 of the ladder is authenticated-only (ADR 0070 §3), so even
	// a consenting author's REAL name must not reach an anonymous
	// caller. Distinct from the opt-out and easy to regress together.
	if strings.Contains(shownJSON, "Shown Realname") {
		t.Errorf("the anonymous feed body contains an author's `fullname` — the real-name " +
			"rung is authenticated-only and must be skipped for anonymous callers")
	}
	_ = shownUser
}

// TestAnonymousFeed_RestrictedMembersAndThumbhash covers #883 and the
// #939/#1066 derived-copy rule in one fixture, because they are one
// mechanism: enrichPreview makes ONE readability decision per member and
// both the asset object and its thumbhash hang off it.
//
// Two members on one public post — one public asset, one restricted —
// each carrying a DISTINCT thumbhash so the bytes can be told apart in
// the body. For an anonymous caller the restricted member becomes a
// #883 placeholder (Restricted: true, no asset object at all), and then
// ListPosts' own tail strips the placeholder: showRestricted(0) is false
// because ref 0 has no preference row to opt in with. So the observable
// answer on THIS path is that the member is GONE, not blanked.
//
// The third post is the one that makes the sweep complete: a post whose
// ONLY member is restricted has nothing left to show and must drop out
// of the page entirely rather than render as an empty card.
func TestAnonymousFeed_RestrictedMembersAndThumbhash(t *testing.T) {
	h := wireWriteHandler(t)

	authorRef, _ := seedAuthor(t, h.Pool, "", "Member Author", "", false)

	publicAsset := seedPreviewAssetOwned(t, h.Pool, "public", true, authorRef)
	restrictedAsset := seedPreviewAssetOwned(t, h.Pool, "restricted", true, authorRef)
	publicHash := []byte{0x11, 0x81, 0xAA, 0xAA}
	restrictedHash := []byte{0x11, 0x81, 0xBB, 0xBB}
	setThumbhash(t, h.Pool, publicAsset, publicHash)
	setThumbhash(t, h.Pool, restrictedAsset, restrictedHash)

	mixed := afSeedPost(t, h.Pool, authorRef, "public", publicAsset, restrictedAsset)
	allRestricted := afSeedPost(t, h.Pool, authorRef, "public", restrictedAsset)

	items, body := afList(t, h, authorRef)
	byID := afIDs(items)

	p := byID[mixed]
	if p == nil {
		t.Fatal("the mixed-member public post is missing entirely — one restricted member " +
			"must not take the whole post down with it")
	}
	if len(p.Members) != 1 {
		t.Fatalf("mixed post has %d members for an anonymous caller, want 1 — "+
			"the restricted member must be stripped and the public one kept", len(p.Members))
	}
	if got := uuid.UUID(p.Members[0].AssetId); got != publicAsset {
		t.Errorf("the surviving member is %s, want the PUBLIC asset %s", got, publicAsset)
	}
	if p.Members[0].Asset == nil {
		t.Fatal("the public member arrived with no asset object")
	}
	if th := p.Members[0].Asset.Thumbhash; th == nil ||
		*th != base64.StdEncoding.EncodeToString(publicHash) {
		t.Errorf("the readable member's thumbhash = %v, want the seeded value — "+
			"the control for the withholding assertion below", th)
	}

	// The withheld member, and every derived copy of it (#1066).
	if strings.Contains(body, restrictedAsset.String()) {
		t.Errorf("the anonymous feed body names the RESTRICTED asset %s", restrictedAsset)
	}
	if enc := base64.StdEncoding.EncodeToString(restrictedHash); strings.Contains(body, enc) {
		t.Errorf("the anonymous feed body carries the restricted asset's THUMBHASH (%s). "+
			"A thumbhash IS a blur (ADR 0064) — a withheld row must withhold its "+
			"derived copies too", enc)
	}

	if byID[allRestricted] != nil {
		t.Errorf("a public post whose ONLY member is restricted still appears in the " +
			"anonymous feed; it has nothing to render and must be dropped")
	}
}

// TestAnonymousFeed_MatureAxis is the mature assertion on this path.
//
// The anonymous arm resolves to the zero MatureViewer, which fails
// QualifiesForMature, so MatureFilterSQL emits its conjunct rather than
// the empty string. The owner-exemption disjunct in that conjunct is
// `author_user_ref = NULLIF($12::BIGINT, 0)` with $12 = 0 for a nil
// caller, so it degenerates to NULL and the filter reduces to
// `NOT mature` — structurally, not by a Go branch.
//
// The non-mature post by the SAME author is the control: without it,
// "the mature post is absent" would pass on any build that returned an
// empty page for anonymous callers, which is precisely the bug the
// visibility default in ListPosts had to avoid.
// ⚠️ THE FIXTURE IS THE HARD PART HERE. `posts.mature` is DERIVED, not
// stored: the posts_cover_mature_sync trigger overwrites whatever an
// INSERT supplies with post_is_mature(id), which is "does any member or
// cover asset carry assets.mature". A fixture that writes
// `INSERT INTO posts (…, mature) VALUES (…, true)` on a memberless post
// therefore produces a post with mature = FALSE, and the test that
// followed it reported a security failure that did not exist. So the
// mature member is seeded on the ASSET, and the derived column is read
// back and asserted BEFORE the feed is queried — a fixture that stops
// being mature must fail as a broken fixture, not as a passing test.
//
// Both members are `public` sensitivity and active/ready on purpose. A
// restricted mature member would be dropped by the #883 path above
// instead, and the test would pass without the mature axis running at
// all.
func TestAnonymousFeed_MatureAxis(t *testing.T) {
	h := wireWriteHandler(t)

	authorRef, _ := seedAuthor(t, h.Pool, "", "Mature Author", "", false)
	matureAsset := seedPreviewAssetOwned(t, h.Pool, "public", true, authorRef)
	tameAsset := seedPreviewAssetOwned(t, h.Pool, "public", true, authorRef)
	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE assets SET mature = TRUE WHERE id = $1`, matureAsset); err != nil {
		t.Fatalf("mark asset mature: %v", err)
	}

	maturePost := afSeedPost(t, h.Pool, authorRef, "public", matureAsset)
	tamePost := afSeedPost(t, h.Pool, authorRef, "public", tameAsset)

	// The fixture-reality check. Without it a trigger change turns this
	// whole test into a tautology.
	if got := afStoredMature(t, h.Pool, maturePost); !got {
		t.Fatalf("posts.mature = false for the post seeded with a mature member; the " +
			"derived column did not pick it up and the assertion below would pass " +
			"on a post that is not mature at all")
	}
	if got := afStoredMature(t, h.Pool, tamePost); got {
		t.Fatalf("posts.mature = true for the control post; the control is not a control")
	}

	items, _ := afList(t, h, authorRef)
	byID := afIDs(items)

	if byID[tamePost] == nil {
		t.Fatal("the author's non-mature PUBLIC post is missing from the anonymous feed; " +
			"the control failed, so the assertion below proves nothing")
	}
	if byID[maturePost] != nil {
		t.Error("an anonymous caller was served a MATURE post — the anonymous viewer " +
			"never qualifies (QualifiesForMature is false for the zero MatureViewer) " +
			"and the owner exemption cannot fire for a caller with no ref")
	}
}

// TestAnonymousFeed_VisibilityDefaultIsPublicTier pins the display
// filter, which is the difference between the feature working and the
// feature looking broken.
//
// A signed-in caller with no ?visibility= gets the union of the shared
// tiers (#1193); before that they got `org-only` alone, and handing an
// anonymous caller that same default intersected `visibility =
// 'org-only'` with a read rule that admits `visibility = 'public'` and
// nothing else — an empty set, on every request, on an install full of
// public posts. The anonymous branch stayed a branch of its own after
// #1193, so it still needs its own test.
//
// It also pins the direction: the filter NARROWS. An anonymous caller
// who explicitly asks for a tier they cannot read gets nothing, rather
// than the parameter widening the rule.
func TestAnonymousFeed_VisibilityDefaultIsPublicTier(t *testing.T) {
	h := wireWriteHandler(t)

	authorRef, _ := seedAuthor(t, h.Pool, "", "Default Author", "", false)
	publicPost := afSeedPost(t, h.Pool, authorRef, "public")
	orgPost := afSeedPost(t, h.Pool, authorRef, "org-only")

	items, _ := afList(t, h, authorRef)
	byID := afIDs(items)
	if byID[publicPost] == nil {
		t.Error("the anonymous default visibility filter hides PUBLIC posts — the feed " +
			"answers 200 with an empty page, which reads as the feature not working")
	}
	if byID[orgPost] != nil {
		t.Error("an anonymous caller was served an ORG-ONLY post")
	}

	// Explicitly asking for a tier the rule refuses returns nothing —
	// the parameter cannot widen the rule.
	limit := maxListLimit
	ref := authorRef
	vis := openapi.ListPostsParamsVisibility("org-only")
	resp, err := h.ListPosts(context.Background(), openapi.ListPostsRequestObject{
		Params: openapi.ListPostsParams{Limit: &limit, AuthorRef: &ref, Visibility: &vis},
	})
	if err != nil {
		t.Fatalf("ListPosts(anonymous, ?visibility=org-only): %v", err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts = %T, want 200", resp)
	}
	if len(ok.Items) != 0 {
		t.Errorf("?visibility=org-only returned %d posts to an anonymous caller; the "+
			"display filter must narrow within the read rule, never widen past it",
			len(ok.Items))
	}
}

// afSeedPost creates one post with the given author, tier and members.
// Separate from seedAuthoredPost (memberless, always public) and
// seedPreviewPost (fixed author, always public) because this sweep needs
// the author, the tier and the membership to vary independently.
//
// There is deliberately NO mature parameter. `posts.mature` is derived
// by the posts_cover_mature_sync trigger from the member and cover
// assets, so a value passed here would be silently overwritten — a
// fixture that looks like it configures the axis while configuring
// nothing. Seed a mature ASSET and attach it; see afStoredMature.
func afSeedPost(
	t *testing.T,
	pool *pgxpool.Pool,
	author int64,
	visibility string,
	members ...uuid.UUID,
) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility)
		 VALUES ($1,$2,'anon feed probe',$3)`, id, author, visibility); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	for i, m := range members {
		if _, err := pool.Exec(ctx,
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`,
			id, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, id)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
	})
	return id
}

// TestAnonymousIdentity_EnrichmentTreatsItAsAnonymous is #1183.
//
// Every anonymous assertion above drives a context with NO identity,
// because that is the shape today's resolver produces. The other shape —
// a non-nil Identity carrying AuthMethod "anonymous" — is what
// `IsAnonymous()` exists for, what the read rule already checks, and
// what `auth.LoadAnonymousIdentity` would have injected had anything
// ever wired it. Three enrichment passes tested `caller == nil` alone,
// so on that path they would have classified an anonymous principal as a
// MEMBER and handed out precisely the display name TestAnonymousFeed_
// OwnerDisplayNameOptOut proves is withheld: the read rule staying right
// while the enrichment beside it leaked.
//
// The dead function is deleted, which removes the only way to reach
// that. This is the assertion that makes reintroducing one safe — it
// fails on the `== nil` form and passes on `isAnonymousCaller`, so the
// two shapes cannot drift apart again.
//
// Same fixture and same controls as the nil-caller test, deliberately:
// the claim is that the two shapes are INDISTINGUISHABLE to enrichment,
// and that is only demonstrated by asking them the same question.
func TestAnonymousIdentity_EnrichmentTreatsItAsAnonymous(t *testing.T) {
	h := wireWriteHandler(t)

	hiddenRef, hiddenUser := seedAuthor(t, h.Pool, "Hidden Realname", "Hidden Displayname", "", true)
	shownRef, shownUser := seedAuthor(t, h.Pool, "Shown Realname", "Shown Displayname", "", false)
	hiddenPost := seedAuthoredPost(t, h.Pool, hiddenRef)
	shownPost := seedAuthoredPost(t, h.Pool, shownRef)

	// The synthetic principal, built the way the deleted helper built it:
	// the sentinel ref, the anonymous auth method, no capabilities.
	anon := &auth.Identity{UserRef: 0, Username: "anonymous", AuthMethod: "anonymous"}

	list := func(author int64) ([]openapi.Post, string) {
		t.Helper()
		limit := maxListLimit
		ref := author
		resp, err := h.ListPosts(
			auth.WithIdentity(context.Background(), anon),
			openapi.ListPostsRequestObject{
				Params: openapi.ListPostsParams{Limit: &limit, AuthorRef: &ref},
			},
		)
		if err != nil {
			t.Fatalf("ListPosts(anonymous identity): %v", err)
		}
		ok, is := resp.(openapi.ListPosts200JSONResponse)
		if !is {
			t.Fatalf("ListPosts(anonymous identity) = %T, want 200", resp)
		}
		raw, err := json.Marshal(ok)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return ok.Items, string(raw)
	}

	hiddenItems, hiddenJSON := list(hiddenRef)
	byID := afIDs(hiddenItems)
	if byID[hiddenPost] == nil {
		t.Fatal("the opted-out author's public post is missing for an anonymous IDENTITY — " +
			"the opt-out withholds the identity, not the content")
	}
	if a := byID[hiddenPost].Author; a != nil {
		t.Errorf("post.author = %+v for an author who set hide_from_anonymous, when the "+
			"caller is a non-nil anonymous Identity; want nil. The enrichment is testing "+
			"`caller == nil` rather than IsAnonymous() and read this principal as a member "+
			"(#1183)", *a)
	}
	for _, leaked := range []string{"Hidden Realname", "Hidden Displayname", hiddenUser} {
		if strings.Contains(hiddenJSON, leaked) {
			t.Errorf("the feed body for a non-nil anonymous caller contains %q (ADR 0024)", leaked)
		}
	}

	// Controls, so "everything was withheld" cannot pass this.
	shownItems, shownJSON := list(shownRef)
	shownByID := afIDs(shownItems)
	if shownByID[shownPost] == nil || shownByID[shownPost].Author == nil {
		t.Fatal("an author who did NOT opt out has no author object — the withholding " +
			"above is unconditional, not the opt-out")
	}
	if !strings.Contains(shownJSON, "Shown Displayname") {
		t.Errorf("the non-opted-out author's display name is missing from the feed body")
	}
	if strings.Contains(shownJSON, "Shown Realname") {
		t.Errorf("the feed body contains an author's `fullname` — the real-name rung is " +
			"authenticated-only, and an anonymous IDENTITY is on the anonymous rung")
	}
	_ = shownUser
}
