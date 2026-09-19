// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: `id:<uuid>` (`!list<uuid>,<uuid>,...`) on real
// rows, driven at the WIRE through the real handlers.
//
// # Why the wire and not the Engine
//
// Three things about this dimension are properties of the HTTP edge and
// cannot be seen from the Engine: that the 50-distinct-id bound is the
// SAME through `!list`, through a typed `id:` chain and through repeated
// `filter=id:`; that a request splitting its ids across `dsl=` and
// `filter=` is bounded on their union; and that a two-page walk over a
// 50-id list under the default page size neither drops nor repeats a
// row. So the cases below go through /search's ServeHTTP with a real
// Identity on the context, and the two suggestion endpoints through
// theirs.
//
// # Written against the surface
//
// On the commit before this sprint `!list…` was free text (an empty
// 200), `id:` an unknown field (a 400), and `filter=id:` an invalid
// filter (a 400). Every positive assertion here goes red there for one
// of those three reasons, and none of them is a compile failure.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

const (
	idOwner    int64 = 25020101
	idOther    int64 = 25020102 // owns one public asset, so contributors has two answers
	idStranger int64 = 25020103
)

// idPhrase is in every fixture title and nowhere else.
const idPhrase = "vexlorquint"

type idFixture struct {
	// public, owned by idOwner, in id order; 52 of them so 51 distinct
	// readable ids exist.
	public []uuid.UUID
	// owned by idOther, public.
	other uuid.UUID
	// restricted, owned by idOwner: a stranger cannot read it.
	restricted uuid.UUID
	// a public post and a public collection, both by idOwner.
	post, collection uuid.UUID
	// shared is BOTH an asset id and a post id.
	shared uuid.UUID
}

func idSeed(t *testing.T, pool *pgxpool.Pool) idFixture {
	t.Helper()
	ctx := context.Background()
	for _, u := range []struct {
		ref  int64
		name string
	}{{idOwner, "id-owner"}, {idOther, "id-other"}} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO "user" (ref, username) VALUES ($1, $2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			u.ref, u.name+"-"+uuid.NewString()[:8]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		ref := u.ref
		t.Cleanup(func() { testdb.Purge(t, pool, ref, `DELETE FROM "user" WHERE ref = $1`) })
	}
	asset := func(id uuid.UUID, owner int64, label, sensitivity string) uuid.UUID {
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, file_extension)
			VALUES ($1,$2,'fixture body',$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready','png')`,
			id, idPhrase+" "+label, owner, sensitivity); err != nil {
			t.Fatalf("seed asset %s: %v", label, err)
		}
		t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM assets WHERE id = $1`) })
		return id
	}
	var f idFixture
	for i := 0; i < 52; i++ {
		f.public = append(f.public, asset(uuid.New(), idOwner, "public", "public"))
	}
	sort.Slice(f.public, func(i, j int) bool { return f.public[i].String() < f.public[j].String() })
	f.other = asset(uuid.New(), idOther, "other", "public")
	f.restricted = asset(uuid.New(), idOwner, "restricted", "restricted")
	f.shared = asset(uuid.New(), idOwner, "shared", "public")

	post := func(id uuid.UUID, label string) uuid.UUID {
		if _, err := pool.Exec(ctx, `
			INSERT INTO posts (id, author_user_ref, title, description, visibility)
			VALUES ($1, $2, $3, $3, 'public')`, id, idOwner, idPhrase+" "+label); err != nil {
			t.Fatalf("seed post %s: %v", label, err)
		}
		t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM posts WHERE id = $1`) })
		return id
	}
	f.post = post(uuid.New(), "post")
	post(f.shared, "shared post")
	f.collection = uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility)
		VALUES ($1, $2, $3, $3, 'public')`, f.collection, idOwner, idPhrase+" collection"); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, f.collection, `DELETE FROM collections WHERE id = $1`) })
	return f
}

type idResponse struct {
	Status     int
	Body       map[string]any
	Hits       []struct{ Type, ID string }
	TotalCount int
	NextCursor string
}

// idGet runs one GET against the real /search handler as this identity
// (nil = anonymous) and decodes the response.
func idGet(t *testing.T, pool *pgxpool.Pool, id *auth.Identity, query url.Values) idResponse {
	t.Helper()
	h := &Handler{Service: NewService(NewEngine(pool), nil, nil), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/search?"+query.Encode(), nil)
	if id != nil {
		req = req.WithContext(auth.WithIdentity(req.Context(), id))
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	out := idResponse{Status: rr.Code}
	if err := json.Unmarshal(rr.Body.Bytes(), &out.Body); err != nil {
		t.Fatalf("decode %s: %v (%s)", query.Encode(), err, rr.Body.String())
	}
	if rr.Code != http.StatusOK {
		return out
	}
	var page struct {
		Hits       []struct{ Type, ID string } `json:"hits"`
		TotalCount int                         `json:"total_count"`
		NextCursor string                      `json:"next_cursor"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	out.Hits, out.TotalCount, out.NextCursor = page.Hits, page.TotalCount, page.NextCursor
	return out
}

