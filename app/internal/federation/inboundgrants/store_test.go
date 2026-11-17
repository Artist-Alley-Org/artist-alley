// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package inboundgrants_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/inboundgrants"
	"github.com/mscrnt/artist-alley/app/internal/testdb"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// fx is one receiver instance's view of one peer: its own peer row,
// local origin and users, all removed at cleanup (the peer row cascades
// every grant, tombstone and membership it holds).
type fx struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *inboundgrants.Store
	peerID uuid.UUID
	sender string // the peer's origin
	local  string // this instance's origin
	n      int
}

func newFx(t *testing.T) *fx {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	name := testdb.Name(t)
	pool, err := pgxpool.New(context.Background(), fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("AA_DB_HOST", "postgres"), env("AA_DB_PORT", "5432"), env("AA_DB_USER", "artist_alley"), pwd, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tag := uuid.NewString()[:8]
	f := &fx{t: t, pool: pool, sender: "https://peer-" + tag + ".example", local: "https://local-" + tag + ".example"}
	f.store = inboundgrants.New(pool, func(context.Context) string { return f.local }, nil, nil)
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM federation_peers WHERE id = $1`, f.peerID); err != nil {
			t.Errorf("cleanup peer: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM "user" WHERE username LIKE $1`, "ig-"+tag+"-%"); err != nil {
			t.Errorf("cleanup users: %v", err)
		}
	})
	var admin int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'grant test admin', 1) RETURNING ref`,
		"ig-"+tag+"-hs").Scan(&admin); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref, capabilities)
		 VALUES ($1, 'grant peer', 'k', TRUE, 'connected', $2, '["share-lifecycle-1"]'::jsonb) RETURNING id`,
		f.sender, admin).Scan(&f.peerID); err != nil {
		t.Fatal(err)
	}
	f.n = 0
	return f
}

// user creates a local user and returns its actor URI on this instance.
func (f *fx) user(name string) string {
	f.t.Helper()
	tag := f.local[len("https://local-"):][:8]
	username := "ig-" + tag + "-" + name
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'grant test', 1)`, username); err != nil {
		f.t.Fatal(err)
	}
	return f.local + "/users/" + username
}

// unknownUser is an actor URI on this instance with no user behind it.
func (f *fx) unknownUser(name string) string {
	tag := f.local[len("https://local-"):][:8]
	return f.local + "/users/ig-" + tag + "-" + name
}

func (f *fx) activity(name string) string { return f.sender + "/activities/" + name }
func (f *fx) asset(name string) string    { return f.sender + "/assets/" + name }

func (f *fx) share(name string, mut ...func(*inboundgrants.Share)) inboundgrants.Share {
	s := inboundgrants.Share{
		PeerID: f.peerID, ActivityURI: f.activity(name), GrantorActorURI: f.sender + "/users/alice",
		ObjectKind: federation.ShareObjectKindAsset, ObjectURI: f.asset("x"), Scope: federation.ShareScopeView,
	}
	for _, m := range mut {
		m(&s)
	}
	return s
}

func (f *fx) apply(s inboundgrants.Share) inboundgrants.Result {
	f.t.Helper()
	r, err := f.store.ApplyShare(context.Background(), s)
	if err != nil {
		f.t.Fatalf("ApplyShare %s: %v", s.ActivityURI, err)
	}
	return r
}

func (f *fx) unshare(name, revokes string, mut ...func(*inboundgrants.Unshare)) inboundgrants.Result {
	f.t.Helper()
	u := inboundgrants.Unshare{PeerID: f.peerID, ActivityURI: f.activity(name), ActorURI: f.sender + "/users/alice",
		ObjectKind: federation.ShareObjectKindAsset, ObjectURI: f.asset("x"), Revokes: f.activity(revokes)}
	for _, m := range mut {
		m(&u)
	}
	r, err := f.store.ApplyUnshare(context.Background(), u)
	if err != nil {
		f.t.Fatalf("ApplyUnshare %s: %v", u.ActivityURI, err)
	}
	return r
}

// allows asks for the original class by default, as the #72 receiver
// does, as actor ("" = the instance).
func (f *fx) allows(actor, object string, mut ...func(*inboundgrants.Query)) (uuid.UUID, error) {
	q := inboundgrants.Query{PeerID: f.peerID, ActorURI: actor, ObjectURI: object}
	for _, m := range mut {
		m(&q)
	}
	return f.store.Allows(context.Background(), q)
}

func (f *fx) wantAllowed(actor, object string, want uuid.UUID, mut ...func(*inboundgrants.Query)) {
	f.t.Helper()
	id, err := f.allows(actor, object, mut...)
	if err != nil {
		f.t.Fatalf("Allows(%q, %s): %v, want grant %s", actor, object, err, want)
	}
	if want != (uuid.UUID{}) && id != want {
		f.t.Fatalf("Allows(%q, %s) by grant %s, want %s", actor, object, id, want)
	}
}

func (f *fx) wantRefused(actor, object string, mut ...func(*inboundgrants.Query)) {
	f.t.Helper()
	if id, err := f.allows(actor, object, mut...); !errors.Is(err, inboundgrants.ErrNoGrant) {
		f.t.Fatalf("Allows(%q, %s) = %s, %v; want refused", actor, object, id, err)
	}
}

func (f *fx) state(name string) (state, reason string) {
	f.t.Helper()
	var r *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT state, end_reason FROM federation_inbound_grants WHERE peer_id = $1 AND grant_activity_uri = $2`,
		f.peerID, f.activity(name)).Scan(&state, &r); err != nil {
		f.t.Fatalf("state of %s: %v", name, err)
	}
	if r != nil {
		reason = *r
	}
	return state, reason
}

