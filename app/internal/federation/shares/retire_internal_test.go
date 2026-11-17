// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1586 correction: expiry retirement is CONDITIONAL on the row still
// being expired at the moment of the UPDATE.
//
// The regrant path and the sweeper both read a share as expired and
// close it in a later statement, with nothing locking the row in
// between. If its expiry is extended in that window, closing it would
// revoke a LIVE grant. This test opens exactly that window: it reads the
// row as expired, extends the expiry, then hands the stale read to both
// production callers (expiryRetirer.retire and Sweeper.sweepOne). An
// internal test because the window cannot be opened from outside the
// package without a hook in the handler.
//
// Skips without AA_DB_PASSWORD.

package shares

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/activities"
	"github.com/mscrnt/artist-alley/app/internal/audit"
	"github.com/mscrnt/artist-alley/app/internal/cache"
	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

type retireFixture struct {
	pool    *pgxpool.Pool
	reg     *Registry
	retirer expiryRetirer
	sweeper *Sweeper
	grantor int64
	peer    uuid.UUID
	seedAct uuid.UUID
}

func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	pool, err := pgxpool.New(t.Context(), "host="+env("AA_DB_HOST", "postgres")+" port="+env("AA_DB_PORT", "5432")+
		" user="+env("AA_DB_USER", "artist_alley")+" dbname="+testdb.Name(t)+" sslmode=disable password="+pwd)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	tag := hex.EncodeToString(b)

	f := &retireFixture{pool: pool}
	if err := pool.QueryRow(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1,'Retire Test',1) RETURNING ref`,
		"retire-"+tag).Scan(&f.grantor); err != nil {
		t.Fatalf("user: %v", err)
	}
	pub, _, err := federation.GenerateActorKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := federation.PublicKeyToPEM(pub)
	if err := pool.QueryRow(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key,
		    trust_tier, encryption_policy, enabled, status, handshake_by_user_ref)
		 VALUES ($1,'Retire Peer',$2,'connected','plaintext',TRUE,'connected',$3) RETURNING id`,
		"https://retire-"+tag+".example", string(pubPEM), f.grantor).Scan(&f.peer); err != nil {
		t.Fatalf("peer: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source)
		 VALUES ($1,'aa:Share','https://local.example/users/retire',$2,'{}'::jsonb,'local') RETURNING id`,
		"https://local.example/activities/"+uuid.NewString(), f.grantor).Scan(&f.seedAct); err != nil {
		t.Fatalf("activity: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM federation_shares WHERE grantor_user_ref=$1`, f.grantor)
		_, _ = pool.Exec(c, `DELETE FROM federation_peers WHERE id=$1`, f.peer)
		_, _ = pool.Exec(c, `DELETE FROM activities WHERE actor_user_ref=$1`, f.grantor)
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref=$1`, f.grantor)
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cr := cache.NewRegistry(pool, logger)
	t.Cleanup(cr.Stop)
	f.reg = NewRegistry(pool, logger, cr)
	writer := activities.NewWriter(pool, logger, nil)
	rec := audit.NewRecorder(pool, logger)
	lookup := func(context.Context, uuid.UUID) (PeerInfo, error) {
		return PeerInfo{ID: f.peer, InstanceURL: "https://retire-peer.example", Enabled: true, Connected: true}, nil
	}
	inst := func(context.Context) string { return "https://local.example" }
	user := func(context.Context, int64) string { return "retire" }
	f.retirer = expiryRetirer{registry: f.reg, activities: writer, auditRec: rec, lookupPeer: lookup, instanceURLFn: inst, usernameFn: user}
	f.sweeper = NewSweeper(SweeperConfig{Interval: time.Hour, BatchSize: 50}, f.reg, writer, rec, lookup, inst, user, logger)
	return f
}

// staleExpiredRead plants an expired share and returns the read the
// retirement callers act on, asserting it says "expired".
func (f *retireFixture) staleExpiredRead(t *testing.T) (*Share, InsertInput) {
	t.Helper()
	in := InsertInput{
		GrantorUserRef: f.grantor, ObjectKind: federation.ShareObjectKindPost, ObjectID: uuid.New(),
		PeerID: f.peer, Scope: federation.ShareScopeView, GrantedActivityID: f.seedAct,
		ExpiresAt: &pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	if _, err := f.reg.Insert(context.Background(), in); err != nil {
		t.Fatalf("insert expired share: %v", err)
	}
	held, expired, err := f.reg.unrevokedForTuple(context.Background(), in)
	if err != nil || !expired {
		t.Fatalf("slot read: share=%v expired=%v err=%v; want an expired row", held, expired, err)
	}
	return held, in
}

func (f *retireFixture) extend(t *testing.T, id uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE federation_shares SET expires_at = NOW() + INTERVAL '1 hour' WHERE id=$1`, id); err != nil {
		t.Fatalf("extend: %v", err)
	}
}

