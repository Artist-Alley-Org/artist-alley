// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25b: `last:N` (`!lastN`) on real rows, driven at the WIRE
// through the real handlers.
//
// # What a window is, driven as rows
//
// The N newest ELIGIBLE rows, globally across the requested entity
// types, inside which every other term narrows; ordered by recency
// (assets and collections on created_at, posts on posted_at), then id
// descending, then type ascending; paged on a recent cursor; counted
// exactly. Each test below seeds its own rows, dated in the FUTURE so
// they are the newest rows in the database whatever else a sibling test
// left behind, and purges them.
//
// # ⛔ Written against the surface
//
// Every request is a string through /search's ServeHTTP (idGet, which
// 25a left on `dev`) or the two suggestion handlers, and every symbol
// named here exists on d54714bb. On that commit `!last3` is an unknown
// verb and `last:3` an unknown field, both 400 dsl_error, so every
// positive assertion below goes red there on MEANING, not on a missing
// helper. The pure parts that name 25b symbols are in
// recent_unit_test.go for the reason keyset_fragment_test.go gives.
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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/vector"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	lwOwner    int64 = 25030101
	lwOther    int64 = 25030102
	lwStranger int64 = 25030103
	// lwPhrase is in every fixture title and nowhere else.
	lwPhrase = "brontalquiv"
)

// lwBase is a moment two days from now, microsecond-truncated (the
// column's precision), so fixture rows dated from it are newer than
// anything a sibling test inserts at NOW().
func lwBase() time.Time {
	return time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
}

func lwUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, u := range []struct {
		ref  int64
		name string
	}{{lwOwner, "lw-owner"}, {lwOther, "lw-other"}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO "user" (ref, username) VALUES ($1, $2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			u.ref, u.name+"-"+uuid.NewString()[:8]); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		ref := u.ref
		t.Cleanup(func() { testdb.Purge(t, pool, ref, `DELETE FROM "user" WHERE ref = $1`) })
	}
}

func lwAsset(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, owner int64, title, sensitivity, ext string, at time.Time) uuid.UUID {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, file_extension, created_at)
		VALUES ($1,$2,'fixture body',$3,(SELECT MIN(ref) FROM asset_types),'active',$4,'ready',$5,$6)`,
		id, lwPhrase+" "+title, owner, sensitivity, ext, at); err != nil {
		t.Fatalf("seed asset %s: %v", title, err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id,
			`DELETE FROM asset_tag WHERE asset_id = $1`,
			`DELETE FROM asset_embedding_d768 WHERE asset_id = $1`,
			`DELETE FROM assets WHERE id = $1`)
	})
	return id
}

func lwPost(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, author int64, title string, created, posted time.Time) uuid.UUID {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO posts (id, author_user_ref, title, description, visibility, created_at, posted_at)
		VALUES ($1, $2, $3, $3, 'public', $4, $5)`, id, author, lwPhrase+" "+title, created, posted); err != nil {
		t.Fatalf("seed post %s: %v", title, err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id,
			`DELETE FROM post_tags WHERE post_id = $1`,
			`DELETE FROM posts WHERE id = $1`)
	})
	return id
}

func lwCollection(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, owner int64, name string, at time.Time) uuid.UUID {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility, created_at)
		VALUES ($1, $2, $3, $3, 'public', $4)`, id, owner, lwPhrase+" "+name, at); err != nil {
		t.Fatalf("seed collection %s: %v", name, err)
	}
	t.Cleanup(func() { testdb.Purge(t, pool, id, `DELETE FROM collections WHERE id = $1`) })
	return id
}

func lwTag(t *testing.T, pool *pgxpool.Pool, table, col string, id uuid.UUID, tag string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO `+table+` (`+col+`, tag) VALUES ($1, $2)`, id, tag); err != nil {
		t.Fatalf("seed tag %s on %s: %v", tag, table, err)
	}
}

func lwOwnerID() *auth.Identity    { return &auth.Identity{UserRef: lwOwner, AuthMethod: "session"} }
func lwStrangerID() *auth.Identity { return &auth.Identity{UserRef: lwStranger, AuthMethod: "session"} }

// lwOrder renders a page as "type:id" in the order it was returned.
func lwOrder(r idResponse) []string {
	out := make([]string, 0, len(r.Hits))
	for _, h := range r.Hits {
		out = append(out, h.Type+":"+h.ID)
	}
	return out
}

func lwKey(typ string, id uuid.UUID) string { return typ + ":" + id.String() }

func lwSame(a, b []string) bool {
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

// lwRawHits decodes the page's hits with every key, for the fields
// idResponse.Hits does not carry (restricted, created_at).
func lwRawHits(t *testing.T, r idResponse) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(r.Body["hits"])
	if err != nil {
		t.Fatal(err)
	}
	var hits []map[string]any
	if err := json.Unmarshal(raw, &hits); err != nil {
		t.Fatal(err)
	}
	return hits
}

