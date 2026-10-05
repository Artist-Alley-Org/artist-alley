// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #431 — the superadmin include_deleted gate, tested at the HANDLER
// level.
//
// TestListAssetsPage_IncludeDeleted covers the QUERY (the predicate
// option waives soft-delete). What it did NOT cover is the CALLER check
// in ListAssets that only threads IncludeDeleted through when the caller
// holds system.admin — a non-admin passing include_deleted=true must
// have it silently ignored. This drives the real handler for both
// caller classes.
//
// Skips without AA_DB_PASSWORD.

package assets_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

const idgUserRef int64 = 4310001

func TestListAssets_IncludeDeletedGate(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	// A soft-deleted asset + a live asset, both owned by the caller. The
	// live one keeps the list non-empty, so a missing deleted row reads
	// as "filtered", not "query broke".
	deletedID, liveID := uuid.New(), uuid.New()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, deleted_at)
		VALUES ($1,'idg-deleted',$2,(SELECT MIN(ref) FROM asset_types),'active','public','ready', NOW())`,
		deletedID, idgUserRef); err != nil {
		t.Fatalf("seed deleted asset: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status)
		VALUES ($1,'idg-live',$2,(SELECT MIN(ref) FROM asset_types),'active','public','ready')`,
		liveID, idgUserRef); err != nil {
		t.Fatalf("seed live asset: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = ANY($1::uuid[])`,
			[]uuid.UUID{deletedID, liveID})
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	assetsH := assets.NewHandler(pool, storage.NewService(backend, pool), logger, nil, nil, nil)

	routerAs := func(caps []string) chi.Router {
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				id := &auth.Identity{UserRef: idgUserRef, AuthMethod: "session", Capabilities: caps}
				next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
			})
		})
		openapi.HandlerFromMux(openapi.NewStrictHandler(
			shimImpl{PanicShim: &strictservershim.PanicShim{}, assets: assetsH}, nil), r)
		return r
	}

	listedIDs := func(r chi.Router) map[uuid.UUID]bool {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/assets?owner_ref="+strconv.FormatInt(idgUserRef, 10)+"&include_deleted=true&limit=200", nil)
		r.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /assets = %d, body=%s", rr.Code, rr.Body.String())
		}
		var page openapi.AssetList
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[uuid.UUID]bool{}
		for _, a := range page.Items {
			out[uuid.UUID(a.Id)] = true
		}
		return out
	}

	// Non-superadmin: include_deleted=true is silently ignored.
	nonAdmin := listedIDs(routerAs([]string{"assets.read"}))
	if !nonAdmin[liveID] {
		t.Error("non-admin: live asset missing from their own list")
	}
	if nonAdmin[deletedID] {
		t.Error("non-admin passing include_deleted=true saw a soft-deleted asset; " +
			"the gate must ignore the flag for non-superadmins")
	}

	// Superadmin: include_deleted=true is honoured.
	admin := listedIDs(routerAs([]string{auth.SuperAdminCapability}))
	if !admin[deletedID] {
		t.Error("superadmin passing include_deleted=true did NOT see the soft-deleted asset")
	}
}
