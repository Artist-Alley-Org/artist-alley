// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #476 — IIIF tile BYTES go through visibility.CanReadContent (ADR 0064).
//
// Drives the REAL serveImage handler with the REAL PoolLookup + a real
// pool against real rows — the byte-plane twin of predicate_test.go's
// row-plane coverage. #472 gated existence; this gates the bytes.
//
// The gap this closes: an authenticated non-owner CAN resolve a
// restricted asset's row (the deferred authenticated-sensitivity rule,
// same as browse), so info.json/existence is 200 — but that must NOT
// let them stream the full-resolution tiles. Restricted/team/embargo
// tiles 404 for such a caller (404 not 403, so the plane never confirms
// the asset exists); the owner and a content.read.all holder get bytes;
// public serves anyone, including anonymous.
//
// Skips without AA_DB_PASSWORD.

package iiif_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// seedIIIFAsset hardcodes this owner ref.
const iiifOwnerRef int64 = 4600001

// anyStreamer streams a fixed body for any (hash, variant). These tests
// exercise the content GATE, not the byte pipeline, so the streamer is
// deliberately permissive — reaching it at all means the gate allowed
// the caller through.
type anyStreamer struct{}

func (anyStreamer) OpenVariant(context.Context, string, string) (io.ReadCloser, int64, string, error) {
	const body = "WEBPBYTES"
	return io.NopCloser(strings.NewReader(body)), int64(len(body)), "image/webp", nil
}

// imageStatus drives the real serveImage for an asset id + caller
// identity (nil = anonymous), returning the HTTP status and body.
func imageStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, identity *auth.Identity) (int, string) {
	t.Helper()
	h := &iiif.Handler{
		Lookup:   iiif.PoolLookup{Pool: pool},
		Variants: fixedVariants{},
		Streamer: anyStreamer{},
		Content:  pool, // the real content-plane gate under test
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
	r := chi.NewRouter()
	if identity != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), identity)))
			})
		})
	}
	h.Mount(r)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest("GET",
		"/iiif/3/"+id.String()+"/full/max/0/default.webp", nil))
	return rr.Code, rr.Body.String()
}

func identity(ref int64, caps ...string) *auth.Identity {
	return &auth.Identity{UserRef: ref, AuthMethod: "session", Capabilities: caps}
}

// TestIIIFImage_ContentGate_NonTeam covers public / restricted / embargo:
// the tiers where owner + content.read.all are the only non-admin ways in.
func TestIIIFImage_ContentGate_NonTeam(t *testing.T) {
	pool := iiifPool(t)

	public := seedIIIFAsset(t, pool, "active", "public", "ready", false)
	stranger := identity(4600999)                           // authenticated non-owner, no caps
	readAll := identity(4600888, visibility.ContentReadAll) // content.read.all, not owner

	// Public serves anyone, including anonymous — unchanged.
	if code, body := imageStatus(t, pool, public, nil); code != http.StatusOK || body != "WEBPBYTES" {
		t.Errorf("anonymous, public tile: code=%d body=%q, want 200 + bytes", code, body)
	}

	for _, tier := range []string{"restricted", "embargo"} {
		id := seedIIIFAsset(t, pool, "active", tier, "ready", false)

		// The #476 gap: an authenticated non-owner resolves the ROW
		// (info.json is 200 — see predicate_test) but must NOT get the
		// tile BYTES. 404, not 403.
		if code, _ := imageStatus(t, pool, id, stranger); code != http.StatusNotFound {
			t.Errorf("authenticated non-owner, %s tile: code=%d, want 404 (bytes gated; 404 not 403)", tier, code)
		}
		// The owner reaches their own bytes at any tier.
		if code, body := imageStatus(t, pool, id, identity(iiifOwnerRef)); code != http.StatusOK || body != "WEBPBYTES" {
			t.Errorf("owner, %s tile: code=%d body=%q, want 200 + bytes", tier, code, body)
		}
		// content.read.all (#474) reaches bytes at any tier without being admin.
		if code, body := imageStatus(t, pool, id, readAll); code != http.StatusOK || body != "WEBPBYTES" {
			t.Errorf("content.read.all, %s tile: code=%d body=%q, want 200 + bytes", tier, code, body)
		}
	}
}

// TestIIIFImage_ContentGate_TeamTier covers the membership-gated tier:
// a member streams the tiles, a non-member 404s.
func TestIIIFImage_ContentGate_TeamTier(t *testing.T) {
	pool := iiifPool(t)
	ctx := context.Background()

	teamAsset := seedIIIFAsset(t, pool, "active", "team", "ready", false)
	teamID := uuid.New()
	const memberRef int64 = 4600777
	slug := "iiif-cg-" + teamID.String()[:8]

	if _, err := pool.Exec(ctx, `INSERT INTO teams (id, name, slug) VALUES ($1,$2,$3)`, teamID, slug, slug); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO team_memberships (team_id, user_ref) VALUES ($1,$2)`, teamID, memberRef); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE assets SET team_id=$1 WHERE id=$2`, teamID, teamAsset); err != nil {
		t.Fatalf("attach team: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM team_memberships WHERE team_id=$1`, teamID)
		_, _ = pool.Exec(c, `DELETE FROM teams WHERE id=$1`, teamID)
	})

	// A team member streams the tiles.
	if code, body := imageStatus(t, pool, teamAsset, identity(memberRef)); code != http.StatusOK || body != "WEBPBYTES" {
		t.Errorf("team member, team tile: code=%d body=%q, want 200 + bytes", code, body)
	}
	// A non-member — even authenticated — 404s the bytes.
	if code, _ := imageStatus(t, pool, teamAsset, identity(4600666)); code != http.StatusNotFound {
		t.Errorf("non-member, team tile: code=%d, want 404", code)
	}
}
