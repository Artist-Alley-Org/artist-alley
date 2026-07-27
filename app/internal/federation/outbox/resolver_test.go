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
	"testing"

	"crypto/rand"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
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
	name := envOr("AA_DB_NAME", "artist_alley")
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

func TestResolver_Private_ReturnsRecipientSetEmpty(t *testing.T) {
	r, _, _, postID, ref := newResolverFixture(t, false)
	got, err := r.Resolve(context.Background(), outbox.Input{
		Verb: "Like", TargetKind: "post", TargetID: postID,
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
		Verb: "Like", TargetKind: "post", TargetID: postID,
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
		Verb: "Like", TargetKind: "post", TargetID: postID,
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
		Verb: "Like", TargetKind: "post", TargetID: postID,
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
		Verb: "Like", TargetKind: "post", TargetID: postID,
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

// --- cache behaviour ------------------------------------------------

func TestResolver_SharesByObjectCache_HitOnSecondResolve(t *testing.T) {
	r, pool, _, postID, ref := newResolverFixture(t, false)
	ctx := context.Background()
	// Warm the cache.
	first, _ := r.Resolve(ctx, outbox.Input{
		Verb: "Like", TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityPublic,
	})
	if len(first.Recipients) != 1 {
		t.Fatalf("warm: %d", len(first.Recipients))
	}
	// Mutate the DB OUT-OF-BAND (no cache invalidation): cache
	// hit should return the stale result, proving the cache is
	// in fact serving. The next test exercises invalidation.
	_, _ = pool.Exec(ctx, `DELETE FROM federation_shares WHERE peer_id = $1`, first.Recipients[0].PeerID)
	second, _ := r.Resolve(ctx, outbox.Input{
		Verb: "Like", TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityPublic,
	})
	if len(second.Recipients) != 1 {
		t.Errorf("cache hit expected 1 recipient (stale); got %d", len(second.Recipients))
	}
}

func TestResolver_InvalidateSharesByObject_ForcesDBRead(t *testing.T) {
	r, pool, _, postID, ref := newResolverFixture(t, false)
	ctx := context.Background()
	first, _ := r.Resolve(ctx, outbox.Input{
		Verb: "Like", TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityPublic,
	})
	if len(first.Recipients) != 1 {
		t.Fatalf("warm: %d", len(first.Recipients))
	}
	// Out-of-band delete + explicit invalidate → next read sees
	// zero recipients.
	_, _ = pool.Exec(ctx, `DELETE FROM federation_shares WHERE peer_id = $1`, first.Recipients[0].PeerID)
	_ = r.InvalidateSharesByObject(ctx, "post", postID)

	second, _ := r.Resolve(ctx, outbox.Input{
		Verb: "Like", TargetKind: "post", TargetID: postID,
		AuthorRef: ref, Visibility: outbox.VisibilityExplicitShare,
		Sensitivity: outbox.SensitivityPublic,
	})
	if second.Skipped != outbox.SkippedRecipientSetEmpty {
		t.Errorf("after invalidate: got %q want recipient_set_empty", second.Skipped)
	}
}
