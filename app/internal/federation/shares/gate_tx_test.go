// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package shares_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mscrnt/artist-alley/app/internal/federation"
	"github.com/mscrnt/artist-alley/app/internal/federation/shares"
)

// #1570: the inbound write gate, decided inside the write transaction.

func (fx *gateFixture) newPost(t *testing.T, ownerRef int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := fx.pool.Exec(context.Background(),
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Inbound gate', 'explicit-share')`,
		id, ownerRef); err != nil {
		t.Fatalf("insert post: %v", err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), `DELETE FROM posts WHERE id = $1`, id) })
	return id
}

const peerActor = "https://share-peer.example/users/bob"

// authorize runs AuthorizeInboundWrite in its own transaction.
func (fx *gateFixture) authorize(t *testing.T, w shares.InboundWrite) error {
	t.Helper()
	if w.PeerID == (uuid.UUID{}) {
		w.PeerID = fx.peerID
	}
	if w.ActorURI == "" {
		w.ActorURI = peerActor
	}
	return pgx.BeginFunc(context.Background(), fx.pool, func(tx pgx.Tx) error {
		return fx.reg.AuthorizeInboundWrite(context.Background(), tx, w)
	})
}

func wantUnshared(t *testing.T, err error, reason shares.RejectReason) {
	t.Helper()
	if !errors.Is(err, shares.ErrInboundUnshared) {
		t.Fatalf("err %v, want unshared (%s)", err, reason)
	}
	if reason != "" && !containsReason(err, reason) {
		t.Fatalf("err %v, want reason %s", err, reason)
	}
}

func containsReason(err error, r shares.RejectReason) bool {
	return err != nil && strings.Contains(err.Error(), string(r))
}

func like(kind federation.ShareObjectKind, id uuid.UUID) shares.InboundWrite {
	return shares.InboundWrite{Verb: federation.ActivityLike, TargetKind: kind, TargetID: id}
}

func comment(kind federation.ShareObjectKind, id uuid.UUID) shares.InboundWrite {
	return shares.InboundWrite{Verb: federation.ActivityCreate, TargetKind: kind, TargetID: id}
}

func TestAuthorizeInboundWrite_TargetMatrixAndExistence(t *testing.T) {
	fx := newGateFixture(t)
	post := fx.newPost(t, fx.grantorRef)
	// Supported: like on post/asset, comment on post/asset/collection.
	for verb, kinds := range map[federation.ActivityType][]federation.ShareObjectKind{
		federation.ActivityLike:   {federation.ShareObjectKindPost, federation.ShareObjectKindAsset},
		federation.ActivityCreate: {federation.ShareObjectKindPost, federation.ShareObjectKindAsset, federation.ShareObjectKindCollection},
	} {
		for _, k := range kinds {
			if _, ok := shares.InboundWriteSupported(verb, k); !ok {
				t.Errorf("%s on %s not supported", verb, k)
			}
		}
	}
	// Unsupported kinds and verbs have no target here.
	for _, w := range []shares.InboundWrite{
		like(federation.ShareObjectKindCollection, post), like(federation.ShareObjectKindUser, post),
		comment(federation.ShareObjectKindWorkspace, post), comment(federation.ShareObjectKindBrandKit, post),
		{Verb: federation.ActivityAnnounce, TargetKind: federation.ShareObjectKindPost, TargetID: post},
	} {
		if err := fx.authorize(t, w); !errors.Is(err, shares.ErrInboundUnknownTarget) {
			t.Errorf("%s on %s: %v, want unknown target", w.Verb, w.TargetKind, err)
		}
	}
	// Missing and soft-deleted targets are unknown, even with a share.
	fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: post, Scope: federation.ShareScopeRemix})
	if err := fx.authorize(t, like(federation.ShareObjectKindPost, uuid.New())); !errors.Is(err, shares.ErrInboundUnknownTarget) {
		t.Fatalf("missing post: %v", err)
	}
	if err := fx.authorize(t, like(federation.ShareObjectKindPost, post)); err != nil {
		t.Fatalf("shared post: %v", err)
	}
	if _, err := fx.pool.Exec(context.Background(), `UPDATE posts SET deleted_at = now() WHERE id = $1`, post); err != nil {
		t.Fatal(err)
	}
	if err := fx.authorize(t, like(federation.ShareObjectKindPost, post)); !errors.Is(err, shares.ErrInboundUnknownTarget) {
		t.Fatalf("deleted post: %v", err)
	}
}

