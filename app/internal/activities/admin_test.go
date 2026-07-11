// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the admin audit endpoint added in 1.22.A-bis-3b.
// Coverage:
//   - cap-gate: non-admin gets 403
//   - cap-gate: anonymous gets 401
//   - filter behaviour: type filter narrows the result set
//   - pagination: limit caps result count; next_cursor advances
//
// Skips without AA_DB_PASSWORD per project convention.

package activities_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// adminFixture wires an AdminHandler against the live pool. The
// admin tests share the same activities-writer setup the
// activities_test.go integration tests use, plus pre-seeds a
// handful of activities the admin endpoint can list.
type adminFixture struct {
	pool    *pgxpool.Pool
	writer  *activities.Writer
	handler *activities.AdminHandler
	actorRef int64
}

func setupAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := cache.NewRegistry(pool, logger)
	t.Cleanup(registry.Stop)

	writer := activities.NewWriter(pool, logger, registry)
	handler := activities.NewAdminHandler(writer)

	// Use the existing fixtureUser helper from activities_test.go
	// (same package).
	ref, _, actorURI := fixtureUser(t, ctx, pool)

	// Seed a small assortment of activities so the list endpoint
	// has something to surface. Three Likes + one Follow + one
	// Create — enough to exercise the type filter + pagination
	// without polluting the table.
	for i := 0; i < 3; i++ {
		seedActivity(t, ctx, writer, ref, actorURI, federation.ActivityLike, activities.ObjectKindPost, randHex(t, 12))
	}
	seedActivity(t, ctx, writer, ref, actorURI, federation.ActivityFollow, activities.ObjectKindUser, "99")
	seedActivity(t, ctx, writer, ref, actorURI, federation.ActivityCreate, activities.ObjectKindPost, randHex(t, 12))

	return &adminFixture{
		pool: pool, writer: writer, handler: handler, actorRef: ref,
	}
}

// seedActivity emits one activity through the writer + commits.
// Helper for setupAdminFixture to populate the table.
func seedActivity(
	t *testing.T,
	ctx context.Context,
	w *activities.Writer,
	actorRef int64,
	actorURI string,
	typ federation.ActivityType,
	kind activities.ActivityObjectKind,
	localID string,
) {
	t.Helper()
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = w.RecordActivity(ctx, tx, activities.Input{
		Type:         typ,
		ActivityURI:  activities.MintActivityURI("https://test.example") + "/" + randHex(t, 4),
		ActorUserRef: &actorRef,
		ActorURI:     actorURI,
		Object: &activities.ObjectRef{
			URI:     "https://test.example/" + string(kind) + "/" + localID,
			Kind:    kind,
			LocalID: localID,
		},
	})
	if err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
}

func adminCtx(ctx context.Context, ref int64) context.Context {
	return auth.WithIdentity(ctx, &auth.Identity{
		UserRef:      ref,
		Username:     "admin-fixture",
		AuthMethod:   "session",
		Capabilities: []string{"system.admin"},
	})
}

// --- cap-gating ----------------------------------------------------------

func TestListAdminActivities_RejectsAnonymous(t *testing.T) {
	fx := setupAdminFixture(t)
	resp, err := fx.handler.ListAdminActivities(context.Background(), openapi.ListAdminActivitiesRequestObject{})
	if err != nil {
		t.Fatalf("ListAdminActivities: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminActivities401JSONResponse); !ok {
		t.Errorf("anonymous caller: expected 401, got %T", resp)
	}
}

func TestListAdminActivities_RejectsNonAdmin(t *testing.T) {
	fx := setupAdminFixture(t)
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{
		UserRef:      fx.actorRef,
		Username:     "non-admin",
		AuthMethod:   "session",
		Capabilities: []string{"posts.read"}, // no system.admin
	})
	resp, err := fx.handler.ListAdminActivities(ctx, openapi.ListAdminActivitiesRequestObject{})
	if err != nil {
		t.Fatalf("ListAdminActivities: %v", err)
	}
	if _, ok := resp.(openapi.ListAdminActivities403JSONResponse); !ok {
		t.Errorf("non-admin caller: expected 403, got %T", resp)
	}
}

// --- filter behaviour ----------------------------------------------------

