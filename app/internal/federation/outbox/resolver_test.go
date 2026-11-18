// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Resolver unit tests — visibility × sensitivity coverage
// matrix per the gold-standard test layering requirement.
// Phase 1.22.D-b-2.
//
// Skips without AA_DB_PASSWORD (the explicit-share path
// hits the real federation_shares table; mocking it would
// just re-implement Postgres).

package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"crypto/rand"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	host := envOr("AA_DB_HOST", "postgres")
	port := envOr("AA_DB_PORT", "5432")
	user := envOr("AA_DB_USER", "artist_alley")
	name := testdb.Name(t)
	dsn := "host=" + host + " port=" + port + " user=" + user +
		" dbname=" + name + " sslmode=disable password=" + pwd
	ctx := t.Context()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}

func newResolverFixture(t *testing.T, encSupported bool) (*outbox.Resolver, *pgxpool.Pool, uuid.UUID, uuid.UUID, int64) {
	t.Helper()
	pool := openTestPool(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)

	// Seed: one user (grantor), one peer (connected), one post
	// (target object), one federation_shares row for the post.
	ctx := context.Background()
	var grantorRef int64
	username := "resolver-" + randHex(4)
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, $2, 1) RETURNING ref`,
		username, "Resolver Test",
	).Scan(&grantorRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var peerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Test Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		"https://resolver-"+randHex(4)+".example", grantorRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, $3, 'explicit-share')`,
		postID, grantorRef, "Resolver Test",
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	// Seed a real activities row for the share's granted_activity_id FK.
	var activityID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb)
		 RETURNING id`,
		"https://resolver-test.example/activities/"+randHex(8),
		"https://resolver-test.example/users/alice",
		grantorRef,
	).Scan(&activityID); err != nil {
		t.Fatalf("seed activity: %v", err)
	}
	// One share row for the post → peer.
	if _, err := pool.Exec(ctx,
		`INSERT INTO federation_shares (
		    grantor_user_ref, object_kind, object_id, peer_id,
		    target_user_url, scope, granted_activity_id
		 ) VALUES ($1, 'post', $2, $3, $4, 'view', $5)`,
		grantorRef, postID, peerID,
		"https://resolver-test.example/users/bob",
		activityID,
	); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE id = $1`, activityID)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
	})

	encFn := func(context.Context) bool { return encSupported }
	r := outbox.NewResolver(pool, reg, encFn)
	return r, pool, peerID, postID, grantorRef
}

// --- visibility × sensitivity matrix --------------------------------

// visibilityRoutedVerb is the verb these tests resolve: one with no
// dedicated route, so Resolve takes the visibility / explicit-share
// tiers they are about. Before #1611 they used Like; a Like now has its
// own route (likeRecipient: the owner of the liked object, under a
// received grant, TestTwoStack_LikeWire) and never reaches these tiers.
// No activity v1 federates reaches them today; they remain the
// contract for one that will.
const visibilityRoutedVerb = "Announce"

func TestResolver_Private_ReturnsRecipientSetEmpty(t *testing.T) {
	r, _, _, postID, ref := newResolverFixture(t, false)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
		AuthorRef: ref, AuthorURI: "https://local.example/users/alice",
		Visibility: outbox.VisibilityPrivate, Sensitivity: outbox.SensitivityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedRecipientSetEmpty {
		t.Errorf("private visibility: got %q want recipient_set_empty", got.Skipped)
	}
	if len(got.Recipients) != 0 {
		t.Errorf("private visibility: got %d recipients want 0", len(got.Recipients))
	}
}

func TestResolver_OrgOnly_ReturnsRecipientSetEmpty(t *testing.T) {
	r, _, _, postID, ref := newResolverFixture(t, false)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityOrgOnly,
		Sensitivity: outbox.SensitivityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedRecipientSetEmpty {
		t.Errorf("org-only: got %q want recipient_set_empty", got.Skipped)
	}
}