func (f *fx) wantState(name, state, reason string) {
	f.t.Helper()
	if s, r := f.state(name); s != state || r != reason {
		f.t.Fatalf("%s: %s/%s, want %s/%s", name, s, r, state, reason)
	}
}

func original(q *inboundgrants.Query) { q.Class = inboundgrants.ClassOriginal }
func scope(s federation.ShareScope) func(*inboundgrants.Query) {
	return func(q *inboundgrants.Query) { q.MinScope = s }
}

func TestApplyShare_FirstGrantDuplicateAndAuthorisation(t *testing.T) {
	f := newFx(t)
	r := f.apply(f.share("g1"))
	if r.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("first grant: %+v", r)
	}
	if d := f.apply(f.share("g1")); d.Outcome != inboundgrants.OutcomeDuplicate || d.GrantID != r.GrantID {
		t.Fatalf("duplicate: %+v", d)
	}
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_inbound_grants WHERE peer_id = $1`, f.peerID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d rows after a duplicate", n)
	}
	bob := f.user("bob")
	f.wantAllowed("", f.asset("x"), r.GrantID) // peer-wide: the instance
	f.wantAllowed(bob, f.asset("x"), r.GrantID)
	// view scope, no original grant.
	f.wantRefused("", f.asset("x"), original)
	f.wantRefused("", f.asset("x"), scope(federation.ShareScopeComment))
	// Another object, even one with the same bytes, is not covered.
	f.wantRefused("", f.asset("y"))
	// An actor URI that is not a local user is no recipient.
	f.wantRefused(f.unknownUser("ghost"), f.asset("x"))
	f.wantRefused(f.sender+"/users/bob", f.asset("x"))
}

func TestApplyShare_SupersedeChainAndDowngrade(t *testing.T) {
	f := newFx(t)
	g1 := f.apply(f.share("g1", func(s *inboundgrants.Share) { s.Scope = federation.ShareScopeRemix; s.AllowOriginal = true }))
	f.wantAllowed("", f.asset("x"), g1.GrantID, original, scope(federation.ShareScopeRemix))
	// Downgrade: view, no original.
	g2 := f.apply(f.share("g2", func(s *inboundgrants.Share) { s.Supersedes = f.activity("g1") }))
	if g2.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("g2: %+v", g2)
	}
	f.wantState("g1", "ended", inboundgrants.EndSuperseded)
	f.wantRefused("", f.asset("x"), original)
	f.wantRefused("", f.asset("x"), scope(federation.ShareScopeRemix))
	f.wantAllowed("", f.asset("x"), g2.GrantID)
	// A late Unshare of the superseded grant changes nothing.
	if r := f.unshare("u1", "g1"); r.Outcome != inboundgrants.OutcomeNoEffect {
		t.Fatalf("unshare of a superseded grant: %+v", r)
	}
	f.wantAllowed("", f.asset("x"), g2.GrantID)
}

func TestApplyShare_ReorderedDeliveriesAreTombstoned(t *testing.T) {
	f := newFx(t)
	// g2 (superseding g1) arrives before g1.
	if r := f.apply(f.share("g2", func(s *inboundgrants.Share) { s.Supersedes = f.activity("g1") })); r.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("g2: %+v", r)
	}
	if r := f.apply(f.share("g1")); r.Outcome != inboundgrants.OutcomeEnded || r.Reason != inboundgrants.EndTombstonedOnArrival {
		t.Fatalf("late g1: %+v", r)
	}
	f.wantState("g2", "live", "")

	// An Unshare before its Share.
	if r := f.unshare("u3", "g3", func(u *inboundgrants.Unshare) { u.ObjectURI = f.asset("z") }); r.Outcome != inboundgrants.OutcomeTombstoned {
		t.Fatalf("early unshare: %+v", r)
	}
	if r := f.apply(f.share("g3", func(s *inboundgrants.Share) { s.ObjectURI = f.asset("z") })); r.Outcome != inboundgrants.OutcomeEnded ||
		r.Reason != inboundgrants.EndTombstonedOnArrival {
		t.Fatalf("revoked-before-arrival g3: %+v", r)
	}
	f.wantRefused("", f.asset("z"))
	// The duplicate Unshare is a no-op.
	if r := f.unshare("u3", "g3", func(u *inboundgrants.Unshare) { u.ObjectURI = f.asset("z") }); r.Outcome != inboundgrants.OutcomeNoEffect {
		t.Fatalf("duplicate unshare: %+v", r)
	}
}

func TestApplyShare_AmbiguityFailsClosed(t *testing.T) {
	f := newFx(t)
	f.apply(f.share("g1"))
	// A second grant for the same tuple that does not name g1.
	r := f.apply(f.share("g9"))
	if r.Outcome != inboundgrants.OutcomeEnded || r.Reason != inboundgrants.EndAmbiguousSupersede {
		t.Fatalf("ambiguous: %+v", r)
	}
	f.wantState("g1", "ended", inboundgrants.EndAmbiguousSupersede)
	f.wantRefused("", f.asset("x"))

	// A supersede naming a grant of another tuple ends both.
	f.apply(f.share("h1", func(s *inboundgrants.Share) { s.ObjectURI = f.asset("other") }))
	r = f.apply(f.share("h2", func(s *inboundgrants.Share) { s.Supersedes = f.activity("h1") }))
	if r.Outcome != inboundgrants.OutcomeEnded || r.Reason != inboundgrants.EndAmbiguousSupersede {
		t.Fatalf("cross-tuple supersede: %+v", r)
	}
	f.wantState("h1", "ended", inboundgrants.EndAmbiguousSupersede)
	f.wantRefused("", f.asset("other"))
}

func TestApplyShare_RecipientBoundOnceToTheUserRef(t *testing.T) {
	f := newFx(t)
	bob, carol := f.user("bob"), f.user("carol")
	r := f.apply(f.share("g1", func(s *inboundgrants.Share) { s.TargetUserURI = bob }))
	if r.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("bob's grant: %+v", r)
	}
	f.wantAllowed(bob, f.asset("x"), r.GrantID)
	f.wantRefused(carol, f.asset("x"))
	f.wantRefused("", f.asset("x")) // the instance is not bob

	// An unknown recipient: stored ended, and a user created later with
	// that name gets nothing.
	dave := f.unknownUser("dave")
	u := f.apply(f.share("g2", func(s *inboundgrants.Share) { s.TargetUserURI = dave; s.ObjectURI = f.asset("d") }))
	if u.Outcome != inboundgrants.OutcomeEnded || u.Reason != inboundgrants.EndRecipientUnbound {
		t.Fatalf("unknown recipient: %+v", u)
	}
	if got := f.user("dave"); got != dave {
		t.Fatalf("dave is %s", got)
	}
	f.wantRefused(dave, f.asset("d"))

	// Deleting the bound user removes the grant with it.
	if _, err := f.pool.Exec(context.Background(), `DELETE FROM "user" WHERE username = $1`, bob[len(f.local+"/users/"):]); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM federation_inbound_grants WHERE id = $1`, r.GrantID).Scan(&n)
	if n != 0 {
		t.Fatal("the grant outlived its recipient")
	}
	// A recipient off this instance is refused outright.
	if _, err := f.store.ApplyShare(context.Background(), f.share("g3", func(s *inboundgrants.Share) {
		s.TargetUserURI = "https://elsewhere.example/users/bob"
	})); !errors.Is(err, inboundgrants.ErrInvalid) {
		t.Fatalf("foreign recipient: %v", err)
	}
}

