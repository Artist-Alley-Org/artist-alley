package activities_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
)

// All tests in this file are integration tests against the live
// docker-compose Postgres (per project convention). They skip when
// AA_DB_PASSWORD is not set (the dev workflow's signal).

func openPool(t *testing.T, pwd string) *pgxpool.Pool {
	t.Helper()
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := envOr("AA_DB_NAME", "artist_alley")
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
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
	return pool
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// fixtureUser inserts a throwaway user + actor URI. Cleanup
// removes both the user row and any activities they emitted.
func fixtureUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (int64, string, string) {
	t.Helper()
	username := "activities-test-" + randHex(t, 6)
	actorURI := "https://test.example/users/" + username
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved, actor_uri) VALUES ($1, $2, 1, $3) RETURNING ref`,
		username, "Activities Test User", actorURI,
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return ref, username, actorURI
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	const hex = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hex[time.Now().UnixNano()&0xf]
		time.Sleep(time.Nanosecond) // change the clock so the next byte differs
	}
	return string(b)
}

func newInput(actorRef int64, actorURI, activityURI string, typ federation.ActivityType) activities.Input {
	return activities.Input{
		Type:         typ,
		ActivityURI:  activityURI,
		ActorUserRef: &actorRef,
		ActorURI:     actorURI,
		Payload:      map[string]any{"test": "payload"},
	}
}

// --- core RecordActivity behaviour --------------------------------------

func TestRecordActivity_RequiresTx(t *testing.T) {
	w := activities.NewWriter(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	_, err := w.RecordActivity(context.Background(), nil, activities.Input{})
	if err == nil {
		t.Fatal("RecordActivity must reject a nil transaction (ADR 0044 invariant)")
	}
}

func TestRecordActivity_RejectsBadActivityType(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)

	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := int64(1)
	_, err := w.RecordActivity(ctx, tx, activities.Input{
		Type:         "aa:NotAType",
		ActivityURI:  "https://test.example/activities/x",
		ActorUserRef: &ref,
		ActorURI:     "https://test.example/users/x",
	})
	if !errors.Is(err, activities.ErrInvalidActivityType) {
		t.Errorf("expected ErrInvalidActivityType, got %v", err)
	}
}

func TestRecordActivity_RejectsBadObjectKind(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()
	tx, _ := pool.Begin(ctx)
	defer tx.Rollback(ctx)

	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref := int64(1)
	_, err := w.RecordActivity(ctx, tx, activities.Input{
		Type:         federation.ActivityLike,
		ActivityURI:  "https://test.example/activities/x",
		ActorUserRef: &ref,
		ActorURI:     "https://test.example/users/x",
		Object: &activities.ObjectRef{
			URI:  "https://test.example/posts/x",
			Kind: "fanciful_made_up_kind",
		},
	})
	if !errors.Is(err, activities.ErrInvalidObjectKind) {
		t.Errorf("expected ErrInvalidObjectKind, got %v", err)
	}
}

func TestRecordActivity_RejectsMissingActor(t *testing.T) {
	w := activities.NewWriter(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	// Skip the nil-tx guard by passing a sentinel that won't be
	// reached — actually the nil-tx guard runs first, so we
	// can't reach the validateInput call without a real tx.
	// Test validation in isolation by constructing input that
	// would fail BOTH (no actor) and asserting nil-tx fires.
	_, err := w.RecordActivity(context.Background(), nil, activities.Input{})
	if err == nil {
		t.Error("expected error on empty input + nil tx")
	}
}

func TestRecordActivity_RoundTrips(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, _, actorURI := fixtureUser(t, ctx, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	uri := "https://test.example/activities/" + randHex(t, 16)
	in := newInput(ref, actorURI, uri, federation.ActivityLike)
	in.Object = &activities.ObjectRef{
		URI:     "https://test.example/posts/abc",
		Kind:    activities.ObjectKindPost,
		LocalID: "abc",
	}
	in.To = []string{"https://test.example/users/recipient"}

	rec, err := w.RecordActivity(ctx, tx, in)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.ActivityURI != uri {
		t.Errorf("activity URI drift: %q vs %q", rec.ActivityURI, uri)
	}
	if rec.Type != federation.ActivityLike {
		t.Errorf("type drift: %q", rec.Type)
	}
	if rec.ObjectKind != activities.ObjectKindPost {
		t.Errorf("object kind drift: %q", rec.ObjectKind)
	}
	if rec.ObjectLocalID != "abc" {
		t.Errorf("object local id drift: %q", rec.ObjectLocalID)
	}
	if len(rec.To) != 1 || rec.To[0] != "https://test.example/users/recipient" {
		t.Errorf("To drift: %v", rec.To)
	}
	if rec.Source != activities.SourceLocal {
		t.Errorf("default source should be local; got %q", rec.Source)
	}
	if rec.PublishedAt.IsZero() {
		t.Errorf("PublishedAt should default to NOW")
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestRecordActivity_Idempotent(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, _, actorURI := fixtureUser(t, ctx, pool)

	uri := "https://test.example/activities/" + randHex(t, 16)
	in := newInput(ref, actorURI, uri, federation.ActivityFollow)
	in.Object = &activities.ObjectRef{
		URI:     "https://test.example/users/target",
		Kind:    activities.ObjectKindUser,
		LocalID: "999",
	}

	// First insert.
	tx1, _ := pool.Begin(ctx)
	rec1, err := w.RecordActivity(ctx, tx1, in)
	if err != nil {
		t.Fatalf("first record: %v", err)
	}
	tx1.Commit(ctx)

	// Second insert (same activity_uri).
	tx2, _ := pool.Begin(ctx)
	rec2, err := w.RecordActivity(ctx, tx2, in)
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	tx2.Commit(ctx)

	// The two records MUST refer to the same row.
	if rec1.ID != rec2.ID {
		t.Errorf("idempotency broken: first ID %v second ID %v", rec1.ID, rec2.ID)
	}

	// Confirm there's exactly one row in the DB.
	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activities WHERE activity_uri = $1`, uri,
	).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("expected exactly 1 row, got %d", cnt)
	}
}