func TestAuthorizeInboundWrite_ShareRules(t *testing.T) {
	fx := newGateFixture(t)
	ctx := context.Background()

	// No share at all.
	bare := fx.newPost(t, fx.grantorRef)
	wantUnshared(t, fx.authorize(t, like(federation.ShareObjectKindPost, bare)), shares.RejectReasonNoShareRow)

	// view covers a like, not a comment.
	viewed := fx.newPost(t, fx.grantorRef)
	fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: viewed, Scope: federation.ShareScopeView})
	if err := fx.authorize(t, like(federation.ShareObjectKindPost, viewed)); err != nil {
		t.Fatalf("like under view: %v", err)
	}
	wantUnshared(t, fx.authorize(t, comment(federation.ShareObjectKindPost, viewed)), shares.RejectReasonInsufficientScope)

	// Actor-specific: only that actor.
	mine := fx.newPost(t, fx.grantorRef)
	target := peerActor
	fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: mine, Scope: federation.ShareScopeComment, TargetUserURL: &target})
	if err := fx.authorize(t, comment(federation.ShareObjectKindPost, mine)); err != nil {
		t.Fatalf("named actor: %v", err)
	}
	other := comment(federation.ShareObjectKindPost, mine)
	other.ActorURI = "https://share-peer.example/users/eve"
	wantUnshared(t, fx.authorize(t, other), shares.RejectReasonWrongUser)

	// Another peer gets nothing from this peer's share.
	strangerPeer := fx.otherPeer(t)
	w := like(federation.ShareObjectKindPost, viewed)
	w.PeerID = strangerPeer
	wantUnshared(t, fx.authorize(t, w), shares.RejectReasonNoShareRow)

	// Expired by the database clock.
	exp := fx.newPost(t, fx.grantorRef)
	s := fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: exp, Scope: federation.ShareScopeView})
	if _, err := fx.pool.Exec(ctx, `UPDATE federation_shares SET expires_at = now() - interval '1 second' WHERE id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	wantUnshared(t, fx.authorize(t, like(federation.ShareObjectKindPost, exp)), shares.RejectReasonExpired)

	// Revoked: refused at once, with no cache to outlive it (the
	// cached gate was asked first and warmed).
	rev := fx.newPost(t, fx.grantorRef)
	rs := fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: rev, Scope: federation.ShareScopeView})
	if err := fx.authorize(t, like(federation.ShareObjectKindPost, rev)); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.reg.CanPeerAccess(ctx, shares.AccessRequest{PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindPost, ObjectID: rev,
		RequiredScope: federation.ShareScopeView, PeerEnabled: true, PeerConnected: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.pool.Exec(ctx, `UPDATE federation_shares SET revoked_at = now(), revoked_activity_id = $2 WHERE id = $1`, rs.ID, fx.activityID); err != nil {
		t.Fatal(err)
	}
	wantUnshared(t, fx.authorize(t, like(federation.ShareObjectKindPost, rev)), "")

	// Peer disabled, then not connected: refused from the peer row.
	for _, set := range []string{`enabled = FALSE`, `status = 'pending_outbound'`} {
		if _, err := fx.pool.Exec(ctx, `UPDATE federation_peers SET `+set+` WHERE id = $1`, fx.peerID); err != nil {
			t.Fatal(err)
		}
		wantUnshared(t, fx.authorize(t, like(federation.ShareObjectKindPost, viewed)), "")
		if _, err := fx.pool.Exec(ctx, `UPDATE federation_peers SET enabled = TRUE, status = 'connected' WHERE id = $1`, fx.peerID); err != nil {
			t.Fatal(err)
		}
	}
	if err := fx.authorize(t, like(federation.ShareObjectKindPost, viewed)); err != nil {
		t.Fatalf("peer restored: %v", err)
	}
}

// remix never grants edit (#1524): a modifying verb is refused under
// every share, here through the transactional gate.
func TestCanPeerAccessTx_EditReservedUnderRemix(t *testing.T) {
	fx := newGateFixture(t)
	post := fx.newPost(t, fx.grantorRef)
	fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: post, Scope: federation.ShareScopeRemix})
	err := pgx.BeginFunc(context.Background(), fx.pool, func(tx pgx.Tx) error {
		d, err := fx.reg.CanPeerAccessTx(context.Background(), tx, shares.AccessRequest{
			PeerID: fx.peerID, ObjectKind: federation.ShareObjectKindPost, ObjectID: post, RequiredScope: federation.ShareScopeEditReserved})
		if err != nil {
			return err
		}
		if d.Allowed || d.Reason != shares.RejectReasonInsufficientScope {
			t.Fatalf("edit under remix: %+v", d)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Transitive: a collection share reaches a member asset only when its
// grantor could have shared that asset directly.
func TestAuthorizeInboundWrite_TransitiveCollectionShare(t *testing.T) {
	fx := newGateFixture(t)
	coll := fx.newCollection(t, fx.grantorRef)
	own := fx.newAsset(t, fx.grantorRef)
	stranger := fx.newUser(t)
	foreign := fx.newAsset(t, stranger)
	fx.addMember(t, coll, own)
	fx.addMember(t, coll, foreign)
	fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindCollection, ObjectID: coll, Scope: federation.ShareScopeComment})
	if err := fx.authorize(t, comment(federation.ShareObjectKindAsset, own)); err != nil {
		t.Fatalf("own member through the collection: %v", err)
	}
	wantUnshared(t, fx.authorize(t, like(federation.ShareObjectKindAsset, foreign)), shares.RejectReasonGrantorNotOwner)
	// The collection itself takes comments under its own share.
	if err := fx.authorize(t, comment(federation.ShareObjectKindCollection, coll)); err != nil {
		t.Fatalf("comment on the shared collection: %v", err)
	}
	// An admin grantor reaches the foreign member.
	admin := fx.newUser(t)
	fx.makeAdmin(t, admin)
	fx.insertShare(t, shares.InsertInput{GrantorUserRef: admin, ObjectKind: federation.ShareObjectKindCollection, ObjectID: coll, Scope: federation.ShareScopeView})
	if err := fx.authorize(t, like(federation.ShareObjectKindAsset, foreign)); err != nil {
		t.Fatalf("admin grantor: %v", err)
	}
}

// Revoke versus write: a decision inside an open write transaction
// holds the share row, so the revoke waits for that commit; a revoke
// that committed first makes the decision refuse. There is no third
// order.
func TestAuthorizeInboundWrite_RevokeSerialisesWithTheWrite(t *testing.T) {
	fx := newGateFixture(t)
	ctx := context.Background()
	post := fx.newPost(t, fx.grantorRef)
	s := fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: post, Scope: federation.ShareScopeView})

	tx, err := fx.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := fx.reg.AuthorizeInboundWrite(ctx, tx, shares.InboundWrite{Verb: federation.ActivityLike, PeerID: fx.peerID,
		ActorURI: peerActor, TargetKind: federation.ShareObjectKindPost, TargetID: post}); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan error, 1)
	go func() {
		_, err := fx.reg.Revoke(ctx, s.ID, fx.activityID)
		revoked <- err
	}()
	select {
	case err := <-revoked:
		t.Fatalf("the revoke committed while the write transaction was open: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoke never completed")
	}
	wantUnshared(t, fx.authorize(t, like(federation.ShareObjectKindPost, post)), "")

	// The same for a soft delete of the target.
	post2 := fx.newPost(t, fx.grantorRef)
	fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: post2, Scope: federation.ShareScopeView})
	tx2, err := fx.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(ctx)
	if err := fx.reg.AuthorizeInboundWrite(ctx, tx2, shares.InboundWrite{Verb: federation.ActivityLike, PeerID: fx.peerID,
		ActorURI: peerActor, TargetKind: federation.ShareObjectKindPost, TargetID: post2}); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := fx.pool.Exec(ctx, `UPDATE posts SET deleted_at = now() WHERE id = $1`, post2)
		deleted <- err
	}()
	select {
	case <-deleted:
		t.Fatal("the delete committed while the write transaction was open")
	case <-time.After(500 * time.Millisecond):
	}
	_ = tx2.Commit(ctx)
	<-deleted
}

// otherPeer is a second connected peer with no shares.
func (fx *gateFixture) otherPeer(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := fx.pool.QueryRow(context.Background(),
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		 VALUES ($1, 'Other', 'k', TRUE, 'connected', $2) RETURNING id`,
		"https://other-"+uuid.NewString()[:8]+".example", fx.grantorRef).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = fx.pool.Exec(context.Background(), `DELETE FROM federation_peers WHERE id = $1`, id) })
	return id
}

