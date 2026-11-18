// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Outbox dispatcher integration tests per the gold-standard
// "integration: real Postgres" layer of the test-layering
// requirement. Phase 1.22.D-b-3.
//
// Two paths exercised:
//   1. INSERT activities row → LISTEN/NOTIFY signal → dispatcher
//      RunOnce → federation_outbox row materialises with the
//      right recipient.
//   2. Restricted-sensitivity activity → emission-skipped audit
//      event + cursor still advances + zero outbox rows.
//
// The full LISTEN goroutine path is tested separately
// (TestDispatcher_NotifyWakesLoop) so the deterministic
// RunOnce + the async-signal path both have coverage.

package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// dispatcherFixture seeds: one user, one peer, one post, one
// share row, and constructs a wired dispatcher (with audit spy
// + visibility lookup pointing at the post).
type dispatcherFixture struct {
	t    *testing.T
	pool interface {
		QueryRow(context.Context, string, ...any) interface{}
		Exec(context.Context, string, ...any) (any, error)
	}
}

func TestDispatcher_HappyPath_ActivityInsertFansOutToOutbox(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)

	// Seed user / peer / post / share with FK-correct activity.
	username := "outbox-" + randHex(4)
	var grantorRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Outbox Test', 1) RETURNING ref`,
		username,
	).Scan(&grantorRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	var peerID uuid.UUID
	peerURL := "https://outbox-" + randHex(4) + ".example"
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Outbox Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		peerURL, grantorRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Outbox', 'explicit-share')`,
		postID, grantorRef,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	var grantActivityID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'aa:Share', $2, $3, '{}'::jsonb) RETURNING id`,
		"https://outbox-test.example/activities/"+randHex(8),
		"https://outbox-test.example/users/alice",
		grantorRef,
	).Scan(&grantActivityID); err != nil {
		t.Fatalf("seed grant activity: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO federation_shares (
		    grantor_user_ref, object_kind, object_id, peer_id,
		    target_user_url, scope, granted_activity_id
		 ) VALUES ($1, 'post', $2, $3, $4, 'view', $5)`,
		grantorRef, postID, peerID,
		"https://outbox-test.example/users/bob",
		grantActivityID,
	); err != nil {
		t.Fatalf("seed share: %v", err)
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, grantorRef)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id = $1`, peerID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
		// Reset cursor so the next test starts fresh.
		_, _ = pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	})

	// Reset cursor to before this test's activities so RunOnce
	// processes our seed rows (the dev DB may have prior state).
	_, _ = pool.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)

	// A Like of the peer's post (#1611) has no local tier, so it is
	// unresolved: this resolver can encrypt, or it would be skipped.
	resolver := outbox.NewResolver(pool, reg, func(context.Context) bool { return true })
	withReceivedGrants(resolver, pool)
	d := outbox.NewDispatcher(
		outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, // tick never fires; we drive RunOnce
		pool, resolver, logger,
	)
	auditCalls := &auditSpy{}
	d.SetSkippedAudit(auditCalls.record)
	// The seeded post carries no assets, so its tier is public.
	// Stated explicitly: with no lookup the dispatcher now treats
	// the tier as unresolved and this resolver (which cannot
	// encrypt) would skip the activity (#1525).
	d.SetSensitivityLookup(fixedSensitivity(outbox.SensitivityPublic))
	// Visibility lookup: posts have visibility=explicit-share
	// (we seeded it that way).
	d.SetVisibilityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		var v string
		err := pool.QueryRow(ctx,
			`SELECT visibility FROM posts WHERE id = $1`, id,
		).Scan(&v)
		if err != nil {
			return "", err
		}
		return outbox.Visibility(v), nil
	})

	// Insert a Like of a post the peer holds, under a live grant the
	// peer gave this instance (#1611). The trigger fires NOTIFY but we
	// drive RunOnce directly for determinism.
	target := peerURL + "/posts/" + uuid.NewString()
	seedReceivedGrant(t, pool, peerID, target)
	likeActivityID := seedRemoteLike(t, pool, grantorRef, localActorURI(t, pool, grantorRef), target, peerID)

	// Dev DB may have many pre-existing activities behind the
	// cursor; drive RunOnce until our activity has been
	// processed (cursor reached or passed our id).
	totalProcessed := 0
	for i := 0; i < 50; i++ {
		processed, _, _ := d.RunOnce(ctx)
		totalProcessed += processed
		if processed == 0 {
			break
		}
		// Check whether our activity's outbox row has landed.
		var n int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM federation_outbox WHERE activity_id = $1`,
			likeActivityID,
		).Scan(&n)
		if n > 0 {
			break
		}
	}
	if totalProcessed < 1 {
		t.Fatalf("processed nothing across 50 iterations")
	}

	// federation_outbox row exists for the peer + the Like activity.
	var outboxCount int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM federation_outbox WHERE activity_id = $1 AND peer_id = $2`,
		likeActivityID, peerID,
	).Scan(&outboxCount)
	if outboxCount != 1 {
		t.Errorf("federation_outbox row for Like: count=%d want 1", outboxCount)
	}

	// Cursor advanced.
	var cursorID *uuid.UUID
	_ = pool.QueryRow(ctx,
		`SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`,
	).Scan(&cursorID)
	if cursorID == nil {
		t.Error("cursor still NULL after dispatch")
	}
}

func TestDispatcher_RestrictedSensitivity_SkipsWithAudit(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)

	// Seed minimal fixtures (no shares needed — emission
	// refusal short-circuits before recipient resolution).
	username := "outbox-skip-" + randHex(4)
	var grantorRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Outbox Skip', 1) RETURNING ref`,
		username,
	).Scan(&grantorRef); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	postID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Restricted', 'explicit-share')`,
		postID, grantorRef,
	); err != nil {
		t.Fatalf("seed post: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE activity_id IN (SELECT id FROM activities WHERE actor_user_ref = $1)`, grantorRef)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, grantorRef)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
		_, _ = pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
	})
	_, _ = pool.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)

	resolver := outbox.NewResolver(pool, reg, func(context.Context) bool { return false })
	d := outbox.NewDispatcher(
		outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100},
		pool, resolver, logger,
	)
	spy := &auditSpy{}
	d.SetSkippedAudit(spy.record)
	// Visibility = explicit-share, Sensitivity = restricted.
	// Resolver MUST refuse emission per §3.9.
	d.SetVisibilityLookup(func(context.Context, string, uuid.UUID) (outbox.Visibility, error) {
		return outbox.VisibilityExplicitShare, nil
	})
	d.SetSensitivityLookup(func(context.Context, string, uuid.UUID) (outbox.Sensitivity, error) {
		return outbox.SensitivityRestricted, nil
	})

	// Insert a Create activity targeting the post.
	var activityID uuid.UUID
	postIDStr := postID.String()
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    object_uri, object_kind, object_local_id, payload)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '{}'::jsonb)
		 RETURNING id`,
		"https://outbox-test.example/activities/"+randHex(8),
		"https://outbox-test.example/users/alice", grantorRef,
		"https://outbox-test.example/posts/"+postIDStr, postIDStr,
	).Scan(&activityID); err != nil {
		t.Fatalf("seed activity: %v", err)
	}

	// Drive until our activity has been audited (dev DB may
	// have prior activities).
	for i := 0; i < 50; i++ {
		_, _, _ = d.RunOnce(ctx)
		if spy.findByActivityID(activityID.String()) != nil {
			break
		}
	}

	// Audit spy recorded the skip with the right reason for
	// THIS activity (dev DB may have many pre-existing
	// activities — find ours by id).
	got := spy.findByActivityID(activityID.String())
	if got == nil {
		t.Fatal("audit spy: no skipped audit recorded for our activity")
	}
	if got.reason != "encryption_required_but_not_supported" {
		t.Errorf("audit reason: got %q want encryption_required_but_not_supported", got.reason)
	}
	if got.sensitivity != "restricted" {
		t.Errorf("audit sensitivity: got %q want restricted", got.sensitivity)
	}

	// Cursor still advanced past the skipped activity.
	var cursorID *uuid.UUID
	_ = pool.QueryRow(ctx,
		`SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`,
	).Scan(&cursorID)
	if cursorID == nil {
		t.Error("cursor still NULL — should advance even on skip")
	}

	// Zero outbox rows for this activity.
	var n int
	_ = pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM federation_outbox WHERE activity_id = $1`,
		activityID,
	).Scan(&n)
	if n != 0 {
		t.Errorf("federation_outbox count for restricted activity: %d want 0", n)
	}
}

