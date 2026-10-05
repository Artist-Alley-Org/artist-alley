// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1243 — A POST MEMBER CARRIES ITS OWN DECLARATION, NOT THE POST'S.
//
// # The gap this closes, and why the schema said otherwise
//
// `PostMember.asset` is `allOf: [$ref: Asset]`, so `ai_provenance` has
// been in that schema since #1167 and a reader checking the spec would
// conclude the value already reached the post route. It did not:
// `ListPostAssets` never selected the column and `memberToAsset` never
// mapped it, so every member of every post shipped with the field
// absent. The standalone `/assets/{id}` route carried it the whole time
// (`rowToAssetWithDetails`), which is exactly the shape that hides this
// kind of hole — one of two routes works, and the schema agrees with
// both.
//
// # ⭐ THE MIXED POST IS THE ASSERTION, and it is not the count
//
// ADR 0094's fourth amendment turns on a post whose members disagree:
//
//	> It should still show AI content in the post if the search returns
//	> it, just make sure it is labelled.
//
// `posts.ai_provenance` propagates a positive claim on ANY contributor,
// so a post holding one `generated` member beside one UNDECLARED member
// reads `generated` AT THE POST LEVEL. A client that labelled members
// from that value would mark the undeclared member as AI — a fabricated
// claim about a maker nobody asked, which is the single error decision 2
// of that ADR exists to prevent.
//
// So the case below plants exactly that post and asserts BOTH halves at
// once: the declared member says `generated` and the member beside it
// says NOTHING. "The AI member is labelled" passes on an implementation
// that stamps the post's value onto every member; only the pair of
// assertions separates them.
//
// `none` is the third member for the same reason. It is a DECLARATION
// (the maker states no generative AI) and must arrive intact rather than
// collapsing into the undeclared member's absence — the distinction the
// whole column exists to hold.
//
// Skips without AA_DB_PASSWORD (reuses the previewPool harness).

package posts

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// memberDecl reads back what one member's asset payload carries, as a
// pointer so ABSENT and a value are distinguishable — which is the whole
// point of the axis.
func memberDecl(t *testing.T, p *openapi.Post, assetID uuid.UUID) (*openapi.AssetAiProvenance, bool) {
	t.Helper()
	for _, m := range p.Members {
		if uuid.UUID(m.AssetId) != assetID {
			continue
		}
		if m.Asset == nil {
			t.Fatalf("member %s carries no asset object", assetID)
		}
		return m.Asset.AiProvenance, true
	}
	return nil, false
}

func wantDecl(t *testing.T, p *openapi.Post, assetID uuid.UUID, want string, why string) {
	t.Helper()
	got, found := memberDecl(t, p, assetID)
	if !found {
		t.Fatalf("%s: member %s missing from the payload", why, assetID)
	}
	if want == "" {
		if got != nil {
			t.Fatalf("%s: member %s carries %q, want ABSENT — undeclared is not a declaration",
				why, assetID, string(*got))
		}
		return
	}
	if got == nil {
		t.Fatalf("%s: member %s carries NO ai_provenance, want %q", why, assetID, want)
	}
	if string(*got) != want {
		t.Fatalf("%s: member %s carries %q, want %q", why, assetID, string(*got), want)
	}
}

// TestPostMember_CarriesItsOwnDeclaration is the labelling contract the
// viewer reads.
func TestPostMember_CarriesItsOwnDeclaration(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	gen := aiAsset(t, pool, "png", 1, "generated")
	undeclared := aiAsset(t, pool, "png", 1, "")
	declaredNone := aiAsset(t, pool, "png", 1, "none")
	assisted := aiAsset(t, pool, "png", 1, "assisted")
	post := aiPost(t, pool, aiAuthor, "public", gen, undeclared, declaredNone, assisted)

	resp, err := h.GetPost(ctxAs(aiAuthor), openapi.GetPostRequestObject{Id: post})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	ok, is := resp.(openapi.GetPost200JSONResponse)
	if !is {
		t.Fatalf("GetPost returned %T, want 200", resp)
	}
	got := openapi.Post(ok)

	// The post-level fact FIRST, because the member assertions are only
	// meaningful against it: this post says `generated`, and three of its
	// four members do not.
	if got.AiProvenance == nil || string(*got.AiProvenance) != "generated" {
		t.Fatalf("post-level ai_provenance = %v, want \"generated\" — the fixture is not the mixed post this test is about", got.AiProvenance)
	}

	wantDecl(t, &got, gen, "generated", "the declared member")
	wantDecl(t, &got, undeclared, "",
		"⭐ the UNDECLARED member beside it — the post says `generated` and this member must not")
	wantDecl(t, &got, declaredNone, "none",
		"⭐ the member that declared NO AI — a declaration, not an absence")
	wantDecl(t, &got, assisted, "assisted",
		"⭐ `assisted` survives as itself rather than collapsing into `generated`")
}

// TestPostMember_UndeclaredPostCarriesNothingAtAll is the negative wall.
//
// Every member undeclared: no member may carry a value and neither may
// the post. An implementation that defaulted the column to `none`
// anywhere — the seductive spelling, since the enum has a "no AI" term —
// passes the mixed case above and fails here, because it would be
// asserting a disclaimer on behalf of four makers nobody asked.
func TestPostMember_UndeclaredPostCarriesNothingAtAll(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	a := aiAsset(t, pool, "png", 1, "")
	b := aiAsset(t, pool, "png", 1, "")
	post := aiPost(t, pool, aiAuthor, "public", a, b)

	resp, err := h.GetPost(ctxAs(aiAuthor), openapi.GetPostRequestObject{Id: post})
	if err != nil {
		t.Fatalf("GetPost: %v", err)
	}
	ok, is := resp.(openapi.GetPost200JSONResponse)
	if !is {
		t.Fatalf("GetPost returned %T, want 200", resp)
	}
	got := openapi.Post(ok)

	if got.AiProvenance != nil {
		t.Fatalf("post-level ai_provenance = %q, want ABSENT", string(*got.AiProvenance))
	}
	wantDecl(t, &got, a, "", "an all-undeclared post's first member")
	wantDecl(t, &got, b, "", "an all-undeclared post's second member")
}

// TestPostMember_ListRouteCarriesItToo pins the OTHER entry point.
//
// The browse feed does not call GetPost; it pages `ListPosts` and
// hydrates each row through `fetchFullPost`. That shares `postRowToAPI`
// today, so one fix covers both — and this test is what notices if a
// later pass gives the list its own leaner mapper, which is exactly the
// divergence that produced this issue on the member payload.
func TestPostMember_ListRouteCarriesItToo(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)

	gen := aiAsset(t, pool, "png", 1, "generated")
	undeclared := aiAsset(t, pool, "png", 1, "")
	post := aiPost(t, pool, aiAuthor, "public", gen, undeclared)

	author := aiAuthor
	resp := aiFeedRaw(t, h, aiAuthor, "", "", &author)
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts returned %T, want 200", resp)
	}
	var found *openapi.Post
	for i := range ok.Items {
		if uuid.UUID(ok.Items[i].Id) == post {
			found = &ok.Items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("post %s absent from its own author's feed", post)
	}
	wantDecl(t, found, gen, "generated", "the list route's declared member")
	wantDecl(t, found, undeclared, "", "⭐ the list route's undeclared member")
}
