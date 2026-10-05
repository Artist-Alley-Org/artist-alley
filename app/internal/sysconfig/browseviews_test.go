// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #709 — the operator's browse-layout allowlist.
//
// The invariant these tests exist for is that AT LEAST ONE LAYOUT STAYS
// ENABLED, and every refusal here is asserted on the PERSISTED VALUE
// rather than on the response status. A handler that 400s and writes
// anyway, or that "repairs" a bad payload into something the operator
// did not choose, passes a status assertion and fails the install.

package sysconfig_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// ---------------------------------------------------------------------------
// Pure resolution — no DB, runs everywhere
// ---------------------------------------------------------------------------

// TestBrowseViewsResolved_UnsetIsAllFive pins the default-by-absence
// contract: an install that has never configured this offers everything
// it ships. This is the property that lets the feature be added without
// a migration, so it is the one that must not quietly change.
func TestBrowseViewsResolved_UnsetIsAllFive(t *testing.T) {
	got := sysconfig.BrowseViewsConfig{}.Resolved()
	if !reflect.DeepEqual(got, sysconfig.AllBrowseViewModes) {
		t.Errorf("unset config resolved to %v, want all five %v", got, sysconfig.AllBrowseViewModes)
	}
}

// TestBrowseViewsResolved_CanonicalOrder confirms the stored order does
// not decide the switcher's order — layout order is a frontend
// decision, availability is the operator's.
func TestBrowseViewsResolved_CanonicalOrder(t *testing.T) {
	cfg := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{
		sysconfig.BrowseViewFeed,
		sysconfig.BrowseViewGrid,
		sysconfig.BrowseViewList,
	}}
	want := []sysconfig.BrowseViewMode{
		sysconfig.BrowseViewGrid,
		sysconfig.BrowseViewList,
		sysconfig.BrowseViewFeed,
	}
	if got := cfg.Resolved(); !reflect.DeepEqual(got, want) {
		t.Errorf("Resolved() = %v, want %v", got, want)
	}
}

// TestBrowseViewsResolved_FailsOpenOnUnservableSet covers the
// degenerate case a later release can create: every mode the operator
// named has since been retired, so the filter empties the set. Browse
// must stay reachable — an install that renders no layout is bricked,
// and there is no security property on the other side of this trade.
func TestBrowseViewsResolved_FailsOpenOnUnservableSet(t *testing.T) {
	cfg := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{"carousel", "lightbox"}}
	if got := cfg.Resolved(); !reflect.DeepEqual(got, sysconfig.AllBrowseViewModes) {
		t.Errorf("unservable set resolved to %v, want all five", got)
	}
}

func TestBrowseViewsEnables(t *testing.T) {
	cfg := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{sysconfig.BrowseViewGrid}}
	if !cfg.Enables(sysconfig.BrowseViewGrid) {
		t.Error("grid should be enabled")
	}
	if cfg.Enables(sysconfig.BrowseViewMasonry) {
		t.Error("masonry should not be enabled")
	}
	// Unset enables everything, including the coarse-pointer default.
	if !(sysconfig.BrowseViewsConfig{}).Enables(sysconfig.BrowseViewFeed) {
		t.Error("unset config should enable feed")
	}
}

// ---------------------------------------------------------------------------
// Store-level validation — the invariant's real home
// ---------------------------------------------------------------------------

// TestSetBrowseViews_EmptySetRefused_NothingPersisted is the empty-set
// invariant at the layer every caller shares.
//
// Asserted on the row, not on the error: the failure being guarded is
// "accepted but inert", where a validator returns an error after the
// upsert already committed. Reading the raw JSONB back is the only
// assertion that can tell those apart.
func TestSetBrowseViews_EmptySetRefused_NothingPersisted(t *testing.T) {
	withBrowseViewsStore(t, func(ctx context.Context, store *sysconfig.Store, pool *pgxpool.Pool) {
		seed := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{
			sysconfig.BrowseViewGrid, sysconfig.BrowseViewList,
		}}
		if err := store.SetBrowseViews(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.SetBrowseViews(ctx, sysconfig.BrowseViewsConfig{}); err == nil {
			t.Fatal("SetBrowseViews with an empty set returned nil, want a refusal")
		}
		assertStoredBrowseViews(t, pool, []string{"grid", "list"})
	})
}