func TestResolver_ExplicitShare_ReturnsRecipientWithSeededShare(t *testing.T) {
	r, _, peerID, postID, ref := newResolverFixture(t, false)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedNone {
		t.Fatalf("explicit-share: got skipped=%q; want none", got.Skipped)
	}
	if len(got.Recipients) != 1 {
		t.Fatalf("explicit-share: got %d recipients want 1", len(got.Recipients))
	}
	if got.Recipients[0].PeerID != peerID {
		t.Errorf("recipient peer: got %v want %v", got.Recipients[0].PeerID, peerID)
	}
	if got.Recipients[0].TargetUserURL != "https://resolver-test.example/users/bob" {
		t.Errorf("recipient target URL: got %q", got.Recipients[0].TargetUserURL)
	}
	if got.Recipients[0].Scope != "view" {
		t.Errorf("recipient scope: got %q want view", got.Recipients[0].Scope)
	}
}

func TestResolver_Followers_NotYetWired_ReturnsRecipientSetEmpty(t *testing.T) {
	// Per the TODO(1.22.D-b-7) marker in followers(): the
	// followers tier returns empty until user.uuid lands.
	// Resolver maps empty → SkippedRecipientSetEmpty.
	r, _, _, postID, ref := newResolverFixture(t, false)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: "Create", TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityFollowers,
		Sensitivity: outbox.SensitivityPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedRecipientSetEmpty {
		t.Errorf("followers (not wired): got %q want recipient_set_empty", got.Skipped)
	}
}

// --- sensitivity refusal --------------------------------------------

func TestResolver_RestrictedSensitivity_NoEncryption_RefusesEmission(t *testing.T) {
	r, _, _, postID, ref := newResolverFixture(t, false /* encryption NOT supported */)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityRestricted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedEncryptionRequiredButNotSupported {
		t.Errorf("restricted sensitivity: got %q want encryption_required_but_not_supported", got.Skipped)
	}
	if len(got.Recipients) != 0 {
		t.Errorf("restricted: should have ZERO recipients; got %d", len(got.Recipients))
	}
}

func TestResolver_EmbargoSensitivity_NoEncryption_RefusesEmission(t *testing.T) {
	r, _, _, postID, ref := newResolverFixture(t, false)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: "Create", TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityEmbargo,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedEncryptionRequiredButNotSupported {
		t.Errorf("embargo sensitivity: got %q want encryption_required_but_not_supported", got.Skipped)
	}
}

func TestResolver_RestrictedSensitivity_WithEncryption_AllowsEmission(t *testing.T) {
	// When 1.22.I ships X25519, encryptionSupported returns true
	// + the refusal path drops out → recipients return normally.
	r, _, peerID, postID, ref := newResolverFixture(t, true /* encryption SUPPORTED */)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityRestricted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Skipped != outbox.SkippedNone {
		t.Errorf("encryption supported: got skipped=%q want none", got.Skipped)
	}
	if len(got.Recipients) != 1 || got.Recipients[0].PeerID != peerID {
		t.Errorf("encryption supported: expected recipient with peer=%v, got %+v", peerID, got.Recipients)
	}
}

// --- every legal tier + public (#1233) ------------------------------
//
// `public` reached the posts/collections CHECK constraints in
// migration 00008 without a case in Resolve, so a Like on a public
// post returned `unsupported visibility: "public"`; the dispatcher
// rolled back its whole batch on that error and never advanced the
// cursor, so every activity behind it stalled (#1233). These tests
// read the legal tiers from the LIVE constraints rather than from a
// list in this file, so a tier added to the schema without a
// deliberate resolver outcome fails here instead of in production.
//
// Tiers are built with outbox.Visibility("...") from those strings on
// purpose: the tests compile against a resolver that predates the
// public constant, which is how they were checked to fail on it.

var quotedTextLiteral = regexp.MustCompile(`'([^']+)'::text`)