// lwGet is idGet with a real vector fetcher attached, so `similar_to:`
// resolves the way it does in production.
func lwGet(t *testing.T, pool *pgxpool.Pool, id *auth.Identity, query url.Values) idResponse {
	t.Helper()
	h := &Handler{
		Service: NewService(NewEngine(pool), nil, nil).WithVector(vector.NewFetcher(pool)),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
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

// ── A. global, not per arm ───────────────────────────────────────────

func TestLastWindow_IsGlobalNotPerArm(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	a1 := lwAsset(t, pool, uuid.New(), lwOwner, "A1", "public", "png", b.Add(4*time.Second))
	p1 := lwPost(t, pool, uuid.New(), lwOwner, "P1", b.Add(3*time.Second), b.Add(3*time.Second))
	a2 := lwAsset(t, pool, uuid.New(), lwOwner, "A2", "public", "png", b.Add(2*time.Second))
	p2 := lwPost(t, pool, uuid.New(), lwOwner, "P2", b.Add(1*time.Second), b.Add(1*time.Second))
	_ = p2

	for _, spelling := range []string{"!last3", "last:3"} {
		r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {spelling}, "types": {"asset,post"}})
		if r.Status != http.StatusOK {
			t.Fatalf("%q → %d %v", spelling, r.Status, r.Body)
		}
		want := []string{lwKey("asset", a1), lwKey("post", p1), lwKey("asset", a2)}
		if got := lwOrder(r); !lwSame(got, want) {
			t.Errorf("%q over asset+post returned %v, want exactly %v in that order (global, not 3 per arm)", spelling, got, want)
		}
		if r.TotalCount != 3 || r.Body["total_count_capped"] != false {
			t.Errorf("%q: total_count %d capped %v, want 3 / false", spelling, r.TotalCount, r.Body["total_count_capped"])
		}
	}
	one := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last1"}, "types": {"asset,post"}})
	if got := lwOrder(one); !lwSame(got, []string{lwKey("asset", a1)}) || one.TotalCount != 1 {
		t.Errorf("!last1 returned %v (count %d), want exactly A1", got, one.TotalCount)
	}
	// Requested types define the arms: posts alone are the two posts.
	posts := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last2"}, "types": {"post"}})
	if got := lwOrder(posts); !lwSame(got, []string{lwKey("post", p1), lwKey("post", p2)}) {
		t.Errorf("!last2 over posts returned %v", got)
	}
	// No types: the all-types population, and a newer collection takes
	// a slot.
	c0 := lwCollection(t, pool, uuid.New(), lwOwner, "C0", b.Add(5*time.Second))
	all := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3"}})
	if got := lwOrder(all); !lwSame(got, []string{lwKey("collection", c0), lwKey("asset", a1), lwKey("post", p1)}) {
		t.Errorf("!last3 with no types returned %v", got)
	}
	// Anonymous, every arm public: the same three, through the three
	// anonymous baselines.
	anon := lwGet(t, pool, nil, url.Values{"dsl": {"!last3"}})
	if anon.Status != http.StatusOK || !lwSame(lwOrder(anon), lwOrder(all)) {
		t.Errorf("anonymous !last3 → %d %v, want the owner's three public rows", anon.Status, lwOrder(anon))
	}
}

// ── B. narrowing happens inside the window ───────────────────────────

func TestLastWindow_TextNarrowsInsideTheWindow(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	lwAsset(t, pool, uuid.New(), lwOwner, "A1", "public", "png", b.Add(4*time.Second))
	a2 := lwAsset(t, pool, uuid.New(), lwOwner, "A2 quillopax", "public", "png", b.Add(3*time.Second))
	lwAsset(t, pool, uuid.New(), lwOwner, "A3", "public", "png", b.Add(2*time.Second))
	p2 := lwPost(t, pool, uuid.New(), lwOwner, "P2 zebrantik", b.Add(1*time.Second), b.Add(1*time.Second))

	// The word is findable at all: without the window, P2 is a hit.
	plain := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"zebrantik"}, "types": {"asset,post"}})
	if got := lwOrder(plain); !lwSame(got, []string{lwKey("post", p2)}) {
		t.Fatalf("zebrantik alone returned %v; the fixture is wrong", got)
	}
	for _, spelling := range []string{"!last3 zebrantik", "zebrantik AND last:3", "last:3 AND zebrantik"} {
		r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {spelling}, "types": {"asset,post"}})
		if r.Status != http.StatusOK || len(r.Hits) != 0 || r.TotalCount != 0 {
			t.Errorf("%q → %d, %d hits, count %d; the word is only in the fourth-newest row, so the window must return nothing",
				spelling, r.Status, len(r.Hits), r.TotalCount)
		}
	}
	in := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3 quillopax"}, "types": {"asset,post"}})
	if got := lwOrder(in); !lwSame(got, []string{lwKey("asset", a2)}) || in.TotalCount != 1 {
		t.Errorf("!last3 quillopax returned %v (count %d), want exactly A2", got, in.TotalCount)
	}
	// And a typed dimension narrows inside too: the extension of the
	// three in the window.
	ext := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3"}, "filter": {"extension:jpg"}, "types": {"asset,post"}})
	if len(ext.Hits) != 0 || ext.TotalCount != 0 {
		t.Errorf("!last3 with extension:jpg returned %v; nothing in the window is a jpg", lwOrder(ext))
	}
}