// TestDispatcher_UnresolvedSensitivity_FailsClosed pins #1525's
// dispatcher fallback. When the tier cannot be learned (no lookup
// wired, a lookup error, an empty answer) the activity is treated
// as unresolved, which requires encryption: a resolver that cannot
// encrypt skips it with the encryption audit, exactly as for
// restricted. Before #1525 all three fell back to public and the
// activity was enqueued for plaintext delivery.
func TestDispatcher_UnresolvedSensitivity_FailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		lookup outbox.SensitivityLookup
	}{
		{"no lookup wired", nil},
		{"lookup error", func(context.Context, string, uuid.UUID) (outbox.Sensitivity, error) {
			return "", errors.New("db down")
		}},
		{"empty answer", func(context.Context, string, uuid.UUID) (outbox.Sensitivity, error) {
			return "", nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := openTestPool(t)
			ctx := context.Background()
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg := cache.NewRegistry(pool, logger)
			t.Cleanup(reg.Stop)

			var grantorRef int64
			if err := pool.QueryRow(ctx,
				`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Outbox Unresolved', 1) RETURNING ref`,
				"outbox-unres-"+randHex(4),
			).Scan(&grantorRef); err != nil {
				t.Fatalf("seed user: %v", err)
			}
			postID := uuid.New()
			if _, err := pool.Exec(ctx,
				`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Unresolved', 'explicit-share')`,
				postID, grantorRef,
			); err != nil {
				t.Fatalf("seed post: %v", err)
			}
			t.Cleanup(func() {
				c := context.Background()
				_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE activity_id IN (SELECT id FROM activities WHERE actor_user_ref = $1)`, grantorRef)
				_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, grantorRef)
				_, _ = pool.Exec(c, `DELETE FROM posts WHERE id = $1`, postID)
				_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, grantorRef)
				_, _ = pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
			})
			_, _ = pool.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)

			resolver := outbox.NewResolver(pool, reg, func(context.Context) bool { return false })
			d := outbox.NewDispatcher(outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, pool, resolver, logger)
			spy := &auditSpy{}
			d.SetSkippedAudit(spy.record)
			d.SetVisibilityLookup(func(context.Context, string, uuid.UUID) (outbox.Visibility, error) {
				return outbox.VisibilityExplicitShare, nil
			})
			if tc.lookup != nil {
				d.SetSensitivityLookup(tc.lookup)
			}

			var activityID uuid.UUID
			postIDStr := postID.String()
			if err := pool.QueryRow(ctx,
				`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
				    object_uri, object_kind, object_local_id, payload)
				 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '{}'::jsonb)
				 RETURNING id`,
				"https://outbox-test.example/activities/"+randHex(8),
				"https://outbox-test.example/users/alice", grantorRef,
				"https://outbox-test.example/posts/"+postIDStr, postIDStr,
			).Scan(&activityID); err != nil {
				t.Fatalf("seed activity: %v", err)
			}
			for i := 0; i < 50; i++ {
				_, _, _ = d.RunOnce(ctx)
				if spy.findByActivityID(activityID.String()) != nil {
					break
				}
			}
			got := spy.findByActivityID(activityID.String())
			if got == nil {
				t.Fatal("no skipped audit for our activity: the unresolved tier was not treated as requiring encryption")
			}
			if got.reason != string(outbox.SkippedEncryptionRequiredButNotSupported) {
				t.Errorf("audit reason: got %q want %q", got.reason, outbox.SkippedEncryptionRequiredButNotSupported)
			}
			if got.sensitivity != string(outbox.SensitivityUnresolved) {
				t.Errorf("audit sensitivity: got %q want %q", got.sensitivity, outbox.SensitivityUnresolved)
			}
			var n int
			_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM federation_outbox WHERE activity_id = $1`, activityID).Scan(&n)
			if n != 0 {
				t.Errorf("federation_outbox rows for unresolved activity: %d want 0", n)
			}
		})
	}
}

