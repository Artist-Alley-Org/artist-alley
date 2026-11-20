// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574: migration 00087 (the received asset import schedule), against a
// real, fresh database: a record is due when it is bound, a pending
// record's next attempt can be moved, a complete record still cannot be
// changed at all (the 00086 guard covers the new column), and Down then
// Up removes and restores the column and its due index without touching
// the records.

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMigration00087_FederationReceivedAssetSchedule_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	column := `SELECT count(*) FROM information_schema.columns WHERE table_name = 'federation_received_assets' AND column_name = 'next_attempt_at'`
	index := `SELECT count(*) FROM pg_indexes WHERE indexname = 'federation_received_assets_due_idx'`
	if _, err := p.UpTo(ctx, 87); err != nil {
		t.Fatalf("up to 87: %v", err)
	}
	if count(column) != 1 || count(index) != 1 {
		t.Fatalf("after Up: column %d, due index %d", count(column), count(index))
	}

	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm87', 1) RETURNING ref`,
		"m87-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm87', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m87-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	insert := func(state string) string {
		t.Helper()
		var reason any
		if state == "rejected" {
			reason = "no_inbound_grant"
		}
		var id string
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id, state, reason)
			VALUES ($1, $2, 'https://o.example/users/a', $3, gen_random_uuid(), 'https://o.example/v/1', 1, $4, '{}'::jsonb,
			        'public', 'k', $5, $6) RETURNING id`,
			peer, "https://o.example/a/"+uuid.NewString(), "https://o.example/assets/"+uuid.NewString(),
			strings.Repeat("0", 64), state, reason).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pending := insert("pending")
	if count(`SELECT count(*) FROM federation_received_assets WHERE id = $1 AND next_attempt_at <= now()`, pending) != 1 {
		t.Fatal("a bound record is not due at once")
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE federation_received_assets SET next_attempt_at = now() + interval '1 hour' WHERE id = $1`, pending); err != nil {
		t.Fatalf("moving a pending record's next attempt: %v", err)
	}
	rejected := insert("rejected")
	_, err := sqlDB.ExecContext(ctx, `UPDATE federation_received_assets SET next_attempt_at = now() WHERE id = $1`, rejected)
	if err == nil || !strings.Contains(err.Error(), "#1574") {
		t.Fatalf("moving a rejected record's next attempt: %v, want the #1574 update guard", err)
	}
	// The due index serves the worker's claim.
	if count(`SELECT count(*) FROM federation_received_assets WHERE state = 'pending' AND next_attempt_at <= now() + interval '2 hours'`) != 1 {
		t.Fatal("the pending record is not found as due")
	}

	if _, err := p.DownTo(ctx, 86); err != nil {
		t.Fatalf("down to 86: %v", err)
	}
	if count(column) != 0 || count(index) != 0 {
		t.Fatalf("after Down: column %d, due index %d", count(column), count(index))
	}
	if count(`SELECT count(*) FROM federation_received_assets WHERE id IN ($1, $2)`, pending, rejected) != 2 {
		t.Fatal("Down lost records")
	}
	if _, err := p.UpTo(ctx, 87); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(column) != 1 || count(index) != 1 {
		t.Fatal("after Up again: 00087 is not fully applied")
	}
	if count(`SELECT count(*) FROM federation_received_assets WHERE id = $1 AND next_attempt_at <= now()`, pending) != 1 {
		t.Fatal("after Up again an existing pending record is not due")
	}
}