// TestSetBrowseViews_UnknownModeRefused_NothingPersisted: a typo must
// not save a smaller set than the operator chose. Refused, not
// filtered.
func TestSetBrowseViews_UnknownModeRefused_NothingPersisted(t *testing.T) {
	withBrowseViewsStore(t, func(ctx context.Context, store *sysconfig.Store, pool *pgxpool.Pool) {
		seed := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{sysconfig.BrowseViewGrid}}
		if err := store.SetBrowseViews(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		bad := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{
			sysconfig.BrowseViewGrid, "carousel",
		}}
		if err := store.SetBrowseViews(ctx, bad); err == nil {
			t.Fatal("SetBrowseViews with an unknown mode returned nil, want a refusal")
		}
		assertStoredBrowseViews(t, pool, []string{"grid"})
	})
}

// TestSetBrowseViews_PersistsCanonicalDeduped: what lands in the row is
// canonical order with duplicates dropped, so a save that reordered
// nothing produces no audit diff.
func TestSetBrowseViews_PersistsCanonicalDeduped(t *testing.T) {
	withBrowseViewsStore(t, func(ctx context.Context, store *sysconfig.Store, pool *pgxpool.Pool) {
		in := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{
			sysconfig.BrowseViewFeed,
			sysconfig.BrowseViewGrid,
			sysconfig.BrowseViewFeed,
		}}
		if err := store.SetBrowseViews(ctx, in); err != nil {
			t.Fatalf("SetBrowseViews: %v", err)
		}
		assertStoredBrowseViews(t, pool, []string{"grid", "feed"})
	})
}

// TestGetBrowseViews_UnsetHasNoRow: the shipped default is the absence
// of a row, not a seeded one. If this ever starts finding a row, the
// "adding a sixth mode ships it disabled everywhere" hazard is live.
func TestGetBrowseViews_UnsetHasNoRow(t *testing.T) {
	withBrowseViewsStore(t, func(ctx context.Context, store *sysconfig.Store, pool *pgxpool.Pool) {
		cfg, err := store.GetBrowseViews(ctx)
		if err != nil {
			t.Fatalf("GetBrowseViews on unset key: %v", err)
		}
		if cfg.Enabled != nil {
			t.Errorf("unset key read back %v, want nil", cfg.Enabled)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM system_config WHERE key = 'browse_views'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("found %d browse_views rows on an unconfigured install, want 0", n)
		}
		if !reflect.DeepEqual(cfg.Resolved(), sysconfig.AllBrowseViewModes) {
			t.Errorf("unconfigured install resolved to %v, want all five", cfg.Resolved())
		}
	})
}

// ---------------------------------------------------------------------------
// Handler — the surface the admin UI actually reaches
// ---------------------------------------------------------------------------

// TestUpdateBrowseViews_EmptySetRefused_NothingPersisted is acceptance
// criterion 3: the empty set is refused AT THE API, and the proof is
// the stored value rather than the status code.
func TestUpdateBrowseViews_EmptySetRefused_NothingPersisted(t *testing.T) {
	withBrowseViewsHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		seed := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{
			sysconfig.BrowseViewGrid, sysconfig.BrowseViewMasonry,
		}}
		if err := h.Store.SetBrowseViews(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.BrowseViewsConfig{Enabled: []openapi.BrowseViewsConfigEnabled{}}
			resp, err := h.UpdateBrowseViews(ctx, openapi.UpdateBrowseViewsRequestObject{Body: &body})
			if err != nil {
				t.Fatalf("UpdateBrowseViews: %v", err)
			}
			if _, ok := resp.(openapi.UpdateBrowseViews400JSONResponse); !ok {
				t.Fatalf("expected 400 for the empty set, got %T", resp)
			}
		})
		// The assertion that matters: the install still offers what it
		// offered before the refused write.
		assertStoredBrowseViews(t, pool, []string{"grid", "masonry"})

		// And the public read agrees — a refusal that left the cache
		// holding an empty set would be just as broken as one that
		// persisted it.
		resp, err := h.GetPublicBrowseViews(ctx, openapi.GetPublicBrowseViewsRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicBrowseViews: %v", err)
		}
		got, ok := resp.(openapi.GetPublicBrowseViews200JSONResponse)
		if !ok {
			t.Fatalf("expected 200, got %T", resp)
		}
		if len(got.Enabled) != 2 {
			t.Errorf("public read returned %v, want the two seeded modes", got.Enabled)
		}
	})
}

