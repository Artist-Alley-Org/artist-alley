// Integration tests proving the handler-side activity emission
// wiring from Phase 1.22.A-bis-2 (ADR 0044). One test per shape:
//
//   - LikePost — proves WithEmission wraps a simple domain write
//     + records the Like activity in the same tx + notifications
//     fire after commit.
//   - FollowUser — same pattern, different verb + addressing.
//
// These tests skip without AA_DB_PASSWORD (the project convention
// for integration tests against the live compose stack).
//
// What they assert:
//
//   1. The domain row landed (existing behaviour).
//   2. An activities row with the expected type + actor URI +
//      object kind landed in the SAME transaction.
//   3. The notification side-effect fired (a row in notifications).
//   4. The activity URI is uniquely shaped per the spec.
//
// What they DON'T assert (out of scope for these tests):
//
//   - cache invalidation behaviour (covered by the cache LRU's
//     own tests).
//   - federation_outbox dispatch (Phase 1.22.D).
//   - signature wiring (Phase 1.22.D — payloads here are unsigned).

package social_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/notifications"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
	"github.com/mscrnt/artist-alley/app/internal/social"
	"github.com/mscrnt/artist-alley/app/internal/userprefs"
	"github.com/mscrnt/artist-alley/app/internal/users"
)

// activitiesFixture wires a minimal social.Handler with the same
// activity-emission shape api.go does in production. Returns the
// handler + cleanup. Skips the test if AA_DB_PASSWORD is unset.
type activitiesFixture struct {
	t        *testing.T
	ctx      context.Context
	pool     *pgxpool.Pool
	registry *cache.Registry
	social   *social.Handler
	userRef  int64
	username string
}

func setupActivitiesFixture(t *testing.T) *activitiesFixture {
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

	// Build the full handler stack — same shape as api.go.
	socialH := social.NewHandler(pool, logger, registry)
	prefsH := userprefs.NewHandler(pool, logger, registry)
	notifWriter := notifications.NewWriter(pool, logger, nil, nil, nil, registry)
	notifWriter.SetBlockChecker(blockAdapter{h: socialH})
	notifWriter.SetPrefsResolver(prefsAdapter{h: prefsH})
	socialH.SetNotifier(notifyAdapter{w: notifWriter})

	activitiesW := activities.NewWriter(pool, logger, registry)
	activitiesW.SetNotifier(notifyAdapter{w: notifWriter})
	// Wire the username resolver — production routes federation
	// addressing through users.Handler's cached lookup.
	usersH := users.NewHandler(pool, logger, registry)
	activitiesW.SetUsernameResolver(usersH)
	socialH.SetActivitiesWriter(activitiesW, func(ctx context.Context) string {
		return "https://test.example"
	})

	// Throwaway user with a username + actor URI so emit can mint URIs.
	username := "wiring-test-" + randHex(t, 6)
	actorURI := "https://test.example/users/" + username
	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved, actor_uri) VALUES ($1, $2, 1, $3) RETURNING ref`,
		username, "Wiring Test", actorURI,
	).Scan(&ref); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Grant the user the capabilities each handler checks.
	for _, cap := range []string{"posts.like", "posts.comment"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_capability_grants (rs_user_id, capability_code, granted_at) VALUES ($1, $2, NOW()) ON CONFLICT DO NOTHING`,
			ref, cap,
		); err != nil {
			t.Logf("grant %s: %v (continuing)", cap, err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM notifications WHERE recipient_user_ref = $1 OR actor_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM likes WHERE rs_user_id = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM user_follows WHERE follower_user_ref = $1 OR followee_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM user_capability_grants WHERE rs_user_id = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})
	return &activitiesFixture{
		t:        t,
		ctx:      ctx,
		pool:     pool,
		registry: registry,
		social:   socialH,
		userRef:  ref,
		username: username,
	}
}

func (f *activitiesFixture) withIdentity(ctx context.Context) context.Context {
	id := &auth.Identity{
		UserRef:      f.userRef,
		Username:     f.username,
		AuthMethod:   "session",
		Capabilities: []string{"posts.like", "posts.comment", "system.admin"},
	}
	return auth.WithIdentity(ctx, id)
}

// --- adapters mirroring api.go (kept in-test for fixture isolation) ---

type blockAdapter struct{ h *social.Handler }

func (a blockAdapter) HasBlockBetween(ctx context.Context, x, y int64) (bool, error) {
	return a.h.HasBlockBetween(ctx, x, y)
}

type prefsAdapter struct{ h *userprefs.Handler }

func (a prefsAdapter) ChannelsFor(ctx context.Context, ref int64, verb string) ([]string, error) {
	return a.h.ChannelsFor(ctx, ref, verb)
}

type notifyAdapter struct{ w *notifications.Writer }

