// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #899 — the search half. An asset you cannot open must not hand you
// its metadata through a search result, a completion, or a facet count.
//
// This package's own doc used to state the gap plainly: *"assets,
// authenticated: not-deleted only; the sensitivity rule for signed-in
// callers is deliberately deferred."* It was deferred because the
// capabilities never reached the engine — the HTTP handler copied
// `id.UserRef` and dropped `id.Capabilities` — so there was no way to
// tell a `content.read.all` holder from a stranger and therefore no way
// to apply the rule without breaking the demo-viewer role.
//
// Assertions are on the SERIALIZED hit (MarshalHitJSON), for the same
// reason as the asset suite: a struct field a client never reads is not
// the leak; the JSON is.
//
// The three surfaces have three different correct answers, which is why
// they are tested separately rather than by one shared helper:
//
//   - a HIT degrades to a placeholder (the row stays, ADR 0064);
//   - a SUGGESTION disappears (a completion IS the title — there is no
//     placeholder shape for it);
//   - a FACET counts only what the caller can open.
//
// #902 AMENDED THE FIRST OF THOSE, and this file was rewritten for it.
// A hit still degrades to a placeholder wherever an unreadable row can
// reach the projection — which, since the text match itself became
// readability-gated, is the vector/hybrid path only. On the BM25 path
// the row no longer MATCHES, because matching it was the leak: it
// answered "does this asset's withheld title contain this word" one
// token at a time. So the placeholder assertion here is a unit test on
// withheldHit, and the row's absence from a text search has its own
// suite in search_text_oracle_test.go. ADR 0064's placeholder is
// unaffected on browse, which lists the row with no query text at all.
//
// Skips without AA_DB_PASSWORD.

package search

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/search/suggest"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	hwOwner    int64 = 8991101
	hwStranger int64 = 8991102
)

// hwPhrase appears only in this fixture's titles. Nonsense on purpose,
// so a hit is attributable to these rows and to nothing in any
// developer's database.
const hwPhrase = "quenlibrium"

// hwHitAllowList is the COMPLETE key set a withheld hit may carry. The
// three scores are properties of the QUERY (they carry the ordering the
// client renders, which is observable from the array anyway); everything
// else is a property of the ITEM.
var hwHitAllowList = map[string]bool{
	"type":               true,
	"id":                 true,
	"restricted":         true,
	"owner_display_name": true,
	"score":              true,
	"vector_score":       true,
	"hybrid_score":       true,
}

func hwSeedOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	const display = "Sam O'Brien"
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user" (ref, username) VALUES ($1, $2)
		 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
		hwOwner, "hw-owner-899"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_profiles (user_ref, display_name) VALUES ($1, $2)
		 ON CONFLICT (user_ref) DO UPDATE SET display_name = EXCLUDED.display_name`,
		hwOwner, display); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, hwOwner)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, hwOwner)
	})
	return display
}

func hwSeedAsset(t *testing.T, pool *pgxpool.Pool, title, sensitivity string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status,
		                    sensitivity, processing_status, file_extension, thumbhash)
		VALUES ($1, $2, $3, $4, (SELECT MIN(ref) FROM asset_types), 'active', $5, 'ready',
		        'ogg', $6)`,
		id, title, "UNRELEASED — do not distribute", hwOwner, sensitivity,
		[]byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id) })
	return id
}

