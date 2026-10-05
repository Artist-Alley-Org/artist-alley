// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1417, sprint 24: A KIND IS SEARCHABLE VOCABULARY.
//
// A person typing `ebook`, `sprite` or `video` into the ordinary search
// box could not find a post that contains that kind unless somebody had
// written the word into a title, description, tag or field. The
// structured `kind:` filter found it; free text did not. Migration
// 00071 makes the resolved kind an ingredient of the asset document
// (weight D) and lets it reach the post document through the existing
// eligible-member fold, which it also corrects.
//
// # Why these are one file and an EXTERNAL test package
//
// Free text has four surfaces and one column behind each pair: posts
// are read by the engine (`/search`) and by browse (`GET /posts?q=`),
// assets by the engine and by the asset list (`GET /assets?q=`). A fix
// in the document is only proven where the document is READ, so the
// post cases assert both post surfaces and the asset cases both asset
// surfaces. That takes the engine, posts.Handler and
// assets.ListAssetsPageGated in one test, which is why this is
// `package search_test`: an internal test in any of the three cannot
// import the other two.
//
// # Vacuous fixtures are the hazard
//
// Every fixture title, description, tag and field value below is
// NONSENSE, and none contains a kind word or a file extension. The
// existing kind_filter fixtures are titled "kf-epub" and "video asset",
// which tokenise to the very words this file searches for, so a copy of
// them would have passed on the old behaviour. The words here were
// chosen so that a match is attributable to the derived kind and to
// nothing else.
//
// ⛔ Every case in the "fail before fix" group was run against a scratch
// checkout of origin/dev before the change and fails there on the
// assertion its comment names; the handoff records each one.
//
// Skips without AA_DB_PASSWORD.

package search_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/posts"
	"github.com/mscrnt/artist-alley/app/internal/search"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// Synthetic refs, distinct from every other file's.
const (
	kvAuthor   int64 = 14170001
	kvStranger int64 = 14170002
)

// The eleven kinds a single asset can resolve to, each with an
// extension the resolver maps to it (viewkind.go) and the asset_type
// ref to plant it under. `sprite` is the one override: a PNG under ref
// 13. `placeholder` and `sequence` are deliberately absent, see A10.
var kvKinds = []struct {
	kind, ext string
	ref       int64
}{
	{"image", "png", 1},
	{"video", "mp4", 1},
	{"pdf", "pdf", 1},
	{"audio", "mp3", 1},
	{"font", "ttf", 1},
	{"sprite", "png", 13},
	{"3d", "glb", 1},
	{"ebook", "epub", 1},
	{"doc", "txt", 1},
	{"audiobook", "m4b", 1},
	{"archive", "zip", 1},
}

func kvPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, u := range []struct {
		ref  int64
		name string
	}{{kvAuthor, "kv-author-1417"}, {kvStranger, "kv-stranger-1417"}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO "user" (ref, username) VALUES ($1,$2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			u.ref, u.name); err != nil {
			t.Fatalf("seed user %d: %v", u.ref, err)
		}
		ref := u.ref
		t.Cleanup(func() {
			testdb.Purge(t, pool, ref, `DELETE FROM "user" WHERE ref = $1`)
		})
	}
	return pool
}

// kvAsset plants one asset. `title` and `description` are the caller's
// responsibility to keep free of kind words.
func kvAsset(t *testing.T, pool *pgxpool.Pool, title, description, ext string, ref int64, sensitivity string, owner int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type,
		                    status, sensitivity, processing_status, file_extension)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, 'ready', $7)`,
		id, title, description, owner, ref, sensitivity, ext); err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id,
			`DELETE FROM asset_field_value WHERE asset_id = $1`,
			`DELETE FROM assets WHERE id = $1`)
	})
	return id
}

// kvFieldValue attaches one searchable, active text field value, the
// ingredient rebuild_asset_search_text folds at weight D.
func kvFieldValue(t *testing.T, pool *pgxpool.Pool, asset uuid.UUID, value string) {
	t.Helper()
	var fieldID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM field_definition
		  WHERE searchable = TRUE AND status = 'active' AND type = 'text'
		    AND mirrors_column IS NULL
		  ORDER BY id LIMIT 1`).Scan(&fieldID); err != nil {
		t.Fatalf("no searchable text field definition to attach a value to: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, $3)`,
		asset, fieldID, value); err != nil {
		t.Fatalf("seed field value: %v", err)
	}
}

