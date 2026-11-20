// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574: migration 00088 (received-asset view), against a real, fresh
// database: a complete record's originals must be identified, only a
// verified original can carry an identification, and Down then Up removes
// and restores the columns and the rule.

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMigration00088_FederationReceivedAssetView_UpDownUp(t *testing.T) {
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
	columns := `SELECT count(*) FROM information_schema.columns WHERE table_name = 'federation_received_asset_files'
		AND column_name IN ('detected_media_type', 'detected_by')`
	rule := `SELECT count(*) FROM pg_proc WHERE proname = 'federation_received_asset_require_consistent' AND prosrc LIKE '%not identified%'`
	if _, err := p.UpTo(ctx, 88); err != nil {
		t.Fatalf("up to 88: %v", err)
	}
	if count(columns) != 2 || count(rule) != 1 {
		t.Fatalf("after Up: columns %d, rule %d", count(columns), count(rule))
	}

	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm88', 1) RETURNING ref`,
		"m88-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm88', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m88-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
		VALUES ($1, 3, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
		t.Fatal(err)
	}
	// complete tries to commit a complete record whose original is
	// verified with (or without) an identification.
	complete := func(detect bool) error {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, $2, 'https://o.example/users/a', $3, gen_random_uuid(), 'https://o.example/v/1', 1, $4, '{}'::jsonb, 'public', 'k')
			RETURNING id`, peer, "https://o.example/a/"+uuid.NewString(), "https://o.example/assets/"+uuid.NewString(),
			strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		verify := `UPDATE federation_received_asset_files SET state = 'verified', verified_at = now() WHERE received_asset_id = $1 AND $2::text <> ''`
		if detect {
			verify = `UPDATE federation_received_asset_files SET state = 'verified', verified_at = now(),
				detected_media_type = 'image/png', detected_by = 'net/http.DetectContentType' WHERE received_asset_id = $1 AND $2::text <> ''`
		}
		for _, q := range []string{
			`INSERT INTO federation_received_asset_files (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			 VALUES ($1, 0, 'original', $2, 3, '[]'::jsonb, 'image/png')`,
			`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($2, 'federation_received_asset', $1::text)`,
			verify,
			`UPDATE federation_received_assets SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid() WHERE id = $1 AND $2::text <> ''`,
		} {
			if _, err := tx.ExecContext(ctx, q, id, hash); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	if err := complete(false); err == nil || !strings.Contains(err.Error(), "not identified") {
		t.Fatalf("a complete record with an unidentified original: %v, want the #1574 rule", err)
	}
	if err := complete(true); err != nil {
		t.Fatalf("a complete record with an identified original: %v", err)
	}
	// Only a verified original carries an identification: in the
	// transaction that creates its record (so the 00086 file-set guard
	// admits the row), the 00088 check refuses a pending original that
	// claims one.
	{
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, 'https://o.example/a/pending', 'https://o.example/users/a', 'https://o.example/assets/p', gen_random_uuid(),
			        'https://o.example/v/1', 1, $2, '{}'::jsonb, 'public', 'k') RETURNING id`, peer, strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, detected_media_type, detected_by)
			VALUES ($1, 0, 'original', $2, 3, '[]'::jsonb, 'image/png', 'image/png', 'x')`, id, hash)
		_ = tx.Rollback()
		if err == nil || !strings.Contains(err.Error(), "federation_received_asset_files_detected_check") {
			t.Fatalf("an identified pending original: %v, want the detected check", err)
		}
	}

	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM federation_peers WHERE id = $1`, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 87); err != nil {
		t.Fatalf("down to 87: %v", err)
	}
	if count(columns) != 0 || count(rule) != 0 {
		t.Fatalf("after Down: columns %d, rule %d", count(columns), count(rule))
	}
	if count(`SELECT count(*) FROM pg_proc WHERE proname = 'federation_received_asset_require_consistent'`) != 1 {
		t.Fatal("Down removed the 00086 rule instead of restoring it")
	}
	if _, err := p.UpTo(ctx, 88); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(columns) != 2 || count(rule) != 1 {
		t.Fatal("after Up again: 00088 is not fully applied")
	}
}