func idOwnerIdentity() *auth.Identity {
	return &auth.Identity{UserRef: idOwner, AuthMethod: "session"}
}

func idHitIDs(r idResponse) []string {
	out := make([]string, 0, len(r.Hits))
	for _, h := range r.Hits {
		out = append(out, h.ID)
	}
	sort.Strings(out)
	return out
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

func idEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func idList(ids []uuid.UUID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id.String())
	}
	return "!list" + strings.Join(parts, ",")
}

func idChain(ids []uuid.UUID) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "id:"+id.String())
	}
	return strings.Join(parts, " AND ")
}

func idFilters(ids []uuid.UUID) url.Values {
	v := url.Values{}
	for _, id := range ids {
		v.Add("filter", "id:"+id.String())
	}
	return v
}

// ── membership ───────────────────────────────────────────────────────

func TestIDList_ExactMembership(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	owner := idOwnerIdentity()

	for _, c := range []struct {
		name string
		ids  []uuid.UUID
		want []uuid.UUID
	}{
		{"one id", f.public[:1], f.public[:1]},
		{"three ids, exactly three", f.public[:3], f.public[:3]},
		{"an unknown id among known ones changes nothing",
			append([]uuid.UUID{uuid.New()}, f.public[:2]...), f.public[:2]},
		{"an id from another owner is a member too", []uuid.UUID{f.public[0], f.other}, []uuid.UUID{f.public[0], f.other}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, spelling := range []string{idList(c.ids), idChain(c.ids)} {
				r := idGet(t, pool, owner, url.Values{"dsl": {spelling}, "types": {"asset"}, "limit": {"100"}})
				if r.Status != http.StatusOK {
					t.Fatalf("%q → %d %v", spelling, r.Status, r.Body)
				}
				if got, want := idHitIDs(r), idStrings(c.want); !idEqual(got, want) {
					t.Errorf("%q returned %v, want exactly %v", spelling, got, want)
				}
				if r.TotalCount != len(c.want) {
					t.Errorf("%q: total_count %d, want %d", spelling, r.TotalCount, len(c.want))
				}
			}
			// And the same set through the rail's spelling.
			q := idFilters(c.ids)
			q.Set("types", "asset")
			q.Set("limit", "100")
			r := idGet(t, pool, owner, q)
			if r.Status != http.StatusOK {
				t.Fatalf("filter=id: → %d %v", r.Status, r.Body)
			}
			if got, want := idHitIDs(r), idStrings(c.want); !idEqual(got, want) {
				t.Errorf("filter=id: returned %v, want exactly %v", got, want)
			}
		})
	}
}

