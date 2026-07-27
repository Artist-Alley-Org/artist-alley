// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #657 / #612 — `GET /assets?tag=` must be the SAME read as
// `GET /assets`, narrowed.
//
// It was not. The tag filter ran its own static sqlc query whose whole
// WHERE clause was `deleted_at IS NULL`, so on a public-mode install an
// anonymous visitor got draft, archived, still-processing and
// `restricted` rows back by adding one query parameter — content the
// unfiltered browse withholds. The same omission left
// preview_available / ladder_available false on every tag-filtered row
// (#612), so one asset reported two different answers depending on
// whether a filter was applied.
//
// These tests run against the HTTP surface rather than the query
// helper on purpose: the defect was never in the SQL builder, it was in
// a handler branch choosing a different query. A test at the builder
// level cannot see that choice, and would have stayed green throughout
// the bug's life.
//
// TestListAssetsByTag_AnonymousSubsetOfUnfiltered is the security
// regression test; it fails on the pre-#657 handler.
//
// The counterweight test is TestListAssetsByTag_AuthenticatedNotNarrower.
// For an AUTHENTICATED caller the EntityAsset predicate is only
// `deleted_at IS NULL` — listing restricted/draft rows to signed-in
// callers is deliberate (ADR 0020: listed but blurred, gated at the
// content plane per ADR 0064). Tightening the anonymous path must not
// tighten theirs: signing in may never remove access. That inversion is
// what #451 had to undo for collections.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package assets_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

const (
	byTagOwner int64 = 4290657
	// A tag no other fixture uses, so the filtered page is exactly the
	// set this file planted.
	byTagTag = "aa657-probe"
)

// byTagSeed is one planted asset: a label to assert about, plus the
// three dimensions the anonymous predicate cares about.
type byTagSeed struct {
	label       string
	status      string
	sensitivity string
	processing  string
	deleted     bool
	withCol     bool
	tagged      bool
	// anonVisible mirrors visibility/predicate.go's EntityAsset rule for
	// an anonymous caller: active + public + ready + not deleted.
	anonVisible bool
}

var byTagSeeds = []byTagSeed{
	{label: "public-with-col", status: "active", sensitivity: "public", processing: "ready", withCol: true, tagged: true, anonVisible: true},
	{label: "public-no-col", status: "active", sensitivity: "public", processing: "ready", tagged: true, anonVisible: true},
	{label: "draft", status: "draft", sensitivity: "public", processing: "ready", tagged: true},
	{label: "archived", status: "archived", sensitivity: "public", processing: "ready", tagged: true},
	{label: "processing", status: "active", sensitivity: "public", processing: "processing", tagged: true},
	{label: "restricted", status: "active", sensitivity: "restricted", processing: "ready", withCol: true, tagged: true},
	{label: "team", status: "active", sensitivity: "team", processing: "ready", tagged: true},
	{label: "soft-deleted", status: "active", sensitivity: "public", processing: "ready", deleted: true, tagged: true},
	// Untagged control: proves the unified query still FILTERS. A fix
	// that dropped the tag constraint would satisfy every subset
	// assertion above and be completely wrong.
	{label: "untagged-public", status: "active", sensitivity: "public", processing: "ready", withCol: true, anonVisible: true},
}