func TestAllows_ExpiryIsTheReceiversClock(t *testing.T) {
	f := newFx(t)
	past := time.Now().Add(-time.Hour)
	if r := f.apply(f.share("g0", func(s *inboundgrants.Share) { s.ExpiresAt = &past })); r.Outcome != inboundgrants.OutcomeEnded ||
		r.Reason != inboundgrants.EndExpiredOnArrival {
		t.Fatalf("expired on arrival: %+v", r)
	}
	future := time.Now().Add(time.Hour)
	r := f.apply(f.share("g1", func(s *inboundgrants.Share) { s.ExpiresAt = &future; s.ObjectURI = f.asset("e") }))
	f.wantAllowed("", f.asset("e"), r.GrantID)
	// Time passes (by the database clock): nothing writes the row, and
	// the very next decision refuses.
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE federation_inbound_grants SET expires_at = now() - interval '1 second' WHERE id = $1`, r.GrantID); err != nil {
		t.Fatal(err)
	}
	f.wantRefused("", f.asset("e"))
	f.wantState("g1", "live", "")
}

func TestAllows_PeerDisabledDisconnectedOrRemoved(t *testing.T) {
	f := newFx(t)
	r := f.apply(f.share("g1"))
	f.wantAllowed("", f.asset("x"), r.GrantID)
	ctx := context.Background()
	for _, set := range []string{`enabled = FALSE`, `status = 'pending_outbound'`} {
		if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET `+set+` WHERE id = $1`, f.peerID); err != nil {
			t.Fatal(err)
		}
		f.wantRefused("", f.asset("x"))
		if _, err := f.pool.Exec(ctx, `UPDATE federation_peers SET enabled = TRUE, status = 'connected' WHERE id = $1`, f.peerID); err != nil {
			t.Fatal(err)
		}
		f.wantAllowed("", f.asset("x"), r.GrantID)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM federation_peers WHERE id = $1`, f.peerID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM federation_inbound_grants WHERE peer_id = $1`, f.peerID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d grants outlived their peer", n)
	}
	f.wantRefused("", f.asset("x"))
}

