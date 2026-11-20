// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #196 - a manifest's canvas carries the asset's REAL pixel dimensions.
//
// Before, EntityRef.Width/Height were never populated, so every asset
// manifest advertised a 1200x900 canvas whatever the image was, while
// the same asset's info.json reported the true size. A viewer then
// scales the tiles into the wrong canvas: a portrait photo is squashed
// into landscape.
//
// The dimensions are written the way production writes them, through
// asset/pixeldims.Record (the preview pipeline's only writer), and read
// back through the MOUNTED manifest route, so the test checks what a
// IIIF client receives, not the loader's SQL. The malformed rows are
// planted directly because Record refuses them; they stand for data a
// future writer or a manual edit could leave behind. External package
// so it can cross-check against iiif.PoolLookup (info.json's reader),
// which imports this package.
//
// Skips without AA_DB_PASSWORD, same convention as the sibling suites.

package presentation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/asset/pixeldims"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/iiif/presentation"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const dimsOwner int64 = 4290664

func dimsPool(t *testing.T) *pgxpool.Pool {
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
	pool, err := pgxpool.New(t.Context(), "host="+env("AA_DB_HOST", "postgres")+
		" port="+env("AA_DB_PORT", "5432")+" user="+env("AA_DB_USER", "artist_alley")+
		" dbname="+testdb.Name(t)+" sslmode=disable password="+pwd)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedDimsAsset plants an asset; status decides whether an anonymous
// caller may see it ('active' + public + ready is the visible shape).
func seedDimsAsset(t *testing.T, pool *pgxpool.Pool, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	// A file, because the dimensions are measured from one: Record only
	// writes for the bytes the asset shows (#1596).
	hash := fmt.Sprintf("%064x", id[:])
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 1024, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("seed object: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, file_hash, file_extension, created_at)
		VALUES ($1,'#196 dims probe',$2,(SELECT MIN(ref) FROM asset_types),$3,'public','ready',$4,'png',NOW())`,
		id, dimsOwner, status, hash); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM asset_field_value WHERE asset_id = $1`, id)
		_, _ = pool.Exec(bg, `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

// dimsHash is the file the asset shows, which Record is told it measured.
func dimsHash(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var h string
	if err := pool.QueryRow(context.Background(), `SELECT file_hash FROM assets WHERE id = $1`, id).Scan(&h); err != nil {
		t.Fatalf("asset hash: %v", err)
	}
	return h
}

// plantRaw writes one dimension value directly, bypassing Record's
// positivity check, to stand for malformed stored data.
func plantRaw(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, code string, v float64) {
	t.Helper()
	ct, err := pool.Exec(context.Background(), `
		INSERT INTO asset_field_value (asset_id, field_id, value_num, set_by)
		SELECT $1, fd.id, $2::DOUBLE PRECISION, 'computed' FROM field_definition fd WHERE fd.code = $3`,
		id, v, code)
	if err != nil {
		t.Fatalf("plant %s=%v: %v", code, v, err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("no field_definition %q (migration 00017 seeds it)", code)
	}
}

func dimsRoute(pool *pgxpool.Pool) chi.Router {
	h := &presentation.Handler{
		Loader: presentation.NewLoader(pool),
		Builder: presentation.NewBuilder(presentation.BuilderConfig{
			SiteBaseURL: "https://art.example.test",
			Provider:    presentation.Provider{Label: presentation.EN("Test"), Type: "Agent"},
		}),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	r := chi.NewRouter() // no identity middleware: anonymous caller; no cache
	h.Mount(r)
	return r
}

// canvasSizes fetches the manifest and returns (status, canvas w/h,
// painting image body w/h, raw body).
func canvasSizes(t *testing.T, r chi.Router, id uuid.UUID) (int, [2]int, [2]int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/iiif/3/asset/"+id.String()+"/manifest.json", nil))
	if rr.Code != http.StatusOK {
		return rr.Code, [2]int{}, [2]int{}, rr.Body.String()
	}
	var m struct {
		Items []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
			Items  []struct {
				Items []struct {
					Body struct {
						Width  int `json:"width"`
						Height int `json:"height"`
					} `json:"body"`
				} `json:"items"`
			} `json:"items"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, rr.Body.String())
	}
	if len(m.Items) != 1 || len(m.Items[0].Items) != 1 || len(m.Items[0].Items[0].Items) != 1 {
		t.Fatalf("unexpected manifest shape: %s", rr.Body.String())
	}
	c := m.Items[0]
	b := c.Items[0].Items[0].Body
	return rr.Code, [2]int{c.Width, c.Height}, [2]int{b.Width, b.Height}, rr.Body.String()
}