func TestListAdminActivities_TypeFilter(t *testing.T) {
	fx := setupAdminFixture(t)
	ctx, cancel := context.WithTimeout(adminCtx(context.Background(), fx.actorRef), 10*time.Second)
	defer cancel()

	likeStr := "Like"
	actorRef := fx.actorRef
	resp, err := fx.handler.ListAdminActivities(ctx, openapi.ListAdminActivitiesRequestObject{
		Params: openapi.ListAdminActivitiesParams{
			ActivityType: &likeStr,
			ActorUserRef: &actorRef,
		},
	})
	if err != nil {
		t.Fatalf("ListAdminActivities: %v", err)
	}
	ok := resp.(openapi.ListAdminActivities200JSONResponse)
	if len(ok.Items) < 3 {
		t.Errorf("Like filter: expected ≥3 items (we seeded 3), got %d", len(ok.Items))
	}
	for _, it := range ok.Items {
		if it.ActivityType != "Like" {
			t.Errorf("Like filter returned non-Like activity: %s", it.ActivityType)
		}
		if it.ActorUserRef == nil || *it.ActorUserRef != fx.actorRef {
			t.Errorf("actor filter leaked another actor's row: actor_user_ref=%v", it.ActorUserRef)
		}
	}
}

// --- pagination ---------------------------------------------------------

func TestListAdminActivities_PaginationCursor(t *testing.T) {
	fx := setupAdminFixture(t)
	ctx, cancel := context.WithTimeout(adminCtx(context.Background(), fx.actorRef), 10*time.Second)
	defer cancel()

	// Page 1: limit=2 → expect next_cursor since we have 5 seeded.
	actorRef := fx.actorRef
	lim := 2
	resp1, err := fx.handler.ListAdminActivities(ctx, openapi.ListAdminActivitiesRequestObject{
		Params: openapi.ListAdminActivitiesParams{
			ActorUserRef: &actorRef,
			Limit:        &lim,
		},
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	page1 := resp1.(openapi.ListAdminActivities200JSONResponse)
	if len(page1.Items) != 2 {
		t.Errorf("page 1: expected 2 items (limit), got %d", len(page1.Items))
	}
	if page1.NextCursor == nil {
		t.Fatal("page 1: expected next_cursor (we seeded 5 rows for this actor)")
	}

	// Page 2: use the cursor; expect the cursor advances past
	// page 1's content.
	resp2, err := fx.handler.ListAdminActivities(ctx, openapi.ListAdminActivitiesRequestObject{
		Params: openapi.ListAdminActivitiesParams{
			ActorUserRef: &actorRef,
			Limit:        &lim,
			Cursor:       page1.NextCursor,
		},
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	page2 := resp2.(openapi.ListAdminActivities200JSONResponse)
	if len(page2.Items) == 0 {
		t.Fatal("page 2: expected items, got empty")
	}
	// Page 2 IDs must not overlap page 1 IDs.
	for _, it1 := range page1.Items {
		for _, it2 := range page2.Items {
			if it1.Id == it2.Id {
				t.Errorf("cursor pagination overlap: id %s appears on both pages", it1.Id)
			}
		}
	}
}

// --- shape check --------------------------------------------------------

func TestListAdminActivities_ProjectsFullRow(t *testing.T) {
	fx := setupAdminFixture(t)
	ctx, cancel := context.WithTimeout(adminCtx(context.Background(), fx.actorRef), 10*time.Second)
	defer cancel()

	actorRef := fx.actorRef
	lim := 1
	resp, err := fx.handler.ListAdminActivities(ctx, openapi.ListAdminActivitiesRequestObject{
		Params: openapi.ListAdminActivitiesParams{
			ActorUserRef: &actorRef,
			Limit:        &lim,
		},
	})
	if err != nil {
		t.Fatalf("ListAdminActivities: %v", err)
	}
	ok := resp.(openapi.ListAdminActivities200JSONResponse)
	if len(ok.Items) == 0 {
		t.Fatal("expected at least one item")
	}
	it := ok.Items[0]
	if it.ActivityUri == "" {
		t.Error("activity_uri should be populated")
	}
	if it.ActorUri == "" {
		t.Error("actor_uri should be populated")
	}
	if it.Source == "" {
		t.Error("source should be populated")
	}
	if it.PublishedAt.IsZero() {
		t.Error("published_at should be populated")
	}
	// Payload may be empty for our seeds but the field should
	// exist (decoded into the map).
	if it.Payload == nil {
		t.Error("payload field should be non-nil (empty object is allowed)")
	}
}