func TestIDList_DuplicatesCollapseAndOrderDoesNotRank(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	owner := idOwnerIdentity()
	a, b := f.public[0], f.public[1]
	dup := idGet(t, pool, owner, url.Values{"dsl": {idList([]uuid.UUID{a, a, b, a})}, "types": {"asset"}})
	if dup.Status != http.StatusOK || dup.TotalCount != 2 || len(dup.Hits) != 2 {
		t.Errorf("duplicated ids: status %d, %d hits, count %d; want 200, 2, 2", dup.Status, len(dup.Hits), dup.TotalCount)
	}
	// The same two ids in the opposite order return the same order: the
	// engine ranks, the list does not.
	fwd := idGet(t, pool, owner, url.Values{"dsl": {idList([]uuid.UUID{a, b})}, "types": {"asset"}})
	rev := idGet(t, pool, owner, url.Values{"dsl": {idList([]uuid.UUID{b, a})}, "types": {"asset"}})
	if len(fwd.Hits) != 2 || len(rev.Hits) != 2 || fwd.Hits[0].ID != rev.Hits[0].ID || fwd.Hits[1].ID != rev.Hits[1].ID {
		t.Errorf("list order changed the result order: %v vs %v", fwd.Hits, rev.Hits)
	}
}

// TestIDList_VisibilitySplit: naming an id is not a right to read it,
// and the stranger's count does not move for the restricted member.
func TestIDList_VisibilitySplit(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	stranger := &auth.Identity{UserRef: idStranger, AuthMethod: "session"}
	ids := []uuid.UUID{f.public[0], f.restricted}

	asOwner := idGet(t, pool, idOwnerIdentity(), url.Values{"dsl": {idList(ids)}, "types": {"asset"}})
	if got := idHitIDs(asOwner); !idEqual(got, idStrings(ids)) || asOwner.TotalCount != 2 {
		t.Errorf("owner: %v / count %d, want both / 2", got, asOwner.TotalCount)
	}
	asStranger := idGet(t, pool, stranger, url.Values{"dsl": {idList(ids)}, "types": {"asset"}})
	if got := idHitIDs(asStranger); !idEqual(got, idStrings(ids[:1])) {
		t.Errorf("stranger: %v, want only the public id", got)
	}
	onlyPublic := idGet(t, pool, stranger, url.Values{"dsl": {idList(ids[:1])}, "types": {"asset"}})
	if asStranger.TotalCount != onlyPublic.TotalCount || asStranger.TotalCount != 1 {
		t.Errorf("stranger's count moved when the restricted id was added: %d vs %d (want 1 and 1)",
			asStranger.TotalCount, onlyPublic.TotalCount)
	}
	asAnon := idGet(t, pool, nil, url.Values{"dsl": {idList(ids)}, "types": {"asset"}})
	if asAnon.Status != http.StatusOK || asAnon.TotalCount != 1 {
		t.Errorf("anonymous: status %d count %d, want 200 / 1", asAnon.Status, asAnon.TotalCount)
	}
}

// TestIDList_EveryEntityAnswersOnItsOwnID: a post id and a collection id
// under mixed types return the post and the collection, and one UUID
// present in two tables returns both rows.
func TestIDList_EveryEntityAnswersOnItsOwnID(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	owner := idOwnerIdentity()

	mixed := idGet(t, pool, owner, url.Values{"dsl": {idList([]uuid.UUID{f.post, f.collection, f.public[0]})}})
	if mixed.Status != http.StatusOK {
		t.Fatalf("%d %v", mixed.Status, mixed.Body)
	}
	got := map[string]string{}
	for _, h := range mixed.Hits {
		got[h.ID] = h.Type
	}
	if got[f.post.String()] != "post" || got[f.collection.String()] != "collection" || got[f.public[0].String()] != "asset" || len(got) != 3 {
		t.Errorf("mixed-type list returned %v; want the post, the collection and the asset, each as itself", got)
	}

	shared := idGet(t, pool, owner, url.Values{"dsl": {idList([]uuid.UUID{f.shared})}})
	types := []string{}
	for _, h := range shared.Hits {
		if h.ID == f.shared.String() {
			types = append(types, h.Type)
		}
	}
	sort.Strings(types)
	if !idEqual(types, []string{"asset", "post"}) || shared.TotalCount != 2 {
		t.Errorf("one UUID in two tables returned %v (count %d); want an asset AND a post", types, shared.TotalCount)
	}
	// Asset-only: the same list contributes only the asset.
	assetOnly := idGet(t, pool, owner, url.Values{"dsl": {idList([]uuid.UUID{f.shared, f.post})}, "types": {"asset"}})
	if len(assetOnly.Hits) != 1 || assetOnly.Hits[0].Type != "asset" {
		t.Errorf("asset-only list returned %v; a post id contributes nothing", assetOnly.Hits)
	}
}

