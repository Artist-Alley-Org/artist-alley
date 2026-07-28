// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #661 — `GET /assets/{id}/similar` performed NO identity check.
//
// Three omissions, all on one route that sits on the public-mode
// allowlist (`/assets` prefix, auth/publicmode.go):
//
//   - the anchor was checked for EXISTENCE with a bare
//     `SELECT EXISTS (... WHERE id = $1 AND deleted_at IS NULL)`, so any
//     asset id worked as an anchor — draft, archived, still-processing,
//     `restricted` — and answered 200-vs-404 about ids the caller may
//     not see;
//   - FindSimilarByAnchor takes no caller, so the kNN cannot filter;
//   - the neighbour re-fetch selected the FULL openapi.Asset projection
//     with `deleted_at IS NULL` as its whole WHERE clause.
//
// # Why this was called unreproducible, and why that was wrong
//
// asset_embedding_d768 is empty on a dev box, so every request
// short-circuits on ErrAnchorHasNoEmbedding and returns
// `{results: [], anchor_has_embedding: false}`. Reading that as "safe"
// is the reasoning epic #665 exists to reject: the endpoint is DORMANT,
// and it starts leaking on the first day the embedding worker runs.
//
// So these tests seed the embeddings themselves — real rows in
// asset_embedding_d768, driven through the real embeddings.Reader — and
// the leak reproduces immediately. Every assertion below FAILS on the
// pre-#661 handler.
//
// The counterweight is TestSimilar_AuthenticatedNotNarrowed: the
// EntityAsset predicate for an authenticated caller is `deleted_at IS
// NULL` and nothing more (ADR 0063 — listed but blurred, gated at the
// content plane per ADR 0064). Tightening the anonymous path must not
// tighten theirs; signing in may never remove access, which is the
// inversion #451 had to undo for collections.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package assets_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/ai/embeddings"
	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
)

const (
	simOwner int64 = 4290661
	// The (provider, model, modality) tuple the handler hard-codes /
	// reads from system_config. Seeding under the same tuple is what
	// makes the kNN find these rows.
	simProvider = "router"
	simModel    = "nomic-embed-text"
	simModality = "text"
	simDim      = 768
)

// simSeed is one planted asset. The three columns the anonymous
// EntityAsset predicate reads, plus the expectation.
type simSeed struct {
	label       string
	status      string
	sensitivity string
	processing  string
	deleted     bool
	// anonVisible mirrors visibility/predicate.go's EntityAsset
	// anonymous branch: active + public + ready + not deleted.
	anonVisible bool
}

var simSeeds = []simSeed{
	// The anchor an anonymous caller is allowed to use.
	{label: "anchor-public", status: "active", sensitivity: "public", processing: "ready", anonVisible: true},
	// Neighbours across every dimension the predicate gates on.
	{label: "n-public", status: "active", sensitivity: "public", processing: "ready", anonVisible: true},
	{label: "n-draft", status: "draft", sensitivity: "public", processing: "ready"},
	{label: "n-archived", status: "archived", sensitivity: "public", processing: "ready"},
	{label: "n-processing", status: "active", sensitivity: "public", processing: "processing"},
	{label: "n-restricted", status: "active", sensitivity: "restricted", processing: "ready"},
	{label: "n-team", status: "active", sensitivity: "team", processing: "ready"},
	{label: "n-deleted", status: "active", sensitivity: "public", processing: "ready", deleted: true},
	// A hidden ANCHOR. Using it must 404 for anonymous callers: the
	// endpoint may not confirm that a draft asset exists, and it may
	// certainly not seed a neighbour list from one.
	{label: "anchor-draft", status: "draft", sensitivity: "public", processing: "ready"},
}

// simVector builds a deterministic unit-ish vector for one seed. Every
// vector is close to every other (all coordinates positive, one index
// carrying the variation) so the kNN returns the WHOLE set within the
// default limit — a fixture where the hidden rows fell off the end for
// distance reasons would pass vacuously.
func simVector(i int) string {
	parts := make([]string, simDim)
	for d := range parts {
		parts[d] = "0.01"
	}
	parts[i%simDim] = "1"
	return "[" + strings.Join(parts, ",") + "]"
}