// ── C. the field plane ───────────────────────────────────────────────

func TestLastWindow_RestrictedRowConsumesNoSlotForAStranger(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	restricted := lwAsset(t, pool, uuid.New(), lwOwner, "R", "restricted", "png", b.Add(4*time.Second))
	pub1 := lwAsset(t, pool, uuid.New(), lwOwner, "pub1", "public", "png", b.Add(3*time.Second))
	pub2 := lwAsset(t, pool, uuid.New(), lwOwner, "pub2", "public", "png", b.Add(2*time.Second))
	pub3 := lwAsset(t, pool, uuid.New(), lwOwner, "pub3", "public", "png", b.Add(1*time.Second))

	// Positive control, from the two authorities themselves: the ROW
	// plane admits R for the stranger (it is the row a placeholder
	// surface would list, ADR 0064) and the FIELD plane refuses it (the
	// placeholder is withheld). So R's absence from the stranger's
	// window below is the field plane at work, not an invisible row.
	//
	// ⚠️ Not /search's own text path: since #902 the text MATCH is
	// gated on the field plane (visibility.AssetSearchMatchSQL), so a
	// stranger's `brontalquiv` search never lists R at all, placeholder
	// or otherwise. The placeholder surfaces are browse and the hybrid
	// enrich pass; the brief's "unfiltered /search shows R as a
	// placeholder" premise is recorded in the handoff as not holding.
	strangerCaller := visibility.NewCaller(func() *int64 { r := lwStranger; return &r }())
	rowPlane, err := visibility.Filter(context.Background(), visibility.EntityAsset, strangerCaller)
	if err != nil {
		t.Fatal(err)
	}
	frag, args := rowPlane.ToSQL("", 1)
	var listed bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM assets WHERE id = $1`+frag+`)`,
		append([]any{restricted}, args...)...).Scan(&listed); err != nil {
		t.Fatal(err)
	}
	if !listed {
		t.Fatalf("the row plane does not admit R for the stranger; the fixture is wrong")
	}
	fr, _, err := visibility.LoadFieldsRow(context.Background(), pool, strangerCaller, restricted, visibility.AssetMutationCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if visibility.FieldsReadable(fr, strangerCaller, nil) {
		t.Fatal("the field plane admits R for the stranger; the fixture is wrong")
	}

	stranger := lwGet(t, pool, lwStrangerID(), url.Values{"dsl": {"!last3"}, "types": {"asset"}})
	want := []string{lwKey("asset", pub1), lwKey("asset", pub2), lwKey("asset", pub3)}
	if got := lwOrder(stranger); !lwSame(got, want) || stranger.TotalCount != 3 {
		t.Errorf("stranger's !last3 returned %v (count %d), want the three public rows: a withheld row consumes no slot", got, stranger.TotalCount)
	}
	for _, h := range lwRawHits(t, stranger) {
		if h["restricted"] == true {
			t.Errorf("stranger's window carried a withheld placeholder: %v", h)
		}
	}
	owner := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3"}, "types": {"asset"}})
	if got := lwOrder(owner); !lwSame(got, []string{lwKey("asset", restricted), lwKey("asset", pub1), lwKey("asset", pub2)}) {
		t.Errorf("owner's !last3 returned %v; R takes the newest slot for its owner", got)
	}
	// And anonymous, in public mode: the public rows only.
	anon := lwGet(t, pool, nil, url.Values{"dsl": {"!last3"}, "types": {"asset"}})
	if anon.Status != http.StatusOK || !lwSame(lwOrder(anon), want) {
		t.Errorf("anonymous !last3 → %d %v", anon.Status, lwOrder(anon))
	}
}

// ── D. a post's clock is posted_at ───────────────────────────────────

func TestLastWindow_PostOrdersByPostedAtAndReportsCreatedAt(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	// X was created last and back-dated to first; Y created early and
	// posted later; an asset sits between them.
	x := lwPost(t, pool, uuid.New(), lwOwner, "X", b.Add(10*time.Second), b.Add(1*time.Second))
	y := lwPost(t, pool, uuid.New(), lwOwner, "Y", b.Add(2*time.Second), b.Add(5*time.Second))
	a := lwAsset(t, pool, uuid.New(), lwOwner, "A", "public", "png", b.Add(3*time.Second))

	r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3"}, "types": {"asset,post"}})
	if got := lwOrder(r); !lwSame(got, []string{lwKey("post", y), lwKey("asset", a), lwKey("post", x)}) {
		t.Errorf("order %v; posts must rank on posted_at (Y 5s, A 3s, X 1s), not created_at", got)
	}
	for _, h := range lwRawHits(t, r) {
		if h["id"] != x.String() {
			continue
		}
		got, err := time.Parse(time.RFC3339Nano, h["created_at"].(string))
		if err != nil || !got.Equal(b.Add(10*time.Second)) {
			t.Errorf("X's created_at on the wire is %v (%v), want its stored created_at %v, not its posted_at", h["created_at"], err, b.Add(10*time.Second))
		}
	}
	// The inverse: last:1 is Y, whose created_at is the OLDEST of the
	// three.
	one := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last1"}, "types": {"asset,post"}})
	if got := lwOrder(one); !lwSame(got, []string{lwKey("post", y)}) {
		t.Errorf("!last1 returned %v, want Y", got)
	}
}

// ── E. the count is exact inside the window ──────────────────────────

func TestLastWindow_CountIsExactAndUncapped(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	for i := 0; i < 5; i++ {
		lwAsset(t, pool, uuid.New(), lwOwner, "A", "public", "png", b.Add(time.Duration(i)*time.Second))
	}
	lwAsset(t, pool, uuid.New(), lwOwner, "A quillopax", "public", "png", b.Add(9*time.Second))
	for _, c := range []struct {
		dsl  string
		want int
	}{
		{"!last2", 2}, {"!last6", 6}, {"!last3 quillopax", 1}, {"!last1 quillopax", 1},
	} {
		r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {c.dsl}, "types": {"asset"}, "limit": {"1"}})
		if r.Status != http.StatusOK || r.TotalCount != c.want || r.Body["total_count_capped"] != false {
			t.Errorf("%q: status %d count %d capped %v, want 200 / %d / false", c.dsl, r.Status, r.TotalCount, r.Body["total_count_capped"], c.want)
		}
	}
	// A window wider than the eligible corpus counts the corpus: at
	// least this fixture's six, never more than N, never capped. (The
	// shared test database may hold other packages' rows.)
	wide := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last100"}, "types": {"asset"}, "limit": {"1"}})
	if wide.Status != http.StatusOK || wide.TotalCount < 6 || wide.TotalCount > 100 || wide.Body["total_count_capped"] != false {
		t.Errorf("!last100: status %d count %d capped %v", wide.Status, wide.TotalCount, wide.Body["total_count_capped"])
	}
}

// ── F. placement and validation, at the wire ─────────────────────────

func TestLastWindow_PlacementAndValidationAtTheWire(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	lwAsset(t, pool, uuid.New(), lwOwner, "A", "public", "png", lwBase())
	owner := lwOwnerID()

	for _, in := range []string{"NOT !last3", "NOT last:3", lwPhrase + " OR !last3", lwPhrase + " OR last:3", "(x OR !last3) AND y"} {
		r := lwGet(t, pool, owner, url.Values{"dsl": {in}})
		if r.Status != http.StatusBadRequest || r.Body["error"] != "dsl_error" {
			t.Errorf("dsl=%q → %d %v, want 400 dsl_error", in, r.Status, r.Body)
			continue
		}
		if msg, _ := r.Body["message"].(string); !strings.Contains(msg, "top-level") {
			t.Errorf("dsl=%q refused for the wrong reason: %v", in, r.Body)
		}
	}
	for _, in := range []string{
		"!last", "!last 5", "!last0", "!last10001", "!last-1", "!lastx", "!last5x", "!last:5",
		"last:0", "last:10001", "last:abc", "last:3 AND last:5", "!last3 !last5", "!last3 AND last:5",
	} {
		r := lwGet(t, pool, owner, url.Values{"dsl": {in}})
		if r.Status != http.StatusBadRequest || r.Body["error"] != "dsl_error" {
			t.Errorf("dsl=%q → %d %v, want 400 dsl_error", in, r.Status, r.Body)
		}
	}
	// An empty value is the parser's own refusal, the one every
	// `field:` with nothing after the colon gets.
	if r := lwGet(t, pool, owner, url.Values{"dsl": {"last:"}}); r.Status != http.StatusBadRequest {
		t.Errorf("dsl=last: → %d %v, want 400", r.Status, r.Body)
	}
	for name, q := range map[string]url.Values{
		"filter=last:3&filter=last:5": {"filter": {"last:3", "last:5"}},
		"dsl=!last3&filter=last:5":    {"dsl": {"!last3"}, "filter": {"last:5"}},
		"filter=last:0":               {"filter": {"last:0"}},
		"filter=last:10001":           {"filter": {"last:10001"}},
		"filter=last:abc":             {"filter": {"last:abc"}},
		"filter=last:":                {"filter": {"last:"}},
	} {
		r := lwGet(t, pool, owner, q)
		code, _ := r.Body["error"].(string)
		if r.Status != http.StatusBadRequest || (code != "invalid_filter" && code != "dsl_error") {
			t.Errorf("%s → %d %v, want 400", name, r.Status, r.Body)
		}
	}
	for name, q := range map[string]url.Values{
		"!last1":                       {"dsl": {"!last1"}},
		"!last10000":                   {"dsl": {"!last10000"}},
		"last:1":                       {"dsl": {"last:1"}},
		"last:10000":                   {"dsl": {"last:10000"}},
		"filter=last:5":                {"filter": {"last:5"}},
		"dsl=!last3&filter=last:3":     {"dsl": {"!last3"}, "filter": {"last:3"}},
		"dsl=!last05&filter=last:5":    {"dsl": {"!last05"}, "filter": {"last:5"}},
		"(x OR y) AND !last3":          {"dsl": {"(" + lwPhrase + " OR fixture) AND !last3"}},
		"!last3 AND extension:png":     {"dsl": {"!last3 AND extension:png"}},
		"!last3 !nopreviews":           {"dsl": {"!last3 !nopreviews"}},
		"nav q is plain text":          {"q": {"!last3"}},
		"legal top-level, both verbs":  {"dsl": {"!last3 AND !nopreviews AND " + lwPhrase}},
		"typed and alias, same window": {"dsl": {"last:3"}, "filter": {"last:3"}},
	} {
		r := lwGet(t, pool, owner, q)
		if r.Status != http.StatusOK {
			t.Errorf("%s → %d %v, want 200", name, r.Status, r.Body)
		}
	}
}

// ── G. a same-key tie across two tables walks without a duplicate ────

func TestLastWindow_CursorSameKeyTie(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	at := lwBase()
	shared := uuid.New()
	lwAsset(t, pool, shared, lwOwner, "shared", "public", "png", at)
	lwPost(t, pool, shared, lwOwner, "shared post", at, at)

	first := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last2"}, "types": {"asset,post"}, "limit": {"1"}})
	if first.Status != http.StatusOK || !lwSame(lwOrder(first), []string{lwKey("asset", shared)}) || first.NextCursor == "" || first.TotalCount != 2 {
		t.Fatalf("page 1: %d %v cursor %q count %d; want the asset (type ASC), a cursor, count 2", first.Status, lwOrder(first), first.NextCursor, first.TotalCount)
	}
	second := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last2"}, "types": {"asset,post"}, "limit": {"1"}, "cursor": {first.NextCursor}})
	if second.Status != http.StatusOK || !lwSame(lwOrder(second), []string{lwKey("post", shared)}) {
		t.Fatalf("page 2: %d %v; want the post, no duplicate, no gap", second.Status, lwOrder(second))
	}
	if second.NextCursor != "" {
		third := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last2"}, "types": {"asset,post"}, "limit": {"1"}, "cursor": {second.NextCursor}})
		if len(third.Hits) != 0 {
			t.Errorf("a third page returned %v past the whole window", lwOrder(third))
		}
	}
}

// ── ties within one table ────────────────────────────────────────────

func TestLastWindow_TiesOrderByIDDescAcrossCallsAndPages(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	at := lwBase()
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range ids {
		lwAsset(t, pool, id, lwOwner, "tie", "public", "png", at)
		_ = i
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() > ids[j].String() })
	want := []string{lwKey("asset", ids[0]), lwKey("asset", ids[1]), lwKey("asset", ids[2])}
	for call := 0; call < 2; call++ {
		r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3"}, "types": {"asset"}})
		if got := lwOrder(r); !lwSame(got, want) {
			t.Errorf("call %d: identical timestamps ordered %v, want id DESC %v", call, got, want)
		}
	}
	walk := []string{}
	q := url.Values{"dsl": {"!last3"}, "types": {"asset"}, "limit": {"1"}}
	for page := 0; page < 4; page++ {
		r := lwGet(t, pool, lwOwnerID(), q)
		walk = append(walk, lwOrder(r)...)
		if r.NextCursor == "" {
			break
		}
		q.Set("cursor", r.NextCursor)
	}
	if !lwSame(walk, want) {
		t.Errorf("one-row pages over three ties walked %v, want %v", walk, want)
	}
}

// ── H. the 60-row walk ───────────────────────────────────────────────

func TestLastWindow_SixtyRowWalkIsExact(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	type row struct {
		key string
		ts  time.Time
		id  uuid.UUID
		typ string
	}
	rows := make([]row, 0, 60)
	// 20 of each; every third pair shares a timestamp so the id and the
	// type tie-breaks are exercised inside the walk.
	for i := 0; i < 20; i++ {
		ts := b.Add(time.Duration(i) * time.Second)
		tsTie := ts
		if i%3 == 0 {
			tsTie = ts.Add(500 * time.Millisecond)
		}
		a := lwAsset(t, pool, uuid.New(), lwOwner, "walk asset", "public", "png", ts)
		rows = append(rows, row{lwKey("asset", a), ts, a, "asset"})
		p := lwPost(t, pool, uuid.New(), lwOwner, "walk post", b, tsTie)
		rows = append(rows, row{lwKey("post", p), tsTie, p, "post"})
		c := lwCollection(t, pool, uuid.New(), lwOwner, "walk collection", tsTie)
		rows = append(rows, row{lwKey("collection", c), tsTie, c, "collection"})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].ts.Equal(rows[j].ts) {
			return rows[i].ts.After(rows[j].ts)
		}
		if rows[i].id != rows[j].id {
			return rows[i].id.String() > rows[j].id.String()
		}
		return rows[i].typ < rows[j].typ
	})
	want := make([]string, 0, 60)
	for _, r := range rows {
		want = append(want, r.key)
	}

	got := []string{}
	pages := 0
	q := url.Values{"dsl": {"!last60"}}
	for {
		r := lwGet(t, pool, lwOwnerID(), q)
		if r.Status != http.StatusOK {
			t.Fatalf("page %d → %d %v", pages+1, r.Status, r.Body)
		}
		if r.TotalCount != 60 || r.Body["total_count_capped"] != false {
			t.Errorf("page %d: total_count %d capped %v, want 60 / false", pages+1, r.TotalCount, r.Body["total_count_capped"])
		}
		pages++
		got = append(got, lwOrder(r)...)
		if r.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("the walk did not terminate")
		}
		q.Set("cursor", r.NextCursor)
	}
	if pages != 3 {
		t.Errorf("the walk took %d pages, want 3 (25, 25, 10)", pages)
	}
	if !lwSame(got, want) {
		seen := map[string]int{}
		for _, k := range got {
			seen[k]++
		}
		for _, k := range want {
			if seen[k] != 1 {
				t.Errorf("%s appeared %d times, want once", k, seen[k])
			}
		}
		t.Errorf("walk order differs from recency DESC, id DESC, type ASC:\n got %v\nwant %v", got, want)
	}
}

// ── I. suggestions: the global window first ──────────────────────────

func TestLastWindow_SuggestionsProjectFromTheGlobalWindow(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	lwCollection(t, pool, uuid.New(), lwOwner, "C0", b.Add(4*time.Second))
	p0 := lwPost(t, pool, uuid.New(), lwOwner, "P0", b.Add(3*time.Second), b.Add(3*time.Second))
	a1 := lwAsset(t, pool, uuid.New(), lwOwner, "A1", "public", "png", b.Add(2*time.Second))
	a2 := lwAsset(t, pool, uuid.New(), lwOther, "A2", "public", "jpg", b.Add(1*time.Second))
	lwTag(t, pool, "post_tags", "post_id", p0, "lwtagp")
	lwTag(t, pool, "asset_tag", "asset_id", a1, "lwtaga")
	lwTag(t, pool, "asset_tag", "asset_id", a2, "lwtaga2")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	facets := func(dslInput string) map[string]map[string]int64 {
		t.Helper()
		fh := &FacetHandler{Dispatcher: facet.NewDispatcher(pool, logger), Logger: logger}
		req := httptest.NewRequest(http.MethodGet, "/search/facets?"+url.Values{
			"dsl": {dslInput}, "facets": {"extension,tag,owner"}}.Encode(), nil)
		req = req.WithContext(auth.WithIdentity(req.Context(), lwOwnerID()))
		rr := httptest.NewRecorder()
		fh.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("/search/facets?dsl=%s → %d %s", dslInput, rr.Code, rr.Body.String())
		}
		var resp struct {
			Facets map[string]struct {
				Buckets []struct {
					Value string `json:"value"`
					Count int64  `json:"count"`
				} `json:"buckets"`
			} `json:"facets"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]int64{}
		for name, f := range resp.Facets {
			out[name] = map[string]int64{}
			for _, bkt := range f.Buckets {
				out[name][bkt.Value] = bkt.Count
			}
		}
		return out
	}
	contributors := func(dslInput string) []int64 {
		t.Helper()
		ch := &ContributorsHandler{Pool: pool, Logger: logger}
		req := httptest.NewRequest(http.MethodGet, "/search/contributors?"+url.Values{"dsl": {dslInput}}.Encode(), nil)
		req = req.WithContext(auth.WithIdentity(req.Context(), lwOwnerID()))
		rr := httptest.NewRecorder()
		ch.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("/search/contributors?dsl=%s → %d %s", dslInput, rr.Code, rr.Body.String())
		}
		var resp struct {
			Contributors []struct {
				UserRef int64 `json:"user_ref"`
			} `json:"contributors"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		refs := []int64{}
		for _, c := range resp.Contributors {
			refs = append(refs, c.UserRef)
		}
		return refs
	}
	same := func(a map[string]int64, b map[string]int64) bool {
		if len(a) != len(b) {
			return false
		}
		for k, v := range a {
			if b[k] != v {
				return false
			}
		}
		return true
	}

	// last:3 = {C0, P0, A1}: extension projects A1 alone; tag projects
	// P0 and A1; owner projects A1's owner; contributors is A1's owner.
	three := facets("!last3")
	if !same(three["extension"], map[string]int64{"png": 1}) {
		t.Errorf("extension under !last3 = %v, want png 1 only (A2 is outside the window, C0 and P0 are not files)", three["extension"])
	}
	if !same(three["tag"], map[string]int64{"lwtagp": 1, "lwtaga": 1}) {
		t.Errorf("tag under !last3 = %v, want lwtagp 1 and lwtaga 1", three["tag"])
	}
	if !same(three["owner"], map[string]int64{"25030101": 1}) {
		t.Errorf("owner under !last3 = %v, want the owner of A1 alone", three["owner"])
	}
	if refs := contributors("!last3"); len(refs) != 1 || refs[0] != lwOwner {
		t.Errorf("contributors under !last3 = %v, want [%d]", refs, lwOwner)
	}
	// last:2 = {C0, P0}: no asset in the window, so extension, owner and
	// contributors are empty and tag is P0's alone. The collection
	// consumed a slot even though nothing aggregates it.
	two := facets("!last2")
	if len(two["extension"]) != 0 || len(two["owner"]) != 0 {
		t.Errorf("extension/owner under !last2 = %v / %v, want empty", two["extension"], two["owner"])
	}
	if !same(two["tag"], map[string]int64{"lwtagp": 1}) {
		t.Errorf("tag under !last2 = %v, want lwtagp 1 only", two["tag"])
	}
	if refs := contributors("!last2"); len(refs) != 0 {
		t.Errorf("contributors under !last2 = %v, want none", refs)
	}
	// And the rail count equals the filtered result: the extension
	// bucket's number is what ticking it returns.
	r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last3"}, "filter": {"extension:png"}})
	if r.TotalCount != 1 || !lwSame(lwOrder(r), []string{lwKey("asset", a1)}) {
		t.Errorf("!last3 + extension:png returned %v (count %d), want A1 alone, the bucket's count", lwOrder(r), r.TotalCount)
	}
}

// ── J. similarity is refused with one contract ───────────────────────

func TestLastWindow_SimilarityIsOneDeterministicRefusal(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	anchor := lwAsset(t, pool, uuid.New(), lwOwner, "anchor", "public", "png", lwBase())
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO asset_embedding_d768 (asset_id, provider, model, modality, embedding, updated_at)
		VALUES ($1,'router','nomic-embed-text','text',$2::vector,NOW())`, anchor, simtoVector(0)); err != nil {
		t.Fatalf("seed embedding: %v", err)
	}
	// Positive control: the anchor resolves on its own.
	alone := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"similar_to:" + anchor.String()}})
	if alone.Status != http.StatusOK {
		t.Fatalf("similar_to alone → %d %v; the fixture's embedding did not resolve", alone.Status, alone.Body)
	}

	var contract map[string]any
	for name, q := range map[string]url.Values{
		"dsl=last:5 AND similar_to":      {"dsl": {"last:5 AND similar_to:" + anchor.String()}},
		"dsl=!last5 AND similar_to":      {"dsl": {"!last5 AND similar_to:" + anchor.String()}},
		"dsl=similar_to&filter=last:5":   {"dsl": {"similar_to:" + anchor.String()}, "filter": {"last:5"}},
		"dsl=similar_to cat&filter=last": {"dsl": {"similar_to:" + anchor.String() + " " + lwPhrase}, "filter": {"last:5"}},
	} {
		r := lwGet(t, pool, lwOwnerID(), q)
		if r.Status != http.StatusBadRequest || r.Body["error"] != "dsl_error" {
			t.Errorf("%s → %d %v, want 400 dsl_error", name, r.Status, r.Body)
			continue
		}
		got := map[string]any{"error": r.Body["error"], "kind": r.Body["kind"], "message": r.Body["message"]}
		if contract == nil {
			contract = got
			continue
		}
		if got["kind"] != contract["kind"] || got["message"] != contract["message"] {
			t.Errorf("%s produced %v, another seam produced %v; the contract must be one", name, got, contract)
		}
	}
	if contract == nil || contract["message"] == "" {
		t.Fatalf("no contract captured: %v", contract)
	}
	// The engine seam directly, for callers that do not come through
	// the handler: the same refusal, and no rows.
	sel, _ := facet.ParseSelection([]string{"last:5"})
	owner := lwOwner
	_, err := NewEngine(pool).Run(context.Background(), Query{
		Filters: sel, CallerUserRef: &owner, SimilarityHintID: "asset:" + anchor.String(), SimilarityHint: simtoVector(0),
	})
	if err == nil || !strings.Contains(err.Error(), contract["message"].(string)) {
		t.Errorf("Engine.Run with a window and a hint: %v, want the same refusal", err)
	}
}

