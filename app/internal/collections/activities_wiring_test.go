// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Integration tests proving the activity emission wiring from
// Phase 1.22.A-bis-4 (ADR 0044) for the collections handler.
//
// One test per shape:
//   - CreateCollection — Create(aa:Collection) lands.
//   - AddCollectionResource covered the Add(asset, target=collection)
//     emission until #1161 retired that endpoint (ADR 0091: a
//     collection holds posts). The Add verb is still exercised on the
//     surviving membership path — posts.AddCollectionPost, in
//     posts/collection_posts_test.go.
//
// Skips without AA_DB_PASSWORD per project convention.

package collections_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/collections"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func openWiringPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envWiringOr("AA_DB_HOST", "postgres")
	port := envWiringOr("AA_DB_PORT", "5432")
	user := envWiringOr("AA_DB_USER", "artist_alley")
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
	return pool
}

func envWiringOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func randWiringHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

type wiringFixture struct {
	pool        *pgxpool.Pool
	registry    *cache.Registry
	collections *collections.Handler
	userRef     int64
	username    string
}

func setupWiringFixture(t *testing.T) *wiringFixture {
	t.Helper()
	pool := openWiringPool(t)
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := cache.NewRegistry(pool, logger)
	t.Cleanup(registry.Stop)

	collH := collections.NewHandler(pool, logger, registry)
	activitiesW := activities.NewWriter(pool, logger, registry)
	collH.SetActivitiesWriter(activitiesW, func(ctx context.Context) string {
		return "https://test.example"
	})

	ctx := context.Background()
	username := "coll-wiring-" + randWiringHex(t, 6)
	actorURI := "https://test.example/users/" + username
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved, actor_uri) VALUES ($1, $2, 1, $3) RETURNING ref`,
		username, "Coll Wiring", actorURI,
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM collection_resources WHERE collection_id IN (SELECT id FROM collections WHERE owner_user_ref = $1)`, ref)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE owner_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return &wiringFixture{
		pool: pool, registry: registry, collections: collH,
		userRef: ref, username: username,
	}
}

func (f *wiringFixture) withIdentity(ctx context.Context) context.Context {
	id := &auth.Identity{
		UserRef:      f.userRef,
		Username:     f.username,
		AuthMethod:   "session",
		Capabilities: []string{"system.admin", "collections.create", "collections.write"},
	}
	return auth.WithIdentity(ctx, id)
}

func TestCreateCollection_EmitsActivity(t *testing.T) {
	fx := setupWiringFixture(t)
	ctx, cancel := context.WithTimeout(fx.withIdentity(context.Background()), 15*time.Second)
	defer cancel()

	req := openapi.CreateCollectionRequestObject{
		Body: &openapi.CollectionCreate{Name: "Wiring Test Collection"},
	}
	resp, err := fx.collections.CreateCollection(ctx, req)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	created, ok := resp.(openapi.CreateCollection201JSONResponse)
	if !ok {
		t.Fatalf("unexpected response: %T", resp)
	}

	// Activity row exists with the expected shape.
	var activityType, objectKind, payloadName string
	var objectLocalID *string
	if err := fx.pool.QueryRow(ctx,
		`SELECT activity_type, COALESCE(object_kind,''), object_local_id, payload->>'name'
		 FROM activities WHERE actor_user_ref=$1 AND activity_type='Create' AND object_kind='collection'
		 ORDER BY published_at DESC LIMIT 1`,
		fx.userRef,
	).Scan(&activityType, &objectKind, &objectLocalID, &payloadName); err != nil {
		t.Fatalf("read activity: %v", err)
	}
	if activityType != "Create" {
		t.Errorf("activity_type: got %q want Create", activityType)
	}
	if objectKind != "collection" {
		t.Errorf("object_kind: got %q want collection", objectKind)
	}
	if objectLocalID == nil || *objectLocalID != created.Id.String() {
		got := "<nil>"
		if objectLocalID != nil {
			got = *objectLocalID
		}
		t.Errorf("object_local_id: got %q want %s", got, created.Id)
	}
	if payloadName != "Wiring Test Collection" {
		t.Errorf("payload.name: got %q", payloadName)
	}
}
