// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The seeder carries the maker's AI declaration (#1251 slice 3, ADR
// 0094), so a freshly seeded instance has something for the browse
// footer's "Hide AI-made work" toggle to hide.
//
// # The defect this guards is SILENCE, and it has a name
//
// #1217. The mature axis shipped its column, its predicate and its UI,
// and then sat unexercised on every seeded instance for months — because
// `manifestAsset` did not model the key, and an unmodelled key is
// dropped by encoding/json without a word. The seed reported success,
// every count was right, and the feature was simply never reachable.
//
// The AI axis is at the same risk for the same reason and one worse: its
// absent state is INDISTINGUISHABLE from its working state on a corpus
// where nobody has declared anything, which is every corpus this project
// has. A toggle that hides nothing looks exactly like a toggle that
// hides nothing because there is nothing to hide.
//
// So there are two halves here and both are necessary:
//
//   - TestManifestAsset_CarriesAIDeclaration — the DECODE. A key in the
//     catalogue reaches the struct at all.
//   - TestSeedInsertAsset_WritesDeclarationAndDerivesPurity — the WRITE
//     and the DERIVATION. The value reaches the column, and the triggers
//     turn it into the `posts.ai_pure` the filter reads. This is the
//     assertion that a decode test alone cannot make: a field that
//     decodes and is never bound to a placeholder is exactly as invisible
//     as one that never decoded.
//   - TestSeedProfiles_DeclareTheDemonstrablePair — the DATA. The
//     committed profiles actually name assets, so the pipeline having a
//     path for declarations is not mistaken for the path being used.

package seed

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

// ---------------------------------------------------------------------------
// The decode
// ---------------------------------------------------------------------------