func TestAllows_OverlappingGrantsAreIndependent(t *testing.T) {
	f := newFx(t)
	bob := f.user("bob")
	// Two grantors on the origin, one object: two tuples, both live.
	ga := f.apply(f.share("ga"))
	gb := f.apply(f.share("gb", func(s *inboundgrants.Share) { s.GrantorActorURI = f.sender + "/users/admin" }))
	if ga.Outcome != inboundgrants.OutcomeLive || gb.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("two grantors: %+v %+v", ga, gb)
	}
	// And a user-specific grant beside the peer-wide ones.
	gu := f.apply(f.share("gu", func(s *inboundgrants.Share) { s.TargetUserURI = bob }))
	if gu.Outcome != inboundgrants.OutcomeLive {
		t.Fatalf("user grant: %+v", gu)
	}
	f.unshare("ua", "ga")
	f.wantAllowed("", f.asset("x"), gb.GrantID)
	f.unshare("uu", "gu", func(u *inboundgrants.Unshare) { u.TargetUserURI = bob })
	f.wantAllowed(bob, f.asset("x"), gb.GrantID) // still covered peer-wide
	f.unshare("ub", "gb")
	f.wantRefused("", f.asset("x"))
	f.wantRefused(bob, f.asset("x"))
}

func TestAllows_ContainerOnlyThroughRecordedMembership(t *testing.T) {
	f := newFx(t)
	coll := f.sender + "/collections/c1"
	r := f.apply(f.share("gc", func(s *inboundgrants.Share) {
		s.ObjectKind, s.ObjectURI, s.AllowOriginal = federation.ShareObjectKindCollection, coll, true
	}))
	// No membership recorded yet (#1574): the collection grant covers no
	// file of it.
	f.wantRefused("", f.asset("m1"), original)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO federation_inbound_memberships (peer_id, container_uri, member_uri) VALUES ($1, $2, $3)`,
		f.peerID, coll, f.asset("m1")); err != nil {
		t.Fatal(err)
	}
	f.wantAllowed("", f.asset("m1"), r.GrantID, original)
	f.wantRefused("", f.asset("m2"), original)
	// A non-collection grant never covers through a membership row.
	f.apply(f.share("gp", func(s *inboundgrants.Share) {
		s.ObjectKind, s.ObjectURI = federation.ShareObjectKindPost, f.sender+"/posts/p1"
	}))
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO federation_inbound_memberships (peer_id, container_uri, member_uri) VALUES ($1, $2, $3)`,
		f.peerID, f.sender+"/posts/p1", f.asset("m3")); err != nil {
		t.Fatal(err)
	}
	f.wantRefused("", f.asset("m3"))
}

