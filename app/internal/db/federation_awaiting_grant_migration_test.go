// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00090_FederationAwaitingGrant_UpDownUp (#1574 slice 3): the
// record's and the ledger's reason sets gain awaiting_grant; Down holds
// new writes to the 00089 sets and keeps rows that already say it (NOT
// VALID); Up again allows it.
func TestMigration00090_FederationAwaitingGrant_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 90); err != nil {
		t.Fatalf("up to 90: %v", err)
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm90', 1) RETURNING ref`,
		"m90-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm90', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m90-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	// pending inserts a pending record with one original, reason as given.
	pending := func(reason string) (string, error) {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id, reason)
			VALUES ($1, $2, 'https://m90.example/users/a', $3, gen_random_uuid(),
			        'https://m90.example/assets/1/versions/1', 1, $4, '{}'::jsonb, 'public', 'k', $5) RETURNING id`,
			peer, "https://m90.example/activities/"+uuid.NewString(), "https://m90.example/assets/"+uuid.NewString(),
			strings.Repeat("0", 64), reason).Scan(&id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 0, 'original', $2, 1, '[]'::jsonb, 'image/png')`, id, hash); err != nil {
			return "", err
		}
		return id, tx.Commit()
	}
	ledger := func(id string) error {
		t.Helper()
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO federation_transfer_events
			(peer_id, peer_instance_url, received_asset_id, activity_uri, origin_asset_uri, version_uri, version_number,
			 sensitivity, event_type, outcome, reason, attempt, agent_name, record_state_after)
			VALUES ($1, 'https://m90.example', $2, 'a', 'o', 'v', 1, 'public', 'ingestion', 'warning', 'awaiting_grant', 0, 'test', 'pending')`,
			peer, id)
		return err
	}
	kept, err := pending("awaiting_grant")
	if err != nil {
		t.Fatalf("awaiting_grant record after Up: %v", err)
	}
	if err := ledger(kept); err != nil {
		t.Fatalf("awaiting_grant ledger row after Up: %v", err)
	}

	if _, err := p.DownTo(ctx, 89); err != nil {
		t.Fatalf("down to 89: %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_assets WHERE id = $1 AND reason = 'awaiting_grant'`, kept).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Down removed or rewrote a waiting record (%d, %v)", n, err)
	}
	if _, err := pending("awaiting_grant"); err == nil {
		t.Fatal("after Down a new awaiting_grant record was accepted")
	}
	if err := ledger(kept); err == nil {
		t.Fatal("after Down a new awaiting_grant ledger row was accepted")
	}

	if _, err := p.UpTo(ctx, 90); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := pending("awaiting_grant"); err != nil {
		t.Fatalf("awaiting_grant after Up again: %v", err)
	}
}