// seedByTagAssets plants the spread and returns label -> id.
func seedByTagAssets(t *testing.T, pool *pgxpool.Pool) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := make(map[string]uuid.UUID, len(byTagSeeds))
	var hashes []string

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM asset_tag WHERE asset_id IN (SELECT id FROM assets WHERE owner_user_ref=$1)`, byTagOwner)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE owner_user_ref=$1`, byTagOwner)
		_, _ = pool.Exec(bg, `DELETE FROM storage_variants WHERE object_hash = ANY($1::text[])`, hashes)
		_, _ = pool.Exec(bg, `DELETE FROM storage_objects WHERE hash = ANY($1::text[])`, hashes)
	})

	for i, s := range byTagSeeds {
		id := uuid.New()
		ids[s.label] = id

		// storage_objects.hash is CHECKed against ^[0-9a-f]{64}$, so the
		// fixture needs a real-shaped digest.
		sum := sha256.Sum256([]byte("#657 by-tag " + id.String()))
		hash := hex.EncodeToString(sum[:])
		hashes = append(hashes, hash)
		if _, err := pool.Exec(ctx, `
			INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
			VALUES ($1, 1, 'image/webp', 'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
			t.Fatalf("seed object %s: %v", s.label, err)
		}
		if s.withCol {
			if _, err := pool.Exec(ctx, `
				INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type)
				VALUES ($1, 'col', 1, 'image/webp') ON CONFLICT DO NOTHING`, hash); err != nil {
				t.Fatalf("seed col variant %s: %v", s.label, err)
			}
		}

		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		// Distinct created_at keeps ordering deterministic.
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
			                    processing_status, file_hash, created_at, deleted_at)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5,$6,$7,
			        NOW() - ($8::int * INTERVAL '1 minute'), `+del+`)`,
			id, "#657 "+s.label, byTagOwner, s.status, s.sensitivity, s.processing, hash, i); err != nil {
			t.Fatalf("seed asset %s: %v", s.label, err)
		}
		if s.tagged {
			if _, err := pool.Exec(ctx,
				`INSERT INTO asset_tag (asset_id, tag) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
				id, byTagTag); err != nil {
				t.Fatalf("tag asset %s: %v", s.label, err)
			}
		}
	}
	return ids
}

// byTagRouter wires the real strict-server stack for one caller. A nil
// identity means anonymous — no identity middleware at all, which is
// exactly what the resolver leaves behind for an anonymous request on a
// public-mode install.
func byTagRouter(t *testing.T, pool *pgxpool.Pool, id *auth.Identity) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := assets.NewHandler(pool, nil, logger, nil, nil, nil)
	// A single-rung ladder so ladder_available is a MEANINGFUL true for
	// the seeded `col` asset. With no reader the ladder is empty, the
	// flag is false everywhere, and the #612 equivalence assertion would
	// pass vacuously.
	h.SetPreviewLadder(func(context.Context) []string { return []string{"col"} })

	router := chi.NewRouter()
	if id != nil {
		router.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), id)))
			})
		})
	}
	openapi.HandlerFromMux(openapi.NewStrictHandler(
		shimImpl{PanicShim: &strictservershim.PanicShim{}, assets: h}, nil), router)
	return router
}

// listAssets issues one browse request and returns the items keyed by
// id. `tag` empty means the unfiltered browse.
func listAssets(t *testing.T, router chi.Router, tag string) map[uuid.UUID]openapi.Asset {
	t.Helper()
	url := "/assets?limit=200&owner_ref=" + strconv.FormatInt(byTagOwner, 10)
	if tag != "" {
		url += "&tag=" + tag
	}
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", url, rr.Code, rr.Body.String())
	}
	var list openapi.AssetList
	mustDecode(t, rr.Body.Bytes(), &list)
	out := make(map[uuid.UUID]openapi.Asset, len(list.Items))
	for _, a := range list.Items {
		out[a.Id] = a
	}
	return out
}

func byTagPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)
	return pool
}

// TestListAssetsByTag_AnonymousSubsetOfUnfiltered is the #657
// regression test. The invariant is set-theoretic and therefore
// survives changes to the predicate: whatever the unfiltered anonymous
// browse returns, the tag-filtered browse may only return a SUBSET of
// it. A filter cannot add rows.
func TestListAssetsByTag_AnonymousSubsetOfUnfiltered(t *testing.T) {
	pool := byTagPool(t)
	ids := seedByTagAssets(t, pool)
	anon := byTagRouter(t, pool, nil)

	unfiltered := listAssets(t, anon, "")
	tagged := listAssets(t, anon, byTagTag)

	// Non-vacuity: the filtered page must actually contain the rows an
	// anonymous caller is allowed to see, or every assertion below is
	// trivially satisfied by an empty page.
	for _, s := range byTagSeeds {
		if s.tagged && s.anonVisible {
			if _, ok := tagged[ids[s.label]]; !ok {
				t.Fatalf("anonymous ?tag= dropped %q, which the unfiltered browse serves — the filter over-narrowed", s.label)
			}
		}
	}

	// The subset property itself.
	for id, a := range tagged {
		if _, ok := unfiltered[id]; !ok {
			t.Errorf("anonymous ?tag= returned asset %v (%q) that the unfiltered browse withholds", id, a.Title)
		}
	}

	// And the concrete form of it, so a failure names the dimension.
	for _, s := range byTagSeeds {
		if !s.tagged || s.anonVisible {
			continue
		}
		if a, ok := tagged[ids[s.label]]; ok {
			t.Errorf("anonymous ?tag= leaked %q asset %v (status=%s processing=%s title=%q)",
				s.label, a.Id, a.Status, a.ProcessingStatus, a.Title)
		}
	}

	// Belt and braces on the two dimensions the API surfaces directly,
	// in case the fixture ever grows a row the table above forgets.
	for id, a := range tagged {
		if string(a.Status) != "active" {
			t.Errorf("anonymous ?tag= returned non-active asset %v status=%s", id, a.Status)
		}
		if string(a.ProcessingStatus) != "ready" {
			t.Errorf("anonymous ?tag= returned not-ready asset %v processing_status=%s", id, a.ProcessingStatus)
		}
	}

	// The tag constraint still constrains.
	if _, ok := tagged[ids["untagged-public"]]; ok {
		t.Error("?tag= returned an asset that does not carry the tag — the filter stopped filtering")
	}
}

// TestListAssetsByTag_FlagEquivalence is #612: the SAME asset must
// report the SAME preview_available and ladder_available whether or not
// a tag filter is applied. These flags describe the asset and the
// caller, never the query shape.
func TestListAssetsByTag_FlagEquivalence(t *testing.T) {
	pool := byTagPool(t)
	ids := seedByTagAssets(t, pool)

	for _, tc := range []struct {
		name string
		id   *auth.Identity
	}{
		{"anonymous", nil},
		{"authenticated owner", &auth.Identity{UserRef: byTagOwner, AuthMethod: "session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := byTagRouter(t, pool, tc.id)
			unfiltered := listAssets(t, router, "")
			tagged := listAssets(t, router, byTagTag)

			for id, want := range unfiltered {
				got, ok := tagged[id]
				if !ok {
					continue // legitimately filtered out by the tag
				}
				if got.PreviewAvailable != want.PreviewAvailable {
					t.Errorf("asset %v (%q): preview_available=%v with ?tag=, %v without",
						id, want.Title, got.PreviewAvailable, want.PreviewAvailable)
				}
				if got.LadderAvailable != want.LadderAvailable {
					t.Errorf("asset %v (%q): ladder_available=%v with ?tag=, %v without",
						id, want.Title, got.LadderAvailable, want.LadderAvailable)
				}
			}

			// Non-vacuity: at least one row must report TRUE, or
			// "identical" is just false==false and the flags could still
			// be unwired.
			withCol := tagged[ids["public-with-col"]]
			if !withCol.PreviewAvailable {
				t.Error("preview_available is false for the seeded col-variant asset under ?tag= — the flag is not being computed")
			}
			if !withCol.LadderAvailable {
				t.Error("ladder_available is false for the seeded col-variant asset under ?tag= — the ladder is not reaching this path")
			}
		})
	}
}

// TestListAssetsByTag_AuthenticatedNotNarrower pins the counterweight.
// Fixing the anonymous leak must not shrink what a signed-in caller
// sees: the authenticated EntityAsset predicate is `deleted_at IS NULL`
// and nothing more, so every live asset carrying the tag — draft,
// archived, processing, restricted, team — must still be listed.
//
// The oracle is the OLD query's semantics, spelled out in SQL: the
// pre-#657 by-tag statement returned exactly `deleted_at IS NULL AND
// tag = $1`. Comparing against it is what makes "not narrower"
// checkable rather than asserted.
func TestListAssetsByTag_AuthenticatedNotNarrower(t *testing.T) {
	pool := byTagPool(t)
	ids := seedByTagAssets(t, pool)

	authed := byTagRouter(t, pool, &auth.Identity{UserRef: byTagOwner, AuthMethod: "session"})
	tagged := listAssets(t, authed, byTagTag)

	rows, err := pool.Query(context.Background(), `
		SELECT a.id
		  FROM assets a
		  JOIN asset_tag t ON t.asset_id = a.id
		 WHERE a.deleted_at IS NULL
		   AND t.tag = $1
		   AND a.owner_user_ref = $2`, byTagTag, byTagOwner)
	if err != nil {
		t.Fatalf("oracle query: %v", err)
	}
	defer rows.Close()
	var oracle []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("oracle scan: %v", err)
		}
		oracle = append(oracle, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("oracle rows: %v", err)
	}
	if len(oracle) == 0 {
		t.Fatal("oracle returned nothing; the fixture did not seed")
	}

	for _, id := range oracle {
		if _, ok := tagged[id]; !ok {
			t.Errorf("authenticated ?tag= no longer lists %v — signing in removed access (cf. #451)", id)
		}
	}

	// Named, so a regression says WHICH tier vanished.
	for _, label := range []string{"draft", "archived", "processing", "restricted", "team"} {
		if _, ok := tagged[ids[label]]; !ok {
			t.Errorf("authenticated ?tag= dropped the %q asset; ADR 0020 lists it (blurred), it is gated at the content plane", label)
		}
	}

	// Soft-deleted stays hidden without include_deleted, for both.
	if _, ok := tagged[ids["soft-deleted"]]; ok {
		t.Error("authenticated ?tag= returned a soft-deleted asset without include_deleted")
	}

	// Superset of the anonymous view: whatever anonymous can see, a
	// signed-in caller can see too.
	anonTagged := listAssets(t, byTagRouter(t, pool, nil), byTagTag)
	for id := range anonTagged {
		if _, ok := tagged[id]; !ok {
			t.Errorf("asset %v is visible anonymously but NOT to a signed-in caller — access inverted", id)
		}
	}
}