// ── cardinality, at all three entry paths ────────────────────────────

func TestIDList_CardinalityBoundIsOneBound(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	owner := idOwnerIdentity()

	for _, n := range []int{49, 50, 51} {
		ids := f.public[:n]
		wantStatus := http.StatusOK
		// 50 is facet.MaxIDTerms; spelled as a literal so this witness
		// compiles on the commit before the constant existed. The facet
		// unit tests pin the constant itself.
		if n > 50 {
			wantStatus = http.StatusBadRequest
		}
		for name, q := range map[string]url.Values{
			"!list":      {"dsl": {idList(ids)}, "types": {"asset"}, "limit": {"100"}},
			"id: chain":  {"dsl": {idChain(ids)}, "types": {"asset"}, "limit": {"100"}},
			"filter=id:": func() url.Values { v := idFilters(ids); v.Set("types", "asset"); v.Set("limit", "100"); return v }(),
			"filter+dsl": func() url.Values {
				v := idFilters(ids[:n/2])
				v.Set("dsl", idList(ids[n/2:]))
				v.Set("types", "asset")
				v.Set("limit", "100")
				return v
			}(),
		} {
			r := idGet(t, pool, owner, q)
			if r.Status != wantStatus {
				t.Errorf("%d ids via %s: status %d, want %d (%v)", n, name, r.Status, wantStatus, r.Body)
				continue
			}
			if wantStatus == http.StatusOK && (r.TotalCount != n || len(r.Hits) != n) {
				t.Errorf("%d ids via %s: %d hits, count %d; want exactly %d", n, name, len(r.Hits), r.TotalCount, n)
			}
			if wantStatus == http.StatusBadRequest {
				code, _ := r.Body["error"].(string)
				if code != "invalid_filter" && code != "dsl_error" {
					t.Errorf("%d ids via %s: error code %q, want invalid_filter or dsl_error", n, name, code)
				}
			}
		}
	}
	// >50 raw entries collapsing to <=50 distinct: accepted on every path.
	raw := append(append([]uuid.UUID{}, f.public[:50]...), f.public[0], f.public[1])
	for name, q := range map[string]url.Values{
		"!list":      {"dsl": {idList(raw)}, "types": {"asset"}, "limit": {"100"}},
		"filter=id:": func() url.Values { v := idFilters(raw); v.Set("types", "asset"); v.Set("limit", "100"); return v }(),
	} {
		r := idGet(t, pool, owner, q)
		if r.Status != http.StatusOK || r.TotalCount != 50 {
			t.Errorf("52 raw / 50 distinct via %s: status %d count %d, want 200 / 50", name, r.Status, r.TotalCount)
		}
	}
}

