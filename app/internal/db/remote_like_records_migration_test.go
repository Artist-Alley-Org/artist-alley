// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1572: migration 00078 binds every inbound Like to its activity id
// (federation_remote_likes). Pinned here against a real, fresh database:
//
//   - a remote like received before the migration is bound to a real
//     Like id only when a PROCESSED inbox Like row with the same peer,
//     actor and target is evidence for it (one record per such row);
//   - a remote like with no such evidence is kept, bound to the
//     synthetic urn:aa:legacy-like:<likes.id>, and no Undo removes it:
//     not one guessing an https id, and not the Undo of a later Like by
//     the same actor;
//   - an inbox row that was not processed, or that has no like row, is
//     not evidence and creates nothing;
//   - Down removes the table and Up again restores it.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/social"
)

const (
	remoteLikeRecordsBeforeVersion = 77 // 00077_federation_inbound_grants
	remoteLikeRecordsAtVersion     = 78 // 00078_federation_remote_like_records
)

func TestMigration00078_RemoteLikeRecords_BackfillAndLegacy(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, remoteLikeRecordsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", remoteLikeRecordsBeforeVersion, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var owner int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ('m78-owner', 'M78', 1) RETURNING ref`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	var peer uuid.UUID
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, handshake_by_user_ref)
		 VALUES ('https://m78-peer.example', 'M78 peer', 'k', $1) RETURNING id`, owner).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	post := uuid.New()
	exec(`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'm78', 'public')`, post, owner)
	const base = "https://m78-peer.example"
	alice, carol, dave := base+"/users/alice", base+"/users/carol", base+"/users/dave"
	exec(`INSERT INTO likes (target_kind, target_id, peer_id, actor_uri) VALUES ('post', $1, $2, $3), ('post', $1, $2, $4)`,
		post, peer, alice, carol)
	inboxLike := func(id, actor, status string) {
		t.Helper()
		exec(`INSERT INTO federation_inbox (activity_uri, peer_id, actor_uri, activity_type, object_kind, object_id,
			envelope_json, http_sig_key, status, processed_at)
			VALUES ($1, $2, $3, 'Like', 'post', $4, '{}'::jsonb, 'k', $5, CASE WHEN $5 = 'processed' THEN now() END)`,
			id, peer, actor, post, status)
	}
	aliceL1, aliceL2 := base+"/activities/a1", base+"/activities/a2"
	inboxLike(aliceL1, alice, "processed")
	inboxLike(aliceL2, alice, "processed")
	inboxLike(base+"/activities/c1", carol, "rejected") // not evidence
	inboxLike(base+"/activities/d1", dave, "processed") // no like row: nothing to bind

	if _, err := p.UpTo(ctx, remoteLikeRecordsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", remoteLikeRecordsAtVersion, err)
	}
	records := func() map[string]string {
		t.Helper()
		rows, err := sqlDB.QueryContext(ctx, `SELECT actor_uri, activity_uri, binding, undone_at IS NOT NULL FROM federation_remote_likes ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var actor, id, binding string
			var undone bool
			if err := rows.Scan(&actor, &id, &binding, &undone); err != nil {
				t.Fatal(err)
			}
			out[actor+" "+id] = fmt.Sprintf("%s/undone=%v", binding, undone)
		}
		return out
	}
	var carolLikeID string
	if err := sqlDB.QueryRowContext(ctx, `SELECT id::text FROM likes WHERE actor_uri = $1`, carol).Scan(&carolLikeID); err != nil {
		t.Fatal(err)
	}
	legacy := "urn:aa:legacy-like:" + carolLikeID
	want := map[string]string{
		alice + " " + aliceL1: "backfilled/undone=false",
		alice + " " + aliceL2: "backfilled/undone=false",
		carol + " " + legacy:  "legacy_unbound/undone=false",
	}
	got := records()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records after 00078:\n got %v\nwant %v", got, want)
	}

	// Behaviour on the migrated rows, through the production write.
	pool, err := pgxpool.New(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName, cfg.DBSSLMode))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sh := social.NewHandler(pool, nil, nil)
	undo := func(actor, like, id string) {
		t.Helper()
		if _, err := sh.UndoRemoteLike(context.Background(), social.RemoteUndoLikeInput{
			PeerID: peer, ActorURI: actor, LikeActivityURI: like, UndoActivityURI: base + "/activities/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	likeRows := func(actor string) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM likes WHERE actor_uri = $1`, actor).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A backfilled Like is undone by its real id; the actor stays liked
	// while its other evidenced Like is live.
	undo(alice, aliceL1, "ua1")
	if likeRows(alice) != 1 {
		t.Fatal("alice unliked while her second Like is live")
	}
	undo(alice, aliceL2, "ua2")
	if likeRows(alice) != 0 {
		t.Fatal("alice still liked after both her Likes were undone")
	}
	// carol's unbound like: a guessed https id stores a tombstone and
	// changes nothing; a later Like and its Undo leave it in place.
	undo(carol, base+"/activities/c1", "uc1")
	if likeRows(carol) != 1 {
		t.Fatal("a guessed Undo removed an unbound legacy like")
	}
	allow := func(context.Context, pgx.Tx) error { return nil }
	if _, err := sh.InsertRemoteLike(context.Background(), "post", post, peer, carol, base+"/activities/c2", allow); err != nil {
		t.Fatal(err)
	}
	undo(carol, base+"/activities/c2", "uc2")
	if likeRows(carol) != 1 {
		t.Fatal("the Undo of a later Like removed carol's unbound legacy like")
	}
	var count int
	if err := sqlDB.QueryRowContext(ctx, `SELECT like_count FROM posts WHERE id = $1`, post).Scan(&count); err != nil || count != 1 {
		t.Fatalf("like_count %d (%v), want 1 (carol's legacy like)", count, err)
	}

	// Down removes the table; Up restores it and binds what is left:
	// carol's like, again unbound (alice's like rows are gone, so her
	// processed inbox Likes have nothing to bind to).
	if _, err := p.DownTo(ctx, remoteLikeRecordsBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", remoteLikeRecordsBeforeVersion, err)
	}
	var exists bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public.federation_remote_likes') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("federation_remote_likes exists after Down (%v)", err)
	}
	if _, err := p.UpTo(ctx, remoteLikeRecordsAtVersion); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if got := records(); got[carol+" "+legacy] != "legacy_unbound/undone=false" {
		t.Fatalf("after Up again: %v", got)
	}
}
