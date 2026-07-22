// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #478 slice-1 — public user-profile page access rules (ADR 0070).
//
// Verifies the two behaviours the public read handlers add on top of the
// existing profile row: an anonymous viewer never sees the real name
// (ADR 0070 §3), and an owner who opted out of anonymous exposure
// (ADR 0024) is a 404 to anonymous viewers but still visible to
// authenticated ones. Public-mode admission itself is enforced upstream
// by the auth middleware (auth.PublicSurfaceRoutes) and is covered there.
//
// Skips without AA_DB_PASSWORD, like the other users integration tests.

package users_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

func seedProfileSubjectWithName(t *testing.T, pool *pgxpool.Pool, fullname string) (ref int64, username string) {
	t.Helper()
	ref = seedProfileSubject(t, pool, "public")
	if err := pool.QueryRow(context.Background(),
		`UPDATE "user" SET fullname = $1 WHERE ref = $2 RETURNING username`,
		fullname, ref).Scan(&username); err != nil {
		t.Fatalf("set fullname: %v", err)
	}
	return ref, username
}

func TestPublicProfile_AnonymousStripsRealName(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openProfileAuditPool(t, pwd)
	ref, username := seedProfileSubjectWithName(t, pool, "Ada Lovelace")
	h := newProfileHandler(t, pool)

	anon := context.Background()
	authed := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: ref + 1, Username: "viewer"}) // a non-owner authenticated viewer

	// --- by-ref path ---
	anonResp, err := h.GetUserPublicByRefPath(anon, openapi.GetUserPublicByRefPathRequestObject{Ref: ref})
	if err != nil {
		t.Fatalf("by-ref anon: %v", err)
	}
	anon200, ok := anonResp.(openapi.GetUserPublicByRefPath200JSONResponse)
	if !ok {
		t.Fatalf("by-ref anon: expected 200, got %T", anonResp)
	}
	if anon200.Fullname != nil {
		t.Errorf("anonymous profile leaked real name: %q", *anon200.Fullname)
	}
	// Display name must not silently become the real name either.
	if anon200.DisplayName == "Ada Lovelace" {
		t.Errorf("anonymous display_name fell back to the real name")
	}

	authResp, err := h.GetUserPublicByRefPath(authed, openapi.GetUserPublicByRefPathRequestObject{Ref: ref})
	if err != nil {
		t.Fatalf("by-ref authed: %v", err)
	}
	auth200, ok := authResp.(openapi.GetUserPublicByRefPath200JSONResponse)
	if !ok {
		t.Fatalf("by-ref authed: expected 200, got %T", authResp)
	}
	if auth200.Fullname == nil || *auth200.Fullname != "Ada Lovelace" {
		t.Errorf("authenticated viewer should see the real name, got %v", auth200.Fullname)
	}

	// --- by-username path — same stripping ---
	uResp, err := h.GetUserPublicByUsername(anon, openapi.GetUserPublicByUsernameRequestObject{Username: username})
	if err != nil {
		t.Fatalf("by-username anon: %v", err)
	}
	u200, ok := uResp.(openapi.GetUserPublicByUsername200JSONResponse)
	if !ok {
		t.Fatalf("by-username anon: expected 200, got %T", uResp)
	}
	if u200.Fullname != nil {
		t.Errorf("anonymous by-username leaked real name: %q", *u200.Fullname)
	}
}

func TestPublicProfile_AnonymousOptOutIs404(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openProfileAuditPool(t, pwd)
	ref, username := seedProfileSubjectWithName(t, pool, "Grace Hopper")
	h := newProfileHandler(t, pool)

	// Owner opts out of anonymous exposure.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_profiles (user_ref, hide_from_anonymous) VALUES ($1, true)
		 ON CONFLICT (user_ref) DO UPDATE SET hide_from_anonymous = true`, ref); err != nil {
		t.Fatalf("set opt-out: %v", err)
	}

	anon := context.Background()
	authed := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: ref + 1, Username: "viewer"})

	// Anonymous → 404 (don't confirm the profile exists).
	r, err := h.GetUserPublicByRefPath(anon, openapi.GetUserPublicByRefPathRequestObject{Ref: ref})
	if err != nil {
		t.Fatalf("by-ref anon opt-out: %v", err)
	}
	if _, ok := r.(openapi.GetUserPublicByRefPath404JSONResponse); !ok {
		t.Errorf("opted-out profile should 404 to anonymous, got %T", r)
	}
	ur, err := h.GetUserPublicByUsername(anon, openapi.GetUserPublicByUsernameRequestObject{Username: username})
	if err != nil {
		t.Fatalf("by-username anon opt-out: %v", err)
	}
	if _, ok := ur.(openapi.GetUserPublicByUsername404JSONResponse); !ok {
		t.Errorf("opted-out profile should 404 to anonymous by-username, got %T", ur)
	}

	// Authenticated viewer still sees it — the opt-out is about anonymous
	// exposure only.
	ar, err := h.GetUserPublicByRefPath(authed, openapi.GetUserPublicByRefPathRequestObject{Ref: ref})
	if err != nil {
		t.Fatalf("by-ref authed opt-out: %v", err)
	}
	if _, ok := ar.(openapi.GetUserPublicByRefPath200JSONResponse); !ok {
		t.Errorf("opted-out profile should still be visible to authenticated viewers, got %T", ar)
	}
}