// TestLastWindow_SaveAsCollectionSavesTheWindow: the window flows
// through save-as's own Run (its `filters` are the same wire form), and
// what is persisted is the N newest eligible assets.
//
// ⚠️ Save-as does not EXECUTE its `dsl` field (it is stored as
// provenance only; the query it runs is `q` plus `filters`), so the
// `last` + `similar_to` refusal is mapped there but cannot be reached
// through its body. Recorded in the handoff.
func TestLastWindow_SaveAsCollectionSavesTheWindow(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	newest := lwAsset(t, pool, uuid.New(), lwOwner, "newest", "public", "png", b.Add(3*time.Second))
	middle := lwAsset(t, pool, uuid.New(), lwOwner, "middle", "public", "png", b.Add(2*time.Second))
	lwAsset(t, pool, uuid.New(), lwOwner, "oldest", "public", "png", b.Add(1*time.Second))

	sh := &SaveAsCollectionHandler{Service: NewService(NewEngine(pool), nil, nil), Pool: pool}
	body, _ := json.Marshal(map[string]any{"name": "lw " + uuid.NewString()[:8], "q": lwPhrase, "filters": []string{"last:2"}})
	req := httptest.NewRequest(http.MethodPost, "/search/save-as-collection", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithIdentity(req.Context(), lwOwnerID()))
	rr := httptest.NewRecorder()
	sh.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("save-as → %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		CollectionID string `json:"collection_id"`
		SavedCount   int    `json:"saved_count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	collID := uuid.MustParse(resp.CollectionID)
	t.Cleanup(func() {
		testdb.Purge(t, pool, collID,
			`DELETE FROM collection_resources WHERE collection_id = $1`,
			`DELETE FROM collections WHERE id = $1`)
	})
	rows, err := pool.Query(context.Background(), `SELECT asset_id FROM collection_resources WHERE collection_id = $1`, collID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	saved := []string{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		saved = append(saved, id.String())
	}
	sort.Strings(saved)
	want := []string{newest.String(), middle.String()}
	sort.Strings(want)
	if resp.SavedCount != 2 || !lwSame(saved, want) {
		t.Errorf("save-as under last:2 saved %v (count %d), want the two newest", saved, resp.SavedCount)
	}
}

// ── cursors across orders ────────────────────────────────────────────

func TestLastWindow_CursorOrderMismatchIsInvalidCursor(t *testing.T) {
	pool := coPool(t)
	lwUsers(t, pool)
	b := lwBase()
	lwAsset(t, pool, uuid.New(), lwOwner, "one", "public", "png", b.Add(2*time.Second))
	lwAsset(t, pool, uuid.New(), lwOwner, "two", "public", "png", b.Add(1*time.Second))

	recent := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last2"}, "types": {"asset"}, "limit": {"1"}})
	relevance := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {lwPhrase}, "types": {"asset"}, "limit": {"1"}})
	if recent.NextCursor == "" || relevance.NextCursor == "" {
		t.Fatalf("cursors: recent %q relevance %q", recent.NextCursor, relevance.NextCursor)
	}
	if recent.NextCursor == relevance.NextCursor {
		t.Fatal("the two orders minted one cursor")
	}
	for name, q := range map[string]url.Values{
		"recent cursor on a relevance query":     {"dsl": {lwPhrase}, "types": {"asset"}, "cursor": {recent.NextCursor}},
		"relevance cursor on a recent query":     {"dsl": {"!last2"}, "types": {"asset"}, "cursor": {relevance.NextCursor}},
		"relevance cursor, window via filter=":   {"dsl": {lwPhrase}, "filter": {"last:2"}, "types": {"asset"}, "cursor": {relevance.NextCursor}},
		"recent cursor without its timestamp":    {"dsl": {"!last2"}, "cursor": {"eyJvIjoicmVjZW50IiwiaSI6IjExMTExMTExLTIyMjItMzMzMy00NDQ0LTU1NTU1NTU1NTU1NSIsInQiOiJhc3NldCJ9"}},
		"unknown order discriminator":            {"dsl": {"!last2"}, "cursor": {"eyJvIjoibmV3ZXN0IiwidHMiOjEsImkiOiIxMTExMTExMS0yMjIyLTMzMzMtNDQ0NC01NTU1NTU1NTU1NTUiLCJ0IjoiYXNzZXQifQ"}},
		"malformed":                              {"dsl": {"!last2"}, "cursor": {"not-a-cursor"}},
		"recent cursor on relevance, via q only": {"q": {lwPhrase}, "cursor": {recent.NextCursor}},
	} {
		r := lwGet(t, pool, lwOwnerID(), q)
		if r.Status != http.StatusBadRequest || r.Body["error"] != "invalid_cursor" {
			t.Errorf("%s → %d %v, want 400 invalid_cursor", name, r.Status, r.Body)
		}
	}
	// Each cursor on its own order pages.
	if r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {"!last2"}, "types": {"asset"}, "limit": {"1"}, "cursor": {recent.NextCursor}}); r.Status != http.StatusOK || len(r.Hits) != 1 {
		t.Errorf("recent cursor on its own order → %d %v", r.Status, r.Body)
	}
	if r := lwGet(t, pool, lwOwnerID(), url.Values{"dsl": {lwPhrase}, "types": {"asset"}, "limit": {"1"}, "cursor": {relevance.NextCursor}}); r.Status != http.StatusOK {
		t.Errorf("relevance cursor on its own order → %d %v", r.Status, r.Body)
	}
}