// TestUpdateBrowseViews_UnknownModeRefused_NothingPersisted: the same
// proof for the typo case. A payload of nothing but typos is the
// dangerous one — filtered, it would be the empty set, which fails open
// to all five and disagrees with what the operator saved.
func TestUpdateBrowseViews_UnknownModeRefused_NothingPersisted(t *testing.T) {
	withBrowseViewsHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		seed := sysconfig.BrowseViewsConfig{Enabled: []sysconfig.BrowseViewMode{sysconfig.BrowseViewList}}
		if err := h.Store.SetBrowseViews(ctx, seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.BrowseViewsConfig{Enabled: []openapi.BrowseViewsConfigEnabled{"carousel"}}
			resp, err := h.UpdateBrowseViews(ctx, openapi.UpdateBrowseViewsRequestObject{Body: &body})
			if err != nil {
				t.Fatalf("UpdateBrowseViews: %v", err)
			}
			if _, ok := resp.(openapi.UpdateBrowseViews400JSONResponse); !ok {
				t.Fatalf("expected 400 for an unknown mode, got %T", resp)
			}
		})
		assertStoredBrowseViews(t, pool, []string{"list"})
	})
}

// TestUpdateBrowseViews_EchoesStoredNotSubmitted: the response is what
// was persisted, so the operator's UI redraws from the truth rather
// than from whatever order its checkboxes serialised in.
func TestUpdateBrowseViews_EchoesStoredNotSubmitted(t *testing.T) {
	withBrowseViewsHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.BrowseViewsConfig{Enabled: []openapi.BrowseViewsConfigEnabled{"feed", "grid", "feed"}}
			resp, err := h.UpdateBrowseViews(ctx, openapi.UpdateBrowseViewsRequestObject{Body: &body})
			if err != nil {
				t.Fatalf("UpdateBrowseViews: %v", err)
			}
			got, ok := resp.(openapi.UpdateBrowseViews200JSONResponse)
			if !ok {
				t.Fatalf("expected 200, got %T", resp)
			}
			want := []openapi.BrowseViewsConfigEnabled{"grid", "feed"}
			if !reflect.DeepEqual([]openapi.BrowseViewsConfigEnabled(got.Enabled), want) {
				t.Errorf("response = %v, want canonical deduped %v", got.Enabled, want)
			}
		})
		assertStoredBrowseViews(t, pool, []string{"grid", "feed"})
	})
}

// TestUpdateBrowseViews_InvalidatesCachedRead is the "the save looks
// inert" guard. The public endpoint reads through a one-entry LRU; if
// the write does not drop it, the switcher keeps drawing the disabled
// button until the entry ages out, which every operator will report as
// the setting not working.
func TestUpdateBrowseViews_InvalidatesCachedRead(t *testing.T) {
	withBrowseViewsHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		if err := h.Store.SetBrowseViews(ctx, sysconfig.BrowseViewsConfig{
			Enabled: sysconfig.AllBrowseViewModes,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Warm the cache through the public endpoint.
		if _, err := h.GetPublicBrowseViews(ctx, openapi.GetPublicBrowseViewsRequestObject{}); err != nil {
			t.Fatalf("warm: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.BrowseViewsConfig{Enabled: []openapi.BrowseViewsConfigEnabled{"grid"}}
			resp, err := h.UpdateBrowseViews(ctx, openapi.UpdateBrowseViewsRequestObject{Body: &body})
			if err != nil {
				t.Fatalf("UpdateBrowseViews: %v", err)
			}
			if _, ok := resp.(openapi.UpdateBrowseViews200JSONResponse); !ok {
				t.Fatalf("expected 200, got %T", resp)
			}
		})
		resp, err := h.GetPublicBrowseViews(ctx, openapi.GetPublicBrowseViewsRequestObject{})
		if err != nil {
			t.Fatalf("GetPublicBrowseViews: %v", err)
		}
		got, ok := resp.(openapi.GetPublicBrowseViews200JSONResponse)
		if !ok {
			t.Fatalf("expected 200, got %T", resp)
		}
		want := []openapi.BrowseViewsConfigEnabled{"grid"}
		if !reflect.DeepEqual([]openapi.BrowseViewsConfigEnabled(got.Enabled), want) {
			t.Errorf("public read after the write = %v, want %v (stale cache)", got.Enabled, want)
		}
	})
}

