// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #460 — IIIF row-resolution goes through the visibility predicate.
//
// Drives the REAL handler (serveInfo) with the REAL PoolLookup against
// real rows, per the brief — not the loader in isolation. Asserts the
// row-plane outcome: a caller who may not see an asset gets 404 (never
// 403), a public asset resolves, and an authenticated caller resolves a
// private asset (the deferred authenticated-sensitivity rule, same as
// browse).
//
// Skips without AA_DB_PASSWORD.

package iiif_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/iiif"
)

func iiifPool(t *testing.T) *pgxpool.Pool {
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
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + env("AA_DB_NAME", "artist_alley") +
		" sslmode=disable password=" + pwd
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ensurePixelFields makes sure the two IIIF pixel field definitions
// exist and returns their ids. IIIF info.json 404s an asset with no
// pixel dimensions, so a resolvable asset needs them to reach 200 —
// this lets the test distinguish "404 because hidden" (the #460 fix)
// from "404 because no pixels".
func ensurePixelFields(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	one := func(code string) uuid.UUID {
		var id uuid.UUID
		err := pool.QueryRow(context.Background(),
			`INSERT INTO field_definition (code, label, type) VALUES ($1,$1,'number')
			 ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
			 RETURNING id`, code).Scan(&id)
		if err != nil {
			t.Fatalf("ensure field %s: %v", code, err)
		}
		return id
	}
	return one("pixel_width"), one("pixel_height")
}

// hashFor derives a unique valid 64-hex storage hash from an asset id.
// serveInfo 404s an asset with an empty file_hash (which FKs to
// storage_objects), and (owner_user_ref, file_hash) is unique, so each
// seeded asset needs its own object.
func hashFor(id uuid.UUID) string {
	sum := sha256.Sum256(id[:])
	return hex.EncodeToString(sum[:])
}

func ensureStorageObject(t *testing.T, pool *pgxpool.Pool, hash string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')
		 ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatalf("ensure storage object: %v", err)
	}
}

func seedIIIFAsset(t *testing.T, pool *pgxpool.Pool, status, sensitivity, processing string, deleted bool) uuid.UUID {
	t.Helper()
	wField, hField := ensurePixelFields(t, pool)
	id := uuid.New()
	hash := hashFor(id)
	ensureStorageObject(t, pool, hash)
	del := "NULL"
	if deleted {
		del = "NOW()"
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity, processing_status, has_image, file_hash, deleted_at)
		 VALUES ($1,$2,4600001,(SELECT MIN(ref) FROM asset_types),$3,$4,$5,true,$6, `+del+`)`,
		id, "iiif-vis", status, sensitivity, processing, hash)
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	// Pixel dims so a resolvable asset yields a real 200.
	for _, fv := range []struct {
		field uuid.UUID
		val   int
	}{{wField, 1000}, {hField, 1000}} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO asset_field_value (asset_id, field_id, value_num) VALUES ($1,$2,$3)`,
			id, fv.field, fv.val); err != nil {
			t.Fatalf("seed pixel value: %v", err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id=$1`, id) })
	return id
}

// infoStatus drives the real serveInfo handler for an asset id and
// caller identity, returning the HTTP status.
func infoStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, identity *auth.Identity) int {
	t.Helper()
	h := &iiif.Handler{
		Lookup:   iiif.PoolLookup{Pool: pool},
		Variants: fixedVariants{},
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
	r.ServeHTTP(rr, httptest.NewRequest("GET", "/iiif/3/"+id.String()+"/info.json", nil))
	return rr.Code
}

// fixedVariants advertises one size so a resolvable asset yields 200
// rather than a variant-related error.
type fixedVariants struct{}

func (fixedVariants) ListIIIFVariants(context.Context) ([]iiif.VariantSize, error) {
	return []iiif.VariantSize{{Key: "full", MaxDim: 1000}}, nil
}

func TestIIIFInfo_AnonymousThroughPredicate(t *testing.T) {
	pool := iiifPool(t)

	pub := seedIIIFAsset(t, pool, "active", "public", "ready", false)
	private := seedIIIFAsset(t, pool, "active", "restricted", "ready", false)
	softDel := seedIIIFAsset(t, pool, "active", "public", "ready", true)

	if got := infoStatus(t, pool, pub, nil); got != http.StatusOK {
		t.Errorf("anonymous, public asset: info.json = %d, want 200", got)
	}
	// A non-public asset must not confirm its existence to anonymous — 404,
	// not 403.
	if got := infoStatus(t, pool, private, nil); got != http.StatusNotFound {
		t.Errorf("anonymous, restricted asset: info.json = %d, want 404 (row hidden, "+
			"and 404 not 403 so existence is not confirmed)", got)
	}
	if got := infoStatus(t, pool, softDel, nil); got != http.StatusNotFound {
		t.Errorf("anonymous, soft-deleted asset: info.json = %d, want 404", got)
	}
}

func TestIIIFInfo_AuthenticatedResolvesPrivate(t *testing.T) {
	pool := iiifPool(t)
	private := seedIIIFAsset(t, pool, "active", "restricted", "ready", false)
	softDel := seedIIIFAsset(t, pool, "active", "public", "ready", true)

	// Authenticated: the predicate is soft-delete-only, so a private
	// asset resolves (same as browse; the authenticated-sensitivity rule
	// is deferred, ADR 0063).
	id := &auth.Identity{UserRef: 999, AuthMethod: "session"}
	if got := infoStatus(t, pool, private, id); got != http.StatusOK {
		t.Errorf("authenticated, restricted asset: info.json = %d, want 200", got)
	}
	// But a soft-deleted asset is 404 for everyone.
	if got := infoStatus(t, pool, softDel, id); got != http.StatusNotFound {
		t.Errorf("authenticated, soft-deleted asset: info.json = %d, want 404", got)
	}
}