// schemaVisibilityTiers returns the values a CHECK constraint allows,
// sorted.
func schemaVisibilityTiers(t *testing.T, pool *pgxpool.Pool, constraint string) []string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint,
	).Scan(&def); err != nil {
		t.Fatalf("read constraint %s: %v", constraint, err)
	}
	var out []string
	for _, m := range quotedTextLiteral.FindAllStringSubmatch(def, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("constraint %s lists no values: %s", constraint, def)
	}
	return out
}

// seedUnsharedPeer adds an enabled, connected peer that holds NO share
// of anything: the peer a broadcast would wrongly reach.
func seedUnsharedPeer(t *testing.T, pool *pgxpool.Pool, ref int64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Unshared Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		"https://unshared-"+randHex(4)+".example", ref,
	).Scan(&id); err != nil {
		t.Fatalf("seed unshared peer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, id)
	})
	return id
}

func onlyPeer(t *testing.T, label string, got outbox.Result, want uuid.UUID) {
	t.Helper()
	if got.Skipped != outbox.SkippedNone {
		t.Fatalf("%s: skipped=%q, want the shared peer as the only recipient", label, got.Skipped)
	}
	if len(got.Recipients) != 1 || got.Recipients[0].PeerID != want {
		t.Fatalf("%s: recipients=%+v, want exactly peer %v", label, got.Recipients, want)
	}
}

func noRecipients(t *testing.T, label string, got outbox.Result) {
	t.Helper()
	if got.Skipped != outbox.SkippedRecipientSetEmpty || len(got.Recipients) != 0 {
		t.Fatalf("%s: got skipped=%q recipients=%+v, want recipient_set_empty with none",
			label, got.Skipped, got.Recipients)
	}
}

func TestResolver_EveryLegalVisibilityTier_HasADeliberateOutcome(t *testing.T) {
	_, pool, _, _, _ := newResolverFixture(t, false)

	// The outcome each tier must have when the target object HAS a
	// live share to one peer and another connected peer has none.
	want := map[string]string{
		"private":        "none",
		"org-only":       "none",
		"followers":      "none", // TODO(1.22.D-b-7): not wired yet
		"explicit-share": "shared-peer-only",
		"public":         "shared-peer-only",
	}
	var expected []string
	for k := range want {
		expected = append(expected, k)
	}
	sort.Strings(expected)
	for _, c := range []string{"posts_visibility_check", "collections_visibility_check"} {
		if got := schemaVisibilityTiers(t, pool, c); !slices.Equal(got, expected) {
			t.Fatalf("%s allows %v; this test (and Resolve) cover %v. "+
				"A new tier needs a deliberate resolver case first (#1233).", c, got, expected)
		}
	}
	// The federation vocabulary catalogue must name the same tiers
	// (vocab.go: catalogue/schema drift is a code-review block; this
	// makes it a test failure).
	var catalogue []string
	for v := range federation.KnownObjectVisibilities {
		catalogue = append(catalogue, string(v))
	}
	sort.Strings(catalogue)
	if !slices.Equal(catalogue, expected) {
		t.Fatalf("federation.KnownObjectVisibilities = %v; schema and resolver cover %v", catalogue, expected)
	}

	for _, tier := range expected {
		t.Run(tier, func(t *testing.T) {
			// Fresh fixture per tier: a cold shares cache, so no tier
			// is answered from another tier's cached lookup.
			r, pool, peerID, postID, ref := newResolverFixture(t, false)
			unshared := seedUnsharedPeer(t, pool, ref)
			got, err := r.Resolve(context.Background(), outbox.Input{
				Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
				AuthorRef: ref, Visibility: outbox.Visibility(tier),
				Sensitivity: outbox.SensitivityPublic,
			})
			if err != nil {
				t.Fatalf("tier %q is legal in the schema but Resolve errored: %v "+
					"(an error rolls back the whole dispatch batch)", tier, err)
			}
			for _, rec := range got.Recipients {
				if rec.PeerID == unshared {
					t.Fatalf("tier %q reached a peer with no share: %+v", tier, got.Recipients)
				}
			}
			switch want[tier] {
			case "none":
				noRecipients(t, tier, got)
			case "shared-peer-only":
				onlyPeer(t, tier, got, peerID)
			}
		})
	}
}

