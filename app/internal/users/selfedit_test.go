// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.F — self-edit gate tests.
//
// Mixed unit (typed predicate + error wrap) + integration (real
// Postgres + LoadSelfEditGates against the migration-seeded
// system_config rows + UpdateUserProfile end-to-end gate rejection).

package users_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// ---------------------------------------------------------------
// SelfEditGates.Allows + FieldGateError contract (pure-Go)
// ---------------------------------------------------------------

func TestSelfEditGatesAllows_AllFalse_AllRejected(t *testing.T) {
	g := users.SelfEditGates{} // zero = every field disabled
	for _, f := range users.AllSelfEditFields {
		if g.Allows(f) {
			t.Errorf("zero-value gates allowed %s; want denied", f)
		}
	}
}

func TestSelfEditGatesAllows_AllTrue_AllAllowed(t *testing.T) {
	g := users.SelfEditGates{
		DisplayName: true, Bio: true, AvatarURL: true,
		Location: true, WebsiteURL: true,
	}
	for _, f := range users.AllSelfEditFields {
		if !g.Allows(f) {
			t.Errorf("all-true gates rejected %s; want allowed", f)
		}
	}
}

func TestSelfEditGatesAllows_UnknownField_FailsOpen(t *testing.T) {
	// Defence in depth — an unknown field name (somehow leaked
	// through the typed const) returns true. The caller wouldn't
	// pass an unknown field in practice; pin the fail-open
	// behavior so a future refactor can't accidentally lock down
	// unknown fields silently.
	g := users.SelfEditGates{} // all disabled
	if !g.Allows(users.SelfEditField("brand-new-field")) {
		t.Error("unknown field should fail-open to allowed")
	}
}

func TestFieldGateError_IsSentinel(t *testing.T) {
	e := &users.FieldGateError{Field: users.SelfEditBio}
	if !errors.Is(e, users.ErrFieldDisabledByOperator) {
		t.Errorf("errors.Is should match the sentinel")
	}
	if !contains(e.Error(), "bio") {
		t.Errorf("error message should include the field name; got %q", e.Error())
	}
}

// ---------------------------------------------------------------
// LoadSelfEditGates against the migration-seeded rows
// ---------------------------------------------------------------

func TestLoadSelfEditGates_DefaultsAllTrue(t *testing.T) {
	pool := openPoolF(t)
	defer restoreGates(t, pool)

	h := newHandlerF(t, pool)
	g, err := h.LoadSelfEditGates(context.Background())
	if err != nil {
		t.Fatalf("LoadSelfEditGates: %v", err)
	}
	if !g.DisplayName || !g.Bio || !g.AvatarURL || !g.Location || !g.WebsiteURL {
		t.Errorf("expected all gates true post-migration; got %+v", g)
	}
}

func TestLoadSelfEditGates_OperatorTogglesBio_ReflectsAfterInvalidate(t *testing.T) {
	pool := openPoolF(t)
	defer restoreGates(t, pool)

	h := newHandlerF(t, pool)
	// Warm the cache.
	if _, err := h.LoadSelfEditGates(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// Operator toggles bio off via direct DB write (the admin
	// handler exercises the upsert + invalidate; this test pins
	// the cache contract).
	if _, err := pool.Exec(context.Background(),
		`UPDATE system_config SET value = $1::jsonb WHERE key = 'users.allow_self_edit.bio'`,
		"false",
	); err != nil {
		t.Fatalf("toggle: %v", err)
	}
	// Pre-invalidate: cache still serves stale.
	g, _ := h.LoadSelfEditGates(context.Background())
	if !g.Bio {
		t.Errorf("expected pre-invalidate cache to still show bio=true; got %+v", g)
	}
	// Invalidate → next read picks up.
	h.InvalidateSelfEditGates(context.Background())
	g, _ = h.LoadSelfEditGates(context.Background())
	if g.Bio {
		t.Errorf("post-invalidate read should show bio=false; got %+v", g)
	}
}

// ---------------------------------------------------------------
// UpdateUserProfile end-to-end gate enforcement
// ---------------------------------------------------------------

func TestUpdateUserProfile_GatedFieldOnSelfEdit_Rejects422(t *testing.T) {
	pool := openPoolF(t)
	defer restoreGates(t, pool)
	subject := seedUserF(t, pool)

	// Disable bio in the gate.
	setGate(t, pool, "bio", false)

	h := newHandlerF(t, pool)
	caller := &auth.Identity{
		UserRef:      subject,
		Capabilities: []string{users.CapUpdateSelfProfile},
	}
	ctx := auth.WithIdentity(context.Background(), caller)

	bio := "new bio"
	resp, err := h.UpdateUserProfile(ctx, openapi.UpdateUserProfileRequestObject{
		Ref:  subject,
		Body: &openapi.UserProfileUpdate{Bio: &bio},
	})
	if err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}
	r422, ok := resp.(openapi.UpdateUserProfile422JSONResponse)
	if !ok {
		t.Fatalf("expected 422, got %T", resp)
	}
	if r422.Field != "bio" {
		t.Errorf("422.field = %q, want bio", r422.Field)
	}
	if r422.Reason != openapi.FieldDisabledByOperator {
		t.Errorf("422.reason = %q, want field_disabled_by_operator", r422.Reason)
	}
}