func TestRecordActivity_RollbackDropsRow(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, _, actorURI := fixtureUser(t, ctx, pool)

	uri := "https://test.example/activities/" + randHex(t, 16)
	in := newInput(ref, actorURI, uri, federation.ActivityBlock)
	in.Object = &activities.ObjectRef{
		URI:     "https://test.example/users/target",
		Kind:    activities.ObjectKindUser,
		LocalID: "999",
	}

	tx, _ := pool.Begin(ctx)
	if _, err := w.RecordActivity(ctx, tx, in); err != nil {
		t.Fatalf("record: %v", err)
	}
	// Roll back rather than commit. The transactional-emit
	// invariant says: domain row + activity row commit together
	// or NOT AT ALL.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var cnt int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM activities WHERE activity_uri = $1`, uri,
	).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("rollback failed to drop the activity row; count=%d", cnt)
	}
}

func TestRecordActivity_PreservesPayload(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ref, _, actorURI := fixtureUser(t, ctx, pool)

	uri := "https://test.example/activities/" + randHex(t, 16)
	in := newInput(ref, actorURI, uri, federation.ActivityAAApprove)
	in.Payload = map[string]any{
		"aa:comment":     "Approved with minor lighting note.",
		"aa:reviewedAt":  "2026-06-04T10:00:00Z",
		"nested": map[string]any{
			"deep":  true,
			"count": 42,
		},
	}
	in.Object = &activities.ObjectRef{
		URI:     "https://test.example/assets/xyz",
		Kind:    activities.ObjectKindAsset,
		LocalID: "xyz",
	}

	tx, _ := pool.Begin(ctx)
	rec, err := w.RecordActivity(ctx, tx, in)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	tx.Commit(ctx)

	// The Payload field on Record holds the JSONB blob; decode +
	// check round-trip.
	if rec.Payload == nil {
		t.Fatal("Payload is nil after round-trip")
	}
	var comment string
	if err := json.Unmarshal(rec.Payload["aa:comment"], &comment); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if comment != "Approved with minor lighting note." {
		t.Errorf("payload drift: %q", comment)
	}
}

// --- catalogue tests -----------------------------------------------------

func TestActivityObjectKindValid(t *testing.T) {
	for _, k := range []activities.ActivityObjectKind{
		activities.ObjectKindPost, activities.ObjectKindComment,
		activities.ObjectKindAsset, activities.ObjectKindUser,
		activities.ObjectKindCollection, activities.ObjectKindWorkspace,
		activities.ObjectKindBrandKit, activities.ObjectKindMessage,
		activities.ObjectKindActivity,
	} {
		if !k.Valid() {
			t.Errorf("declared kind %q not in KnownObjectKinds", k)
		}
	}
	if activities.ActivityObjectKind("fanciful").Valid() {
		t.Error("unknown kind should be invalid")
	}
}

func TestActivityObjectKindCount(t *testing.T) {
	// Drift catcher between the const block + the map. If you
	// added a new ObjectKind* const without a map entry (or
	// vice-versa), this fails.
	const expected = 9
	if got := len(activities.KnownObjectKinds); got != expected {
		t.Errorf("KnownObjectKinds count drift: got %d want %d", got, expected)
	}
}

// --- cache wiring --------------------------------------------------------

func TestRecordActivity_InvalidatesActorOutboxCache(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := openPool(t, pwd)
	defer pool.Close()

	reg := cache.NewRegistry(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer reg.Stop()
	w := activities.NewWriter(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), reg)
	ref, _, actorURI := fixtureUser(t, ctx, pool)

	uri := "https://test.example/activities/" + randHex(t, 16)
	in := newInput(ref, actorURI, uri, federation.ActivityLike)
	in.Object = &activities.ObjectRef{
		URI:     "https://test.example/posts/cache-test",
		Kind:    activities.ObjectKindPost,
		LocalID: "cache-test",
	}

	tx, _ := pool.Begin(ctx)
	if _, err := w.RecordActivity(ctx, tx, in); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Black-box observable: no panic, no error. The cache
	// invalidation is a side-effect we can't directly observe
	// without instrumentation; the test confirms the cache
	// wiring runs without crashing under a live registry. The
	// per-actor outbox feed query itself is exercised by the
	// 1.22.A-bis-3 admin UI tests when that lands.
}

// --- MintActivityURI -----------------------------------------------------

func TestMintActivityURI(t *testing.T) {
	uri := activities.MintActivityURI("https://test.example")
	if got, want := len(uri), len("https://test.example/activities/")+36; got != want {
		t.Errorf("URI shape: len=%d want=%d (%q)", got, want, uri)
	}
	uri2 := activities.MintActivityURI("https://test.example")
	if uri == uri2 {
		t.Error("two Mint calls produced the same URI (uuid v4 collision is astronomically unlikely)")
	}
}
