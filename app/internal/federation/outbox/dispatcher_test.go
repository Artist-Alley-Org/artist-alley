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
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation/outbox"
)

// dispatcherFixture seeds: one user, one peer, one post, one
// share row, and constructs a wired dispatcher (with audit spy
// + visibility lookup pointing at the post).
type dispatcherFixture struct {
	t            *testing.T
	pool         interface {
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
	if err := pool.QueryRow(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Outbox Peer', '', 'connected', 'plaintext', TRUE, 'connected', $2)
		 RETURNING id`,
		"https://outbox-"+randHex(4)+".example", grantorRef,
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

	resolver := outbox.NewResolver(pool, reg, func(context.Context) bool { return false })
	d := outbox.NewDispatcher(
		outbox.DispatcherConfig{TickInterval: time.Hour, BatchSize: 100}, // tick never fires; we drive RunOnce
		pool, resolver, logger,
	)
	auditCalls := &auditSpy{}
	d.SetSkippedAudit(auditCalls.record)
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

	// Insert a Like activity targeting the post. The trigger
	// fires NOTIFY but we drive RunOnce directly for
	// determinism.
	var likeActivityID uuid.UUID
	postIDStr := postID.String()
	if err := pool.QueryRow(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref,
		    object_uri, object_kind, object_local_id, payload)
		 VALUES ($1, 'Like', $2, $3, $4, 'post', $5, '{}'::jsonb)
		 RETURNING id`,
		"https://outbox-test.example/activities/"+randHex(8),
		"https://outbox-test.example/users/alice",
		grantorRef,
		"https://outbox-test.example/posts/"+postIDStr, postIDStr,
	).Scan(&likeActivityID); err != nil {
		t.Fatalf("seed like activity: %v", err)
	}

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
		 VALUES ($1, 'Create', $2, $3, $4, 'post', $5, '{}'::jsonb)
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