// --- audit spy ------------------------------------------------------

type auditSpy struct {
	mu      sync.Mutex
	entries []auditEntry
}

type auditEntry struct {
	activityID, activityType, objectKind, objectID, visibility, sensitivity, reason string
}

func (s *auditSpy) record(_ context.Context, activityID, activityType, objectKind, objectID, visibility, sensitivity, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, auditEntry{activityID, activityType, objectKind, objectID, visibility, sensitivity, reason})
}

func (s *auditSpy) last() *auditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return nil
	}
	e := s.entries[len(s.entries)-1]
	return &e
}

func (s *auditSpy) findByActivityID(id string) *auditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.activityID == id {
			return &e
		}
	}
	return nil
}

var _ = io.Discard // import live

// TestDispatcher_PublicPostWithoutShare_SkipsAndTheBatchAdvances is the
// #1233 regression. A Like on a `public` post used to make Resolve
// return `unsupported visibility: "public"`; RunOnce rolled the WHOLE
// batch back on that error and never advanced the cursor, so the
// activities behind it never reached the outbox and the dispatcher
// logged the same error every tick.
//
// One RunOnce over exactly three activities, in order:
//
//	A1  Like on a local public post nobody shares    -> skipped, 0 rows
//	A2  Like on S's post, under S's grant (#1611)     -> 1 row, to S
//	A3  Like on a local public post shared to S      -> skipped, 0 rows
//
// Since #1611 a Like goes only to the owner of the liked object: A3 used
// to be sent to S, which refuses a Like of an object it does not hold
// (unknown_object). U is a second enabled + connected peer with no
// share of anything: public must never broadcast to it. The cursor is
// parked on a marker activity first, so this batch holds exactly these
// three rows whatever the database already contains.
func TestDispatcher_PublicPostWithoutShare_SkipsAndTheBatchAdvances(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := cache.NewRegistry(pool, logger)
	t.Cleanup(reg.Stop)

	var ref int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Outbox Public', 1) RETURNING ref`,
		"outbox-public-"+randHex(4),
	).Scan(&ref); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedPeer := func(label string) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
			    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
			 VALUES ($1, $2, '', 'connected', 'plaintext', TRUE, 'connected', $3)
			 RETURNING id`,
			"https://"+label+"-"+randHex(4)+".example", label, ref,
		).Scan(&id); err != nil {
			t.Fatalf("seed peer %s: %v", label, err)
		}
		return id
	}
	sharedPeer, unsharedPeer := seedPeer("shared"), seedPeer("unshared")

	seedPost := func(vis string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, $3, $4)`,
			id, ref, "Outbox "+vis, vis,
		); err != nil {
			t.Fatalf("seed %s post: %v", vis, err)
		}
		return id
	}
	publicUnshared := seedPost("public")
	explicitShared := seedPost("explicit-share")
	publicShared := seedPost("public")

	// Grant activity for the shares' FK, inserted BEFORE the marker so
	// it sits behind the cursor and is not part of the batch.
	var grantID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'aa:Share', 'https://outbox-test.example/users/alice', $2, '{}'::jsonb) RETURNING id`,
		"https://outbox-test.example/activities/"+randHex(8), ref,
	).Scan(&grantID); err != nil {
		t.Fatalf("seed grant activity: %v", err)
	}
	for _, post := range []uuid.UUID{explicitShared, publicShared} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id,
			    target_user_url, scope, granted_activity_id)
			 VALUES ($1, 'post', $2, $3, 'https://shared.example/users/bob', 'view', $4)`,
			ref, post, sharedPeer, grantID,
		); err != nil {
			t.Fatalf("seed share: %v", err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id IN ($1, $2)`, sharedPeer, unsharedPeer)
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE peer_id = $1`, sharedPeer)
		_, _ = pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1`, ref)
		_, _ = pool.Exec(c, `DELETE FROM posts WHERE id IN ($1, $2, $3)`, publicUnshared, explicitShared, publicShared)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id IN ($1, $2)`, sharedPeer, unsharedPeer)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = $1`, ref)
	})

	// Park the cursor on a marker, then add the three activities with
	// created_at strictly after it, in a fixed order.
	var markerID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'Update', 'https://outbox-test.example/users/alice', $2, '{}'::jsonb) RETURNING id`,
		"https://outbox-test.example/activities/marker-"+randHex(8), ref,
	).Scan(&markerID); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, markerID,
	); err != nil {
		t.Fatalf("park cursor: %v", err)
	}
	like := func(n int, post uuid.UUID) uuid.UUID {
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
			    object_uri, object_kind, object_local_id, payload, created_at)
			 VALUES ($1, 'Like', 'https://outbox-test.example/users/alice', $2, $3, 'post', $4, '{}'::jsonb,
			         (SELECT created_at FROM activities WHERE id = $5) + make_interval(secs => $6))
			 RETURNING id`,
			"https://outbox-test.example/activities/"+randHex(8), ref,
			"https://outbox-test.example/posts/"+post.String(), post.String(), markerID, float64(n),
		).Scan(&id); err != nil {
			t.Fatalf("seed like %d: %v", n, err)
		}
		return id
	}
	var sharedURL string
	if err := pool.QueryRow(ctx, `SELECT instance_url FROM federation_peers WHERE id = $1`, sharedPeer).Scan(&sharedURL); err != nil {
		t.Fatal(err)
	}
	remote := sharedURL + "/posts/" + uuid.NewString()
	seedReceivedGrant(t, pool, sharedPeer, remote)
	a1 := like(1, publicUnshared)
	var a2 uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    object_uri, object_kind, payload, created_at)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', jsonb_build_object('target_uri', $4::text, 'target_peer_id', $5::text),
		         (SELECT created_at FROM activities WHERE id = $6) + make_interval(secs => 2))
		 RETURNING id`,
		"https://outbox-test.example/activities/"+randHex(8), localActorURI(t, pool, ref), ref,
		remote, sharedPeer.String(), markerID,
	).Scan(&a2); err != nil {
		t.Fatalf("seed remote like: %v", err)
	}
	a3 := like(3, publicShared)

	// A Like of S's post has no local tier (unresolved): this resolver
	// can encrypt, so the received grant alone decides.
	resolver := outbox.NewResolver(pool, reg, func(context.Context) bool { return true })
	withReceivedGrants(resolver, pool)
	d := outbox.NewDispatcher(outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, pool, resolver, logger)
	spy := &auditSpy{}
	d.SetSkippedAudit(spy.record)
	// Public tier stated explicitly (#1525: no lookup = unresolved).
	d.SetSensitivityLookup(fixedSensitivity(outbox.SensitivityPublic))
	// Same shape as the production lookup (outboxVisibilityLookup in
	// internal/http/api.go): a post's stored visibility, verbatim.
	d.SetVisibilityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		if kind != "post" {
			return "", nil
		}
		var v string
		if err := pool.QueryRow(ctx,
			`SELECT visibility FROM posts WHERE id = $1 AND deleted_at IS NULL`, id,
		).Scan(&v); err != nil {
			return "", nil
		}
		return outbox.Visibility(v), nil
	})

	processed, skipped, enqueued := d.RunOnce(ctx)
	if processed != 3 || skipped != 2 || enqueued != 1 {
		t.Fatalf("RunOnce = processed %d, skipped %d, enqueued %d; want 3, 2, 1 "+
			"(0,0,0 is the #1233 whole-batch rollback)", processed, skipped, enqueued)
	}

	var cursor uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`,
	).Scan(&cursor); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != a3 {
		t.Fatalf("cursor = %v, want the batch's last activity %v (marker %v)", cursor, a3, markerID)
	}

	rowsFor := func(activity uuid.UUID) []uuid.UUID {
		rows, err := pool.Query(ctx, `SELECT peer_id FROM federation_outbox WHERE activity_id = $1`, activity)
		if err != nil {
			t.Fatalf("read outbox: %v", err)
		}
		defer rows.Close()
		var out []uuid.UUID
		for rows.Next() {
			var p uuid.UUID
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	for name, a := range map[string]uuid.UUID{"A1 (public, no share)": a1, "A3 (local public, shared)": a3} {
		if got := rowsFor(a); len(got) != 0 {
			t.Errorf("%s: outbox rows to %v, want none (a Like of a local post stays local)", name, got)
		}
	}
	if got := rowsFor(a2); len(got) != 1 || got[0] != sharedPeer {
		t.Errorf("A2 (S's post, S's grant): outbox rows to %v, want exactly [%v]", got, sharedPeer)
	}
	var toUnshared int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM federation_outbox WHERE peer_id = $1`, unsharedPeer).Scan(&toUnshared)
	if toUnshared != 0 {
		t.Errorf("unshared peer got %d outbox rows; public must not broadcast", toUnshared)
	}

	for name, a := range map[string]uuid.UUID{"A1": a1, "A3": a3} {
		got := spy.findByActivityID(a.String())
		if got == nil {
			t.Fatalf("%s: no emission-skipped audit", name)
		}
		if got.reason != string(outbox.SkippedRecipientSetEmpty) || got.visibility != "public" {
			t.Errorf("%s audit: reason=%q visibility=%q, want recipient_set_empty / public", name, got.reason, got.visibility)
		}
	}
	if spy.findByActivityID(a2.String()) != nil {
		t.Errorf("activity %v was delivered AND audited as skipped", a2)
	}

	// Nothing is left behind the cursor: the next scan is empty
	// instead of replaying A1's failure forever.
	if p, _, _ := d.RunOnce(ctx); p != 0 {
		t.Errorf("second RunOnce processed %d, want 0", p)
	}
}