// untouched asserts the share is still live and that no aa:Unshare and
// no revoke audit was committed for it.
func (f *retireFixture) untouched(t *testing.T, label string, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var revoked bool
	if err := f.pool.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM federation_shares WHERE id=$1`, id).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	var unshares, audits int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM activities WHERE activity_type='aa:Unshare' AND payload->>'share_id'=$1`, id.String()).Scan(&unshares)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE event_type='federation.share.revoked' AND metadata->>'share_id'=$1`, id.String()).Scan(&audits)
	if revoked || unshares != 0 || audits != 0 {
		t.Fatalf("%s: a share extended after the read was closed: revoked=%v aa:Unshare=%d revoke audits=%d (want false/0/0)",
			label, revoked, unshares, audits)
	}
}

func TestRetire_ExpiryExtendedAfterRead_LeavesTheLiveShareAlone(t *testing.T) {
	f := newRetireFixture(t)
	held, _ := f.staleExpiredRead(t)
	f.extend(t, held.ID)

	retired, err := f.retirer.retire(context.Background(), held, "test", map[string]any{"retired_by": "regrant"})
	if err != nil || retired {
		t.Fatalf("retire on a re-extended share: retired=%v err=%v; want false, nil", retired, err)
	}
	f.untouched(t, "regrant retirer", held.ID)
}

func TestSweeper_ExpiryExtendedAfterListing_LeavesTheLiveShareAlone(t *testing.T) {
	f := newRetireFixture(t)
	held, _ := f.staleExpiredRead(t) // stands in for ListExpiring's row
	f.extend(t, held.ID)

	if err := f.sweeper.sweepOne(context.Background(), held); err != nil {
		t.Fatalf("sweepOne on a re-extended share: %v", err)
	}
	f.untouched(t, "sweeper", held.ID)
}

func TestRetire_StillExpired_ClosesItOnce(t *testing.T) {
	// The positive control for the two tests above: the same stale read,
	// no extension, is closed, and a second attempt is a no-op.
	f := newRetireFixture(t)
	held, in := f.staleExpiredRead(t)
	retired, err := f.retirer.retire(context.Background(), held, "test", map[string]any{"retired_by": "regrant"})
	if err != nil || !retired {
		t.Fatalf("retire on an expired share: retired=%v err=%v; want true, nil", retired, err)
	}
	again, err := f.retirer.retire(context.Background(), held, "test", nil)
	if err != nil || again {
		t.Fatalf("second retire: retired=%v err=%v; want false, nil", again, err)
	}
	var unshares, audits int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM activities WHERE activity_type='aa:Unshare' AND payload->>'share_id'=$1`, held.ID.String()).Scan(&unshares)
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE event_type='federation.share.revoked' AND metadata->>'share_id'=$1`, held.ID.String()).Scan(&audits)
	if unshares != 1 || audits != 1 {
		t.Fatalf("closed once: aa:Unshare=%d revoke audits=%d, want 1/1", unshares, audits)
	}
	if _, _, err := f.reg.unrevokedForTuple(context.Background(), in); err != ErrShareNotFound {
		t.Fatalf("slot after retirement: %v, want free", err)
	}
}