// seedSimilarAssets plants the assets AND their embeddings, and returns
// label -> id. Seeding the embedding table is the whole point: without
// it the handler short-circuits and every assertion here is vacuous.
func seedSimilarAssets(t *testing.T, pool *pgxpool.Pool) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := make(map[string]uuid.UUID, len(simSeeds))
	var hashes []string

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM asset_embedding_d768 WHERE asset_id IN (SELECT id FROM assets WHERE owner_user_ref=$1)`, simOwner)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE owner_user_ref=$1`, simOwner)
		_, _ = pool.Exec(bg, `DELETE FROM storage_objects WHERE hash = ANY($1::text[])`, hashes)
	})

	// The dim registry + default model live in system_config; a fresh
	// test database has migration 00011's seed values, but pin them
	// here so the fixture does not depend on a seed staying put.
	if _, err := pool.Exec(ctx, `
		INSERT INTO system_config (key, value) VALUES
			('ai.embedding.dim_registry', $1::jsonb),
			('ai.embedding.default_model', $2::jsonb)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,
		fmt.Sprintf(`{"%s": %d}`, simModel, simDim),
		fmt.Sprintf(`"%s"`, simModel),
	); err != nil {
		t.Fatalf("seed ai config: %v", err)
	}

	for i, s := range simSeeds {
		id := uuid.New()
		ids[s.label] = id

		sum := sha256.Sum256([]byte("#661 similar " + id.String()))
		hash := hex.EncodeToString(sum[:])
		hashes = append(hashes, hash)
		if _, err := pool.Exec(ctx, `
			INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
			VALUES ($1, 1, 'image/webp', 'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
			t.Fatalf("seed object %s: %v", s.label, err)
		}

		del := "NULL"
		if s.deleted {
			del = "NOW()"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
			                    processing_status, file_hash, created_at, deleted_at)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),$4,$5,$6,$7,
			        NOW() - ($8::int * INTERVAL '1 minute'), `+del+`)`,
			id, "#661 "+s.label, simOwner, s.status, s.sensitivity, s.processing, hash, i); err != nil {
			t.Fatalf("seed asset %s: %v", s.label, err)
		}

		// THE STEP THAT MAKES THIS REPRODUCIBLE.
		if _, err := pool.Exec(ctx, `
			INSERT INTO asset_embedding_d768
				(asset_id, provider, model, modality, embedding, updated_at)
			VALUES ($1,$2,$3,$4,$5::vector,NOW())
			ON CONFLICT (asset_id, provider, model, modality) DO UPDATE
				SET embedding = EXCLUDED.embedding`,
			id, simProvider, simModel, simModality, simVector(i)); err != nil {
			t.Fatalf("seed embedding %s: %v", s.label, err)
		}
	}
	return ids
}

// simReaderAdapter mirrors the boot-time adapter in http/api.go so the
// test drives the REAL embeddings.Reader against the REAL tables.
type simReaderAdapter struct{ r *embeddings.Reader }

func (a simReaderAdapter) HasEmbedding(ctx context.Context, anchorID uuid.UUID, provider, model, modality string) (bool, error) {
	return a.r.HasEmbedding(ctx, anchorID, provider, model, modality)
}

func (a simReaderAdapter) FindSimilarByAnchor(ctx context.Context, anchorID uuid.UUID, provider, model, modality string, limit int) ([]assets.SimilarNeighbour, error) {
	ns, err := a.r.FindSimilarByAnchor(ctx, anchorID, provider, model, modality, limit)
	if err != nil {
		return nil, err
	}
	out := make([]assets.SimilarNeighbour, 0, len(ns))
	for _, n := range ns {
		out = append(out, assets.SimilarNeighbour{AssetID: n.AssetID, Distance: n.Distance})
	}
	return out, nil
}

// simRouter wires the real strict-server stack for one caller. A nil
// identity is anonymous — exactly what the resolver leaves behind for
// an anonymous request on a public-mode install.
func simRouter(t *testing.T, pool *pgxpool.Pool, id *auth.Identity) chi.Router {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := assets.NewHandler(pool, nil, logger, nil, nil, nil)

	dims := embeddings.NewDimRegistry(pool)
	if _, err := dims.Refresh(t.Context()); err != nil {
		t.Fatalf("dim registry refresh: %v", err)
	}
	h.SetSimilarReader(simReaderAdapter{r: embeddings.NewReader(pool, dims)})

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

// getSimilar issues one request and returns (status, payload).
func getSimilar(t *testing.T, router chi.Router, anchor uuid.UUID) (int, openapi.SimilarAssets) {
	t.Helper()
	url := "/assets/" + anchor.String() + "/similar?limit=50"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	var out openapi.SimilarAssets
	if rr.Code == http.StatusOK {
		mustDecode(t, rr.Body.Bytes(), &out)
	}
	return rr.Code, out
}

// TestSimilar_AnonymousSeesOnlyVisibleNeighbours is the #661 regression
// test for the neighbour re-fetch. It fails on the pre-fix handler,
// which returned the full Asset projection for every seeded row.
func TestSimilar_AnonymousSeesOnlyVisibleNeighbours(t *testing.T) {
	pool := byTagPool(t)
	ids := seedSimilarAssets(t, pool)
	anon := simRouter(t, pool, nil)

	status, got := getSimilar(t, anon, ids["anchor-public"])
	if status != http.StatusOK {
		t.Fatalf("anonymous similar on a public anchor: status=%d, want 200", status)
	}

	// Non-vacuity, twice over. The endpoint must actually have run the
	// kNN, and it must actually have returned the one neighbour an
	// anonymous caller may see — otherwise "no leak" is just "no data",
	// which is the reasoning this issue forbids.
	if !got.AnchorHasEmbedding {
		t.Fatal("anchor_has_embedding=false — the fixture failed to seed embeddings, so every assertion below is vacuous")
	}
	byID := make(map[uuid.UUID]openapi.Asset, len(got.Results))
	for _, r := range got.Results {
		byID[r.Asset.Id] = r.Asset
	}
	if _, ok := byID[ids["n-public"]]; !ok {
		t.Fatalf("anonymous similar dropped the public neighbour — the gate over-narrowed (got %d results)", len(got.Results))
	}

	for _, s := range simSeeds {
		if s.anonVisible || s.label == "anchor-draft" {
			continue
		}
		if a, ok := byID[ids[s.label]]; ok {
			t.Errorf("anonymous /similar leaked %q asset %v (status=%s processing=%s title=%q)",
				s.label, a.Id, a.Status, a.ProcessingStatus, a.Title)
		}
	}

	// Belt and braces on the two dimensions the response surfaces, so a
	// fixture that grows a row the table forgets is still caught.
	for _, r := range got.Results {
		if string(r.Asset.Status) != "active" {
			t.Errorf("anonymous /similar returned non-active asset %v status=%s", r.Asset.Id, r.Asset.Status)
		}
		if string(r.Asset.ProcessingStatus) != "ready" {
			t.Errorf("anonymous /similar returned not-ready asset %v processing_status=%s", r.Asset.Id, r.Asset.ProcessingStatus)
		}
	}
}

// TestSimilar_AnonymousHiddenAnchorIs404 is the #661 regression test for
// the anchor check. On the pre-fix handler a draft anchor returned 200
// with a full neighbour list; it must 404, and 404 rather than 403 so
// the response does not confirm that a hidden asset exists at that id.
func TestSimilar_AnonymousHiddenAnchorIs404(t *testing.T) {
	pool := byTagPool(t)
	ids := seedSimilarAssets(t, pool)
	anon := simRouter(t, pool, nil)

	status, got := getSimilar(t, anon, ids["anchor-draft"])
	if status != http.StatusNotFound {
		t.Errorf("anonymous /similar on a DRAFT anchor: status=%d (%d results), want 404 — "+
			"a hidden asset may not be used as an anchor, nor confirmed to exist",
			status, len(got.Results))
	}

	// Same for an id that does not exist at all: the two cases must be
	// indistinguishable or the endpoint is an existence oracle.
	if s, _ := getSimilar(t, anon, uuid.New()); s != http.StatusNotFound {
		t.Errorf("anonymous /similar on a NONEXISTENT anchor: status=%d, want 404", s)
	}
}

// TestSimilar_SubsetOfBrowse ties the endpoint to the invariant epic
// #665 names: a list path may never return a row the corresponding
// browse would withhold from the same caller. Stated set-theoretically
// so it survives changes to the predicate itself.
func TestSimilar_SubsetOfBrowse(t *testing.T) {
	pool := byTagPool(t)
	ids := seedSimilarAssets(t, pool)

	for _, tc := range []struct {
		name string
		id   *auth.Identity
	}{
		{"anonymous", nil},
		{"authenticated stranger", &auth.Identity{UserRef: simOwner + 1, AuthMethod: "session"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := simRouter(t, pool, tc.id)

			// The browse page for the same owner, as the same caller.
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
				"/assets?limit=200&owner_ref="+fmt.Sprint(simOwner), nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("browse: status=%d body=%s", rr.Code, rr.Body.String())
			}
			var list openapi.AssetList
			mustDecode(t, rr.Body.Bytes(), &list)
			browse := make(map[uuid.UUID]bool, len(list.Items))
			for _, a := range list.Items {
				browse[a.Id] = true
			}

			status, got := getSimilar(t, router, ids["anchor-public"])
			if status != http.StatusOK {
				t.Fatalf("similar: status=%d", status)
			}
			if len(got.Results) == 0 {
				t.Fatal("similar returned nothing — vacuous")
			}
			for _, r := range got.Results {
				if !browse[r.Asset.Id] {
					t.Errorf("/similar returned asset %v (%q) that GET /assets withholds from the same caller",
						r.Asset.Id, r.Asset.Title)
				}
			}
		})
	}
}

// TestSimilar_AuthenticatedNotNarrowed is the counterweight. The
// EntityAsset predicate for an authenticated caller is soft-delete only
// — a signed-in non-owner still LISTS draft and restricted rows, gated
// at the content plane instead (ADR 0020 / ADR 0064). Fixing the
// anonymous leak must not narrow that, or signing in removes access.
func TestSimilar_AuthenticatedNotNarrowed(t *testing.T) {
	pool := byTagPool(t)
	ids := seedSimilarAssets(t, pool)
	router := simRouter(t, pool, &auth.Identity{UserRef: simOwner + 1, AuthMethod: "session"})

	status, got := getSimilar(t, router, ids["anchor-public"])
	if status != http.StatusOK {
		t.Fatalf("authenticated similar: status=%d, want 200", status)
	}
	byID := make(map[uuid.UUID]bool, len(got.Results))
	for _, r := range got.Results {
		byID[r.Asset.Id] = true
	}
	for _, label := range []string{"n-public", "n-draft", "n-archived", "n-processing", "n-restricted", "n-team"} {
		if !byID[ids[label]] {
			t.Errorf("authenticated /similar dropped %q — the anonymous fix narrowed the authenticated path; "+
				"signing in must never remove access (#451)", label)
		}
	}
	// Soft-deleted stays out on every path.
	if byID[ids["n-deleted"]] {
		t.Error("authenticated /similar returned a soft-deleted asset")
	}
	// And a draft anchor is reachable for an authenticated caller,
	// because the authenticated predicate admits it.
	if s, _ := getSimilar(t, router, ids["anchor-draft"]); s != http.StatusOK {
		t.Errorf("authenticated /similar on a draft anchor: status=%d, want 200", s)
	}
}