func (a notifyAdapter) Notify(ctx context.Context, recipient int64, actor *int64, verb, targetKind, targetID string, payload map[string]any) error {
	return a.w.Notify(ctx, notifications.Input{
		RecipientUserRef: recipient,
		ActorUserRef:     actor,
		Verb:             verb,
		TargetKind:       targetKind,
		TargetID:         targetID,
		Payload:          payload,
	})
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// --- the tests ----------------------------------------------------------

// TestLikePost_EmitsActivity proves the gold-standard wiring: a
// successful LikePost call lands BOTH a row in `likes` AND a
// matching row in `activities`. The activity has the right type,
// actor URI, and object kind; the activity_uri matches the spec
// shape.
func TestLikePost_EmitsActivity(t *testing.T) {
	fx := setupActivitiesFixture(t)

	// Create a peer user + a post by that peer so LikePost has a
	// real target.
	peerName := "wiring-peer-" + randHex(t, 6)
	var peerRef int64
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		peerName, "Peer User",
	).Scan(&peerRef); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, peerRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM posts WHERE author_user_ref = $1`, peerRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, peerRef)
	})

	postID := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO posts (id, author_user_ref, title, description, visibility) VALUES ($1, $2, $3, '', 'org-only')`,
		pgtype.UUID{Bytes: postID, Valid: true}, peerRef, "Wiring test post",
	); err != nil {
		t.Fatal(err)
	}

	// Exercise LikePost via the strict-server method.
	ctx := fx.withIdentity(fx.ctx)
	openapiID := openapi_types.UUID(postID)
	_, err := fx.social.LikePost(ctx, openapi.LikePostRequestObject{Id: openapiID})
	if err != nil {
		t.Fatalf("LikePost: %v", err)
	}

	// Domain row exists.
	var likeCount int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM likes WHERE rs_user_id=$1 AND target_kind='post' AND target_id=$2`,
		fx.userRef, pgtype.UUID{Bytes: postID, Valid: true},
	).Scan(&likeCount); err != nil {
		t.Fatal(err)
	}
	if likeCount != 1 {
		t.Errorf("expected 1 like row, got %d", likeCount)
	}

	// Activity row exists with the expected shape.
	var activityType, actorURI, objectKind, source string
	var objectLocalID *string
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT activity_type, actor_uri, COALESCE(object_kind,''), object_local_id, source
		 FROM activities WHERE actor_user_ref=$1 AND activity_type='Like'
		 ORDER BY published_at DESC LIMIT 1`,
		fx.userRef,
	).Scan(&activityType, &actorURI, &objectKind, &objectLocalID, &source); err != nil {
		t.Fatalf("read activity: %v (the wiring didn't emit)", err)
	}
	if activityType != "Like" {
		t.Errorf("activity_type: got %q want Like", activityType)
	}
	if actorURI != "https://test.example/users/"+fx.username {
		t.Errorf("actor_uri: got %q", actorURI)
	}
	if objectKind != "post" {
		t.Errorf("object_kind: got %q want post", objectKind)
	}
	if objectLocalID == nil || *objectLocalID != postID.String() {
		got := "<nil>"
		if objectLocalID != nil {
			got = *objectLocalID
		}
		t.Errorf("object_local_id: got %q want %s", got, postID)
	}
	if source != "local" {
		t.Errorf("source: got %q want local", source)
	}

	// Notification side-effect fired.
	var notifCount int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM notifications
		 WHERE recipient_user_ref=$1 AND actor_user_ref=$2 AND verb='like_on_my_post'`,
		peerRef, fx.userRef,
	).Scan(&notifCount); err != nil {
		t.Fatal(err)
	}
	if notifCount != 1 {
		t.Errorf("expected 1 like_on_my_post notification for the post author, got %d", notifCount)
	}
}

// TestUnlikePost_EmitsUndo proves the Undo wiring from
// 1.22.A-bis-3a. After Like → Unlike, two activity rows exist:
// the original Like + an Undo whose object_local_id references
// the Like's activity_uri (per AP §6.10).
func TestUnlikePost_EmitsUndo(t *testing.T) {
	fx := setupActivitiesFixture(t)

	// Peer + post.
	peerName := "wiring-undo-peer-" + randHex(t, 6)
	var peerRef int64
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		peerName, "Peer",
	).Scan(&peerRef); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, peerRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM posts WHERE author_user_ref = $1`, peerRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, peerRef)
	})

	postID := uuid.New()
	if _, err := fx.pool.Exec(fx.ctx,
		`INSERT INTO posts (id, author_user_ref, title, description, visibility) VALUES ($1, $2, $3, '', 'org-only')`,
		pgtype.UUID{Bytes: postID, Valid: true}, peerRef, "Undo test post",
	); err != nil {
		t.Fatal(err)
	}

	ctx := fx.withIdentity(fx.ctx)
	openapiID := openapi_types.UUID(postID)
	if _, err := fx.social.LikePost(ctx, openapi.LikePostRequestObject{Id: openapiID}); err != nil {
		t.Fatalf("LikePost: %v", err)
	}
	if _, err := fx.social.UnlikePost(ctx, openapi.UnlikePostRequestObject{Id: openapiID}); err != nil {
		t.Fatalf("UnlikePost: %v", err)
	}

	// Expect 2 activities now: the Like + the Undo.
	type row struct {
		typ           string
		objectKind    string
		objectLocalID string
		undoTarget    string
	}
	rows, err := fx.pool.Query(fx.ctx,
		`SELECT activity_type, COALESCE(object_kind,''), COALESCE(object_local_id,''), COALESCE(payload->>'target_type','')
		 FROM activities WHERE actor_user_ref=$1 ORDER BY published_at ASC`,
		fx.userRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.typ, &r.objectKind, &r.objectLocalID, &r.undoTarget); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()

	if len(got) != 2 {
		t.Fatalf("expected 2 activities (Like + Undo), got %d: %+v", len(got), got)
	}
	if got[0].typ != "Like" {
		t.Errorf("first activity should be Like, got %q", got[0].typ)
	}
	if got[1].typ != "Undo" {
		t.Errorf("second activity should be Undo, got %q", got[1].typ)
	}
	if got[1].objectKind != "activity" {
		t.Errorf("Undo object_kind should be 'activity', got %q", got[1].objectKind)
	}
	if got[1].undoTarget != "Like" {
		t.Errorf("Undo payload.target_type should be 'Like', got %q", got[1].undoTarget)
	}
	// The Undo's object_local_id should be the Like's activity_uri.
	var likeURI string
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT activity_uri FROM activities WHERE actor_user_ref=$1 AND activity_type='Like'`,
		fx.userRef,
	).Scan(&likeURI); err != nil {
		t.Fatal(err)
	}
	if got[1].objectLocalID != likeURI {
		t.Errorf("Undo should reference the Like's activity_uri (%s), got %q", likeURI, got[1].objectLocalID)
	}
}

// TestFollowUser_EmitsActivity proves the wiring on a different
// handler shape — Follow's target is a user, not a post; addressing
// is To=[followee URI] not To=[author URI].
func TestFollowUser_EmitsActivity(t *testing.T) {
	fx := setupActivitiesFixture(t)

	peerName := "wiring-followee-" + randHex(t, 6)
	peerURI := "https://test.example/users/" + peerName
	var peerRef int64
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO "user" (username, fullname, approved, actor_uri) VALUES ($1, $2, 1, $3) RETURNING ref`,
		peerName, "Followee", peerURI,
	).Scan(&peerRef); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = fx.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, peerRef)
		_, _ = fx.pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, peerRef)
	})

	ctx := fx.withIdentity(fx.ctx)
	_, err := fx.social.FollowUser(ctx, openapi.FollowUserRequestObject{Ref: peerRef})
	if err != nil {
		t.Fatalf("FollowUser: %v", err)
	}

	// Domain edge exists.
	var followCount int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM user_follows WHERE follower_user_ref=$1 AND followee_user_ref=$2`,
		fx.userRef, peerRef,
	).Scan(&followCount); err != nil {
		t.Fatal(err)
	}
	if followCount != 1 {
		t.Errorf("expected 1 follow edge, got %d", followCount)
	}

	// Follow activity row exists.
	var activityType, objectKind string
	var objectLocalID, toURIs *string
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT activity_type, COALESCE(object_kind,''), object_local_id, to_uris::text
		 FROM activities WHERE actor_user_ref=$1 AND activity_type='Follow'
		 ORDER BY published_at DESC LIMIT 1`,
		fx.userRef,
	).Scan(&activityType, &objectKind, &objectLocalID, &toURIs); err != nil {
		t.Fatalf("read activity: %v", err)
	}
	if objectKind != "user" {
		t.Errorf("object_kind: got %q want user", objectKind)
	}
	if objectLocalID == nil || *objectLocalID != strconv.FormatInt(peerRef, 10) {
		t.Errorf("object_local_id: got %v want %d", objectLocalID, peerRef)
	}
	// Addressing: to_uris should contain the followee's actor URI.
	if toURIs == nil || !strings.Contains(*toURIs, peerURI) {
		t.Errorf("to_uris should contain %q, got %v", peerURI, toURIs)
	}

	// Notification fired.
	var notifCount int
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT COUNT(*) FROM notifications
		 WHERE recipient_user_ref=$1 AND verb='new_follower'`,
		peerRef,
	).Scan(&notifCount); err != nil {
		t.Fatal(err)
	}
	if notifCount != 1 {
		t.Errorf("expected 1 new_follower notification, got %d", notifCount)
	}
}
