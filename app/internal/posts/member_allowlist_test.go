// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #883 — the post-member allow-list, asserted on the SERIALIZED
// RESPONSE.
//
// A component test would pass while the API still shipped the data, and
// the API is what a determined caller reads. So these tests marshal the
// PostMember exactly as the HTTP layer would and enumerate the JSON keys
// present.
//
// ALLOW-LIST, NOT DENY-LIST. The assertion is that the key set is a
// SUBSET of what is permitted — not that a list of known-bad names is
// absent. A deny-list fails open on the first field someone adds to
// `Asset` later, which is precisely how the free-form `config` blob
// leaked SSO secrets in v0.8.0.
//
// Skips without AA_DB_PASSWORD.

package posts

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// memberAllowList is the COMPLETE set of JSON keys a restricted post
// member may carry.
//
//	asset_id, sort_order  — the post_assets row's own columns. Not the
//	                        asset's: they say where a slot is in the
//	                        carousel, and #881 needs asset_id to name
//	                        what access is being requested.
//	restricted            — the marker that makes the placeholder
//	                        legible as a placeholder.
//	owner_display_name    — the ONE asset-derived value the owner's rule
//	                        permits ("only the owner's name").
//
// Nothing from the `assets` table crosses this boundary. If a field is
// added to PostMember and it belongs on a placeholder, it is a
// deliberate decision recorded HERE, not a test that quietly widened.
var memberAllowList = map[string]bool{
	"asset_id":           true,
	"sort_order":         true,
	"restricted":         true,
	"owner_display_name": true,
}

// jsonKeys marshals a value the way the HTTP layer will and returns the
// top-level keys actually present on the wire. `omitempty` is what makes
// "absent" mean absent, so reading the struct fields instead of the JSON
// would test the wrong thing.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func findMember(t *testing.T, p *openapi.Post, assetID uuid.UUID) openapi.PostMember {
	t.Helper()
	for _, m := range p.Members {
		if uuid.UUID(m.AssetId) == assetID {
			return m
		}
	}
	t.Fatalf("asset %v is not in the member array — a restricted member must be PRESENT "+
		"as a placeholder, never dropped (#883: dropping it hides that a restriction "+
		"exists and breaks the request-access affordance #881 hangs off)", assetID)
	return openapi.PostMember{}
}