// TestDispatcher_WarmState_GrantThenRevoke_NoRestart is rev344's live
// failure as a real-Postgres test: ONE long-lived dispatcher (one
// resolver) sees, in order,
//
//	L1  Like on S's post, no grant from S    -> skipped
//	    S grants the post (a live federation_inbound_grants row)
//	L2  Like on it                           -> exactly one row, to S
//	    S's grant ends (revoked)
//	L3  Like on it                           -> skipped, no new row
//
// with no restart and no invalidation call in between. Live (rev344), L2
// was skipped and L3 was delivered to the revoked peer, because L1 had
// warmed a recipient cache nothing invalidated. Since #1611 a Like is
// authorised by the grant its owner gave this instance, read from the
// database on every resolve. U (connected, never shared to) must get
// nothing throughout.
func TestDispatcher_WarmState_GrantThenRevoke_NoRestart(t *testing.T) {
	f := newShareFixture(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `DELETE FROM federation_outbox WHERE peer_id IN ($1, $2)`, f.peer, f.other)
		_, _ = f.pool.Exec(c, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = NULL WHERE id = 1`)
		_, _ = f.pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref = $1 AND activity_type IN ('Like', 'Update')`, f.ref)
	})

	var marker uuid.UUID
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		 VALUES ($1, 'Update', 'https://outbox-test.example/users/alice', $2, '{}'::jsonb) RETURNING id`,
		"https://outbox-test.example/activities/marker-"+randHex(8), f.ref,
	).Scan(&marker); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE federation_dispatch_state SET last_dispatched_activity_id = $1 WHERE id = 1`, marker); err != nil {
		t.Fatalf("park cursor: %v", err)
	}

	// A Like of S's post has no local tier (unresolved): this resolver
	// can encrypt, so the received grant alone decides.
	reg := cache.NewRegistry(f.pool, logger)
	t.Cleanup(reg.Stop)
	r := outbox.NewResolver(f.pool, reg, func(context.Context) bool { return true })
	withReceivedGrants(r, f.pool)
	var peerURL string
	if err := f.pool.QueryRow(ctx, `SELECT instance_url FROM federation_peers WHERE id = $1`, f.peer).Scan(&peerURL); err != nil {
		t.Fatal(err)
	}
	target := peerURL + "/posts/" + uuid.NewString()
	actor := localActorURI(t, f.pool, f.ref)
	d := outbox.NewDispatcher(outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, f.pool, r, logger)
	d.SetSensitivityLookup(fixedSensitivity(outbox.SensitivityPublic))
	d.SetVisibilityLookup(func(ctx context.Context, kind string, id uuid.UUID) (outbox.Visibility, error) {
		if kind != "post" {
			return "", nil
		}
		var v string
		if err := f.pool.QueryRow(ctx, `SELECT visibility FROM posts WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&v); err != nil {
			return "", nil
		}
		return outbox.Visibility(v), nil
	})

	like := func() uuid.UUID { return seedRemoteLike(t, f.pool, f.ref, actor, target, f.peer) }
	rowsTo := func(activity uuid.UUID) []uuid.UUID {
		rows, err := f.pool.Query(ctx, `SELECT peer_id FROM federation_outbox WHERE activity_id = $1`, activity)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []uuid.UUID
		for rows.Next() {
			var p uuid.UUID
			if err := rows.Scan(&p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
		return out
	}
	drain := func(label string) {
		for i := 0; i < 10; i++ {
			if p, _, _ := d.RunOnce(ctx); p == 0 {
				return
			}
		}
		t.Fatalf("%s: dispatcher did not drain", label)
	}

	l1 := like()
	drain("L1")
	if got := rowsTo(l1); len(got) != 0 {
		t.Fatalf("L1 (no share yet): outbox rows to %v, want none", got)
	}

	grant := seedReceivedGrant(t, f.pool, f.peer, target)
	l2 := like()
	drain("L2")
	if got := rowsTo(l2); len(got) != 1 || got[0] != f.peer {
		t.Fatalf("L2 (after grant, same dispatcher): outbox rows to %v, want exactly [%v]", got, f.peer)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE federation_inbound_grants SET state = 'ended', end_reason = 'revoked', ended_at = now()
		WHERE id = $1`, grant); err != nil {
		t.Fatalf("end grant: %v", err)
	}
	l3 := like()
	drain("L3")
	if got := rowsTo(l3); len(got) != 0 {
		t.Fatalf("L3 (after revoke, same dispatcher): outbox rows to %v, want none (revoked peer)", got)
	}

	var toPeer, toOther int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_outbox WHERE peer_id = $1`, f.peer).Scan(&toPeer)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_outbox WHERE peer_id = $1`, f.other).Scan(&toOther)
	if toPeer != 1 || toOther != 0 {
		t.Errorf("outbox totals: shared peer %d (want 1), never-shared peer %d (want 0)", toPeer, toOther)
	}
	var cursor uuid.UUID
	_ = f.pool.QueryRow(ctx, `SELECT last_dispatched_activity_id FROM federation_dispatch_state WHERE id = 1`).Scan(&cursor)
	if cursor != l3 {
		t.Errorf("cursor = %v, want the last activity L3 %v", cursor, l3)
	}
}