// TestManifestAsset_CarriesAIDeclaration drives the exact decoder the
// seeder uses over the exact three shapes a catalogue entry can take.
//
// ⚠️ ABSENT AND `null` BOTH MEAN UNDECLARED, AND NEITHER MEANS `none`.
// The column is nullable and unbackfilled precisely so a row predating
// the feature does not assert a disclaimer its maker never made, and a
// `string` field here would have collapsed both into `""` — a value the
// seeder would then have had to invent a rule for, and the obvious rule
// (`"" -> 'none'`) is the lie the nullable column exists to prevent.
func TestManifestAsset_CarriesAIDeclaration(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *string
	}{
		{"declared generated", `{"id":"a","ai_provenance":"generated"}`, ptr("generated")},
		{"declared assisted", `{"id":"a","ai_provenance":"assisted"}`, ptr("assisted")},
		{"declared none", `{"id":"a","ai_provenance":"none"}`, ptr("none")},
		{"absent means UNDECLARED", `{"id":"a"}`, nil},
		{"explicit null means UNDECLARED", `{"id":"a","ai_provenance":null}`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got manifestAsset
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			switch {
			case tc.want == nil && got.AiProvenance != nil:
				t.Errorf("decoded %q, want UNDECLARED (nil) — an absent declaration must "+
					"not become a claim on the maker's behalf", *got.AiProvenance)
			case tc.want != nil && got.AiProvenance == nil:
				t.Errorf("decoded nil, want %q — the key is being dropped, which is #1217's "+
					"defect: the axis ships and no seeded instance can exercise it", *tc.want)
			case tc.want != nil && *got.AiProvenance != *tc.want:
				t.Errorf("decoded %q, want %q", *got.AiProvenance, *tc.want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// The write, and the derivation it feeds
// ---------------------------------------------------------------------------

func openAIProvenancePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	envOr := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Skipf("dev Postgres not reachable (%v); skipping", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestSeedInsertAsset_WritesDeclarationAndDerivesPurity runs the
// seeder's OWN insert query — not a hand-written one — and then asserts
// the fact the browse filter actually reads.
//
// ⭐ IT ASSERTS `posts.ai_pure`, NOT `assets.ai_provenance`, and the
// difference is the whole test. A seeder that wrote the asset column
// correctly and a filter keyed on the post column are two halves that
// have to MEET, and the thing that joins them is a trigger — the one
// piece neither the seeder's code nor the filter's code contains. So the
// assertion is made where the join is observable: one post whose only
// contributor is declared `generated` must derive `ai_pure = true`, and
// one whose second contributor is UNDECLARED must not.
//
// That mixed row is the owner's ruling, and it is the row a plausible
// wrong seeder still gets right: this test would pass on a seeder that
// wrote nothing at all if it only checked the pure case, because
// `ai_pure` defaults to false and "the mixed post is not pure" would
// hold vacuously. Hence both, and hence the pure case is the one whose
// failure means the write never happened.
func TestSeedInsertAsset_WritesDeclarationAndDerivesPurity(t *testing.T) {
	pool := openAIProvenancePool(t)
	ctx := context.Background()
	q := New(pool)

	// The seeder always stamps acquisition_source (ADR 0095's fixture
	// sweep partitions the asset table on exactly that key), so the
	// fixture carries it too — a declared seeded asset must carry BOTH,
	// and a test that dropped the stamp would model an asset the seeder
	// never writes.
	metadata := []byte(`{"acquisition_source":"test-fixture"}`)

	// `assets.created_at` / `updated_at` are NOT NULL with no default —
	// the seeder always supplies them (Runner.rowTimes carries the
	// catalogue's own dates so a seeded library has a history). A fixture
	// that omitted them would fail on the constraint rather than on
	// anything this test is about.
	at := pgtype.Timestamptz{Time: time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC), Valid: true}

	insert := func(decl *string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		title, status, ext := "ai-seed-fixture", "active", "png"
		// ⚠️ NO `file_hash`. `assets.file_hash` is a FOREIGN KEY into
		// `storage_objects`, so an invented hash is a 23503 rather than a
		// plausible fixture — and staging real bytes would be a test of
		// the storage service, which is not what this file is about. The
		// column is nullable; the declaration is what is under test.
		var size int64 = 1
		got, err := q.SeedInsertAsset(ctx, SeedInsertAssetParams{
			ID:            pgtype.UUID{Bytes: id, Valid: true},
			Title:         title,
			AssetType:     1,
			Status:        status,
			FileExtension: &ext,
			FileSizeBytes: &size,
			Metadata:      metadata,
			Sensitivity:   "public",
			AiProvenance:  decl,
			CreatedAt:     at,
			UpdatedAt:     at,
		})
		if err != nil {
			t.Fatalf("SeedInsertAsset: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id)
		})
		if uuid.UUID(got.Bytes) != id {
			t.Fatalf("SeedInsertAsset returned %v, want %v", uuid.UUID(got.Bytes), id)
		}
		return id
	}

	gen := insert(ptr("generated"))
	gen2 := insert(ptr("generated"))
	undeclared := insert(nil)

	// The asset column first, because everything below depends on it.
	for _, tc := range []struct {
		id   uuid.UUID
		want *string
	}{{gen, ptr("generated")}, {undeclared, nil}} {
		var got *string
		if err := pool.QueryRow(ctx,
			`SELECT ai_provenance FROM assets WHERE id=$1`, tc.id).Scan(&got); err != nil {
			t.Fatalf("read asset declaration: %v", err)
		}
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("asset %v stored %q, want NULL (undeclared)", tc.id, *got)
		case tc.want != nil && got == nil:
			t.Errorf("asset %v stored NULL, want %q — the seeder is not binding the "+
				"declaration, so no seeded instance can exercise the axis", tc.id, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("asset %v stored %q, want %q", tc.id, *got, *tc.want)
		}
	}

	// ⭐ And now the fact the FILTER reads.
	plantPost := func(members ...uuid.UUID) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO posts (id, author_user_ref, title, description, visibility, cover_asset_id)
			 VALUES ($1, 12510777, 'ai seed post', '', 'public', $2)`, id, members[0]); err != nil {
			t.Fatalf("plant post: %v", err)
		}
		for i, m := range members {
			if _, err := pool.Exec(ctx,
				`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1,$2,$3)`,
				id, m, i); err != nil {
				t.Fatalf("plant membership: %v", err)
			}
		}
		t.Cleanup(func() {
			c := context.Background()
			_, _ = pool.Exec(c, `DELETE FROM post_assets WHERE post_id=$1`, id)
			_, _ = pool.Exec(c, `DELETE FROM posts WHERE id=$1`, id)
		})
		return id
	}

	purePost := plantPost(gen, gen2)
	mixedPost := plantPost(gen, undeclared)

	for _, tc := range []struct {
		name string
		id   uuid.UUID
		pure bool
		prov string
	}{
		{"every contributor declared `generated`", purePost, true, "generated"},
		{"one declared, one UNDECLARED", mixedPost, false, "generated"},
	} {
		var prov *string
		var pure bool
		if err := pool.QueryRow(ctx,
			`SELECT ai_provenance, ai_pure FROM posts WHERE id=$1`, tc.id).Scan(&prov, &pure); err != nil {
			t.Fatalf("read derived post facts: %v", err)
		}
		gotProv := ""
		if prov != nil {
			gotProv = *prov
		}
		if pure != tc.pure || gotProv != tc.prov {
			t.Errorf("%s: derived ai_pure=%v ai_provenance=%q, want %v / %q",
				tc.name, pure, gotProv, tc.pure, tc.prov)
		}
	}
}

// ---------------------------------------------------------------------------
// The data
// ---------------------------------------------------------------------------

// TestSeedProfiles_DeclareTheDemonstrablePair asserts the committed
// catalogues actually carry declarations, on both dogfood profiles and
// on both of their published aliases.
//
// ⚠️ WITHOUT THIS THE OTHER TWO TESTS ARE SATISFIED BY AN EMPTY
// CATALOGUE. "The seeder can carry a declaration" and "some asset is
// declared" are different claims, and the first one passing is exactly
// the state #1217 sat in: the plumbing was fine, nothing flowed through
// it, and every test was green.
//
// It also asserts the ALIASES agree, because `demo` and `dev` are
// byte-for-byte copies of `studio-a` and `studio-b` (seed/README.md) and
// the failure mode is documented: #572 shipped a demo profile 36 records
// behind its source because an upgrade landed on the dogfood pair and
// missed the aliases. A declaration is exactly the kind of small edit
// that goes the same way.
func TestSeedProfiles_DeclareTheDemonstrablePair(t *testing.T) {
	type entry struct {
		ID           string          `json:"id"`
		AiProvenance *string         `json:"ai_provenance"`
		Metadata     json.RawMessage `json:"metadata"`
	}
	read := func(path string) []entry {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var out []entry
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		return out
	}

	declaredIn := func(path string) map[string]string {
		out := map[string]string{}
		for _, e := range read(path) {
			if e.AiProvenance == nil {
				continue
			}
			out[e.ID] = *e.AiProvenance
			// ⛔ ADR 0095. A seeded asset without
			// `metadata.acquisition_source` is indistinguishable from
			// real uploaded content to the fixture sweep, which would
			// then classify it as a fixture and be free to delete it.
			// Declaring AI on an asset must not cost it that stamp.
			var meta map[string]any
			if err := json.Unmarshal(e.Metadata, &meta); err != nil {
				t.Errorf("%s: asset %s has unparseable metadata: %v", path, e.ID, err)
				continue
			}
			if _, ok := meta["acquisition_source"]; !ok {
				t.Errorf("%s: declared asset %s has NO metadata.acquisition_source. The "+
					"fixture sweep partitions the asset table on that key alone (ADR "+
					"0095), so this row is sweep-bait — a seeded asset carries BOTH.",
					path, e.ID)
			}
		}
		return out
	}

	for _, pair := range [][2]string{
		{"../../../seed/profiles/studio-a.assets.json", "../../../seed/profiles/demo.assets.json"},
		{"../../../seed/profiles/studio-b.assets.json", "../../../seed/profiles/dev.assets.json"},
	} {
		src, alias := declaredIn(pair[0]), declaredIn(pair[1])
		if len(src) == 0 {
			t.Errorf("%s declares no AI provenance on any asset. The browse footer's "+
				"hide toggle then has nothing to hide on a seeded instance, and a "+
				"working control is indistinguishable from a broken one.", pair[0])
		}
		// The pair the toggle demonstrates: at least one `generated`, so
		// at least one post can derive `ai_pure`.
		gens := 0
		for _, v := range src {
			if v == "generated" {
				gens++
			}
		}
		if gens < 2 {
			t.Errorf("%s declares %d assets `generated`; the demonstration needs at least "+
				"two — one sole contributor (the PURE post) and one of several (the MIXED "+
				"post that must STAY visible).", pair[0], gens)
		}
		if len(src) != len(alias) {
			t.Errorf("%s declares %d assets and its alias %s declares %d. The two are "+
				"byte-for-byte copies (seed/README.md); an upgrade that lands on one and "+
				"not the other is #572 repeating.", pair[0], len(src), pair[1], len(alias))
		}
		for id, want := range src {
			if got, ok := alias[id]; !ok || got != want {
				t.Errorf("%s declares %s=%q; alias %s has %q (present=%v)",
					pair[0], id, want, pair[1], got, ok)
			}
		}
	}
}
