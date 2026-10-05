// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #937 — GET /account/trash, driven end-to-end through the strict
// server.
//
// Three properties, each with the negative made reachable first:
//
//  1. It is not a probe. Another user's soft-deleted item never appears
//     in mine — proven against a POSITIVE CONTROL that shows the same
//     item DOES appear in its own owner's trash, so a build that
//     returned an empty page for everybody fails here instead of
//     passing.
//
//  2. restorable_by_caller agrees with the restore endpoint, both ways.
//     The false cases are checked against the REAL refusal: the test
//     drives POST /admin/assets/{id}/restore as the owner and requires
//     an actual 403, so "false" is never rendered on a guess.
//
//  3. The (deleted_at, id) keyset holds across kinds and pages,
//     including items that share a deleted_at to the microsecond —
//     which on this surface is the ordinary case, not the exotic one.
//
// Skips without AA_DB_PASSWORD.

package trash_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/openapi/strictservershim"
	"github.com/mscrnt/artist-alley/app/internal/softdelete"
	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/trash"
)

// --- harness ---------------------------------------------------------------

type shimImpl struct {
	*strictservershim.PanicShim
	trash  *trash.Handler
	assets *assets.Handler
}

func (s shimImpl) ListMyTrash(ctx context.Context, req openapi.ListMyTrashRequestObject) (openapi.ListMyTrashResponseObject, error) {
	return s.trash.ListMyTrash(ctx, req)
}

func (s shimImpl) RestoreAsset(ctx context.Context, req openapi.RestoreAssetRequestObject) (openapi.RestoreAssetResponseObject, error) {
	return s.assets.RestoreAsset(ctx, req)
}

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	shim shimImpl
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	pool := openPool(t, pwd)
	t.Cleanup(pool.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	assetsH := assets.NewHandler(pool, storage.NewService(backend, pool), logger, nil, nil, nil)
	assetsH.SoftDelete = softdelete.NewService(pool, nil)

	return &fixture{
		t:    t,
		pool: pool,
		shim: shimImpl{
			PanicShim: &strictservershim.PanicShim{},
			trash:     trash.NewHandler(pool, nil, logger),
			assets:    assetsH,
		},
	}
}

// routerAs mounts the whole strict server behind an identity.
func (f *fixture) routerAs(userRef int64, caps ...string) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := &auth.Identity{UserRef: userRef, AuthMethod: "session", Capabilities: caps}
			next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
		})
	})
	openapi.HandlerFromMux(openapi.NewStrictHandler(f.shim, nil), r)
	return r
}

func (f *fixture) getTrash(userRef int64, query string, caps ...string) openapi.TrashList {
	f.t.Helper()
	rr := httptest.NewRecorder()
	url := "/account/trash"
	if query != "" {
		url += "?" + query
	}
	f.routerAs(userRef, caps...).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		f.t.Fatalf("GET %s as %d = %d, body=%s", url, userRef, rr.Code, rr.Body.String())
	}
	var page openapi.TrashList
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		f.t.Fatalf("decode trash page: %v (body=%s)", err, rr.Body.String())
	}
	return page
}

func (f *fixture) restoreAsset(userRef int64, id uuid.UUID, caps ...string) int {
	f.t.Helper()
	rr := httptest.NewRecorder()
	f.routerAs(userRef, caps...).ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/admin/assets/"+id.String()+"/restore", nil))
	return rr.Code
}

// user creates a throwaway user and returns its ref.
func (f *fixture) user(tag string) int64 {
	f.t.Helper()
	var ref int64
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		"trash-"+tag+"-"+uuid.NewString()[:8],
	).Scan(&ref); err != nil {
		f.t.Fatalf("seed user %s: %v", tag, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref
}

// asset seeds one asset. deletedAt nil => live. deletedBy nil => the
// "we do not know who did this" case.
func (f *fixture) asset(owner int64, title string, deletedAt *time.Time, deletedBy *int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO assets (id, title, owner_user_ref, asset_type, status, sensitivity,
		                    processing_status, deleted_at, deleted_by_user_ref)
		VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','public','ready',$4,$5)`,
		id, title, owner, deletedAt, deletedBy); err != nil {
		f.t.Fatalf("seed asset %s: %v", title, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
	})
	return id
}

