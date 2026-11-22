// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00091_FederationAssetWithdrawal_UpDownUp (#1575): a
// received record and its originals can be withdrawn, a deaccession is
// a ledger event, and the tombstone is kept. Down holds new writes to the
// 00090 rules and keeps every withdrawal already recorded (NOT VALID, the
// column and the tombstone table kept); Up again allows them and
// validates what is there.
func TestMigration00091_FederationAssetWithdrawal_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 91); err != nil {
		t.Fatalf("up to 91: %v", err)
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm91', 1) RETURNING ref`,
		"m91-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm91', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m91-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	// pending inserts a pending record with one original.
	pending := func() string {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, $2, 'https://m91.example/users/a', $3, gen_random_uuid(),
			        'https://m91.example/assets/1/versions/1', 1, $4, '{}'::jsonb, 'public', 'k') RETURNING id`,
			peer, "https://m91.example/activities/"+uuid.NewString(), "https://m91.example/assets/"+uuid.NewString(),
			strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 0, 'original', $2, 1, '[]'::jsonb, 'image/png')`, id, hash); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	// withdraw moves the record, then its original, to withdrawn.
	withdraw := func(id string) error {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		if _, err := tx.ExecContext(ctx, `UPDATE federation_received_assets SET state = 'withdrawn', reason = 'withdrawn', withdrawn_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE federation_received_asset_files SET state = 'withdrawn' WHERE received_asset_id = $1`, id); err != nil {
			return err
		}
		return tx.Commit()
	}
	ledger := func(id string) error {
		t.Helper()
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO federation_transfer_events
			(peer_id, peer_instance_url, received_asset_id, activity_uri, origin_asset_uri, version_uri, version_number,
			 sensitivity, event_type, outcome, attempt, agent_name, record_state_after)
			VALUES ($1, 'https://m91.example', $2, 'a', 'o', 'v', 1, 'public', 'deaccession', 'success', 0, 'test', 'withdrawn')`,
			peer, id)
		return err
	}
	tombstone := func() error {
		t.Helper()
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO federation_received_tombstones
			(peer_id, origin_asset_uri, activity_uri, actor_uri, tombstone, deleted, last_version)
			VALUES ($1, $2, 'a', 'b', '{}'::jsonb, '2026-11-21T09:00:00Z', 'v')`, peer, "https://m91.example/assets/"+uuid.NewString())
		return err
	}

	kept := pending()
	if err := withdraw(kept); err != nil {
		t.Fatalf("withdraw after Up: %v", err)
	}
	if err := ledger(kept); err != nil {
		t.Fatalf("deaccession after Up: %v", err)
	}
	if err := tombstone(); err != nil {
		t.Fatalf("tombstone after Up: %v", err)
	}
	// A claim needs an announcement and a peer, a version and 1 to 256 files.
	var act string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload)
		VALUES ($1, 'aa:AssetVersion', 'https://m91.example/users/a', $2, '{}'::jsonb) RETURNING id`,
		"https://m91.example/activities/"+uuid.NewString(), ref).Scan(&act); err != nil {
		t.Fatal(err)
	}
	claim := func(number int, files string) error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO federation_asset_announcement_claims (activity_id, peer_id, asset_id, version_number, files)
			VALUES ($1, $2, gen_random_uuid(), $3, $4::jsonb) ON CONFLICT DO NOTHING`, act, peer, number, files)
		return err
	}
	if err := claim(0, `[{"role":"original","sha256":"`+hash+`"}]`); err == nil {
		t.Fatal("a claim of version 0 was accepted")
	}
	if err := claim(1, `[]`); err == nil {
		t.Fatal("a claim without files was accepted")
	}
	if err := claim(1, `[{"role":"original","sha256":"`+hash+`"}]`); err != nil {
		t.Fatalf("a claim after Up: %v", err)
	}
	// The shape holds: a withdrawn record needs its time; a deaccession is
	// a success ending withdrawn; a withdrawn record is final.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE federation_received_assets SET withdrawn_at = NULL WHERE id = $1`, kept); err == nil {
		t.Fatal("a withdrawn record without withdrawn_at was accepted")
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO federation_transfer_events
		(peer_id, peer_instance_url, received_asset_id, activity_uri, origin_asset_uri, version_uri, version_number,
		 sensitivity, event_type, outcome, reason, attempt, agent_name, record_state_after)
		VALUES ($1, 'x', $2, 'a', 'o', 'v', 1, 'public', 'deaccession', 'failure', 'withdrawn', 0, 'test', 'rejected')`, peer, kept); err == nil {
		t.Fatal("a failed deaccession was accepted")
	}
	// Other records are unaffected by the column.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE federation_received_assets SET withdrawn_at = now() WHERE id = $1`, pending()); err == nil {
		t.Fatal("a pending record with withdrawn_at was accepted")
	}

	if _, err := p.DownTo(ctx, 90); err != nil {
		t.Fatalf("down to 90: %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_assets r
		JOIN federation_received_asset_files f ON f.received_asset_id = r.id AND f.state = 'withdrawn'
		WHERE r.id = $1 AND r.state = 'withdrawn' AND r.withdrawn_at IS NOT NULL`, kept).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Down removed or rewrote a withdrawn record (%d, %v)", n, err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_transfer_events WHERE received_asset_id = $1 AND event_type = 'deaccession'`, kept).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Down removed a deaccession (%d, %v)", n, err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_tombstones WHERE peer_id = $1`, peer).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Down removed a kept tombstone (%d, %v)", n, err)
	}
	// The sender's record of what each announcement told its peer is
	// kept by Down too.
	var claims bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public.federation_asset_announcement_claims') IS NOT NULL`).Scan(&claims); err != nil || !claims {
		t.Fatalf("Down dropped federation_asset_announcement_claims (%v, %v)", claims, err)
	}
	if err := withdraw(pending()); err == nil {
		t.Fatal("after Down a new withdrawal was accepted")
	}
	if err := ledger(kept); err == nil {
		t.Fatal("after Down a new deaccession was accepted")
	}

	if _, err := p.UpTo(ctx, 91); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := withdraw(pending()); err != nil {
		t.Fatalf("withdraw after Up again: %v", err)
	}
	if err := ledger(kept); err != nil {
		t.Fatalf("deaccession after Up again: %v", err)
	}
}