// Expiry is judged by the database clock AFTER the decision's lock
// waits (#1570 review): a write transaction that started before a grant
// expired, then waited on a lock (the share row, the peer row or the
// target) until after it expired, must be refused once the lock is
// released. A transaction-start now() would admit it. The same wait with
// a grant that has not expired still admits.
func TestAuthorizeInboundWrite_ExpiryJudgedAfterLockWait(t *testing.T) {
	for _, held := range []string{"share", "peer", "target"} {
		for _, expires := range []string{"soon", "later"} {
			t.Run(held+"/"+expires, func(t *testing.T) {
				fx := newGateFixture(t)
				ctx := context.Background()
				post := fx.newPost(t, fx.grantorRef)
				s := fx.insertShare(t, shares.InsertInput{ObjectKind: federation.ShareObjectKindPost, ObjectID: post, Scope: federation.ShareScopeView})
				ttl := "2 seconds"
				if expires == "later" {
					ttl = "1 hour"
				}
				var expiresAt time.Time
				if err := fx.pool.QueryRow(ctx, `UPDATE federation_shares SET expires_at = clock_timestamp() + $2::interval
					WHERE id = $1 RETURNING expires_at`, s.ID, ttl).Scan(&expiresAt); err != nil {
					t.Fatal(err)
				}

				// The holder takes the row lock the decision will need.
				holder, err := fx.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Rollback(ctx)
				lockSQL := map[string]string{
					"share":  `SELECT 1 FROM federation_shares WHERE id = $1 FOR UPDATE`,
					"peer":   `SELECT 1 FROM federation_peers WHERE id = $1 FOR UPDATE`,
					"target": `SELECT 1 FROM posts WHERE id = $1 FOR UPDATE`,
				}[held]
				lockID := map[string]uuid.UUID{"share": s.ID, "peer": fx.peerID, "target": post}[held]
				var one int
				if err := holder.QueryRow(ctx, lockSQL, lockID).Scan(&one); err != nil {
					t.Fatal(err)
				}

				// The write transaction starts BEFORE the expiry.
				writer, err := fx.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback(ctx)
				var writerPID int
				var started time.Time
				if err := writer.QueryRow(ctx, `SELECT pg_backend_pid(), now()`).Scan(&writerPID, &started); err != nil {
					t.Fatal(err)
				}
				if expires == "soon" && !started.Before(expiresAt) {
					t.Fatalf("the write transaction started at %s, not before the expiry %s", started, expiresAt)
				}
				done := make(chan error, 1)
				go func() {
					done <- fx.reg.AuthorizeInboundWrite(ctx, writer, shares.InboundWrite{Verb: federation.ActivityLike,
						PeerID: fx.peerID, ActorURI: peerActor, TargetKind: federation.ShareObjectKindPost, TargetID: post})
				}()
				// It is waiting on the holder's lock.
				deadline := time.Now().Add(10 * time.Second)
				for {
					var waiting bool
					if err := fx.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
						WHERE pid = $1 AND wait_event_type = 'Lock')`, writerPID).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case err := <-done:
						t.Fatalf("the decision did not wait on the %s lock: %v", held, err)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatalf("the decision never waited on the %s lock", held)
					}
					time.Sleep(20 * time.Millisecond)
				}
				if expires == "soon" {
					// Hold the lock until the grant has expired by the
					// database's own clock.
					for {
						var past bool
						if err := fx.pool.QueryRow(ctx, `SELECT clock_timestamp() > $1::timestamptz + interval '200 milliseconds'`, expiresAt).Scan(&past); err != nil {
							t.Fatal(err)
						}
						if past {
							break
						}
						time.Sleep(50 * time.Millisecond)
					}
				}
				if err := holder.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				var got error
				select {
				case got = <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("the decision never finished")
				}
				if expires == "later" {
					if got != nil {
						t.Fatalf("a live grant refused after the wait: %v", got)
					}
					return
				}
				wantUnshared(t, got, shares.RejectReasonExpired)
			})
		}
	}
}