// kvPost plants a public post by kvAuthor with an explicit cover (or
// none, uuid.Nil) and the given members in order.
func kvPost(t *testing.T, pool *pgxpool.Pool, title, description string, cover uuid.UUID, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var coverArg any
	if cover != uuid.Nil {
		coverArg = cover
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO posts (id, author_user_ref, title, description, visibility, cover_asset_id)
		VALUES ($1, $2, $3, $4, 'public', $5)`, id, kvAuthor, title, description, coverArg); err != nil {
		t.Fatalf("seed post %q: %v", title, err)
	}
	t.Cleanup(func() {
		testdb.Purge(t, pool, id,
			`DELETE FROM post_tags WHERE post_id = $1`,
			`DELETE FROM post_assets WHERE post_id = $1`,
			`DELETE FROM posts WHERE id = $1`)
	})
	for i, m := range members {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`,
			id, m, i); err != nil {
			t.Fatalf("seed membership %q/%d: %v", title, i, err)
		}
	}
	return id
}

func kvTag(t *testing.T, pool *pgxpool.Pool, post uuid.UUID, tag string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO post_tags (post_id, tag) VALUES ($1, $2)`, post, tag); err != nil {
		t.Fatalf("seed tag: %v", err)
	}
}

// kvSearch runs one engine query for a caller (nil = anonymous).
func kvSearch(t *testing.T, pool *pgxpool.Pool, caller *int64, text string, types ...search.HitType) search.QueryResult {
	t.Helper()
	res, err := search.NewEngine(pool).Run(context.Background(), search.Query{
		Text:          text,
		Types:         types,
		Limit:         200,
		CallerUserRef: caller,
	})
	if err != nil {
		t.Fatalf("search %q: %v", text, err)
	}
	return res
}

// kvHitCount is how many times `id` appears in the hit array.
func kvHitCount(res search.QueryResult, id uuid.UUID) int {
	n := 0
	for _, h := range res.Hits {
		if h.ID == id {
			n++
		}
	}
	return n
}

func kvPostFound(t *testing.T, pool *pgxpool.Pool, caller *int64, text string, id uuid.UUID) bool {
	t.Helper()
	return kvHitCount(kvSearch(t, pool, caller, text, search.HitTypePost), id) > 0
}

func kvAssetFound(t *testing.T, pool *pgxpool.Pool, caller *int64, text string, id uuid.UUID) bool {
	t.Helper()
	return kvHitCount(kvSearch(t, pool, caller, text, search.HitTypeAsset), id) > 0
}

// kvBrowse runs the browse page (`GET /posts?q=`) as kvAuthor and
// reports whether `id` is on it. Same column, different reader.
func kvBrowse(t *testing.T, pool *pgxpool.Pool, q string, id uuid.UUID) bool {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := posts.NewHandler(pool, logger, cache.NewRegistry(pool, logger))
	ref := kvAuthor
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{UserRef: ref, AuthMethod: "session"})
	limit := 200
	vis := openapi.ListPostsParamsVisibility("public")
	resp, err := h.ListPosts(ctx, openapi.ListPostsRequestObject{Params: openapi.ListPostsParams{
		Limit: &limit, Q: &q, Visibility: &vis,
	}})
	if err != nil {
		t.Fatalf("ListPosts(q=%q): %v", q, err)
	}
	ok, is := resp.(openapi.ListPosts200JSONResponse)
	if !is {
		t.Fatalf("ListPosts returned %T, want 200", resp)
	}
	for _, p := range ok.Items {
		if uuid.UUID(p.Id) == id {
			return true
		}
	}
	return false
}

// kvAssetList runs the asset list (`GET /assets?q=`) for a caller and
// reports whether `id` is on it.
func kvAssetList(t *testing.T, pool *pgxpool.Pool, caller *int64, q string, id uuid.UUID) bool {
	t.Helper()
	rows, err := assets.ListAssetsPageGated(context.Background(), pool,
		visibility.NewCaller(caller), nil,
		assets.ListAssetsPageGatedParams{RowLimit: 200, Q: &q})
	if err != nil {
		t.Fatalf("ListAssetsPageGated(q=%q): %v", q, err)
	}
	for _, r := range rows {
		if r.ID.Valid && r.ID.Bytes == id {
			return true
		}
	}
	return false
}

// kvLexeme is the one lexeme `word` tokenises to under `english`, so the
// label assertions below compare lexemes rather than guessing what the
// stemmer does to a nonsense word.
func kvLexeme(t *testing.T, pool *pgxpool.Pool, word string) string {
	t.Helper()
	var doc string
	if err := pool.QueryRow(context.Background(),
		`SELECT to_tsvector('english', $1)::text`, word).Scan(&doc); err != nil {
		t.Fatalf("to_tsvector(%q): %v", word, err)
	}
	labels := kvParse(t, doc)
	if len(labels) != 1 {
		t.Fatalf("%q tokenises to %d lexemes (%s); the fixture needs exactly one", word, len(labels), doc)
	}
	for l := range labels {
		return l
	}
	return ""
}

// kvParse reads a tsvector's text form into lexeme -> sorted weight
// letters, one per position. Weight D is the default and is NOT printed,
// so a position with no letter is D.
var kvEntry = regexp.MustCompile(`'((?:[^']|'')+)':([0-9A-D,]+)`)