// TestUpdateBrowseViews_EmitsAuditEvent: "an operator turned masonry
// off on this date" has to be answerable from the audit log.
func TestUpdateBrowseViews_EmitsAuditEvent(t *testing.T) {
	withBrowseViewsHandler(t, func(ctx context.Context, h *sysconfig.Handler, pool *pgxpool.Pool) {
		if err := h.Store.SetBrowseViews(ctx, sysconfig.BrowseViewsConfig{
			Enabled: sysconfig.AllBrowseViewModes,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		callAsAdmin(t, ctx, pool, func(ctx context.Context) {
			body := openapi.BrowseViewsConfig{Enabled: []openapi.BrowseViewsConfigEnabled{"grid", "list"}}
			if _, err := h.UpdateBrowseViews(ctx, openapi.UpdateBrowseViewsRequestObject{Body: &body}); err != nil {
				t.Fatalf("UpdateBrowseViews: %v", err)
			}
		})
		cs := readChangeset(t, pool, audit.EventAdminBrowseViewsUpdated)
		if cs == nil {
			t.Fatal("no browse-views audit event with a changeset emitted")
		}
		if _, ok := cs["Enabled"]; !ok {
			t.Errorf("changeset missing Enabled entry: %v", cs)
		}
	})
}

// TestGetBrowseViews_RequiresCapability: the admin read is gated, and
// the public read is the one without a check. Anonymous reach for the
// public one is decided by auth.PublicSurfaceRoutes, not here.
func TestGetBrowseViews_RequiresCapability(t *testing.T) {
	withBrowseViewsHandler(t, func(ctx context.Context, h *sysconfig.Handler, _ *pgxpool.Pool) {
		resp, err := h.GetBrowseViews(ctx, openapi.GetBrowseViewsRequestObject{})
		if err != nil {
			t.Fatalf("GetBrowseViews: %v", err)
		}
		if _, ok := resp.(openapi.GetBrowseViews401JSONResponse); !ok {
			t.Fatalf("expected 401 for an anonymous caller, got %T", resp)
		}
	})
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// assertStoredBrowseViews reads the raw JSONB row and compares the
// enabled list. Raw rather than through GetBrowseViews so a bug in the
// reader cannot mask a bug in the writer.
func assertStoredBrowseViews(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT value FROM system_config WHERE key = 'browse_views'`).Scan(&raw)
	if err == pgx.ErrNoRows {
		t.Fatalf("no browse_views row stored, want %v", want)
	}
	if err != nil {
		t.Fatalf("read browse_views row: %v", err)
	}
	var stored struct {
		Enabled []string `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored row %s: %v", raw, err)
	}
	if !reflect.DeepEqual(stored.Enabled, want) {
		t.Errorf("stored browse_views = %v, want %v (raw: %s)", stored.Enabled, want, raw)
	}
}

func withBrowseViewsStore(t *testing.T, fn func(context.Context, *sysconfig.Store, *pgxpool.Pool)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	ctx := t.Context()
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	clean := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM system_config WHERE key = 'browse_views'`)
	}
	clean()
	t.Cleanup(clean)

	fn(ctx, sysconfig.NewStore(pool), pool)
}

// withBrowseViewsHandler stands up the handler WITH a cache registry and
// the cached reader wired, because the invalidation path is part of what
// these tests check. Boot wires it the same way (see
// http.sysconfigHandlerWithAudit).
func withBrowseViewsHandler(t *testing.T, fn func(context.Context, *sysconfig.Handler, *pgxpool.Pool)) {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	ctx := t.Context()
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := sysconfig.NewStore(pool)
	h := sysconfig.NewHTTPHandler(pool, store, logger)
	h.SetAuditRecorder(&audit.Recorder{Pool: pool, Logger: logger})
	h.CacheReg = cache.NewRegistry(pool, logger)
	h.SetBrowseViewsReader(sysconfig.NewBrowseViewsReader(store, h.CacheReg, logger))

	clean := func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM system_config WHERE key = 'browse_views'`)
		_, _ = pool.Exec(c,
			`DELETE FROM audit_events WHERE event_type = $1`,
			audit.EventAdminBrowseViewsUpdated)
	}
	clean()
	t.Cleanup(clean)

	fn(ctx, h, pool)
}
