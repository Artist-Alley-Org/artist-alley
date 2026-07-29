// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// HTTP-surface tests for the instance logo (#517).
//
// The capability tests deliberately need no database: the gate runs
// before any store access, so they exercise the real handler with a
// real Identity and always run, rather than being skipped on a
// developer machine without Postgres. A permission check that is only
// verified when an optional dependency happens to be present is not
// verified.

package sysconfig_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	"github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func testPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------
// Capability gate — no database required
// ---------------------------------------------------------------

// ctxAs returns a context carrying an identity with exactly the given
// capabilities.
func ctxAs(caps ...string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{
		UserRef:      42,
		Username:     "tester",
		Capabilities: caps,
	})
}

// TestLogoWritesRequireAppearanceCapability drives every write
// endpoint as an anonymous caller, as a signed-in user with no
// capabilities, and as a signed-in user holding a DIFFERENT admin
// capability. All three must be refused.
//
// The third case is the one worth having: it is what catches a gate
// that checks "is an admin" rather than "holds system.appearance.write".
func TestLogoWritesRequireAppearanceCapability(t *testing.T) {
	h := &sysconfig.Handler{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	body := testPNG(t, 64, 64, color.RGBA{A: 0xFF})

	callers := []struct {
		name    string
		ctx     context.Context
		want401 bool
	}{
		{"anonymous", context.Background(), true},
		{"no_caps", ctxAs(), false},
		{"unrelated_admin_cap", ctxAs(sysconfig.CapConfigRead, sysconfig.CapAuthWrite, sysconfig.CapAIWrite), false},
	}

	for _, c := range callers {
		t.Run(c.name+"/upload", func(t *testing.T) {
			got, err := h.UploadInstanceLogo(c.ctx, openapi.UploadInstanceLogoRequestObject{
				Body: bytes.NewReader(body),
			})
			if err != nil {
				t.Fatalf("UploadInstanceLogo: %v", err)
			}
			if c.want401 {
				if _, ok := got.(openapi.UploadInstanceLogo401JSONResponse); !ok {
					t.Fatalf("got %T, want 401", got)
				}
				return
			}
			if _, ok := got.(openapi.UploadInstanceLogo403JSONResponse); !ok {
				t.Fatalf("got %T, want 403", got)
			}
		})

		t.Run(c.name+"/select", func(t *testing.T) {
			hash := openapi.SelectInstanceLogoJSONRequestBody{Hash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
			got, err := h.SelectInstanceLogo(c.ctx, openapi.SelectInstanceLogoRequestObject{Body: &hash})
			if err != nil {
				t.Fatalf("SelectInstanceLogo: %v", err)
			}
			if c.want401 {
				if _, ok := got.(openapi.SelectInstanceLogo401JSONResponse); !ok {
					t.Fatalf("got %T, want 401", got)
				}
				return
			}
			if _, ok := got.(openapi.SelectInstanceLogo403JSONResponse); !ok {
				t.Fatalf("got %T, want 403", got)
			}
		})

		t.Run(c.name+"/delete", func(t *testing.T) {
			got, err := h.DeleteInstanceLogo(c.ctx, openapi.DeleteInstanceLogoRequestObject{})
			if err != nil {
				t.Fatalf("DeleteInstanceLogo: %v", err)
			}
			if c.want401 {
				if _, ok := got.(openapi.DeleteInstanceLogo401JSONResponse); !ok {
					t.Fatalf("got %T, want 401", got)
				}
				return
			}
			if _, ok := got.(openapi.DeleteInstanceLogo403JSONResponse); !ok {
				t.Fatalf("got %T, want 403", got)
			}
		})
	}
}

// TestSystemAdminSatisfiesTheLogoGate — the wildcard cap short-circuits
// every check in this package, and the logo must follow that rule
// rather than invent its own. Proven by getting PAST the gate: with no
// storage wired the handler answers 400, which only the authorised
// path can reach.
func TestSystemAdminSatisfiesTheLogoGate(t *testing.T) {
	h := &sysconfig.Handler{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got, err := h.UploadInstanceLogo(ctxAs(sysconfig.CapSystemAdmin), openapi.UploadInstanceLogoRequestObject{
		Body: bytes.NewReader(testPNG(t, 64, 64, color.RGBA{A: 0xFF})),
	})
	if err != nil {
		t.Fatalf("UploadInstanceLogo: %v", err)
	}
	if _, ok := got.(openapi.UploadInstanceLogo400JSONResponse); !ok {
		t.Fatalf("got %T, want 400 (past the gate, refused for want of storage)", got)
	}
}

// ---------------------------------------------------------------
// Full lifecycle — database + storage backed
// ---------------------------------------------------------------

func withLogoHandler(t *testing.T, fn func(context.Context, *sysconfig.Handler, *storage.Service)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	backend, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs backend: %v", err)
	}
	svc := storage.NewService(backend, pool)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := sysconfig.NewStore(pool)
	h := sysconfig.NewHTTPHandler(pool, store, logger)
	h.SetStorage(svc)

	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM system_config WHERE key = 'appearance'`)
		_, _ = pool.Exec(c, `DELETE FROM storage_pins WHERE pin_subject_type = 'appearance'`)
	}
	clean()
	t.Cleanup(clean)

	fn(ctxAs(sysconfig.CapSystemAdmin), h, svc)
}

// uploadLogo drives the real upload endpoint and returns the resulting
// config.
func uploadLogo(t *testing.T, ctx context.Context, h *sysconfig.Handler, body []byte) openapi.AppearanceConfig {
	t.Helper()
	got, err := h.UploadInstanceLogo(ctx, openapi.UploadInstanceLogoRequestObject{Body: bytes.NewReader(body)})
	if err != nil {
		t.Fatalf("UploadInstanceLogo: %v", err)
	}
	ok, is := got.(openapi.UploadInstanceLogo200JSONResponse)
	if !is {
		t.Fatalf("upload got %T, want 200", got)
	}
	return openapi.AppearanceConfig(ok)
}

// TestLogoUnsetFallsBackToDefault is the contract the whole feature
// rests on: an install that has never set a logo reports no logo_url
// and 404s the image route, which the frontend reads as "draw the
// shipped mark".
func TestLogoUnsetFallsBackToDefault(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *storage.Service) {
		pub, err := h.GetPublicAppearance(ctx, openapi.GetPublicAppearanceRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicAppearance: %v", err)
		}
		cfg := openapi.AppearanceConfig(pub.(openapi.GetPublicAppearance200JSONResponse))
		if cfg.LogoUrl != nil {
			t.Errorf("unset install published logo_url = %q, want absent", *cfg.LogoUrl)
		}

		img, err := h.GetPublicInstanceLogo(ctx, openapi.GetPublicInstanceLogoRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicInstanceLogo: %v", err)
		}
		if _, ok := img.(openapi.GetPublicInstanceLogo404JSONResponse); !ok {
			t.Errorf("unset install served %T, want 404", img)
		}
	})
}

// TestLogoRoundTripAndRevert walks the operator's actual path: upload,
// see it applied, revert, see the default return — and confirms the
// reverted logo is still in the list so it can be picked back up.
func TestLogoRoundTripAndRevert(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *storage.Service) {
		body := testPNG(t, 128, 96, color.RGBA{R: 0xE8, G: 0x62, B: 0x2C, A: 0xFF})
		cfg := uploadLogo(t, ctx, h, body)

		if cfg.LogoUrl == nil {
			t.Fatal("no logo_url after upload")
		}
		if cfg.LogoWidth == nil || *cfg.LogoWidth != 128 || cfg.LogoHeight == nil || *cfg.LogoHeight != 96 {
			t.Errorf("dimensions not published correctly: %v x %v", cfg.LogoWidth, cfg.LogoHeight)
		}
		if cfg.LogoHistory == nil || len(*cfg.LogoHistory) != 1 {
			t.Fatalf("history = %v, want 1 entry", cfg.LogoHistory)
		}
		entry := (*cfg.LogoHistory)[0]
		if !entry.Active {
			t.Error("uploaded logo is not marked active")
		}
		if !entry.Available {
			t.Error("freshly uploaded logo reports unavailable")
		}
		if entry.ContentType != "image/png" {
			t.Errorf("content_type = %q, want image/png", entry.ContentType)
		}

		// Serving it must return the exact bytes with the hardened
		// headers.
		rec := serveLogo(t, ctx, h, nil)
		if got := rec.Header().Get("Content-Type"); got != "image/png" {
			t.Errorf("Content-Type = %q, want image/png", got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got == "" {
			t.Error("no Content-Security-Policy on an operator-supplied file")
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("served %d bytes, uploaded %d", rec.Body.Len(), len(body))
		}

		// Revert to the shipped default.
		del, err := h.DeleteInstanceLogo(ctx, openapi.DeleteInstanceLogoRequestObject{})
		if err != nil {
			t.Fatalf("DeleteInstanceLogo: %v", err)
		}
		after := openapi.AppearanceConfig(del.(openapi.DeleteInstanceLogo200JSONResponse))
		if after.LogoUrl != nil {
			t.Errorf("logo_url still published after revert: %q", *after.LogoUrl)
		}
		// ...but the entry survives, because the list is a recovery
		// tool. Reverting is not deleting.
		if after.LogoHistory == nil || len(*after.LogoHistory) != 1 {
			t.Fatalf("revert dropped the history: %v", after.LogoHistory)
		}
		if (*after.LogoHistory)[0].Active {
			t.Error("entry still marked active after revert")
		}

		// And the active image route 404s again — the default is back.
		img, err := h.GetPublicInstanceLogo(ctx, openapi.GetPublicInstanceLogoRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicInstanceLogo: %v", err)
		}
		if _, ok := img.(openapi.GetPublicInstanceLogo404JSONResponse); !ok {
			t.Errorf("after revert served %T, want 404 (shipped default)", img)
		}

		// Re-select it: the recovery path.
		sel, err := h.SelectInstanceLogo(ctx, openapi.SelectInstanceLogoRequestObject{
			Body: &openapi.SelectInstanceLogoJSONRequestBody{Hash: entry.Hash},
		})
		if err != nil {
			t.Fatalf("SelectInstanceLogo: %v", err)
		}
		back := openapi.AppearanceConfig(sel.(openapi.SelectInstanceLogo200JSONResponse))
		if back.LogoUrl == nil {
			t.Error("re-selecting a history entry did not restore the logo")
		}
	})
}

// TestLogoHistoryIsMRUAndCapped is the owner's acceptance case: five
// distinct uploads, a sixth that evicts the oldest, then a re-select
// that moves without duplicating.
func TestLogoHistoryIsMRUAndCapped(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, svc *storage.Service) {
		var hashes []string
		for i := 0; i < sysconfig.MaxLogoHistory; i++ {
			// Distinct pixel colour per upload => distinct content hash.
			cfg := uploadLogo(t, ctx, h, testPNG(t, 64, 64, color.RGBA{R: uint8(10 * (i + 1)), A: 0xFF}))
			hashes = append(hashes, (*cfg.LogoHistory)[0].Hash)
		}

		cfg := mustHistory(t, h, ctx)
		if len(cfg) != sysconfig.MaxLogoHistory {
			t.Fatalf("history = %d entries, want %d", len(cfg), sysconfig.MaxLogoHistory)
		}
		// Most-recently-used first.
		if cfg[0].Hash != hashes[len(hashes)-1] {
			t.Error("most recent upload is not at the front")
		}

		// The sixth upload evicts the oldest.
		sixth := uploadLogo(t, ctx, h, testPNG(t, 64, 64, color.RGBA{B: 0xFF, A: 0xFF}))
		list := *sixth.LogoHistory
		if len(list) != sysconfig.MaxLogoHistory {
			t.Fatalf("history grew to %d, want a hard cap of %d", len(list), sysconfig.MaxLogoHistory)
		}
		for _, e := range list {
			if e.Hash == hashes[0] {
				t.Error("the oldest entry survived the cap")
			}
		}

		// The evicted entry's pin is gone, which is what lets its bytes
		// be collected. Everything still listed must still be pinned.
		if pinned := pinnedLogoHashes(t, ctx, h); len(pinned) != sysconfig.MaxLogoHistory {
			t.Errorf("pinned %d objects, want %d — the pin set must track the list exactly",
				len(pinned), sysconfig.MaxLogoHistory)
		} else {
			for _, e := range list {
				if _, ok := pinned[e.Hash]; !ok {
					t.Errorf("listed entry %s is not pinned; its bytes can be collected out from under the list", e.Hash[:12])
				}
			}
			if _, ok := pinned[hashes[0]]; ok {
				t.Error("evicted entry is still pinned; its bytes leak")
			}
		}

		// Re-selecting an older entry moves it to the front without
		// duplicating it.
		target := list[3].Hash
		sel, err := h.SelectInstanceLogo(ctx, openapi.SelectInstanceLogoRequestObject{
			Body: &openapi.SelectInstanceLogoJSONRequestBody{Hash: target},
		})
		if err != nil {
			t.Fatalf("SelectInstanceLogo: %v", err)
		}
		moved := *openapi.AppearanceConfig(sel.(openapi.SelectInstanceLogo200JSONResponse)).LogoHistory
		if len(moved) != sysconfig.MaxLogoHistory {
			t.Errorf("re-select changed the list length to %d", len(moved))
		}
		if moved[0].Hash != target {
			t.Error("re-selected entry did not move to the front")
		}
		seen := map[string]int{}
		for _, e := range moved {
			seen[e.Hash]++
			if seen[e.Hash] > 1 {
				t.Errorf("re-select duplicated %s", e.Hash[:12])
			}
		}
	})
}

// TestSelectRefusesUnlistedHash is the security boundary on the
// recovery path. If "select by hash" accepted an arbitrary hash, an
// admin could aim the public, unauthenticated logo route at any object
// on the install.
func TestSelectRefusesUnlistedHash(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, svc *storage.Service) {
		uploadLogo(t, ctx, h, testPNG(t, 64, 64, color.RGBA{G: 0xFF, A: 0xFF}))

		// Put a real object in storage that the logo list has never
		// heard of — this stands in for another user's private asset.
		secret := []byte("private asset bytes, not a logo")
		res, err := svc.UploadOriginal(ctx, bytes.NewReader(secret), "application/octet-stream",
			storage.PinRef{SubjectType: "user", SubjectID: "999"})
		if err != nil {
			t.Fatalf("seed foreign object: %v", err)
		}

		got, err := h.SelectInstanceLogo(ctx, openapi.SelectInstanceLogoRequestObject{
			Body: &openapi.SelectInstanceLogoJSONRequestBody{Hash: res.Hash},
		})
		if err != nil {
			t.Fatalf("SelectInstanceLogo: %v", err)
		}
		if _, ok := got.(openapi.SelectInstanceLogo404JSONResponse); !ok {
			t.Fatalf("selecting an unlisted hash got %T, want 404", got)
		}

		// The same hash must also be unreachable through the public
		// image route's `v` parameter.
		img, err := h.GetPublicInstanceLogo(ctx, openapi.GetPublicInstanceLogoRequestObject{
			Params: openapi.GetPublicInstanceLogoParams{V: &res.Hash},
		})
		if err != nil {
			t.Fatalf("GetPublicInstanceLogo: %v", err)
		}
		if _, ok := img.(openapi.GetPublicInstanceLogo404JSONResponse); !ok {
			t.Fatalf("public route served an unlisted object: %T", img)
		}
	})
}

// TestFontPatchPreservesLogo — PATCH /admin/system/appearance is a
// whole-object replace and the logo fields are read-only on it, so a
// font save must not silently reset the operator's brand mark.
func TestFontPatchPreservesLogo(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *storage.Service) {
		uploadLogo(t, ctx, h, testPNG(t, 64, 64, color.RGBA{R: 0x11, A: 0xFF}))

		brand := "limelight"
		// A caller trying to inject a logo through this endpoint is
		// also covered: logo_url is read-only, so it must be ignored.
		injected := "/api/v1/appearance/logo?v=" + "ff" + "00"
		body := openapi.AppearanceConfig{BrandFont: &brand, LogoUrl: &injected}
		got, err := h.UpdateAppearanceConfig(ctx, openapi.UpdateAppearanceConfigRequestObject{Body: &body})
		if err != nil {
			t.Fatalf("UpdateAppearanceConfig: %v", err)
		}
		cfg := openapi.AppearanceConfig(got.(openapi.UpdateAppearanceConfig200JSONResponse))

		if cfg.BrandFont == nil || *cfg.BrandFont != brand {
			t.Errorf("font not saved: %v", cfg.BrandFont)
		}
		if cfg.LogoUrl == nil {
			t.Fatal("saving a font cleared the instance logo")
		}
		if *cfg.LogoUrl == injected {
			t.Error("logo_url was taken from the request body; it must be read-only")
		}
		if cfg.LogoHistory == nil || len(*cfg.LogoHistory) != 1 {
			t.Errorf("font save dropped the recent-logo list: %v", cfg.LogoHistory)
		}
	})
}

// TestLostBlobDegradesHonestly is the scenario the recent list exists
// for. The bytes vanish from the backend behind the pin's back — a
// restored database against a fresh bucket, or manual deletion — and
// the admin surface must SAY so rather than emit a URL that renders a
// broken image.
func TestLostBlobDegradesHonestly(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, svc *storage.Service) {
		cfg := uploadLogo(t, ctx, h, testPNG(t, 64, 64, color.RGBA{R: 0x22, A: 0xFF}))
		hash := (*cfg.LogoHistory)[0].Hash

		// Delete the object bytes directly through the backend,
		// bypassing pins entirely.
		if err := svc.Backend.Delete(ctx, hash, storage.VariantOriginal); err != nil {
			t.Fatalf("delete object bytes: %v", err)
		}

		// The admin picker must flag the entry unavailable.
		list := mustHistory(t, h, ctx)
		if len(list) != 1 {
			t.Fatalf("history = %d entries, want 1", len(list))
		}
		if list[0].Available {
			t.Error("entry with missing bytes still reports available — the picker would render a broken image")
		}

		// Selecting it must be refused rather than swapping a working
		// mark for a broken one.
		sel, err := h.SelectInstanceLogo(ctx, openapi.SelectInstanceLogoRequestObject{
			Body: &openapi.SelectInstanceLogoJSONRequestBody{Hash: hash},
		})
		if err != nil {
			t.Fatalf("SelectInstanceLogo: %v", err)
		}
		if _, ok := sel.(openapi.SelectInstanceLogo400JSONResponse); !ok {
			t.Errorf("selecting an unresolvable logo got %T, want 400", sel)
		}

		// And the public route degrades to the shipped default rather
		// than 500ing — chrome on every page must not be able to break
		// the page.
		img, err := h.GetPublicInstanceLogo(ctx, openapi.GetPublicInstanceLogoRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicInstanceLogo returned an error instead of degrading: %v", err)
		}
		if _, ok := img.(openapi.GetPublicInstanceLogo404JSONResponse); !ok {
			t.Errorf("lost blob served %T, want 404 so the frontend falls back", img)
		}
	})
}

// TestUploadRejectsHostileBodies drives the real endpoint (not just the
// validator) with the inputs that matter.
func TestUploadRejectsHostileBodies(t *testing.T) {
	withLogoHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *storage.Service) {
		bodies := map[string][]byte{
			"svg":      []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><script>alert(1)</script></svg>`),
			"html":     []byte(`<html><script>alert(1)</script></html>`),
			"oversize": make([]byte, sysconfig.MaxLogoBytes+1),
			"tiny":     testPNG(t, 8, 8, color.RGBA{A: 0xFF}),
		}
		for name, body := range bodies {
			t.Run(name, func(t *testing.T) {
				got, err := h.UploadInstanceLogo(ctx, openapi.UploadInstanceLogoRequestObject{
					Body: bytes.NewReader(body),
				})
				if err != nil {
					t.Fatalf("UploadInstanceLogo: %v", err)
				}
				if _, ok := got.(openapi.UploadInstanceLogo400JSONResponse); !ok {
					t.Fatalf("got %T, want 400", got)
				}
			})
		}
		// Nothing hostile may have been persisted.
		if list := mustHistory(t, h, ctx); len(list) != 0 {
			t.Errorf("a rejected upload still landed in the list: %v", list)
		}
	})
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

