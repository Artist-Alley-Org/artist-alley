// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: migration 00105 adds federation_received_access_work, the
// bounded queue of admitted access-copy work, keyed by the exact file
// row. Pinned here against a real, fresh database:
//
//   - a row names an existing file row (composite FK) and goes with it
//     (cascade, also when the record's peer is deleted);
//   - attempts are bounded, a claim token always has its expiry, and
//     last_reason is one of the four transient reasons;
//   - the partial index over unattempted access entries exists with that
//     predicate;
//   - Down removes the table and the index (the queue is derived, nothing
//     decided lives in it), and Up again restores them.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	accessWorkBeforeVersion = 104 // 00104_federation_received_access_copy_outcomes
	accessWorkAtVersion     = 105 // 00105_federation_received_access_work
)

func TestMigration00105_ReceivedAccessWork_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, accessWorkBeforeVersion); err != nil {
		t.Fatalf("up to %d: %v", accessWorkBeforeVersion, err)
	}
	if tableExists(t, sqlDB, "federation_received_access_work") {
		t.Fatal("federation_received_access_work exists before 00105")
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm105', 1) RETURNING ref`,
		"m105-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm105', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m105-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	// A pending record with an original and two access entries.
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rec string
	if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
		(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number, file_set_sha256, record, sensitivity, signer_key_id)
		VALUES ($1, $2, 'https://m105.example/users/a', $3, gen_random_uuid(), $3 || '/versions/1', 1, $4, '{}'::jsonb, 'public', 'k') RETURNING id`,
		peer, "https://m105.example/activities/"+uuid.NewString(), "https://m105.example/assets/"+uuid.NewString(), strings.Repeat("0", 64)).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	for pos, role := range []string{"original", "access", "access"} {
		state := "pending"
		if role != "original" {
			state = "not_transferred"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type, state)
			VALUES ($1, $2, $3, $4, 1, '[]'::jsonb, 'image/png', $5)`, rec, pos, role, strings.Repeat(string(rune('a'+pos)), 64), state); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := p.UpTo(ctx, accessWorkAtVersion); err != nil {
		t.Fatalf("up to %d: %v", accessWorkAtVersion, err)
	}
	exec := func(q string, args ...any) error {
		_, err := sqlDB.ExecContext(ctx, q, args...)
		return err
	}
	count := func() int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_access_work WHERE received_asset_id = $1`, rec).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, 1)`, rec); err != nil {
		t.Fatalf("admit an access entry: %v", err)
	}
	var attempts int
	var claim *string
	if err := sqlDB.QueryRowContext(ctx, `SELECT attempts, claim_token::text FROM federation_received_access_work WHERE received_asset_id = $1 AND position = 1`, rec).Scan(&attempts, &claim); err != nil || attempts != 0 || claim != nil {
		t.Fatalf("defaults: attempts %d claim %v (%v)", attempts, claim, err)
	}
	for name, c := range map[string]struct{ q, want string }{
		"a file that does not exist": {`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, 9)`, "federation_received_access_work_file_fkey"},
		"a duplicate":                {`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, 1)`, "federation_received_access_work_pkey"},
		"attempts past the bound":    {`UPDATE federation_received_access_work SET attempts = 101 WHERE received_asset_id = $1`, "attempts_check"},
		"a token with no expiry":     {`UPDATE federation_received_access_work SET claim_token = gen_random_uuid() WHERE received_asset_id = $1`, "claim_check"},
		"an expiry with no token":    {`UPDATE federation_received_access_work SET claim_expires_at = now() WHERE received_asset_id = $1`, "claim_check"},
		"a non-transient reason":     {`UPDATE federation_received_access_work SET last_reason = 'origin_refused' WHERE received_asset_id = $1`, "last_reason_check"},
	} {
		if err := exec(c.q, rec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	if err := exec(`UPDATE federation_received_access_work SET claim_token = gen_random_uuid(), claim_expires_at = now() + interval '1 minute',
		attempts = 1, last_reason = 'timeout' WHERE received_asset_id = $1`, rec); err != nil {
		t.Fatalf("a claim: %v", err)
	}
	var def string
	if err := sqlDB.QueryRowContext(ctx, `SELECT pg_get_indexdef('federation_received_asset_files_access_unattempted_idx'::regclass)`).Scan(&def); err != nil ||
		!strings.Contains(def, "(received_asset_id, \"position\")") ||
		!strings.Contains(def, "WHERE ((role = 'access'::text) AND (state = 'not_transferred'::text) AND (access_failure_reason IS NULL))") {
		t.Fatalf("discovery index %q (%v)", def, err)
	}

	// Down drops the queue and the index; the file rows stay.
	if _, err := p.DownTo(ctx, accessWorkBeforeVersion); err != nil {
		t.Fatalf("down to %d: %v", accessWorkBeforeVersion, err)
	}
	var idx int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'federation_received_asset_files_access_unattempted_idx'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if tableExists(t, sqlDB, "federation_received_access_work") || idx != 0 {
		t.Fatal("Down left the queue or its index")
	}
	var files int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_asset_files WHERE received_asset_id = $1`, rec).Scan(&files); err != nil || files != 3 {
		t.Fatalf("Down changed the file rows (%d, %v)", files, err)
	}

	// Up again; the queue goes with its file rows (the peer, then its
	// records, then their files).
	if _, err := p.UpTo(ctx, accessWorkAtVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := exec(`INSERT INTO federation_received_access_work (received_asset_id, position) VALUES ($1, 1), ($1, 2)`, rec); err != nil {
		t.Fatalf("admit after Up again: %v", err)
	}
	if count() != 2 {
		t.Fatalf("queue rows %d", count())
	}
	if err := exec(`DELETE FROM federation_peers WHERE id = $1`, peer); err != nil {
		t.Fatalf("delete the peer: %v", err)
	}
	if count() != 0 {
		t.Fatal("queue rows outlived their file rows")
	}
}