func TestResolver_UnknownVisibility_StaysAnError(t *testing.T) {
	r, _, _, postID, ref := newResolverFixture(t, false)
	for _, v := range []string{"everyone", "PUBLIC", "public ", ""} {
		got, err := r.Resolve(context.Background(), outbox.Input{
			Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
			AuthorRef: ref, Visibility: outbox.Visibility(v),
			Sensitivity: outbox.SensitivityPublic,
		})
		if err == nil {
			t.Errorf("visibility %q: got %+v and no error; an unknown tier must not get an audience", v, got)
		}
	}
}

func TestResolver_Public_NoLiveShare_RecipientSetEmpty_NotAnError(t *testing.T) {
	r, pool, _, _, ref := newResolverFixture(t, false)
	// Two connected peers exist (the fixture's, which shares a
	// DIFFERENT post, and an unshared one); this post has no share.
	seedUnsharedPeer(t, pool, ref)
	postID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Public, unshared', 'public')`,
		postID, ref,
	); err != nil {
		t.Fatalf("seed public post: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, postID)
	})
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.Visibility("public"),
		Sensitivity: outbox.SensitivityPublic,
	})
	if err != nil {
		t.Fatalf("public with no share must be a skip, not an error: %v", err)
	}
	noRecipients(t, "public, no share", got)
}

func TestResolver_Public_FollowsTheLiveShareBoundary(t *testing.T) {
	cases := []struct {
		name   string
		mutate string // applied AFTER a first, warming Resolve on the same resolver
		live   bool
	}{
		{"live share", ``, true},
		{"expiry in the future", `UPDATE federation_shares SET expires_at = NOW() + INTERVAL '1 hour' WHERE peer_id = $1`, true},
		{"revoked share", `UPDATE federation_shares SET revoked_at = NOW() WHERE peer_id = $1`, false},
		{"expired share", `UPDATE federation_shares SET expires_at = NOW() - INTERVAL '1 minute' WHERE peer_id = $1`, false},
		{"disabled peer", `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, false},
		{"peer not connected", `UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, pool, peerID, postID, ref := newResolverFixture(t, false)
			seedUnsharedPeer(t, pool, ref)
			resolve := func(tier string) outbox.Result {
				t.Helper()
				got, err := r.Resolve(context.Background(), outbox.Input{
					Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: postID,
					AuthorRef: ref, Visibility: outbox.Visibility(tier),
					Sensitivity: outbox.SensitivityPublic,
				})
				if err != nil {
					t.Fatalf("%s: %v", tier, err)
				}
				return got
			}
			// Warm: the same resolver has already answered for this
			// object, as it has in production once any activity on
			// it was dispatched.
			for _, tier := range []string{"public", "explicit-share"} {
				onlyPeer(t, tier+" (warm-up)", resolve(tier), peerID)
			}
			if tc.mutate != "" {
				if _, err := pool.Exec(context.Background(), tc.mutate, peerID); err != nil {
					t.Fatalf("mutate: %v", err)
				}
			}
			// No restart, no invalidation call: the next resolution
			// must reflect the change.
			for _, tier := range []string{"public", "explicit-share"} {
				if tc.live {
					onlyPeer(t, tier, resolve(tier), peerID)
				} else {
					noRecipients(t, tier, resolve(tier))
				}
			}
		})
	}
}