// serveLogo drives the public image route through its real Visit
// method so the response headers are the ones a browser would see.
func serveLogo(t *testing.T, ctx context.Context, h *sysconfig.Handler, v *string) *httptest.ResponseRecorder {
	t.Helper()
	got, err := h.GetPublicInstanceLogo(ctx, openapi.GetPublicInstanceLogoRequestObject{
		Params: openapi.GetPublicInstanceLogoParams{V: v},
	})
	if err != nil {
		t.Fatalf("GetPublicInstanceLogo: %v", err)
	}
	rec := httptest.NewRecorder()
	if err := got.VisitGetPublicInstanceLogoResponse(rec); err != nil {
		t.Fatalf("visit: %v", err)
	}
	return rec
}

func mustHistory(t *testing.T, h *sysconfig.Handler, ctx context.Context) []openapi.InstanceLogo {
	t.Helper()
	got, err := h.GetAppearanceConfig(ctx, openapi.GetAppearanceConfigRequestObject{})
	if err != nil {
		t.Fatalf("GetAppearanceConfig: %v", err)
	}
	cfg := openapi.AppearanceConfig(got.(openapi.GetAppearanceConfig200JSONResponse))
	if cfg.LogoHistory == nil {
		return nil
	}
	return *cfg.LogoHistory
}

// pinnedLogoHashes reads the appearance pins straight from the
// database — the retention invariant is only meaningful if it is
// checked against the storage layer's own bookkeeping.
func pinnedLogoHashes(t *testing.T, ctx context.Context, h *sysconfig.Handler) map[string]struct{} {
	t.Helper()
	rows, err := h.Pool.Query(ctx,
		`SELECT object_hash FROM storage_pins WHERE pin_subject_type = $1 AND pin_subject_id = $2`,
		"appearance", "logo")
	if err != nil {
		t.Fatalf("query pins: %v", err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			t.Fatalf("scan pin: %v", err)
		}
		out[hash] = struct{}{}
	}
	return out
}