func TestManifest_MeasuredPortrait_CanvasAndImageCarryTheRealSize(t *testing.T) {
	pool := dimsPool(t)
	ctx := context.Background()
	id := seedDimsAsset(t, pool, "active")
	// A portrait phone photo as the preview pipeline records it.
	if err := pixeldims.Record(ctx, pool, id, dimsHash(t, pool, id), 1080, 1920); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
	}

	code, canvas, body, raw := canvasSizes(t, dimsRoute(pool), id)
	if code != http.StatusOK {
		t.Fatalf("manifest: %d %s", code, raw)
	}
	if canvas != [2]int{1080, 1920} || body != [2]int{1080, 1920} {
		t.Fatalf("canvas %v / image body %v, want the recorded 1080x1920 on both", canvas, body)
	}

	ref, err := presentation.NewLoader(pool).LoadAsset(ctx, id, visibility.NewCaller(nil))
	if err != nil {
		t.Fatalf("LoadAsset: %v", err)
	}
	if ref.Width != 1080 || ref.Height != 1920 {
		t.Fatalf("EntityRef %dx%d, want 1080x1920", ref.Width, ref.Height)
	}

	// info.json's reader must describe the same image the canvas does.
	info, err := iiif.PoolLookup{Pool: pool}.GetIIIFAsset(ctx, id, visibility.NewCaller(nil))
	if err != nil {
		t.Fatalf("GetIIIFAsset: %v", err)
	}
	if info.PixelWidth != canvas[0] || info.PixelHeight != canvas[1] {
		t.Fatalf("info.json pair %dx%d disagrees with the manifest canvas %v", info.PixelWidth, info.PixelHeight, canvas)
	}
}

func TestManifest_NoValidPair_KeepsThePlaceholderCanvas(t *testing.T) {
	pool := dimsPool(t)
	r := dimsRoute(pool)
	cases := []struct {
		name string
		w, h *float64 // nil = not recorded
	}{
		{"never measured", nil, nil},
		{"width only", f(1080), nil},
		{"height only", nil, f(1920)},
		{"zero width", f(0), f(1920)},
		{"negative height", f(1080), f(-5)},
		{"fractional", f(1080.5), f(1920)},
		{"absurd (beyond INT)", f(1e12), f(1920)},
		{"NaN", f(math.NaN()), f(1920)},
		{"Infinity", f(1080), f(math.Inf(1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seedDimsAsset(t, pool, "active")
			if tc.w != nil {
				plantRaw(t, pool, id, pixeldims.Width, *tc.w)
			}
			if tc.h != nil {
				plantRaw(t, pool, id, pixeldims.Height, *tc.h)
			}
			code, canvas, body, raw := canvasSizes(t, r, id)
			if code != http.StatusOK {
				t.Fatalf("a bad stored value must not fail the manifest: %d %s", code, raw)
			}
			if canvas != [2]int{1200, 900} || body != [2]int{1200, 900} {
				t.Fatalf("canvas %v / image body %v, want the 1200x900 placeholder on both", canvas, body)
			}
			ref, err := presentation.NewLoader(pool).LoadAsset(context.Background(), id, visibility.NewCaller(nil))
			if err != nil {
				t.Fatalf("LoadAsset: %v", err)
			}
			if ref.Width != 0 || ref.Height != 0 {
				t.Fatalf("EntityRef %dx%d, want 0x0 (no half or invalid pair applied)", ref.Width, ref.Height)
			}
		})
	}
}

func TestManifest_HiddenAssetWithRecordedSize_Stays404(t *testing.T) {
	pool := dimsPool(t)
	id := seedDimsAsset(t, pool, "draft") // not anonymously visible
	if err := pixeldims.Record(context.Background(), pool, id, dimsHash(t, pool, id), 1080, 1920); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
	}
	code, _, _, raw := canvasSizes(t, dimsRoute(pool), id)
	if code != http.StatusNotFound {
		t.Fatalf("hidden asset: got %d, want 404", code)
	}
	if strings.Contains(raw, "1080") || strings.Contains(raw, "1920") {
		t.Fatalf("404 body leaks the recorded size: %s", raw)
	}
	if _, err := presentation.NewLoader(pool).LoadAsset(context.Background(), id, visibility.NewCaller(nil)); err != presentation.ErrNotFound {
		t.Fatalf("LoadAsset for a hidden asset: %v, want ErrNotFound", err)
	}
}

func f(v float64) *float64 { return &v }

// --- manifest cache coherence (#196 correction) -----------------------
//
// The manifest route caches per process (`iiif.presentation`, keys
// asset::<id>::anon / ::auth). The preview pipeline records the size
// AFTER upload and again on every re-render, so a manifest cached before
// that would keep the placeholder (or an old size) unless the write
// drops both cached variants in EVERY process. Migration 00073 makes the
// asset_field_value trigger publish those two keyed invalidations on
// commit. Two registries, each with its own LISTEN connection, stand in
// for two app processes; the size is written only through
// pixeldims.Record, never by calling the cache. Without the trigger's
// notifications the warmed entries never drop and the deadline fails.

type cachedProcess struct {
	name  string
	cache *presentation.Cache
	anon  chi.Router
	auth  chi.Router
}