func kvParse(t *testing.T, doc string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, m := range kvEntry.FindAllStringSubmatch(doc, -1) {
		lexeme := strings.ReplaceAll(m[1], "''", "'")
		for _, pos := range strings.Split(m[2], ",") {
			w := "D"
			if last := pos[len(pos)-1]; last >= 'A' && last <= 'C' {
				w = string(last)
			}
			out[lexeme] = append(out[lexeme], w)
		}
		sort.Strings(out[lexeme])
	}
	return out
}

func kvAssetDoc(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var doc string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(search_text::text, '') FROM assets WHERE id = $1`, id).Scan(&doc); err != nil {
		t.Fatalf("asset doc: %v", err)
	}
	return doc
}

func kvPostDoc(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var doc string
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(search_text::text, '') FROM posts WHERE id = $1`, id).Scan(&doc); err != nil {
		t.Fatalf("post doc: %v", err)
	}
	return doc
}

// kvRank is ts_rank_cd of one row's document against one word, through
// the same plainto_tsquery the engine scores with.
func kvRank(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID, word string) float64 {
	t.Helper()
	var r float64
	if err := pool.QueryRow(context.Background(),
		`SELECT ts_rank_cd(search_text, plainto_tsquery('english', $2))::FLOAT8 FROM `+table+` WHERE id = $1`,
		id, word).Scan(&r); err != nil {
		t.Fatalf("rank %s %q: %v", table, word, err)
	}
	return r
}

