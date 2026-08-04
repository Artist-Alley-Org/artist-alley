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

// hwHit runs the engine and returns the serialized hit for one asset.
func hwHit(t *testing.T, e *Engine, ref *int64, caps visibility.ContentCaps, id uuid.UUID) map[string]json.RawMessage {
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
		return m
	}
	t.Fatalf("asset %v is not in the result set — ADR 0064 keeps a restricted row LISTED; "+
		"only its columns are withheld, so dropping it here would be the wrong fix", id)
	return nil
}

// TestSearchHit_RestrictedIsAllowListed is the core assertion, and it
// fails on today's dev, where the hit carried title, summary and the
// thumbhash (a blurred picture of the content).
func TestSearchHit_RestrictedIsAllowListed(t *testing.T) {
	pool := coPool(t)
	display := hwSeedOwner(t, pool)
	restricted := hwSeedAsset(t, pool, hwPhrase+" unreleased boss theme", "restricted")
	public := hwSeedAsset(t, pool, hwPhrase+" public splash art", "public")

	stranger := hwStranger
	m := hwHit(t, NewEngine(pool), &stranger, visibility.ContentCaps{}, restricted)
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

	// The counterweight, in the same test: a withhold-everything
	// implementation passes everything above.
	pubHit := hwHit(t, NewEngine(pool), &stranger, visibility.ContentCaps{}, public)
	for _, want := range []string{"title", "summary", "extra", "created_at"} {
		if _, ok := pubHit[want]; !ok {
			t.Errorf("a PUBLIC hit lost %q — the withholding is too wide", want)
		}
	}
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
	hwSeedAsset(t, pool, hwPhrase+" unreleased boss theme", "restricted")
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
}