// hwFind runs the engine and returns the serialized hit for one asset,
// plus whether the result set contained it at all.
func hwFind(
	t *testing.T, e *Engine, ref *int64, caps visibility.ContentCaps, id uuid.UUID,
) (map[string]json.RawMessage, bool) {
	t.Helper()
	res, err := e.Run(context.Background(), Query{
		Text:          hwPhrase,
		Types:         []HitType{HitTypeAsset},
		Limit:         50,
		CallerUserRef: ref,
		Caps:          caps,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range res.Hits {
		if h.ID != id {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(MarshalHitJSON(h), &m); err != nil {
			t.Fatalf("unmarshal hit: %v", err)
		}
		return m, true
	}
	return nil, false
}

// hwHit is hwFind for the cases that REQUIRE the row — a readable one.
func hwHit(t *testing.T, e *Engine, ref *int64, caps visibility.ContentCaps, id uuid.UUID) map[string]json.RawMessage {
	t.Helper()
	m, ok := hwFind(t, e, ref, caps, id)
	if !ok {
		t.Fatalf("asset %v is not in the result set, and this caller may READ it — "+
			"the #902 match gate has become too wide", id)
	}
	return m
}

// TestWithheldHit_IsAllowListed is the #899 + #850 payload assertion,
// and it is a UNIT test on the projection rather than an engine run.
//
// It used to drive the engine with a restricted asset and a stranger.
// #902 closed the channel that made that possible: an asset whose
// columns a caller may not read no longer MATCHES that caller's text
// query, so the BM25 path can no longer produce a withheld hit to
// inspect. withheldHit is still reached — the vector/hybrid path
// projects rows it did not text-match (see enrichAssetHits) — so the
// guarantee is still live and still needs pinning; what changed is that
// the cheapest way to reach it is directly.
//
// Starting from a FULLY POPULATED Hit is what makes this a real test: a
// projection that forgot to withhold would carry every one of those
// fields straight through.
func TestWithheldHit_IsAllowListed(t *testing.T) {
	const display = "Sam O'Brien"
	full := Hit{
		Type:             HitTypeAsset,
		ID:               uuid.New(),
		Title:            hwPhrase + " unreleased boss theme",
		Summary:          "UNRELEASED — do not distribute",
		OwnerUserRef:     func() *int64 { r := hwOwner; return &r }(),
		OriginServerID:   func() *uuid.UUID { u := uuid.New(); return &u }(),
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		RawScore:         0.42,
		NormalisedScore:  0.9,
		ExtraJSON:        []byte(`{"file_extension":"ogg","thumbhash":"3q2+7w=="}`),
		Restricted:       false,
		OwnerDisplayName: "",
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(MarshalHitJSON(withheldHit(full, display)), &m); err != nil {
		t.Fatalf("unmarshal hit: %v", err)
	}
	for k := range m {
		if !hwHitAllowList[k] {
			t.Errorf("withheld hit carried key %q, which is not on the allow-list", k)
		}
	}
	for _, leak := range []string{"title", "summary", "extra", "created_at", "updated_at", "owner_user_ref"} {
		if _, present := m[leak]; present {
			t.Errorf("withheld hit still ships %q — this is the #899 search leak", leak)
		}
	}
	var restrictedFlag bool
	if err := json.Unmarshal(m["restricted"], &restrictedFlag); err != nil || !restrictedFlag {
		t.Errorf("withheld hit did not set restricted=true")
	}
	var name string
	if err := json.Unmarshal(m["owner_display_name"], &name); err != nil || name != display {
		t.Errorf("owner_display_name = %q, want %q", name, display)
	}
	// The fields by name, so a failure says WHICH one got out rather
	// than only that the key set grew (#850's card payload rides in
	// `extra`, so its mere presence would be the leak).
	raw := string(mustMarshal(t, m))
	for _, k := range hwCardKeys {
		if strings.Contains(raw, `"`+k+`"`) {
			t.Errorf("withheld hit serialises %q — that is an asset column #899 withholds", k)
		}
	}
}

// TestSearchHit_PublicRowIsUntouched is the counterweight a
// withhold-everything implementation must fail: the readable row still
// arrives with its columns.
func TestSearchHit_PublicRowIsUntouched(t *testing.T) {
	pool := coPool(t)
	hwSeedOwner(t, pool)
	public := hwSeedAsset(t, pool, hwPhrase+" public splash art", "public")

	stranger := hwStranger
	pubHit := hwHit(t, NewEngine(pool), &stranger, visibility.ContentCaps{}, public)
	for _, want := range []string{"title", "summary", "extra", "created_at"} {
		if _, ok := pubHit[want]; !ok {
			t.Errorf("a PUBLIC hit lost %q — the withholding is too wide", want)
		}
	}
}

// hwCardKeys is the set of presentation fields #850 added to a readable
// asset hit's `extra`. Every one of them is a column #899 withholds from
// a restricted asset's payload — `file_extension` IS the media type,
// `thumbhash` is a blurred picture of the content — so widening the
// payload had to be done THROUGH the readability gate, not beside it.
var hwCardKeys = []string{
	"asset_type", "file_hash", "file_extension", "thumbhash",
	"preview_available", "ladder_available", "scrub_available",
	"pixel_width", "pixel_height",
}

// TestSearchHit_ReadableRowCarriesTheCardPayload is the counterweight,
// and it is the acceptance criterion for the sprint: a hit the caller may
// open has to carry what a card renders from, or /search goes back to
// text rows. Without this a withhold-everything implementation passes the
// test above.
func TestSearchHit_ReadableRowCarriesTheCardPayload(t *testing.T) {
	pool := coPool(t)
	hwSeedOwner(t, pool)
	public := hwSeedAsset(t, pool, hwPhrase+" public splash art", "public")

	stranger := hwStranger
	m := hwHit(t, NewEngine(pool), &stranger, visibility.ContentCaps{}, public)
	extraRaw, ok := m["extra"]
	if !ok {
		t.Fatal("a readable asset hit has no `extra` bag — nothing to render a tile from")
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(extraRaw, &extra); err != nil {
		t.Fatalf("extra is not an object: %v", err)
	}
	for _, k := range hwCardKeys {
		if _, present := extra[k]; !present {
			t.Errorf("readable hit's extra is missing %q — the card contract (cardAsset.ts) "+
				"requires it, and a surface that drops a presentation field silently loses "+
				"the media-type badge and the blur-up (#595)", k)
		}
	}
	// The fixture seeds file_extension='ogg' and a four-byte thumbhash, so
	// these two are checked for VALUE, not just presence: "present but
	// always null" is how a card feed dies quietly.
	var ext string
	if err := json.Unmarshal(extra["file_extension"], &ext); err != nil || ext != "ogg" {
		t.Errorf("file_extension = %q, want %q — CardThumb reads the media TYPE off this alone", ext, "ogg")
	}
	var thumb string
	if err := json.Unmarshal(extra["thumbhash"], &thumb); err != nil || thumb == "" {
		t.Errorf("thumbhash = %q, want the base64 of the seeded bytes", thumb)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestSearchHit_EntitledCallersUnaffected is the other half: the owner
// and the two capability holders get the full hit. content.read.all in
// particular exists so the demo-viewer role can render a
// mostly-restricted catalogue, and a catalogue of placeholders is not a
// rendered catalogue.
func TestSearchHit_EntitledCallersUnaffected(t *testing.T) {
	pool := coPool(t)
	hwSeedOwner(t, pool)
	restricted := hwSeedAsset(t, pool, hwPhrase+" unreleased boss theme", "restricted")

	owner, stranger := hwOwner, hwStranger
	for _, c := range []struct {
		name string
		ref  *int64
		caps visibility.ContentCaps
	}{
		{"the owner", &owner, visibility.ContentCaps{}},
		{"content.read.all", &stranger, visibility.ContentCaps{ContentReadAll: true}},
		{"system.admin", &stranger, visibility.ContentCaps{SystemAdmin: true}},
	} {
		m := hwHit(t, NewEngine(pool), c.ref, c.caps, restricted)
		var restrictedFlag bool
		_ = json.Unmarshal(m["restricted"], &restrictedFlag)
		if restrictedFlag {
			t.Errorf("%s: got a placeholder for an asset they may read in full", c.name)
		}
		var title string
		if err := json.Unmarshal(m["title"], &title); err != nil || !strings.Contains(title, "boss theme") {
			t.Errorf("%s: title = %q, want the real one", c.name, title)
		}
	}
}

// TestSearchCache_KeyedOnCapabilities pins the cache correctness this
// depends on. The result stored in the LRU is the POST-withholding one,
// so a key that ignores capabilities would keep serving one caller's
// unredacted result after their capability was REVOKED — for the whole
// 60s TTL.
func TestSearchCache_KeyedOnCapabilities(t *testing.T) {
	ref := hwStranger
	base := Query{Text: hwPhrase, Limit: 25, CallerUserRef: &ref}
	withCap := base
	withCap.Caps = visibility.ContentCaps{ContentReadAll: true}
	if keyForQuery(base) == keyForQuery(withCap) {
		t.Fatal("the search cache key ignores content capabilities: a caller who LOSES " +
			"content.read.all would keep being served the cached unredacted titles " +
			"and thumbhashes until the entry expired")
	}
}

// TestSuggest_RestrictedTitleNeverCompletes covers the sharpest surface
// of the three. /search/suggest takes a PREFIX, so before #899 any
// signed-in caller could reconstruct a restricted asset's title letter
// by letter without ever touching /assets/{id}.
func TestSuggest_RestrictedTitleNeverCompletes(t *testing.T) {
	pool := coPool(t)
	hwSeedOwner(t, pool)
	hwSeedAsset(t, pool, hwPhrase+" unreleased boss theme", "restricted")
	hwSeedAsset(t, pool, hwPhrase+" public splash art", "public")

	svc := suggest.NewService(pool)
	ask := func(ref *int64, caps visibility.ContentCaps) []string {
		t.Helper()
		resp, err := svc.Suggest(context.Background(), suggest.Request{
			Prefix: hwPhrase,
			Caller: visibility.NewCaller(ref),
			Caps:   caps,
			Limit:  10,
		})
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		out := make([]string, 0, len(resp.Suggestions))
		for _, s := range resp.Suggestions {
			if s.Kind == suggest.KindAssetTitle {
				out = append(out, s.Value)
			}
		}
		sort.Strings(out)
		return out
	}

	stranger, owner := hwStranger, hwOwner
	got := ask(&stranger, visibility.ContentCaps{})
	for _, v := range got {
		if strings.Contains(v, "boss theme") {
			t.Errorf("suggest completed a restricted asset's title for a stranger: %q", v)
		}
	}
	// A suggestion is DROPPED, not blanked — but the public one must
	// still be there, or the fix is "turn off suggestions".
	found := false
	for _, v := range got {
		if strings.Contains(v, "splash art") {
			found = true
		}
	}
	if !found {
		t.Errorf("the public asset's title stopped completing — got %v", got)
	}

	// The owner and content.read.all still complete their own.
	for _, c := range []struct {
		name string
		ref  *int64
		caps visibility.ContentCaps
	}{
		{"the owner", &owner, visibility.ContentCaps{}},
		{"content.read.all", &stranger, visibility.ContentCaps{ContentReadAll: true}},
	} {
		hit := false
		for _, v := range ask(c.ref, c.caps) {
			if strings.Contains(v, "boss theme") {
				hit = true
			}
		}
		if !hit {
			t.Errorf("%s: lost a completion for an asset they may read", c.name)
		}
	}
}

// TestFacets_CountOnlyWhatTheCallerCanOpen is the judgement call this
// sprint had to make, written down as a test so the reasoning is
// checkable rather than merely asserted in a comment.
//
// A facet does not report existence — existence is already disclosed by
// ADR 0064 keeping the row listed. It reports a PROPERTY as a filterable
// dimension: `sensitivity: restricted 1` states the item's tier,
// `extension: ogg 1` states its file type. Both are fields #899
// withholds from that same asset's payload. With a narrow enough query
// the aggregate IS the item.
func TestFacets_CountOnlyWhatTheCallerCanOpen(t *testing.T) {
	pool := coPool(t)
	hwSeedOwner(t, pool)
	restrictedID := hwSeedAsset(t, pool, hwPhrase+" unreleased boss theme", "restricted")
	hwSeedAsset(t, pool, hwPhrase+" public splash art", "public")

	d := facet.NewDispatcher(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ask := func(ref *int64, caps visibility.ContentCaps) map[string]map[string]int64 {
		t.Helper()
		resp := d.Run(context.Background(), facet.Request{
			QueryText: hwPhrase,
			Caller:    visibility.NewCaller(ref),
			Caps:      caps,
		})
		out := map[string]map[string]int64{}
		for name, f := range resp.Facets {
			m := map[string]int64{}
			for _, b := range f.Buckets {
				m[b.Value] = b.Count
			}
			out[string(name)] = m
		}
		return out
	}

	stranger, owner := hwStranger, hwOwner

	got := ask(&stranger, visibility.ContentCaps{})
	if n := got["sensitivity"]["restricted"]; n != 0 {
		t.Errorf("the sensitivity facet reported restricted=%d to a caller who can open none "+
			"of them — that discloses the item's TIER as a filterable dimension", n)
	}
	if n := got["sensitivity"]["public"]; n != 1 {
		t.Errorf("the sensitivity facet lost the public bucket (got %d, want 1) — the "+
			"narrowing is too wide", n)
	}
	// The same reasoning applied to the other asset facets, which
	// answer the same question about the same row.
	if n := got["extension"]["ogg"]; n != 1 {
		t.Errorf("extension facet counted %d ogg rows for a stranger, want 1 (the public one "+
			"only) — the restricted row's file type is a withheld field", n)
	}

	// The owner still sees their own restricted work counted, which is
	// the case that makes "just drop the sensitivity facet" wrong.
	ownerGot := ask(&owner, visibility.ContentCaps{})
	if n := ownerGot["sensitivity"]["restricted"]; n != 1 {
		t.Errorf("the OWNER lost the restricted bucket for their own work (got %d, want 1)", n)
	}
	// And so does a content.read.all holder.
	capGot := ask(&stranger, visibility.ContentCaps{ContentReadAll: true})
	if n := capGot["sensitivity"]["restricted"]; n != 1 {
		t.Errorf("content.read.all lost the restricted bucket (got %d, want 1)", n)
	}

	// ── #907: the same floor, now that a count can be TICKED ──────────
	//
	// The reasoning above was about a count. The moment a bucket became
	// a control, the identical question arrives on the result set:
	// `extension: ogg` asks which of these rows is an ogg, and answering
	// it about a row whose columns are withheld hands over the field
	// #899 removed from that row's payload. The count and the filter
	// must therefore make the SAME decision — and, separately, the two
	// numbers must agree, which is the whole of #907.
	//
	// Driven as a POSITIVE CONTROL: one facet value, two callers,
	// opposite verdicts. A test where both callers get the same answer
	// proves only that the query ran.
	engine := NewEngine(pool)
	sel := facet.Selection{}.With(facet.FacetExtension, "ogg")
	run := func(ref *int64, caps visibility.ContentCaps, filters facet.Selection) QueryResult {
		t.Helper()
		res, err := engine.Run(context.Background(), Query{
			Text:          hwPhrase,
			Types:         []HitType{HitTypeAsset},
			Limit:         50,
			CallerUserRef: ref,
			Caps:          caps,
			Filters:       filters,
		})
		if err != nil {
			t.Fatalf("engine: %v", err)
		}
		return res
	}
	has := func(res QueryResult, id uuid.UUID) bool {
		for _, h := range res.Hits {
			if h.ID == id {
				return true
			}
		}
		return false
	}

	// The control used to be "the stranger's UNFILTERED search DOES list
	// the restricted row, as the ADR 0064 placeholder", so its absence
	// under the filter was the filter's doing. #902 retired that: a text
	// match against a row whose fields the caller may not read IS the
	// disclosure, so the stranger no longer matches it filtered OR
	// unfiltered, and the assertion below is now closed twice over. It
	// stays, because it is the property /search owes.
	//
	// The control moved to the OWNER, where it still discriminates
	// between "the filter is correctly narrow" and "the filter returns
	// nothing to anybody".
	if !has(run(&owner, visibility.ContentCaps{}, facet.Selection{}), restrictedID) {
		t.Fatalf("the owner's unfiltered search dropped their own restricted row, so the " +
			"filtered assertions below prove nothing")
	}
	if has(run(&stranger, visibility.ContentCaps{}, facet.Selection{}), restrictedID) {
		t.Errorf("the stranger's UNFILTERED text search still matches the restricted " +
			"row — that is the #902 oracle")
	}

	strangerFiltered := run(&stranger, visibility.ContentCaps{}, sel)
	if has(strangerFiltered, restrictedID) {
		t.Errorf("filtering by `extension: ogg` returned the restricted asset to a caller " +
			"who cannot open it — the selection just told them its file type, which is " +
			"exactly the field the facet COUNT withholds from the same caller")
	}
	if n := len(strangerFiltered.Hits); n != 1 {
		t.Errorf("stranger got %d hits under extension:ogg, want 1 (the public row)", n)
	}
	// The opposite verdict, same value: the owner keeps their own row.
	ownerFiltered := run(&owner, visibility.ContentCaps{}, sel)
	if !has(ownerFiltered, restrictedID) {
		t.Errorf("filtering by `extension: ogg` LOST the owner's own restricted asset — " +
			"the narrowing is by facet value, never by readability for rows the caller " +
			"can read")
	}
	if n := len(ownerFiltered.Hits); n != 2 {
		t.Errorf("owner got %d hits under extension:ogg, want 2", n)
	}
	// And a content.read.all holder is on the owner's side of the line.
	capFiltered := run(&stranger, visibility.ContentCaps{ContentReadAll: true}, sel)
	if !has(capFiltered, restrictedID) {
		t.Errorf("content.read.all lost the restricted asset under a filter")
	}

	// THE INVARIANT #907 EXISTS FOR: the number on the rail is the
	// number of results ticking it returns. Asserted directly against
	// both callers' own counts — a per-caller equality, because the two
	// callers legitimately see different numbers and an assertion that
	// held for only one of them would hide the leak above.
	for _, c := range []struct {
		name  string
		ref   *int64
		caps  visibility.ContentCaps
		count int64
		total int
	}{
		{"stranger", &stranger, visibility.ContentCaps{}, got["extension"]["ogg"], strangerFiltered.TotalCount},
		{"owner", &owner, visibility.ContentCaps{}, ownerGot["extension"]["ogg"], ownerFiltered.TotalCount},
		{"content.read.all", &stranger, visibility.ContentCaps{ContentReadAll: true}, capGot["extension"]["ogg"], capFiltered.TotalCount},
	} {
		if int(c.count) != c.total {
			t.Errorf("%s: the `extension: ogg` bucket says %d but ticking it returns %d — "+
				"a facet count that disagrees with the filter beside it is the whole of "+
				"#907, one level up", c.name, c.count, c.total)
		}
	}
}