// fattenAsset gives the seeded asset something on every field a leak
// could travel through, so a test that passes is passing against a row
// that HAS secrets rather than one that happens to be empty.
func fattenAsset(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		UPDATE assets
		   SET description = $2, file_size_bytes = $3, file_extension = $4,
		       metadata = $5, thumbhash = $6
		 WHERE id = $1`,
		id,
		"UNRELEASED character sheet — do not distribute",
		987654,
		"psd",
		[]byte(`{"exif":{"Artist":"J. Doe","GPSLatitude":47.6}}`),
		[]byte{0xde, 0xad, 0xbe, 0xef, 0x01, 0x02},
	); err != nil {
		t.Fatalf("fatten asset: %v", err)
	}
}

// TestPostMember_RestrictedIsAllowListed is the core assertion.
func TestPostMember_RestrictedIsAllowListed(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	restricted := seedPreviewAsset(t, pool, "restricted", true)
	fattenAsset(t, pool, restricted)
	pub := seedPreviewAsset(t, pool, "public", true)
	postID := seedPreviewPost(t, pool, restricted, pub)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	for _, c := range []struct {
		name string
		ctx  context.Context
	}{
		{"anonymous", context.Background()},
		{"authenticated stranger", ctxAs(peStranger)},
	} {
		p, err := h.fetchFullPost(ctx, pgID)
		if err != nil {
			t.Fatalf("%s: fetch: %v", c.name, err)
		}
		if err := h.enrichPreview(c.ctx, p); err != nil {
			t.Fatalf("%s: enrich: %v", c.name, err)
		}

		m := findMember(t, p, restricted)
		if !m.Restricted {
			t.Fatalf("%s: restricted member did not report restricted=true", c.name)
		}
		if m.Asset != nil {
			raw, _ := json.Marshal(m.Asset)
			t.Errorf("%s: the whole asset object shipped on a restricted member: %s", c.name, raw)
		}
		for _, k := range jsonKeys(t, m) {
			if !memberAllowList[k] {
				raw, _ := json.Marshal(m)
				t.Errorf("%s: key %q is NOT on the #883 allow-list. Full payload: %s\n"+
					"If this field genuinely belongs on a placeholder, add it to "+
					"memberAllowList with a reason; if it does not, stop sending it.",
					c.name, k, raw)
			}
		}

		// The other member is untouched — a test that redacts everything
		// would also pass the loop above.
		other := findMember(t, p, pub)
		if other.Restricted {
			t.Errorf("%s: a PUBLIC member was marked restricted", c.name)
		}
		if other.Asset == nil {
			t.Errorf("%s: a public member lost its asset payload", c.name)
		} else if other.Asset.Title == nil || *other.Asset.Title == "" {
			t.Errorf("%s: a public member shipped an empty title — the redaction is too wide", c.name)
		}
	}
}

// TestPostMember_OwnerSeesEverything is the other half: the rule must
// not cost the owner their own post.
func TestPostMember_OwnerSeesEverything(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	restricted := seedPreviewAsset(t, pool, "restricted", true)
	fattenAsset(t, pool, restricted)
	postID := seedPreviewPost(t, pool, restricted)

	p, err := h.fetchFullPost(ctx, pgtype.UUID{Bytes: postID, Valid: true})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichPreview(ctxAs(pePostOwner), p); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	m := findMember(t, p, restricted)
	if m.Restricted {
		t.Fatal("the OWNER was shown a placeholder for their own restricted asset")
	}
	if m.Asset == nil || vOf(m.Asset.Title) == "" {
		t.Fatal("the owner lost the asset payload for their own restricted member")
	}
	if !vOf(m.Asset.PreviewAvailable) {
		t.Error("the owner's restricted member reported preview_available=false")
	}
}

// TestPostMember_RedactionDoesNotPoisonTheCache is the #471 trap one
// step on. The full Post is cached cross-caller by id and PostMember.Asset
// is now a POINTER into that cached object, so an enrich that mutated
// through it — or that wrote the placeholder into the cached slice —
// would serve one caller's redaction to the next, or worse, serve the
// owner's full payload to a stranger.
func TestPostMember_RedactionDoesNotPoisonTheCache(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	restricted := seedPreviewAsset(t, pool, "restricted", true)
	fattenAsset(t, pool, restricted)
	postID := seedPreviewPost(t, pool, restricted)
	pgID := pgtype.UUID{Bytes: postID, Valid: true}

	// Stranger first: redacts.
	p1, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichPreview(ctxAs(peStranger), p1); err != nil {
		t.Fatalf("enrich stranger: %v", err)
	}
	if !findMember(t, p1, restricted).Restricted {
		t.Fatal("precondition: the stranger should have been given a placeholder")
	}

	// The CACHED post must still carry the full member — the redaction is
	// per-request, and a cache that stored it would hide the asset from
	// its own owner on the next read.
	cached, ok := h.byID.Get(postID.String())
	if !ok {
		t.Fatal("post was not cached; test cannot verify isolation")
	}
	if len(cached.Members) != 1 || cached.Members[0].Restricted || cached.Members[0].Asset == nil {
		t.Fatal("the stranger's redaction was written into the cross-caller cache")
	}

	// Owner second, served off that cache: full payload.
	p2, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch2: %v", err)
	}
	if err := h.enrichPreview(ctxAs(pePostOwner), p2); err != nil {
		t.Fatalf("enrich owner: %v", err)
	}
	if findMember(t, p2, restricted).Restricted {
		t.Fatal("the owner was served the stranger's placeholder from the cache")
	}

	// And back the other way — the owner's enrich must not leave the full
	// asset visible to the next stranger.
	p3, err := h.fetchFullPost(ctx, pgID)
	if err != nil {
		t.Fatalf("fetch3: %v", err)
	}
	if err := h.enrichPreview(ctxAs(peStranger), p3); err != nil {
		t.Fatalf("enrich stranger 2: %v", err)
	}
	if !findMember(t, p3, restricted).Restricted {
		t.Fatal("LEAK: after the owner read the post, a stranger saw the full member")
	}
}

// TestPostMember_AnonymousDraftMember covers the row plane specifically:
// a PUBLIC-tier asset that is still a draft is not visible standalone to
// an anonymous caller, so it must not become visible by being pinned in
// a public post. This is the case a content-plane-only gate would miss —
// ContentReadable says yes to `public` regardless of publication state.
func TestPostMember_AnonymousDraftMember(t *testing.T) {
	pool := previewPool(t)
	h := peHandler(pool)
	ctx := context.Background()

	draft := seedPreviewAsset(t, pool, "public", true)
	if _, err := pool.Exec(ctx, `UPDATE assets SET status = 'draft' WHERE id = $1`, draft); err != nil {
		t.Fatalf("draft: %v", err)
	}
	fattenAsset(t, pool, draft)
	postID := seedPreviewPost(t, pool, draft)

	p, err := h.fetchFullPost(ctx, pgtype.UUID{Bytes: postID, Valid: true})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := h.enrichPreview(context.Background(), p); err != nil { // anonymous
		t.Fatalf("enrich: %v", err)
	}
	if !findMember(t, p, draft).Restricted {
		t.Error("an anonymous caller saw a DRAFT member of a public post in full — " +
			"the row plane is not being applied to members")
	}
}

// vOf dereferences an optional openapi field, returning the zero value
// for nil. openapi.Asset's fields became pointers in #899 so a withheld
// payload can omit them; these assertions only care about the value.
func vOf[T any](p *T) T {
	var z T
	if p == nil {
		return z
	}
	return *p
}