func (f *fixture) post(author int64, title string, deletedAt *time.Time, deletedBy *int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO posts (id, author_user_ref, title, visibility, deleted_at, deleted_by_user_ref)
		VALUES ($1,$2,$3,'private',$4,$5)`,
		id, author, title, deletedAt, deletedBy); err != nil {
		f.t.Fatalf("seed post %s: %v", title, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id)
	})
	return id
}

func (f *fixture) collection(owner int64, name string, deletedAt *time.Time, deletedBy *int64) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `
		INSERT INTO collections (id, owner_user_ref, name, description, visibility, membership,
		                         deleted_at, deleted_by_user_ref)
		VALUES ($1,$2,$3,'','private','manual',$4,$5)`,
		id, owner, name, deletedAt, deletedBy); err != nil {
		f.t.Fatalf("seed collection %s: %v", name, err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

func idsOf(page openapi.TrashList) map[uuid.UUID]openapi.TrashItem {
	out := make(map[uuid.UUID]openapi.TrashItem, len(page.Items))
	for _, it := range page.Items {
		out[uuid.UUID(it.Id)] = it
	}
	return out
}

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	envOr := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok && v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + envOr("AA_DB_HOST", "postgres") +
		" port=" + envOr("AA_DB_PORT", "5432") +
		" user=" + envOr("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(t.Context()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// --- 1. owner-scoped, with a positive control ------------------------------

// The listing must be a projection of MY bin, not a window onto the
// table. The control matters more than the assertion it guards: an
// endpoint that returned nothing at all would satisfy "B's item is
// absent from A's trash" perfectly, so the same item is first shown to
// be visible to B.
func TestListMyTrash_IsOwnerScopedNotAProbe(t *testing.T) {
	f := newFixture(t)
	userA := f.user("a")
	userB := f.user("b")

	now := time.Now().UTC()
	bDeleted := f.asset(userB, "b-deleted", &now, &userB)
	aDeleted := f.asset(userA, "a-deleted", &now, &userA)
	aLive := f.asset(userA, "a-live", nil, nil)

	// POSITIVE CONTROL — B's own trash does contain B's item. Without
	// this, every assertion below passes on a broken build.
	bPage := idsOf(f.getTrash(userB, "limit=200"))
	if _, ok := bPage[bDeleted]; !ok {
		t.Fatalf("positive control failed: B's own soft-deleted asset is missing from B's trash; "+
			"the absence check below would prove nothing (got %d items)", len(bPage))
	}

	aPage := idsOf(f.getTrash(userA, "limit=200"))
	if _, ok := aPage[aDeleted]; !ok {
		t.Error("A's own soft-deleted asset is missing from A's trash")
	}
	if _, ok := aPage[bDeleted]; ok {
		t.Error("A's trash listed an asset owned and deleted by B — /account/trash must be " +
			"owner-scoped, never a probe for other people's deleted rows")
	}
	if _, ok := aPage[aLive]; ok {
		t.Error("A's trash listed a LIVE asset; the listing must be soft-deleted rows only")
	}
}

// The other half of "owner-scoped": ownership decides membership, the
// DELETER does not. An admin removing my asset leaves it in MY bin
// (flagged un-restorable), and not in the admin's.
func TestListMyTrash_AdminDeletedItemStaysInTheOwnersBin(t *testing.T) {
	f := newFixture(t)
	owner := f.user("owner")
	admin := f.user("admin")

	now := time.Now().UTC()
	id := f.asset(owner, "moderated", &now, &admin)

	ownerPage := idsOf(f.getTrash(owner, "limit=200"))
	item, ok := ownerPage[id]
	if !ok {
		t.Fatal("an admin-deleted asset vanished from its owner's trash; it is recoverable by " +
			"request (#931) and must be visible, not silently gone")
	}
	if item.RestorableByCaller {
		t.Error("owner must not be told they can restore an admin's delete")
	}

	adminPage := idsOf(f.getTrash(admin, "limit=200", auth.SuperAdminCapability))
	if _, ok := adminPage[id]; ok {
		t.Error("the admin's own trash listed someone else's asset; /account/trash is owner-" +
			"scoped for everyone, super-admin included — /admin/storage/trash is the operator view")
	}
}

// --- 2. the flag, against the real refusal ---------------------------------

func TestListMyTrash_RestorableFlagMatchesTheRestoreEndpoint(t *testing.T) {
	f := newFixture(t)
	owner := f.user("owner")
	admin := f.user("admin")

	now := time.Now().UTC()
	selfDeleted := f.asset(owner, "self-deleted", &now, &owner)
	adminDeleted := f.asset(owner, "admin-deleted", &now, &admin)
	nullDeleted := f.asset(owner, "null-deleter", &now, nil)

	page := idsOf(f.getTrash(owner, "limit=200"))
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		want bool
	}{
		{"self-deleted", selfDeleted, true},
		{"admin-deleted", adminDeleted, false},
		{"null-deleter", nullDeleted, false},
	} {
		item, ok := page[tc.id]
		if !ok {
			t.Fatalf("%s: missing from owner's trash", tc.name)
		}
		if item.RestorableByCaller != tc.want {
			t.Errorf("%s: restorable_by_caller = %v, want %v", tc.name, item.RestorableByCaller, tc.want)
		}
	}

	// The two `false` rows must be false because the endpoint REFUSES,
	// not because we decided to render an explanation. Prove the
	// refusal is real before trusting the flag that describes it.
	if code := f.restoreAsset(owner, adminDeleted); code != http.StatusForbidden {
		t.Errorf("owner restoring an admin-deleted asset = %d, want 403 — the flag claims "+
			"un-restorable, so the endpoint must actually say no", code)
	}
	if code := f.restoreAsset(owner, nullDeleted); code != http.StatusForbidden {
		t.Errorf("owner restoring a NULL-deleter asset = %d, want 403", code)
	}

	// And the `true` row must be true because the restore SUCCEEDS.
	if code := f.restoreAsset(owner, selfDeleted); code != http.StatusNoContent {
		t.Fatalf("owner restoring their own delete = %d, want 204", code)
	}
	after := idsOf(f.getTrash(owner, "limit=200"))
	if _, ok := after[selfDeleted]; ok {
		t.Error("a restored asset is still in the trash listing")
	}
	if _, ok := after[adminDeleted]; !ok {
		t.Error("restoring one item removed an unrelated one from the trash listing")
	}

	// A super-admin restores anything, including the NULL-deleter row
	// both other cases fail closed to.
	if code := f.restoreAsset(admin, nullDeleted, auth.SuperAdminCapability); code != http.StatusNoContent {
		t.Errorf("system.admin restoring a NULL-deleter asset = %d, want 204", code)
	}
}

// --- 3. keyset pagination --------------------------------------------------

// Six mixed-kind items across three pages, with two ties on deleted_at.
// The tie is the point: batch deletes stamp the same timestamp, so a
// tiebreak pinned the wrong way loses or repeats the common case.
func TestListMyTrash_KeysetPaginatesWithoutSkipsOrRepeats(t *testing.T) {
	f := newFixture(t)
	owner := f.user("owner")

	base := time.Now().UTC().Truncate(time.Millisecond)
	tie := base.Add(-2 * time.Minute)

	seeded := map[uuid.UUID]bool{
		f.asset(owner, "t-a1", ptr(base.Add(-1*time.Minute)), &owner):      true,
		f.post(owner, "t-p1", ptr(base.Add(-3*time.Minute)), &owner):       true,
		f.collection(owner, "t-c1", ptr(base.Add(-4*time.Minute)), &owner): true,
		// Three rows, one instant, three different tables — the case the
		// id tiebreak exists for.
		f.asset(owner, "t-a2", ptr(tie), &owner):      true,
		f.post(owner, "t-p2", ptr(tie), &owner):       true,
		f.collection(owner, "t-c2", ptr(tie), &owner): true,
	}

	seen := make(map[uuid.UUID]int, len(seeded))
	var order []time.Time
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate in 10 pages of 2")
		}
		q := "limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		page := f.getTrash(owner, q)
		if len(page.Items) > 2 {
			t.Fatalf("page returned %d items for limit=2", len(page.Items))
		}
		for _, it := range page.Items {
			seen[uuid.UUID(it.Id)]++
			order = append(order, it.DeletedAt)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}

	for id := range seeded {
		switch seen[id] {
		case 0:
			t.Errorf("item %s was skipped by pagination", id)
		case 1:
		default:
			t.Errorf("item %s was returned %d times", id, seen[id])
		}
	}
	if len(seen) != len(seeded) {
		t.Errorf("paged %d distinct items, seeded %d", len(seen), len(seeded))
	}
	for i := 1; i < len(order); i++ {
		if order[i].After(order[i-1]) {
			t.Errorf("ordering broke across a page boundary at %d: %s came after %s",
				i, order[i], order[i-1])
		}
	}
}

// --- 4. purge_after --------------------------------------------------------

// The retention window is per KIND, and the three defaults are equal —
// so the window is set to three DIFFERENT values before asserting,
// or a handler that read asset_retention_days for everything would
// pass. TestListMyTrash_CoversAllThreeKinds covers the other half: no
// sysconfig store means no purge_after at all, never a guessed one.
func TestListMyTrash_PurgeAfterIsPerKindRetention(t *testing.T) {
	f := newFixture(t)
	f.shim.trash.Sysconfig = sysconfig.NewStore(f.pool)

	prev, err := f.shim.trash.Sysconfig.GetSoftDelete(t.Context())
	if err != nil {
		t.Fatalf("read soft_delete config: %v", err)
	}
	t.Cleanup(func() { _ = f.shim.trash.Sysconfig.SetSoftDelete(context.Background(), prev) })
	cfg := prev
	cfg.AssetRetentionDays, cfg.PostRetentionDays, cfg.CollectionRetentionDays = 7, 14, 21
	if err := f.shim.trash.Sysconfig.SetSoftDelete(t.Context(), cfg); err != nil {
		t.Fatalf("write soft_delete config: %v", err)
	}

	owner := f.user("owner")
	now := time.Now().UTC()
	want := map[uuid.UUID]int{
		f.asset(owner, "r-asset", &now, &owner):     7,
		f.post(owner, "r-post", &now, &owner):       14,
		f.collection(owner, "r-coll", &now, &owner): 21,
	}

	got := idsOf(f.getTrash(owner, "limit=200"))
	for id, days := range want {
		item, ok := got[id]
		if !ok {
			t.Errorf("item %s missing from trash", id)
			continue
		}
		if item.PurgeAfter == nil {
			t.Errorf("item %s (%s): purge_after absent though the window is readable", id, item.Kind)
			continue
		}
		expect := item.DeletedAt.AddDate(0, 0, days)
		if !item.PurgeAfter.Equal(expect) {
			t.Errorf("item %s (%s): purge_after = %s, want deleted_at + %dd = %s",
				id, item.Kind, *item.PurgeAfter, days, expect)
		}
	}
}

// The mixed list really is mixed — a build that only ever queried
// `assets` would pass every ordering assertion above.
func TestListMyTrash_CoversAllThreeKinds(t *testing.T) {
	f := newFixture(t)
	owner := f.user("owner")
	now := time.Now().UTC()

	want := map[uuid.UUID]openapi.TrashItemKind{
		f.asset(owner, "k-asset", &now, &owner):     openapi.TrashItemKindAsset,
		f.post(owner, "k-post", &now, &owner):       openapi.TrashItemKindPost,
		f.collection(owner, "k-coll", &now, &owner): openapi.TrashItemKindCollection,
	}

	got := idsOf(f.getTrash(owner, "limit=200"))
	for id, kind := range want {
		item, ok := got[id]
		if !ok {
			t.Errorf("%s item %s missing from trash", kind, id)
			continue
		}
		if item.Kind != kind {
			t.Errorf("item %s: kind = %q, want %q", id, item.Kind, kind)
		}
		if !item.RestorableByCaller {
			t.Errorf("%s item %s: self-deleted must be restorable", kind, id)
		}
		// Sysconfig is unwired in this fixture, so the window is
		// unknown — and unknown must mean absent, never a guess.
		if item.PurgeAfter != nil {
			t.Errorf("%s item %s: purge_after set with no sysconfig store wired", kind, id)
		}
	}
	if title := got[keyFor(want, openapi.TrashItemKindCollection)].Title; title != "k-coll" {
		t.Errorf("collection title = %q, want the collections.name column", title)
	}
}

func keyFor(m map[uuid.UUID]openapi.TrashItemKind, kind openapi.TrashItemKind) uuid.UUID {
	for id, k := range m {
		if k == kind {
			return id
		}
	}
	return uuid.Nil
}

// An anonymous caller has no bin to look in.
func TestListMyTrash_AnonymousIsRefused(t *testing.T) {
	f := newFixture(t)
	rr := httptest.NewRecorder()
	r := chi.NewRouter()
	openapi.HandlerFromMux(openapi.NewStrictHandler(f.shim, nil), r)
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/account/trash", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("anonymous GET /account/trash = %d, want 401", rr.Code)
	}
}

func ptr(t time.Time) *time.Time { return &t }