// dispatcherTestBase is the base URL these tests' local actors live on.
const dispatcherTestBase = "https://outbox-test.example"

// withReceivedGrants wires r with the production received-grant check
// (#1611): inboundgrants.Store.Allows over pool, this instance at
// dispatcherTestBase (the adapter boot installs in internal/http).
func withReceivedGrants(r *outbox.Resolver, pool *pgxpool.Pool) {
	store := inboundgrants.New(pool, func(context.Context) string { return dispatcherTestBase }, nil, nil)
	r.SetReceivedGrants(func(ctx context.Context, peerID uuid.UUID, actorURI, objectURI string) (bool, error) {
		_, err := store.Allows(ctx, inboundgrants.Query{PeerID: peerID, ActorURI: actorURI, ObjectURI: objectURI})
		if errors.Is(err, inboundgrants.ErrNoGrant) {
			return false, nil
		}
		return err == nil, err
	})
}

// localActorURI is a local user's actor URI on dispatcherTestBase.
func localActorURI(t *testing.T, pool *pgxpool.Pool, ref int64) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), `SELECT username FROM "user" WHERE ref = $1`, ref).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return dispatcherTestBase + "/users/" + name
}

// seedReceivedGrant records a live, peer-wide view grant from peer for
// objectURI, as an applied aa:Share would (#1571), and returns its id.
func seedReceivedGrant(t *testing.T, pool *pgxpool.Pool, peer uuid.UUID, objectURI string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO federation_inbound_grants (peer_id, grant_activity_uri, grantor_actor_uri, object_kind, object_uri, scope, state)
		 VALUES ($1, $2, $3, 'post', $4, 'view', 'live') RETURNING id`,
		peer, objectURI+"#grant-"+randHex(6), objectURI+"#owner", objectURI,
	).Scan(&id); err != nil {
		t.Fatalf("seed received grant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM federation_inbound_grants WHERE id = $1`, id)
	})
	return id
}

// seedRemoteLike records ref's Like of target, held by peer, as
// emit.Like records one with an explicit target (#1611).
func seedRemoteLike(t *testing.T, pool *pgxpool.Pool, ref int64, actorURI, target string, peer uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, object_uri, object_kind, payload)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', jsonb_build_object('target_uri', $4::text, 'target_peer_id', $5::text))
		 RETURNING id`,
		dispatcherTestBase+"/activities/"+randHex(8), actorURI, ref, target, peer.String(),
	).Scan(&id); err != nil {
		t.Fatalf("seed remote like: %v", err)
	}
	return id
}

// fixedSensitivity is a SensitivityLookup that reports one tier
// for every object. Tests that are not about sensitivity use it to
// state the tier their fixture means, since an unwired lookup is
// now treated as unresolved (#1525).
func fixedSensitivity(tier outbox.Sensitivity) outbox.SensitivityLookup {
	return func(context.Context, string, uuid.UUID) (outbox.Sensitivity, error) {
		return tier, nil
	}
}
