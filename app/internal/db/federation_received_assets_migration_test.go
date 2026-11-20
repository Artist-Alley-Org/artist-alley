// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1574: migration 00086 (received asset records), against a real,
// fresh database:
//
//   - a complete record holds its original under its own pin; rows of
//     its file set cannot be added or removed; and a
//     TRUNCATE (seed reset) of the records releases that pin and
//     schedules the bytes for GC;
//   - Down releases every record pin the same way and removes the
//     tables, functions and storage_pins guards; Up again re-creates them.

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMigration00086_FederationReceivedAssets_UpDownUp(t *testing.T) {
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
	objects := `SELECT count(*) FROM pg_class WHERE relname IN ('federation_received_assets', 'federation_received_asset_files')`
	guards := `SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'storage_pins_received_asset_%'`
	funcs := `SELECT count(*) FROM pg_proc WHERE proname LIKE 'federation_received_asset%' OR proname = 'storage_pins_received_asset_commit_check'`
	if _, err := p.UpTo(ctx, 86); err != nil {
		t.Fatalf("up to 86: %v", err)
	}
	if count(objects) != 2 || count(guards) != 3 || count(funcs) != 9 {
		t.Fatalf("after Up: tables %d, pin guards %d, functions %d", count(objects), count(guards), count(funcs))
	}

	// A complete record holding one original.
	hash := strings.Repeat("ab", 32)
	complete := func() string {
		t.Helper()
		var ref int64
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm86', 1) RETURNING ref`,
			"m86-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		var peer string
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
			VALUES ($1, 'm86', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m86-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO storage_objects (hash, size_bytes, content_type, backend)
			VALUES ($1, 3, 'image/png', 'fs') ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
			t.Fatal(err)
		}
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id)
			VALUES ($1, 'https://o.example/a/1', 'https://o.example/users/a', 'https://o.example/assets/x', gen_random_uuid(),
			        'https://o.example/assets/x/v/1', 1, $2, '{}'::jsonb, 'public', 'k') RETURNING id`,
			peer, strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`INSERT INTO federation_received_asset_files (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			 VALUES ($1, 0, 'original', $2, 3, '[]'::jsonb, 'image/png')`,
			`INSERT INTO storage_pins (object_hash, pin_subject_type, pin_subject_id) VALUES ($2, 'federation_received_asset', $1::text)`,
			`UPDATE storage_objects SET gc_eligible_at = NULL WHERE hash = $2 AND $1::text <> ''`,
			`UPDATE federation_received_asset_files SET state = 'verified', verified_at = now() WHERE received_asset_id = $1 AND $2::text <> ''`,
			`UPDATE federation_received_assets SET state = 'complete', completed_at = now(), authorizing_grant_id = gen_random_uuid() WHERE id = $1 AND $2::text <> ''`,
		} {
			if _, err := tx.ExecContext(ctx, q, id, hash); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit a complete record: %v", err)
		}
		return id
	}
	held := func(id string) int {
		return count(`SELECT count(*) FROM storage_pins WHERE pin_subject_type = 'federation_received_asset' AND pin_subject_id = $1`, id)
	}
	scheduled := func() bool {
		return count(`SELECT count(*) FROM storage_objects WHERE hash = $1 AND gc_eligible_at IS NOT NULL`, hash) == 1
	}

	id := complete()
	if held(id) != 1 || scheduled() {
		t.Fatal("the complete record does not hold its original")
	}
	// The file set of a complete record is immutable: no row is added
	// or removed, whatever its role.
	for _, q := range []string{
		`INSERT INTO federation_received_asset_files (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
		 VALUES ($1, 1, 'original', $2, 3, '[]'::jsonb, 'image/png')`,
		`INSERT INTO federation_received_asset_files (received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, state)
		 VALUES ($1, 1, 'access', $2, 3, '[]'::jsonb, 'image/png', 'not_transferred')`,
		`DELETE FROM federation_received_asset_files WHERE received_asset_id = $1 AND $2::text <> ''`,
	} {
		_, err := sqlDB.ExecContext(ctx, q, id, hash)
		if err == nil || !strings.Contains(err.Error(), "#1574") {
			t.Fatalf("%s: %v, want the #1574 file-set guard", q, err)
		}
	}
	if held(id) != 1 || count(`SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1`, id) != 1 ||
		count(`SELECT count(*) FROM federation_received_assets WHERE id = $1 AND state = 'complete'`, id) != 1 {
		t.Fatal("a refused file-set change altered the complete record or its pin")
	}
	if _, err := sqlDB.ExecContext(ctx, `TRUNCATE federation_received_assets CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if held(id) != 0 || !scheduled() {
		t.Fatal("TRUNCATE left the record's pin, or did not schedule the bytes for GC")
	}

	id = complete()
	if _, err := p.DownTo(ctx, 85); err != nil {
		t.Fatalf("down to 85: %v", err)
	}
	if count(objects) != 0 || count(guards) != 0 || count(funcs) != 0 {
		t.Fatalf("after Down: tables %d, pin guards %d, functions %d", count(objects), count(guards), count(funcs))
	}
	if held(id) != 0 || !scheduled() {
		t.Fatal("Down left the record's pin, or did not schedule the bytes for GC")
	}
	if _, err := p.UpTo(ctx, 86); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(objects) != 2 || count(guards) != 3 || count(funcs) != 9 {
		t.Fatal("after Up again: 00086 is not fully applied")
	}
}
