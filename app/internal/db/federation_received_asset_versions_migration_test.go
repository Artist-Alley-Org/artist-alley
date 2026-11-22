// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00092_FederationReceivedAssetVersions_UpDownUp (#1575): a
// received asset can hold several versions, one record each, chained by
// predecessor. Up holds the chain's shape; Down refuses while an asset
// holds more than one version (00091 keeps one live record per asset)
// and, once none does, restores 00091's unique key and keeps the new
// columns; Up again holds the chain again.
func TestMigration00092_FederationReceivedAssetVersions_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 92); err != nil {
		t.Fatalf("up to 92: %v", err)
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm92', 1) RETURNING ref`,
		"m92-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm92', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m92-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	asset := "https://m92.example/assets/" + uuid.NewString()
	version := func(n int) string { return asset + "/versions/" + string(rune('0'+n)) }
	// pending inserts a pending record of version n of asset, following
	// predecessor ("" for none), with one original.
	pending := func(n int, predecessor string) (string, error) {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		var pred any
		if predecessor != "" {
			pred = predecessor
		}
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id, predecessor_version_uri)
			VALUES ($1, $2, 'https://m92.example/users/a', $3, gen_random_uuid(), $4, $5, $6, '{}'::jsonb, 'public', 'k', $7) RETURNING id`,
			peer, "https://m92.example/activities/"+uuid.NewString(), asset, version(n), n, strings.Repeat("0", 64), pred).Scan(&id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 0, 'original', $2, 1, '[]'::jsonb, 'image/png')`, id, strings.Repeat("ab", 32)); err != nil {
			return "", err
		}
		return id, tx.Commit()
	}
	exec := func(q string, args ...any) error {
		_, err := sqlDB.ExecContext(ctx, q, args...)
		return err
	}

	v1, err := pending(1, "")
	if err != nil {
		t.Fatalf("version 1 after Up: %v", err)
	}
	v2, err := pending(2, version(1))
	if err != nil {
		t.Fatalf("version 2 (a successor) after Up: %v", err)
	}
	if _, err := pending(2, version(1)); err == nil {
		t.Fatal("a second record of version 2 was accepted")
	}
	if _, err := pending(3, ""); err == nil {
		t.Fatal("a second first version was accepted")
	}
	if _, err := pending(1, version(1)); err == nil {
		t.Fatal("version 1 following a predecessor was accepted")
	}
	// The link names the previous version of the same asset, with the
	// version id the record follows, and only as the record completes.
	if err := exec(`UPDATE federation_received_assets SET predecessor_id = $1 WHERE id = $2`, v1, v2); err == nil {
		t.Fatal("a pending record was linked to its predecessor")
	}
	if err := exec(`UPDATE federation_received_assets SET predecessor_id = $1 WHERE id = $1`, v2); err == nil {
		t.Fatal("a record was linked to itself")
	}
	if err := exec(`UPDATE federation_received_assets SET state = 'superseded', completed_at = now(),
		authorizing_grant_id = gen_random_uuid() WHERE id = $1`, v1); err == nil {
		t.Fatal("a pending record became superseded")
	}
	if err := exec(`UPDATE federation_received_assets SET reason = 'awaiting_predecessor' WHERE id = $1`, v2); err != nil {
		t.Fatalf("a successor waiting for its predecessor: %v", err)
	}

	// Down refuses while the asset holds two versions.
	if _, err := p.DownTo(ctx, 91); err == nil || !strings.Contains(err.Error(), "more than one version") {
		t.Fatalf("down to 91 with a version history: %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri = $1`, asset).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the refused Down changed the records (%d, %v)", n, err)
	}
	// A rejected successor is not a version; with it, Down goes through.
	if err := exec(`UPDATE federation_received_assets SET state = 'rejected', reason = 'predecessor_missing' WHERE id = $1`, v2); err != nil {
		t.Fatalf("reject version 2: %v", err)
	}
	if _, err := p.DownTo(ctx, 91); err != nil {
		t.Fatalf("down to 91: %v", err)
	}
	if _, err := pending(2, ""); err == nil {
		t.Fatal("after Down a second live record of the asset was accepted")
	}
	var kept sql.NullString
	if err := sqlDB.QueryRowContext(ctx, `SELECT predecessor_version_uri FROM federation_received_assets WHERE id = $1`, v2).Scan(&kept); err != nil || kept.String != version(1) {
		t.Fatalf("Down dropped predecessor_version_uri (%v, %v)", kept, err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_assets WHERE reason = 'predecessor_missing'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Down rewrote a 00092 reason (%d, %v)", n, err)
	}

	if _, err := p.UpTo(ctx, 92); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if _, err := pending(2, version(1)); err != nil {
		t.Fatalf("version 2 after Up again: %v", err)
	}
}