// TestIDList_TwoPageWalkIsExact: 50 readable ids under DefaultLimit walk
// two pages with the cursor and the union is the set, no duplicate, no
// gap.
func TestIDList_TwoPageWalkIsExact(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	owner := idOwnerIdentity()
	ids := f.public[:50]

	first := idGet(t, pool, owner, url.Values{"dsl": {idList(ids)}, "types": {"asset"}})
	if first.Status != http.StatusOK || len(first.Hits) != DefaultLimit || first.NextCursor == "" || first.TotalCount != 50 {
		t.Fatalf("page 1: status %d, %d hits, cursor %q, count %d; want 200, %d, non-empty, 50",
			first.Status, len(first.Hits), first.NextCursor, first.TotalCount, DefaultLimit)
	}
	second := idGet(t, pool, owner, url.Values{"dsl": {idList(ids)}, "types": {"asset"}, "cursor": {first.NextCursor}})
	if second.Status != http.StatusOK || len(second.Hits) != 25 {
		t.Fatalf("page 2: status %d, %d hits; want 200, 25", second.Status, len(second.Hits))
	}
	seen := map[string]int{}
	for _, h := range append(first.Hits, second.Hits...) {
		seen[h.ID]++
	}
	for _, id := range ids {
		if seen[id.String()] != 1 {
			t.Errorf("%s appeared %d times across two pages, want exactly once", id, seen[id.String()])
		}
	}
	if len(seen) != 50 {
		t.Errorf("union has %d ids, want 50", len(seen))
	}
	if second.NextCursor != "" {
		third := idGet(t, pool, owner, url.Values{"dsl": {idList(ids)}, "types": {"asset"}, "cursor": {second.NextCursor}})
		if len(third.Hits) != 0 {
			t.Errorf("a third page returned %d hits past the whole set", len(third.Hits))
		}
	}
}

// ── placement and malformed input, at the wire ───────────────────────

func TestIDList_PlacementAndMalformedAreRefusedAtTheWire(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	owner := idOwnerIdentity()
	a := f.public[0].String()
	for _, in := range []string{
		"NOT !list" + a, "NOT id:" + a, idPhrase + " OR !list" + a, idPhrase + " OR id:" + a,
		"NOT !nopreviews", "NOT preview:missing", idPhrase + " OR !nopreviews",
	} {
		r := idGet(t, pool, owner, url.Values{"dsl": {in}})
		if r.Status != http.StatusBadRequest {
			t.Errorf("dsl=%q → %d, want 400 (%v)", in, r.Status, r.Body)
			continue
		}
		if msg, _ := r.Body["message"].(string); !strings.Contains(msg, "top-level") {
			t.Errorf("dsl=%q refused for the wrong reason: %v", in, r.Body)
		}
	}
	for _, in := range []string{"!list", "!list:" + a, "!listjunk", "!list" + a + ",,", "!bogus", "!last5"} {
		r := idGet(t, pool, owner, url.Values{"dsl": {in}})
		if r.Status != http.StatusBadRequest || r.Body["error"] != "dsl_error" {
			t.Errorf("dsl=%q → %d %v, want 400 dsl_error", in, r.Status, r.Body)
		}
	}
	// Accepted composition: a disjunction beside the list. (Both words
	// are in the row's title: the executed text is the compiled
	// FreeText, which joins words, a property this sprint does not touch.)
	r := idGet(t, pool, owner, url.Values{"dsl": {"(" + idPhrase + " OR public) AND !list" + a}, "types": {"asset"}})
	if r.Status != http.StatusOK || r.TotalCount != 1 {
		t.Errorf("(x OR y) AND !list → %d count %d, want 200 / 1", r.Status, r.TotalCount)
	}
}

// ── the two suggestion endpoints ─────────────────────────────────────