// --- warm-state coherence (#1233 live finding, rev344) ---------------
//
// Live, a share granted through POST /admin/federation/shares was
// ignored by the next Like on that post, and a share revoked through
// DELETE kept delivering, until studio-a restarted: the resolver kept
// a per-object recipient cache whose invalidation hook nothing called.
// Each test below warms the SAME resolver first (as any earlier
// activity on the object does in production), then changes the share
// or peer through the product path where one exists (shares.Registry
// Insert / Revoke, the service the admin handlers call), then resolves
// again with no restart and no invalidation call.

type shareFixture struct {
	r       *outbox.Resolver
	pool    *pgxpool.Pool
	svc     *shares.Registry
	peer    uuid.UUID // shared-to peer
	other   uuid.UUID // connected peer never shared to
	ref     int64
	post    uuid.UUID // a public post with NO share yet
	actorIn outbox.Input
}

func newShareFixture(t *testing.T) *shareFixture {
	t.Helper()
	r, pool, peerID, _, ref := newResolverFixture(t, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger) // the shares service's own registry, as at boot
	t.Cleanup(reg.Stop)
	f := &shareFixture{
		r: r, pool: pool, svc: shares.NewRegistry(pool, logger, reg),
		peer: peerID, other: seedUnsharedPeer(t, pool, ref), ref: ref, post: uuid.New(),
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Warm public', 'public')`,
		f.post, ref,
	); err != nil {
		t.Fatalf("seed public post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE object_id = $1`, f.post)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, f.post)
	})
	f.actorIn = outbox.Input{
		Verb: visibilityRoutedVerb, TargetKind: "post", TargetID: f.post, AuthorRef: ref,
		Visibility: outbox.Visibility("public"), Sensitivity: outbox.SensitivityPublic,
	}
	return f
}