// kvMatches is the product predicate itself, on one row.
func kvMatches(t *testing.T, pool *pgxpool.Pool, table string, id uuid.UUID, word string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT search_text @@ plainto_tsquery('english', $2) FROM `+table+` WHERE id = $1`,
		id, word).Scan(&ok); err != nil {
		t.Fatalf("match %s %q: %v", table, word, err)
	}
	return ok
}

// kvAssertLabel requires `lexeme` to appear in the document with
// exactly the given weight letters (one per occurrence).
func kvAssertLabel(t *testing.T, what string, labels map[string][]string, lexeme string, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := labels[lexeme]
	if strings.Join(got, "") != strings.Join(want, "") {
		t.Errorf("%s: lexeme %q carries weights %v, want %v", what, lexeme, got, want)
	}
}

var (
	kvMarkerJunk = regexp.MustCompile(`^[0-9]+[abc]$`)
	kvBareNumber = regexp.MustCompile(`^[0-9]+$`)
)

// kvAssertNoMarkerJunk fails on any lexeme the old fold manufactured out
// of a tsvector's text form: a position with an A/B/C weight letter
// (`1a`, `2a`) or a bare position number (`3`). The fixtures in this
// file author no numeric tokens, so a bare number can only be junk.
func kvAssertNoMarkerJunk(t *testing.T, what string, labels map[string][]string) {
	t.Helper()
	for l := range labels {
		if kvMarkerJunk.MatchString(l) {
			t.Errorf("%s: document carries the manufactured weight-marker lexeme %q", what, l)
		}
		if kvBareNumber.MatchString(l) {
			t.Errorf("%s: document carries the manufactured position lexeme %q", what, l)
		}
	}
}

// ---------------------------------------------------------------------------
// A. Fail before fix
// ---------------------------------------------------------------------------

// A1: the report itself. An image-covered post with an epub buried
// inside it, no literal "ebook" anywhere, is returned by free text
// `ebook` on BOTH post surfaces.
//
// Failing assertion on origin/dev: the engine does not return the post
// (and browse does not list it).
func TestKindVocabulary_EbookMemberFoundByFreeText(t *testing.T) {
	pool := kvPool(t)
	png := kvAsset(t, pool, "quillbrasse", "", "png", 1, "public", kvAuthor)
	epub := kvAsset(t, pool, "morvantide", "", "epub", 1, "public", kvAuthor)
	post := kvPost(t, pool, "thessaly drop", "a bundle of things", png, png, epub)

	if !kvPostFound(t, pool, nil, "ebook", post) {
		t.Errorf("engine: a post holding a public ready epub is not returned by free text `ebook`")
	}
	if !kvBrowse(t, pool, "ebook", post) {
		t.Errorf("browse: `?q=ebook` does not list the post holding the epub")
	}
	// The unrelated kind is a control against a document that matches
	// everything.
	if kvPostFound(t, pool, nil, "audiobook", post) {
		t.Errorf("engine: the post matched a kind it does not contain")
	}
}

// A2: the override wins over the extension. A sprite atlas is a PNG
// under asset_type 13, and the post that holds one is found by `sprite`,
// on both surfaces.
func TestKindVocabulary_SpriteOverrideFoundByFreeText(t *testing.T) {
	pool := kvPool(t)
	png := kvAsset(t, pool, "quillbrasse", "", "png", 1, "public", kvAuthor)
	atlas := kvAsset(t, pool, "morvantide", "", "png", 13, "public", kvAuthor)
	post := kvPost(t, pool, "thessaly sheet", "", png, png, atlas)

	if !kvPostFound(t, pool, nil, "sprite", post) {
		t.Errorf("engine: a post holding a sprite atlas (asset_type 13, .png) is not returned by `sprite`")
	}
	if !kvBrowse(t, pool, "sprite", post) {
		t.Errorf("browse: `?q=sprite` does not list the post holding the atlas")
	}
}

// A3 + A4: N >= 2, then cover and order inertness. One post with a png,
// an mp4 and a sprite matches each of those kinds and not `ebook`; a
// cover swap alone leaves the post document byte-identical; reversing
// the membership changes no verdict (and, by construction, no byte).
func TestKindVocabulary_MixedMembersAndCoverOrderInert(t *testing.T) {
	pool := kvPool(t)
	png := kvAsset(t, pool, "quillbrasse", "", "png", 1, "public", kvAuthor)
	mp4 := kvAsset(t, pool, "morvantide", "", "mp4", 1, "public", kvAuthor)
	atlas := kvAsset(t, pool, "brindlewax", "", "png", 13, "public", kvAuthor)
	post := kvPost(t, pool, "thessaly bundle", "", png, png, mp4, atlas)

	verdicts := func(stage string) {
		t.Helper()
		for _, k := range []string{"image", "video", "sprite"} {
			if !kvPostFound(t, pool, nil, k, post) {
				t.Errorf("%s: post with png+mp4+sprite members not returned by `%s`", stage, k)
			}
			if !kvBrowse(t, pool, k, post) {
				t.Errorf("%s: browse `?q=%s` does not list the post", stage, k)
			}
		}
		if kvPostFound(t, pool, nil, "ebook", post) {
			t.Errorf("%s: post returned by `ebook`, which no member resolves to", stage)
		}
	}
	verdicts("initial")
	before := kvPostDoc(t, pool, post)

	// Cover swap alone.
	if _, err := pool.Exec(context.Background(),
		`UPDATE posts SET cover_asset_id = $2 WHERE id = $1`, post, mp4); err != nil {
		t.Fatalf("swap cover: %v", err)
	}
	if after := kvPostDoc(t, pool, post); after != before {
		t.Errorf("a cover change alone rewrote the post document:\n before %s\n after  %s", before, after)
	}
	verdicts("after cover swap")

	// Member reversal.
	for i, m := range []uuid.UUID{atlas, mp4, png} {
		if _, err := pool.Exec(context.Background(),
			`UPDATE post_assets SET sort_order = $3 WHERE post_id = $1 AND asset_id = $2`, post, m, i); err != nil {
			t.Fatalf("reorder: %v", err)
		}
	}
	// Force a rebuild the way any member edit would, so the assertion is
	// about the FOLD and not about whether reordering triggers one.
	if _, err := pool.Exec(context.Background(),
		`SELECT public.rebuild_post_search_text($1)`, post); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	verdicts("after member reversal")
	if after := kvPostDoc(t, pool, post); after != before {
		t.Errorf("membership order changed the post document; the fold is ordered by member id and must be inert to it:\n before %s\n after  %s", before, after)
	}
}

// A5: the asset itself. An epub with no kind word in title, description
// or fields is returned by `ebook` through the engine (assets entity)
// and through the asset list.
func TestKindVocabulary_AssetFoundByDerivedKind(t *testing.T) {
	pool := kvPool(t)
	epub := kvAsset(t, pool, "morvantide", "vellichor notes", "epub", 1, "public", kvAuthor)
	kvFieldValue(t, pool, epub, "gorbulent")

	if !kvAssetFound(t, pool, nil, "ebook", epub) {
		t.Errorf("engine: a public epub asset is not returned by free text `ebook`")
	}
	if !kvAssetList(t, pool, nil, "ebook", epub) {
		t.Errorf("asset list: `?q=ebook` does not list the epub")
	}
	if kvAssetFound(t, pool, nil, "video", epub) {
		t.Errorf("engine: the epub matched `video`")
	}
}

// A6: a restricted asset's kind is withheld content. Its owner finds it
// by `ebook` on both asset surfaces; a stranger does not, and the
// stranger's count does not move when the asset comes into existence.
func TestKindVocabulary_RestrictedAssetKindIsOwnerOnly(t *testing.T) {
	pool := kvPool(t)
	owner, stranger := kvAuthor, kvStranger

	strangerBefore := kvSearch(t, pool, &stranger, "ebook", search.HitTypeAsset).TotalCount
	anonBefore := kvSearch(t, pool, nil, "ebook", search.HitTypeAsset).TotalCount

	epub := kvAsset(t, pool, "morvantide", "", "epub", 1, "restricted", owner)

	if !kvAssetFound(t, pool, &owner, "ebook", epub) {
		t.Errorf("engine: the owner cannot find their own restricted epub by `ebook`")
	}
	if !kvAssetList(t, pool, &owner, "ebook", epub) {
		t.Errorf("asset list: the owner's `?q=ebook` does not list their restricted epub")
	}
	if kvAssetFound(t, pool, &stranger, "ebook", epub) {
		t.Errorf("engine: a stranger found a restricted asset by its derived kind")
	}
	if kvAssetList(t, pool, &stranger, "ebook", epub) {
		t.Errorf("asset list: a stranger's `?q=ebook` listed a restricted asset")
	}
	if got := kvSearch(t, pool, &stranger, "ebook", search.HitTypeAsset).TotalCount; got != strangerBefore {
		t.Errorf("stranger total moved %d -> %d when a restricted epub appeared; the count is an oracle", strangerBefore, got)
	}
	if got := kvSearch(t, pool, nil, "ebook", search.HitTypeAsset).TotalCount; got != anonBefore {
		t.Errorf("anonymous total moved %d -> %d when a restricted epub appeared", anonBefore, got)
	}
}

// A7: asset weights, one occurrence per signal. Title A, description B,
// nothing at C, field value D, kind D; and ts_rank_cd agrees with the
// labels.
//
// Failing assertion on origin/dev: the kind lexeme is absent (weights
// [] rather than [D]) and rank(K) is 0 rather than rank(T3).
func TestKindVocabulary_AssetDocumentWeights(t *testing.T) {
	pool := kvPool(t)
	const t1, t2, t3, kind = "quibblenox", "vermilloth", "drastiquell", "ebook"
	epub := kvAsset(t, pool, t1, t2, "epub", 1, "public", kvAuthor)
	kvFieldValue(t, pool, epub, t3)

	labels := kvParse(t, kvAssetDoc(t, pool, epub))
	l1, l2, l3, lk := kvLexeme(t, pool, t1), kvLexeme(t, pool, t2), kvLexeme(t, pool, t3), kvLexeme(t, pool, kind)
	for _, pair := range [][2]string{{l1, l2}, {l1, l3}, {l1, lk}, {l2, l3}, {l2, lk}, {l3, lk}} {
		if pair[0] == pair[1] {
			t.Fatalf("fixture words collide on lexeme %q", pair[0])
		}
	}
	kvAssertLabel(t, "asset title", labels, l1, "A")
	kvAssertLabel(t, "asset description", labels, l2, "B")
	kvAssertLabel(t, "asset field value", labels, l3, "D")
	kvAssertLabel(t, "asset derived kind", labels, lk, "D")
	for l, ws := range labels {
		for _, w := range ws {
			if w == "C" {
				t.Errorf("asset document carries %q at weight C; nothing is placed at C", l)
			}
		}
	}

	r1, r2, r3, rk := kvRank(t, pool, "assets", epub, t1), kvRank(t, pool, "assets", epub, t2),
		kvRank(t, pool, "assets", epub, t3), kvRank(t, pool, "assets", epub, kind)
	if !(r1 > r2 && r2 > r3) {
		t.Errorf("asset ranks: title %v > description %v > field %v does not hold", r1, r2, r3)
	}
	if rk != r3 {
		t.Errorf("asset ranks: kind %v != field value %v; the kind is one D-weight occurrence like the field", rk, r3)
	}
}

// A8: post weights, one occurrence per signal, plus the cross-row
// equal-count check. Title A, description B, tag C, member title D,
// inherited kind D; no weight-marker junk.
//
// Failing assertions on origin/dev: the inherited kind is absent and
// the document carries the manufactured `1a` lexeme.
func TestKindVocabulary_PostDocumentWeights(t *testing.T) {
	pool := kvPool(t)
	const t1, t2, t3, m1, kind = "quibblenox", "vermilloth", "drastiquell", "plinthorax", "ebook"
	epub := kvAsset(t, pool, m1, "", "epub", 1, "public", kvAuthor)
	post := kvPost(t, pool, t1, t2, epub, epub)
	kvTag(t, pool, post, t3)

	labels := kvParse(t, kvPostDoc(t, pool, post))
	l1, l2, l3, lm, lk := kvLexeme(t, pool, t1), kvLexeme(t, pool, t2), kvLexeme(t, pool, t3),
		kvLexeme(t, pool, m1), kvLexeme(t, pool, kind)
	kvAssertLabel(t, "post title", labels, l1, "A")
	kvAssertLabel(t, "post description", labels, l2, "B")
	kvAssertLabel(t, "post tag", labels, l3, "C")
	kvAssertLabel(t, "member title", labels, lm, "D")
	kvAssertLabel(t, "inherited kind", labels, lk, "D")
	kvAssertNoMarkerJunk(t, "post document", labels)

	r1, r2, r3, rm, rk := kvRank(t, pool, "posts", post, t1), kvRank(t, pool, "posts", post, t2),
		kvRank(t, pool, "posts", post, t3), kvRank(t, pool, "posts", post, m1), kvRank(t, pool, "posts", post, kind)
	if !(r1 > r2 && r2 > r3 && r3 > rm) {
		t.Errorf("post ranks: title %v > description %v > tag %v > member %v does not hold", r1, r2, r3, rm)
	}
	if rk != rm {
		t.Errorf("post ranks: inherited kind %v != member title %v", rk, rm)
	}

	// Cross-row, equal occurrence counts: X says the word once in its
	// title and holds a member of ANOTHER kind; Y never says it and holds
	// one member of that kind. Query K matches both exactly once and X
	// outranks Y, because A outranks D.
	png := kvAsset(t, pool, "brindlewax", "", "png", 1, "public", kvAuthor)
	x := kvPost(t, pool, kind+" caskerling", "", png, png)
	y := kvPost(t, pool, "caskerling", "", epub, epub)
	rx, ry := kvRank(t, pool, "posts", x, kind), kvRank(t, pool, "posts", y, kind)
	if !(rx > ry && ry > 0) {
		t.Errorf("cross-row: X (kind word in title) %v must outrank Y (kind inherited from a member) %v > 0", rx, ry)
	}
}

// A9: FOLD POLLUTION. The old fold serialised each member document to
// text and re-tokenised it, so the position and weight markers in that
// text (`'alpha':1A 'beta':2A 'gamma':3`) became lexemes: `1a`, `2a`
// and the bare position `3`. A post was then returned by a search for
// `1a`, a word nobody wrote.
//
// The member title has exactly two lexemes (weight A, positions 1 and
// 2) and the field value one (weight D, position 3). Nothing in the
// fixture authors a numeric token, so a bare number in the post document
// can only have been manufactured.
//
// Failing assertions on origin/dev: the post matches `1a`, `2a` and `3`
// through plainto_tsquery, and its document carries those lexemes.
func TestKindVocabulary_FoldManufacturesNoMarkerLexemes(t *testing.T) {
	pool := kvPool(t)
	member := kvAsset(t, pool, "brindlewax quorrel", "", "png", 1, "public", kvAuthor)
	kvFieldValue(t, pool, member, "gorbulent")
	post := kvPost(t, pool, "thessaly", "", member, member)

	// The member document is what the fixture claims it is.
	mlabels := kvParse(t, kvAssetDoc(t, pool, member))
	kvAssertLabel(t, "member title lexeme 1", mlabels, kvLexeme(t, pool, "brindlewax"), "A")
	kvAssertLabel(t, "member title lexeme 2", mlabels, kvLexeme(t, pool, "quorrel"), "A")
	kvAssertLabel(t, "member field value", mlabels, kvLexeme(t, pool, "gorbulent"), "D")

	for _, junk := range []string{"1a", "2a", "3"} {
		if kvMatches(t, pool, "posts", post, junk) {
			t.Errorf("the post matches `%s` through plainto_tsquery; that word was manufactured from a member's tsvector markers", junk)
		}
		if kvPostFound(t, pool, nil, junk, post) {
			t.Errorf("engine: the post is returned by free text `%s`", junk)
		}
	}
	// The real words still fold.
	for _, word := range []string{"brindlewax", "quorrel", "gorbulent", "image"} {
		if !kvMatches(t, pool, "posts", post, word) {
			t.Errorf("the post does not match the member word `%s`", word)
		}
	}
	kvAssertNoMarkerJunk(t, "post document", kvParse(t, kvPostDoc(t, pool, post)))
}

// A10: every searchable kind round-trips. One fixture asset of each
// kind inside its own post, each found by its own word on the posts
// entity and by none of the other ten.
func TestKindVocabulary_EveryKindRoundTrips(t *testing.T) {
	pool := kvPool(t)
	postOf := map[string]uuid.UUID{}
	for _, k := range kvKinds {
		a := kvAsset(t, pool, "morvantide", "", k.ext, k.ref, "public", kvAuthor)
		postOf[k.kind] = kvPost(t, pool, "thessaly", "", a, a)
	}
	for _, k := range kvKinds {
		res := kvSearch(t, pool, nil, k.kind, search.HitTypePost)
		for _, other := range kvKinds {
			got := kvHitCount(res, postOf[other.kind]) > 0
			if other.kind == k.kind && !got {
				t.Errorf("`%s` did not return the post holding the %s (.%s, ref %d)", k.kind, k.kind, k.ext, k.ref)
			}
			if other.kind != k.kind && got {
				t.Errorf("`%s` returned the post holding a %s", k.kind, other.kind)
			}
		}
	}
	// `placeholder` and `sequence` are not vocabulary: an asset the
	// resolver cannot place emits nothing.
	unknown := kvAsset(t, pool, "morvantide", "", "nosuchext", 1, "public", kvAuthor)
	labels := kvParse(t, kvAssetDoc(t, pool, unknown))
	for _, absent := range []string{"placeholder", "sequence"} {
		if _, has := labels[kvLexeme(t, pool, absent)]; has {
			t.Errorf("an unresolvable asset's document carries %q; that is not a word anyone searches", absent)
		}
	}
}

// A11: duplicate same-kind members. Two epubs in one post make it match
// `ebook` exactly once in the result set, and no other kind.
func TestKindVocabulary_DuplicateKindMembersMatchOnce(t *testing.T) {
	pool := kvPool(t)
	a := kvAsset(t, pool, "morvantide", "", "epub", 1, "public", kvAuthor)
	b := kvAsset(t, pool, "vellichor", "", "epub", 1, "public", kvAuthor)
	post := kvPost(t, pool, "thessaly", "", a, a, b)

	if n := kvHitCount(kvSearch(t, pool, nil, "ebook", search.HitTypePost), post); n != 1 {
		t.Errorf("`ebook` returned the post %d times, want exactly once", n)
	}
	for _, k := range kvKinds {
		if k.kind != "ebook" && kvPostFound(t, pool, nil, k.kind, post) {
			t.Errorf("a post of two epubs was returned by `%s`", k.kind)
		}
	}
}