func TestSuggestionEndpoints_PropagateTheNewDimensions(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	pv := pvSeed(t, pool)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	owner := idOwnerIdentity()

	// /search/facets?dsl=<phrase> AND !nopreviews&facets=extension counts
	// only the missing rows: for the owner that is 2 png + 1 txt.
	fh := &FacetHandler{Dispatcher: facet.NewDispatcher(pool, logger), Logger: logger}
	req := httptest.NewRequest(http.MethodGet, "/search/facets?"+url.Values{
		"dsl": {pvPhrase + " AND !nopreviews"}, "facets": {"extension"}}.Encode(), nil)
	pvOwnerRef := pvOwner
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{UserRef: pvOwnerRef, AuthMethod: "session"}))
	rr := httptest.NewRecorder()
	fh.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/search/facets → %d %s", rr.Code, rr.Body.String())
	}
	var fresp struct {
		Facets map[string]struct {
			Buckets []struct {
				Value string `json:"value"`
				Count int64  `json:"count"`
			} `json:"buckets"`
		} `json:"facets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &fresp); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, b := range fresp.Facets["extension"].Buckets {
		counts[b.Value] = b.Count
	}
	if counts["png"] != 2 || counts["txt"] != 1 || counts["mp4"] != 0 || counts["bin"] != 0 || len(counts) != 2 {
		t.Errorf("/search/facets under !nopreviews counted %v; want png 2, txt 1 and nothing else "+
			"(the fixture's mp4 has a poster and its bin was never previewable)", counts)
	}
	_ = pv

	// /search/contributors?dsl=!list<a>,<b> lists only those rows' owners:
	// one asset by idOwner and one by idOther → exactly two contributors.
	ch := &ContributorsHandler{Pool: pool, Logger: logger}
	creq := httptest.NewRequest(http.MethodGet, "/search/contributors?"+url.Values{
		"dsl": {idList([]uuid.UUID{f.public[0], f.other})}}.Encode(), nil)
	creq = creq.WithContext(auth.WithIdentity(creq.Context(), owner))
	crr := httptest.NewRecorder()
	ch.ServeHTTP(crr, creq)
	if crr.Code != http.StatusOK {
		t.Fatalf("/search/contributors → %d %s", crr.Code, crr.Body.String())
	}
	var cresp struct {
		Contributors []struct {
			UserRef int64 `json:"user_ref"`
		} `json:"contributors"`
	}
	if err := json.Unmarshal(crr.Body.Bytes(), &cresp); err != nil {
		t.Fatal(err)
	}
	refs := []int64{}
	for _, c := range cresp.Contributors {
		refs = append(refs, c.UserRef)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	if len(refs) != 2 || refs[0] != idOwner || refs[1] != idOther {
		t.Errorf("/search/contributors under !list returned %v, want exactly [%d %d]", refs, idOwner, idOther)
	}
	// Narrow the list to one owner's asset and the other owner leaves.
	creq2 := httptest.NewRequest(http.MethodGet, "/search/contributors?"+url.Values{
		"dsl": {idList([]uuid.UUID{f.other})}}.Encode(), nil)
	creq2 = creq2.WithContext(auth.WithIdentity(creq2.Context(), owner))
	crr2 := httptest.NewRecorder()
	ch.ServeHTTP(crr2, creq2)
	var cresp2 struct {
		Contributors []struct {
			UserRef int64 `json:"user_ref"`
		} `json:"contributors"`
	}
	if err := json.Unmarshal(crr2.Body.Bytes(), &cresp2); err != nil {
		t.Fatal(err)
	}
	if len(cresp2.Contributors) != 1 || cresp2.Contributors[0].UserRef != idOther {
		t.Errorf("/search/contributors under a one-id list returned %v, want only %d", cresp2.Contributors, idOther)
	}
}

// TestNavSearch_QIsStillPlainText is preservation: the nav bar's `q=`
// never reaches the parser, so a verb typed there is a word.
func TestNavSearch_QIsStillPlainText(t *testing.T) {
	pool := coPool(t)
	f := idSeed(t, pool)
	r := idGet(t, pool, idOwnerIdentity(), url.Values{"q": {"!nopreviews"}, "types": {"asset"}})
	if r.Status != http.StatusOK {
		t.Fatalf("q=!nopreviews → %d %v", r.Status, r.Body)
	}
	for _, h := range r.Hits {
		for _, id := range f.public {
			if h.ID == id.String() {
				t.Errorf("q=!nopreviews returned a fixture row; the nav bar's q is plain text, not a verb")
			}
		}
	}
}