// activity seeds an activities row for a share's granted/revoked FK.
func (f *shareFixture) activity(t *testing.T, typ string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, $2, 'https://resolver-test.example/users/alice', $3, '{}'::jsonb) RETURNING id`,
		"https://resolver-test.example/activities/"+randHex(8), typ, f.ref,
	).Scan(&id); err != nil {
		t.Fatalf("seed %s activity: %v", typ, err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM federation_shares WHERE granted_activity_id = $1 OR revoked_activity_id = $1`, id)
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM activities WHERE id = $1`, id)
	})
	return id
}

func (f *shareFixture) grant(t *testing.T, expires *time.Time) *shares.Share {
	t.Helper()
	in := shares.InsertInput{
		GrantorUserRef: f.ref, ObjectKind: federation.ShareObjectKindPost, ObjectID: f.post,
		PeerID: f.peer, Scope: federation.ShareScopeView, GrantedActivityID: f.activity(t, "aa:Share"),
	}
	if expires != nil {
		in.ExpiresAt = &pgtype.Timestamptz{Time: *expires, Valid: true}
	}
	s, err := f.svc.Insert(context.Background(), in)
	if err != nil {
		t.Fatalf("grant via shares.Registry.Insert: %v", err)
	}
	return s
}

func (f *shareFixture) resolveWith(t *testing.T, r *outbox.Resolver, tier string) outbox.Result {
	t.Helper()
	in := f.actorIn
	in.Visibility = outbox.Visibility(tier)
	got, err := r.Resolve(context.Background(), in)
	if err != nil {
		t.Fatalf("resolve %s: %v", tier, err)
	}
	for _, rec := range got.Recipients {
		if rec.PeerID == f.other {
			t.Fatalf("%s reached the never-shared peer: %+v", tier, got.Recipients)
		}
	}
	return got
}

func TestResolver_WarmState_GrantIsSeenByTheNextResolve(t *testing.T) {
	f := newShareFixture(t)
	for _, tier := range []string{"public", "explicit-share"} {
		noRecipients(t, tier+" before grant (warms)", f.resolveWith(t, f.r, tier))
	}
	f.grant(t, nil)
	for _, tier := range []string{"public", "explicit-share"} {
		onlyPeer(t, tier+" after grant, same resolver", f.resolveWith(t, f.r, tier), f.peer)
	}
}

func TestResolver_WarmState_RevokeStopsTheNextResolve(t *testing.T) {
	f := newShareFixture(t)
	s := f.grant(t, nil)
	for _, tier := range []string{"public", "explicit-share"} {
		onlyPeer(t, tier+" before revoke (warms)", f.resolveWith(t, f.r, tier), f.peer)
	}
	if _, err := f.svc.Revoke(context.Background(), s.ID, f.activity(t, "aa:Unshare")); err != nil {
		t.Fatalf("revoke via shares.Registry.Revoke: %v", err)
	}
	for _, tier := range []string{"public", "explicit-share"} {
		noRecipients(t, tier+" after revoke, same resolver", f.resolveWith(t, f.r, tier))
	}
}

func TestResolver_WarmState_NaturalExpiryStopsTheNextResolve(t *testing.T) {
	// Expiry writes nothing: there is no event any cache could hook.
	f := newShareFixture(t)
	var dbNow time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT NOW()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	expires := dbNow.Add(1500 * time.Millisecond)
	f.grant(t, &expires)
	onlyPeer(t, "public before expiry (warms)", f.resolveWith(t, f.r, "public"), f.peer)
	time.Sleep(2500 * time.Millisecond) // past expires_at (1.5s after the DB's NOW())
	for _, tier := range []string{"public", "explicit-share"} {
		noRecipients(t, tier+" after expires_at passed, same resolver", f.resolveWith(t, f.r, tier))
	}
}

func TestResolver_WarmState_PeerDisabledOrDisconnectedStopsTheNextResolve(t *testing.T) {
	for _, tc := range []struct{ name, off, on string }{
		{"disabled", `UPDATE federation_peers SET enabled = FALSE WHERE id = $1`, `UPDATE federation_peers SET enabled = TRUE WHERE id = $1`},
		{"not connected", `UPDATE federation_peers SET status = 'pending_outbound' WHERE id = $1`, `UPDATE federation_peers SET status = 'connected' WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShareFixture(t)
			f.grant(t, nil)
			onlyPeer(t, "before (warms)", f.resolveWith(t, f.r, "public"), f.peer)
			if _, err := f.pool.Exec(context.Background(), tc.off, f.peer); err != nil {
				t.Fatal(err)
			}
			noRecipients(t, "peer "+tc.name, f.resolveWith(t, f.r, "public"))
			if _, err := f.pool.Exec(context.Background(), tc.on, f.peer); err != nil {
				t.Fatal(err)
			}
			onlyPeer(t, "peer restored", f.resolveWith(t, f.r, "public"), f.peer)
		})
	}
}

func TestResolver_TwoProcesses_AgreeOnGrantAndRevokeWithoutRestart(t *testing.T) {
	// Two resolvers with their own cache registries stand in for two
	// app processes (or replicas) behind one database.
	f := newShareFixture(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg2 := cache.NewRegistry(f.pool, logger)
	t.Cleanup(reg2.Stop)
	r2 := outbox.NewResolver(f.pool, reg2, func(context.Context) bool { return false })
	for _, r := range []*outbox.Resolver{f.r, r2} {
		noRecipients(t, "before grant (warms)", f.resolveWith(t, r, "public"))
	}
	s := f.grant(t, nil)
	for i, r := range []*outbox.Resolver{f.r, r2} {
		onlyPeer(t, "after grant, process "+strconv.Itoa(i+1), f.resolveWith(t, r, "public"), f.peer)
	}
	if _, err := f.svc.Revoke(context.Background(), s.ID, f.activity(t, "aa:Unshare")); err != nil {
		t.Fatal(err)
	}
	for i, r := range []*outbox.Resolver{f.r, r2} {
		noRecipients(t, "after revoke, process "+strconv.Itoa(i+1), f.resolveWith(t, r, "public"))
	}
}