// No positive cache: the decision right after a revoke, from the same
// store, refuses.
func TestAllows_NoPositiveCache(t *testing.T) {
	f := newFx(t)
	f.apply(f.share("g1"))
	for i := 0; i < 3; i++ {
		f.wantAllowed("", f.asset("x"), uuid.UUID{})
	}
	f.unshare("u1", "g1")
	f.wantRefused("", f.asset("x"))
}

// An in-flight decision (AllowsTx, the #72 state-7 check) holds the
// grant until its transaction ends: a revoke waits for that commit, then
// every later decision refuses.
func TestAllowsTx_RevokeSerialisesWithTheHolder(t *testing.T) {
	f := newFx(t)
	g := f.apply(f.share("g1"))
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if id, err := f.store.AllowsTx(ctx, tx, inboundgrants.Query{PeerID: f.peerID, ObjectURI: f.asset("x")}); err != nil || id != g.GrantID {
		t.Fatalf("AllowsTx: %s %v", id, err)
	}
	done := make(chan inboundgrants.Result, 1)
	go func() {
		r, err := f.store.ApplyUnshare(ctx, inboundgrants.Unshare{PeerID: f.peerID, ActivityURI: f.activity("u1"),
			ActorURI: f.sender + "/users/alice", ObjectKind: federation.ShareObjectKindAsset, ObjectURI: f.asset("x"),
			Revokes: f.activity("g1")})
		if err != nil {
			t.Errorf("unshare: %v", err)
		}
		done <- r
	}()
	select {
	case r := <-done:
		t.Fatalf("the revoke committed while the holder's transaction was open: %+v", r)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.Outcome != inboundgrants.OutcomeRevoked {
			t.Fatalf("revoke: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoke never completed")
	}
	f.wantRefused("", f.asset("x"))

	// And the other order: a revoke committed first makes AllowsTx refuse.
	f.apply(f.share("g2", func(s *inboundgrants.Share) { s.ObjectURI = f.asset("w") }))
	f.unshare("u2", "g2", func(u *inboundgrants.Unshare) { u.ObjectURI = f.asset("w") })
	err = pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
		_, err := f.store.AllowsTx(ctx, tx, inboundgrants.Query{PeerID: f.peerID, ObjectURI: f.asset("w")})
		return err
	})
	if !errors.Is(err, inboundgrants.ErrNoGrant) {
		t.Fatalf("AllowsTx after a committed revoke: %v", err)
	}
}

// A revoke naming a stored grant of another tuple still ends it
// (revoking is the safe direction), and says so.
func TestApplyUnshare_TupleMismatchStillEnds(t *testing.T) {
	f := newFx(t)
	f.apply(f.share("g1"))
	r := f.unshare("u1", "g1", func(u *inboundgrants.Unshare) { u.ObjectURI = f.asset("not-x") })
	if r.Outcome != inboundgrants.OutcomeRevoked || !r.TupleMismatch {
		t.Fatalf("mismatched unshare: %+v", r)
	}
	f.wantRefused("", f.asset("x"))
}

func TestAllows_RefusesIncompleteOrUnknownQueries(t *testing.T) {
	f := newFx(t)
	f.apply(f.share("g1"))
	f.wantRefused("", "")
	f.wantRefused("", f.asset("x"), func(q *inboundgrants.Query) { q.Class = "preview" })
	f.wantRefused("", f.asset("x"), scope("edit"))
	if _, err := f.store.Allows(context.Background(), inboundgrants.Query{ObjectURI: f.asset("x")}); !errors.Is(err, inboundgrants.ErrNoGrant) {
		t.Fatalf("no peer: %v", err)
	}
}