func TestUpdateUserProfile_UnGatedField_Succeeds(t *testing.T) {
	pool := openPoolF(t)
	defer restoreGates(t, pool)
	subject := seedUserF(t, pool)
	setGate(t, pool, "bio", false)
	// display_name stays true (default) — operator can still
	// edit. Pins that all-or-nothing rejection is per-PATCHed-
	// field, not blanket.

	h := newHandlerF(t, pool)
	caller := &auth.Identity{
		UserRef:      subject,
		Capabilities: []string{users.CapUpdateSelfProfile},
	}
	ctx := auth.WithIdentity(context.Background(), caller)

	displayName := "Renamed"
	resp, err := h.UpdateUserProfile(ctx, openapi.UpdateUserProfileRequestObject{
		Ref:  subject,
		Body: &openapi.UserProfileUpdate{DisplayName: &displayName},
	})
	if err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}
	if _, ok := resp.(openapi.UpdateUserProfile200JSONResponse); !ok {
		t.Fatalf("expected 200 for un-gated PATCH, got %T", resp)
	}
}

func TestUpdateUserProfile_AdminEditAnotherUser_BypassesGates(t *testing.T) {
	// CapEditAnyProfile must short-circuit the self-edit gate
	// check — operators editing other users' fields aren't
	// subject to the user-self lockouts.
	pool := openPoolF(t)
	defer restoreGates(t, pool)
	subject := seedUserF(t, pool)
	admin := seedUserF(t, pool)
	setGate(t, pool, "bio", false)

	h := newHandlerF(t, pool)
	caller := &auth.Identity{
		UserRef:      admin,
		Capabilities: []string{users.CapEditAnyProfile},
	}
	ctx := auth.WithIdentity(context.Background(), caller)

	bio := "admin edited"
	resp, err := h.UpdateUserProfile(ctx, openapi.UpdateUserProfileRequestObject{
		Ref:  subject,
		Body: &openapi.UserProfileUpdate{Bio: &bio},
	})
	if err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}
	if _, ok := resp.(openapi.UpdateUserProfile200JSONResponse); !ok {
		t.Fatalf("expected 200 for admin-edit-other bypass, got %T", resp)
	}
}

func TestUpdateUserProfile_SelfEditWithoutCapUpdateSelf_403(t *testing.T) {
	// An operator who's revoked the user's profile.update_self
	// capability should see a 403, not a 422 — distinct from
	// "this specific field is locked".
	pool := openPoolF(t)
	defer restoreGates(t, pool)
	subject := seedUserF(t, pool)

	h := newHandlerF(t, pool)
	caller := &auth.Identity{
		UserRef:      subject,
		Capabilities: []string{}, // no profile.update_self
	}
	ctx := auth.WithIdentity(context.Background(), caller)

	displayName := "nope"
	resp, err := h.UpdateUserProfile(ctx, openapi.UpdateUserProfileRequestObject{
		Ref:  subject,
		Body: &openapi.UserProfileUpdate{DisplayName: &displayName},
	})
	if err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}
	if _, ok := resp.(openapi.UpdateUserProfile403JSONResponse); !ok {
		t.Fatalf("expected 403, got %T", resp)
	}
}

// ---------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------

func openPoolF(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOrF("AA_DB_HOST", "postgres")
	port := envOrF("AA_DB_PORT", "5432")
	user := envOrF("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

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

func envOrF(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func newHandlerF(t *testing.T, pool *pgxpool.Pool) *users.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("registry start: %v", err)
	}
	t.Cleanup(reg.Stop)
	return users.NewHandler(pool, logger, reg)
}

func seedUserF(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	username := "sef-" + uuid.New().String()[:8]
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, password) VALUES ($1, '') RETURNING ref`, username,
	).Scan(&ref); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Profile row so UpdateUserProfile's GetUserPublicByRef finds it.
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_profiles (user_ref, display_name) VALUES ($1, $2)
		 ON CONFLICT (user_ref) DO NOTHING`,
		ref, "Initial",
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

func setGate(t *testing.T, pool *pgxpool.Pool, field string, v bool) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if _, err := pool.Exec(context.Background(),
		`UPDATE system_config SET value = $1::jsonb WHERE key = $2`,
		raw, "users.allow_self_edit."+field,
	); err != nil {
		t.Fatalf("setGate: %v", err)
	}
}

// restoreGates resets all 5 gates to true so adjacent integration
// tests don't see surprise lockouts from a previous test's
// half-cleaned state.
func restoreGates(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, k := range []string{"display_name", "bio", "avatar_url", "location", "website_url"} {
		_, _ = pool.Exec(context.Background(),
			`UPDATE system_config SET value = 'true'::jsonb WHERE key = $1`,
			"users.allow_self_edit."+k)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// keep imports stable
var _ = httptest.NewRequest
