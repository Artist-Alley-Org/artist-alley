// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00093_FederationVersionGaps_UpDownUp (#1575): a sender
// passes over a version that stayed a draft. Up lets a received version
// follow a version more than one number below it and refuses to seal a
// local version once a later one exists. Down refuses, changing nothing,
// while a received version follows, or waits to follow, a held version
// more than one number below it; while a local asset has a sealed version
// above a draft; and while a received version waits for a predecessor
// that is not held. Once none does, it restores the 00092 chain rule and
// the 00085 seal (a lower draft can be sealed again). Up again restores
// both 00093 rules.
func TestMigration00093_FederationVersionGaps_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 93); err != nil {
		t.Fatalf("up to 93: %v", err)
	}
	exec := func(q string, args ...any) error {
		_, err := sqlDB.ExecContext(ctx, q, args...)
		return err
	}
	must := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm93', 1) RETURNING ref`,
		"m93-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}

	// Sending side: a local asset with a draft version 1 and a draft
	// version 2 created over it.
	objects := 0
	object := func() string {
		t.Helper()
		objects++
		h := fmt.Sprintf("%064x", 0x93000+objects)
		must(`INSERT INTO storage_objects (hash, size_bytes, content_type, backend) VALUES ($1, 3, 'image/png', 'fs')`, h)
		must(`INSERT INTO storage_variants (object_hash, variant_key, size_bytes, content_type, content_sha256, content_size_bytes,
			    digest_source, fixity_status) VALUES ($1, 'original', 3, 'image/png', $1, 3, 'write', 'verified')`, h)
		return h
	}
	// local is an asset with a draft version 1 and a draft version 2 over
	// it; it returns version 1.
	local := func() string {
		t.Helper()
		a := uuid.NewString()
		must(`INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
			VALUES ($1, 'm93', (SELECT MIN(ref) FROM asset_types), $2, $3, 3, 'public')`, a, ref, object())
		var v1 string
		if err := sqlDB.QueryRowContext(ctx, `SELECT current_version_id FROM assets WHERE id = $1`, a).Scan(&v1); err != nil {
			t.Fatal(err)
		}
		must(`SELECT asset_version_create($1, gen_random_uuid(), 2, $2, 'replacement', $3, 'png', NULL, 'unknown', NULL)`, a, v1, object())
		return v1
	}
	seal := func(v string) error { return exec(`SELECT asset_version_seal($1, '[]'::jsonb)`, v) }
	const onlyLatest = "only the latest version is sealed"
	if err := seal(local()); err == nil || !strings.Contains(err.Error(), onlyLatest) {
		t.Fatalf("sealing version 1 under version 2 after Up: %v", err)
	}

	// Receiving side: version 1 and version 3, which follows version 1
	// (the sender passed over a draft version 2).
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm93', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m93-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	asset := "https://m93.example/assets/" + uuid.NewString()
	version := func(n int) string { return asset + "/versions/" + string(rune('0'+n)) }
	// received inserts a pending record of version n following predecessor
	// ("" for none) and, when link is set, linked to that record.
	received := func(n int, predecessor, link string) (string, error) {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		var pred, linked any
		if predecessor != "" {
			pred = predecessor
		}
		if link != "" {
			linked = link
		}
		var id string
		if err := tx.QueryRowContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id, predecessor_version_uri, predecessor_id)
			VALUES ($1, $2, 'https://m93.example/users/a', $3, gen_random_uuid(), $4, $5, $6, '{}'::jsonb, 'public', 'k', $7, $8) RETURNING id`,
			peer, "https://m93.example/activities/"+uuid.NewString(), asset, version(n), n, strings.Repeat("0", 64), pred, linked).Scan(&id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO federation_received_asset_files
			(received_asset_id, position, role, object_hash, size_bytes, checksums, declared_media_type)
			VALUES ($1, 0, 'original', $2, 1, '[]'::jsonb, 'image/png')`, id, strings.Repeat("ab", 32)); err != nil {
			return "", err
		}
		return id, tx.Commit()
	}
	v1, err := received(1, "", "")
	if err != nil {
		t.Fatalf("version 1: %v", err)
	}
	v3, err := received(3, version(1), "")
	if err != nil {
		t.Fatalf("version 3 following version 1 after Up: %v", err)
	}
	// The chain trigger, checked and rolled back: a link two numbers down
	// passes it; at 00092 it does not.
	chainTrigger := func(link string) error {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		_, err = tx.ExecContext(ctx, `INSERT INTO federation_received_assets
			(peer_id, activity_uri, actor_uri, origin_asset_uri, origin_asset_id, version_uri, version_number,
			 file_set_sha256, record, sensitivity, signer_key_id, predecessor_version_uri, predecessor_id)
			SELECT peer_id, activity_uri || '-link', actor_uri, origin_asset_uri, origin_asset_id, $2, 5,
			       file_set_sha256, record, sensitivity, signer_key_id, version_uri, id
			  FROM federation_received_assets WHERE id = $1`, link, version(5))
		return err
	}
	if err := chainTrigger(v3); err != nil && strings.Contains(err.Error(), "version of the same asset") {
		t.Fatalf("a link from version 5 to version 3 after Up: %v", err)
	}

	// Down refuses while version 3 waits to follow version 1.
	if _, err := p.DownTo(ctx, 92); err == nil || !strings.Contains(err.Error(), "more than one number below") {
		t.Fatalf("down to 92 with a gapped chain: %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_assets WHERE origin_asset_uri = $1 AND state = 'pending'`, asset).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the refused Down changed the records (%d, %v)", n, err)
	}
	// still93 checks a refused Down left the database at 00093.
	still93 := func(what string) {
		t.Helper()
		if v, err := p.GetDBVersion(ctx); err != nil || v != 93 {
			t.Fatalf("%s: the refused Down left version %d (%v)", what, v, err)
		}
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc WHERE proname IN ('asset_version_seal', 'asset_versions_update_check')
			AND prosrc LIKE '%only the latest version is sealed%'`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("%s: the refused Down changed the seal (%d, %v)", what, n, err)
		}
	}
	still93("a gapped chain")
	// A rejected version is not part of the chain.
	if err := exec(`UPDATE federation_received_assets SET state = 'rejected', reason = 'predecessor_missing' WHERE id = $1`, v3); err != nil {
		t.Fatalf("reject version 3: %v", err)
	}

	// Down refuses while a local asset has a sealed version above a draft:
	// the 00085 seal could seal the draft and change what the sealed
	// version follows.
	sealedOver := uuid.NewString()
	must(`INSERT INTO assets (id, title, asset_type, owner_user_ref, file_hash, file_size_bytes, sensitivity)
		VALUES ($1, 'm93', (SELECT MIN(ref) FROM asset_types), $2, $3, 3, 'public')`, sealedOver, ref, object())
	var d1 string
	if err := sqlDB.QueryRowContext(ctx, `SELECT current_version_id FROM assets WHERE id = $1`, sealedOver).Scan(&d1); err != nil {
		t.Fatal(err)
	}
	d2, s3 := uuid.NewString(), uuid.NewString()
	must(`SELECT asset_version_create($1, $2, 2, $3, 'replacement', $4, 'png', NULL, 'unknown', NULL)`, sealedOver, d2, d1, object())
	must(`SELECT asset_version_create($1, $2, 3, $3, 'replacement', $4, 'png', NULL, 'unknown', NULL)`, sealedOver, s3, d2, object())
	if err := seal(s3); err != nil {
		t.Fatalf("sealing the latest version 3: %v", err)
	}
	if _, err := p.DownTo(ctx, 92); err == nil || !strings.Contains(err.Error(), "sealed version above a version that stayed a draft") {
		t.Fatalf("down to 92 with a sealed version above a draft: %v", err)
	}
	still93("a sealed version above a draft")
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM asset_versions WHERE asset_id = $1 AND sealed_at IS NULL`, sealedOver).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the refused Down sealed a draft (%d drafts, %v)", n, err)
	}
	must(`DELETE FROM assets WHERE id = $1`, sealedOver)

	// Down refuses while a received version waits for a predecessor that
	// is not held: its number is unknown.
	v6, err := received(6, version(5), "")
	if err != nil {
		t.Fatalf("version 6 waiting for version 5: %v", err)
	}
	if _, err := p.DownTo(ctx, 92); err == nil || !strings.Contains(err.Error(), "waits for a predecessor this instance does not hold") {
		t.Fatalf("down to 92 with a version waiting for an unknown predecessor: %v", err)
	}
	still93("a version waiting for an unknown predecessor")
	if err := exec(`UPDATE federation_received_assets SET state = 'rejected', reason = 'predecessor_missing' WHERE id = $1`, v6); err != nil {
		t.Fatalf("reject version 6: %v", err)
	}

	// With all three resolved, Down goes through.
	if _, err := p.DownTo(ctx, 92); err != nil {
		t.Fatalf("down to 92: %v", err)
	}
	if err := chainTrigger(v1); err == nil || !strings.Contains(err.Error(), "is not the previous version") {
		t.Fatalf("a link from version 5 to version 1 at 00092: %v", err)
	}
	if err := seal(local()); err != nil {
		t.Fatalf("sealing version 1 under version 2 at 00092 (the 00085 seal): %v", err)
	}
	var kept sql.NullString
	if err := sqlDB.QueryRowContext(ctx, `SELECT predecessor_version_uri FROM federation_received_assets WHERE id = $1`, v3).Scan(&kept); err != nil || kept.String != version(1) {
		t.Fatalf("Down changed a record (%v, %v)", kept, err)
	}

	if _, err := p.UpTo(ctx, 93); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := seal(local()); err == nil || !strings.Contains(err.Error(), onlyLatest) {
		t.Fatalf("sealing version 1 under version 2 after Up again: %v", err)
	}
	if err := chainTrigger(v1); err != nil && strings.Contains(err.Error(), "version of the same asset") {
		t.Fatalf("a link from version 5 to version 1 after Up again: %v", err)
	}
	if _, err := received(4, version(1), ""); err != nil {
		t.Fatalf("version 4 following version 1 after Up again: %v", err)
	}
}
