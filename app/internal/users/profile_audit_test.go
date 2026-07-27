// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Phase 1.17.D — UpdateUserProfile audit retrofit integration tests.
//
// Verifies the user.profile_updated event emits with a
// metadata.changeset that reflects only the changed fields,
// and that the no-change path (idempotent self-save) emits the
// event without a changeset key.

package users_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

func TestUpdateUserProfile_EmitsChangeset_OnFieldEdit(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openProfileAuditPool(t, pwd)
	subjectRef := seedProfileSubject(t, pool, "edit")
	cleanupProfileEvents(t, pool, subjectRef)
	t.Cleanup(func() { cleanupProfileEvents(t, pool, subjectRef) })

	h := newProfileHandler(t, pool)
	// Phase 1.17.F gates self-edit on profile.update_self — the
	// 1.17.D test path is the canonical self-edit, so grant it
	// here. Without the cap the handler now 403s before the audit
	// event is emitted.
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: subjectRef, Username: "subj", Capabilities: []string{users.CapUpdateSelfProfile}})

	body := openapi.UserProfileUpdate{
		DisplayName: strPtrP("New Display Name"),
		Bio:         strPtrP("a fresh bio"),
	}
	resp, err := h.UpdateUserProfile(ctx, openapi.UpdateUserProfileRequestObject{Ref: subjectRef, Body: &body})
	if err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}
	if _, ok := resp.(openapi.UpdateUserProfile200JSONResponse); !ok {
		t.Fatalf("expected 200, got %T", resp)
	}

	cs := readProfileChangeset(t, pool, subjectRef)
	if cs == nil {
		t.Fatal("no audit row + changeset emitted for profile edit")
	}
	if _, ok := cs["DisplayName"]; !ok {
		t.Errorf("DisplayName missing from changeset: %v", cs)
	}
	if _, ok := cs["Bio"]; !ok {
		t.Errorf("Bio missing from changeset: %v", cs)
	}
	// Untouched fields must not appear.
	for _, untouched := range []string{"AvatarURL", "Location", "WebsiteURL", "Language", "Theme"} {
		if _, leaked := cs[untouched]; leaked {
			t.Errorf("untouched field %s appeared in changeset: %v", untouched, cs)
		}
	}
}

func TestUpdateUserProfile_NoChange_NoChangesetKey(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openProfileAuditPool(t, pwd)
	subjectRef := seedProfileSubject(t, pool, "nochange")
	cleanupProfileEvents(t, pool, subjectRef)
	t.Cleanup(func() { cleanupProfileEvents(t, pool, subjectRef) })

	// Seed a profile so existing has values.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_profiles (user_ref, display_name, bio) VALUES ($1, 'Same', 'same bio')
		 ON CONFLICT (user_ref) DO UPDATE SET display_name = EXCLUDED.display_name, bio = EXCLUDED.bio`,
		subjectRef,
	); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	h := newProfileHandler(t, pool)
	ctx := auth.WithIdentity(context.Background(),
		&auth.Identity{UserRef: subjectRef, Capabilities: []string{users.CapUpdateSelfProfile}})

	// Submit the SAME values as the existing profile → no diff.
	body := openapi.UserProfileUpdate{
		DisplayName: strPtrP("Same"),
		Bio:         strPtrP("same bio"),
	}
	if _, err := h.UpdateUserProfile(ctx, openapi.UpdateUserProfileRequestObject{Ref: subjectRef, Body: &body}); err != nil {
		t.Fatalf("UpdateUserProfile: %v", err)
	}

	md := readProfileMetadata(t, pool, subjectRef)
	if md == nil {
		t.Fatal("no audit row for no-op profile save")
	}
	if _, has := md["changeset"]; has {
		t.Errorf("no-op save should not include changeset key: %v", md)
	}
}

// ---------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------

func openProfileAuditPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOrProfile("AA_DB_HOST", "postgres")
	port := envOrProfile("AA_DB_PORT", "5432")
	user := envOrProfile("AA_DB_USER", "artist_alley")
	name := envOrProfile("AA_DB_NAME", "artist_alley")
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

func envOrProfile(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func newProfileHandler(t *testing.T, pool *pgxpool.Pool) *users.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("registry start: %v", err)
	}
	t.Cleanup(reg.Stop)
	h := users.NewHandler(pool, logger, reg)
	h.SetAuditRecorder(&audit.Recorder{Pool: pool, Logger: logger})
	return h
}

func seedProfileSubject(t *testing.T, pool *pgxpool.Pool, _ string) int64 {
	t.Helper()
	ctx := context.Background()
	q := auth.New(pool)
	username := "ps-" + randHexProfile(8)
	pw := "irrelevant"
	u, err := q.CreateUser(ctx, auth.CreateUserParams{Username: &username, Password: &pw})
	if err != nil {
		t.Fatalf("seed subject: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = $1`, u.Ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, u.Ref)
	})
	return u.Ref
}

func cleanupProfileEvents(t *testing.T, pool *pgxpool.Pool, subjectRef int64) {
	t.Helper()
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM audit_events WHERE event_type = 'user.profile_updated' AND subject_user_ref = $1`,
		subjectRef)
}

func readProfileChangeset(t *testing.T, pool *pgxpool.Pool, subjectRef int64) map[string]any {
	t.Helper()
	md := readProfileMetadata(t, pool, subjectRef)
	if md == nil {
		return nil
	}
	cs, ok := md["changeset"].(map[string]any)
	if !ok {
		return nil
	}
	return cs
}

func readProfileMetadata(t *testing.T, pool *pgxpool.Pool, subjectRef int64) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT metadata FROM audit_events
		 WHERE event_type = 'user.profile_updated' AND subject_user_ref = $1
		 ORDER BY occurred_at DESC LIMIT 1`,
		subjectRef,
	).Scan(&raw)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("metadata unmarshal: %v", err)
	}
	return out
}

func strPtrP(s string) *string { return &s }

func randHexProfile(n int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, n)
	for i := range out {
		out[i] = hex[time.Now().UnixNano()>>uint(i)&0xF]
	}
	return string(out)
}