func newCachedProcess(t *testing.T, pool *pgxpool.Pool, name string) cachedProcess {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	reg := cache.NewRegistry(pool, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { reg.Stop(); cancel() })
	if err := reg.Start(ctx); err != nil {
		t.Fatalf("%s: registry start: %v", name, err)
	}
	c := presentation.NewCache(reg)
	mk := func(id *auth.Identity) chi.Router {
		h := &presentation.Handler{
			Loader: presentation.NewLoader(pool),
			Builder: presentation.NewBuilder(presentation.BuilderConfig{
				SiteBaseURL: "https://art.example.test",
				Provider:    presentation.Provider{Label: presentation.EN("Test"), Type: "Agent"},
			}),
			Cache:  c,
			Logger: logger,
		}
		r := chi.NewRouter()
		if id != nil {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
				})
			})
		}
		h.Mount(r)
		return r
	}
	return cachedProcess{
		name:  name,
		cache: c,
		anon:  mk(nil),
		auth:  mk(&auth.Identity{UserRef: dimsOwner, AuthMethod: "session"}),
	}
}

func (p cachedProcess) cached(id uuid.UUID) (anon, authed bool) {
	_, anon = p.cache.Get(p.cache.Key(presentation.EntityAsset, id, true))
	_, authed = p.cache.Get(p.cache.Key(presentation.EntityAsset, id, false))
	return anon, authed
}

// warm requests both variants through the route, asserts the size each
// serves, and asserts each is now cached.
func (p cachedProcess) warm(t *testing.T, id uuid.UUID, want [2]int) {
	t.Helper()
	for variant, r := range map[string]chi.Router{"anon": p.anon, "auth": p.auth} {
		code, canvas, body, raw := canvasSizes(t, r, id)
		if code != http.StatusOK || canvas != want || body != want {
			t.Fatalf("%s/%s: %d canvas %v body %v, want %v\n%s", p.name, variant, code, canvas, body, want, raw)
		}
	}
	if a, b := p.cached(id); !a || !b {
		t.Fatalf("%s: after warming, cached anon=%v auth=%v; want both", p.name, a, b)
	}
}

// waitDropped polls until neither variant is cached in any process, or
// fails at the deadline with what is still cached (the evidence).
func waitDropped(t *testing.T, id uuid.UUID, label string, procs ...cachedProcess) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		left := ""
		for _, p := range procs {
			if a, b := p.cached(id); a || b {
				left += fmt.Sprintf(" %s(anon=%v auth=%v)", p.name, a, b)
			}
		}
		if left == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: cached manifests not invalidated within 5s:%s", label, left)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestManifestCache_DimensionWritesInvalidateEveryProcess(t *testing.T) {
	pool := dimsPool(t)
	ctx := context.Background()
	id := seedDimsAsset(t, pool, "active")
	a := newCachedProcess(t, pool, "process-A")
	b := newCachedProcess(t, pool, "process-B")

	// Before the preview has run: both processes cache the placeholder.
	a.warm(t, id, [2]int{1200, 900})
	b.warm(t, id, [2]int{1200, 900})

	// The preview pipeline records the size (its only writer).
	if err := pixeldims.Record(ctx, pool, id, dimsHash(t, pool, id), 1080, 1920); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
	}
	waitDropped(t, id, "first measurement", a, b)
	a.warm(t, id, [2]int{1080, 1920})
	b.warm(t, id, [2]int{1080, 1920})

	// A re-render records a new size: the already-measured manifests
	// must drop again, not keep the first size.
	if err := pixeldims.Record(ctx, pool, id, dimsHash(t, pool, id), 1440, 2560); err != nil {
		t.Fatalf("pixeldims.Record (re-render): %v", err)
	}
	waitDropped(t, id, "re-render", a, b)
	a.warm(t, id, [2]int{1440, 2560})
	b.warm(t, id, [2]int{1440, 2560})
}

func TestManifestCache_InvalidationIsKeyedNotDomainWide(t *testing.T) {
	pool := dimsPool(t)
	ctx := context.Background()
	target := seedDimsAsset(t, pool, "active")
	other := seedDimsAsset(t, pool, "active")
	p := newCachedProcess(t, pool, "process-A")
	p.warm(t, target, [2]int{1200, 900})
	p.warm(t, other, [2]int{1200, 900})

	if err := pixeldims.Record(ctx, pool, target, dimsHash(t, pool, target), 1080, 1920); err != nil {
		t.Fatalf("pixeldims.Record: %v", err)
	}
	waitDropped(t, target, "target", p)
	// The target's notification has been processed by now (same LISTEN
	// connection, in order); the other asset's entries must survive.
	if a, b := p.cached(other); !a || !b {
		t.Fatalf("another asset's manifests were dropped (anon=%v auth=%v): the invalidation must be keyed, not a domain purge", a, b)
	}
}
