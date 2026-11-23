// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00098_FederationReceivedAssetMetadata_UpDownUp (#1577): Up
// keeps an asset-metadata-1 snapshot one to one with its revision: the
// four keys only, each of its basic type or null; the published
// nanoseconds within range; both signers named; the row immutable and gone
// with its revision. Down refuses while a snapshot is kept, changing
// nothing; once none is left it drops the table; Up again restores it.
func TestMigration00098_FederationReceivedAssetMetadata_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 98); err != nil {
		t.Fatalf("up to 98: %v", err)
	}
	exec := func(q string, args ...any) error {
		_, err := sqlDB.ExecContext(ctx, q, args...)
		return err
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm98', 1) RETURNING ref`,
		"m98-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm98', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m98-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	asset := "https://m98.example/assets/" + uuid.NewString()
	revision := func(activity string) string {
		t.Helper()
		var id string
		if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_received_asset_revisions
			(peer_id, origin_asset_uri, activity_uri, activity_type, actor_uri, published, version_uri, file_set_sha256, mature, record)
			VALUES ($1, $2, $3, 'Update', 'https://m98.example/users/a', now(), $4, $5, FALSE, '{}'::jsonb) RETURNING id`,
			peer, asset, "https://m98.example/activities/"+activity, asset+"/versions/1", strings.Repeat("0", 64)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	const valid = `{"title":"t","description":null,"rights":null,"requiredStatement":{"label":{"en":["A"]},"value":{"none":["B"]}}}`
	snapshot := func(rev, snap string, nanos int, origin, recipient string) error {
		return exec(`INSERT INTO federation_received_asset_metadata
			(revision_id, profile_token, snapshot, published_seconds, published_nanos, published_text,
			 origin_signer_key_id, origin_signature_algorithm, origin_signature_value, recipient_signer_key_id)
			VALUES ($1, 'asset-metadata-1', $2::jsonb, 1795000000, $3, '2026-11-23T10:00:00.000000001Z', $4, 'Ed25519', 'sig', $5)`,
			rev, snap, nanos, origin, recipient)
	}
	r1, r2 := revision("u1"), revision("u2")
	if err := snapshot(r1, valid, 999999999, "https://m98.example/users/a#main-key", "https://local.example/federation/instance#main-key"); err != nil {
		t.Fatalf("a valid snapshot: %v", err)
	}
	if err := snapshot(r2, `{"title":null,"description":null,"rights":null,"requiredStatement":null}`, 0, "o", "r"); err != nil {
		t.Fatalf("four nulls: %v", err)
	}
	r3 := revision("u3")
	for name, err := range map[string]error{
		"a second snapshot of one revision": snapshot(r1, valid, 1, "o", "r"),
		"no such revision":                  snapshot(uuid.NewString(), valid, 1, "o", "r"),
		"a missing key":                     snapshot(r3, `{"title":null,"description":null,"rights":null}`, 1, "o", "r"),
		"an extra key":                      snapshot(r3, `{"title":null,"description":null,"rights":null,"requiredStatement":null,"creator":"x"}`, 1, "o", "r"),
		"a case-variant key":                snapshot(r3, `{"Title":null,"description":null,"rights":null,"requiredStatement":null}`, 1, "o", "r"),
		"a number title":                    snapshot(r3, `{"title":1,"description":null,"rights":null,"requiredStatement":null}`, 1, "o", "r"),
		"a string statement":                snapshot(r3, `{"title":null,"description":null,"rights":null,"requiredStatement":"x"}`, 1, "o", "r"),
		"an array snapshot":                 snapshot(r3, `[]`, 1, "o", "r"),
		"nanoseconds out of range":          snapshot(r3, valid, 1000000000, "o", "r"),
		"negative nanoseconds":              snapshot(r3, valid, -1, "o", "r"),
		"no origin signer":                  snapshot(r3, valid, 1, "", "r"),
		"no recipient signer":               snapshot(r3, valid, 1, "o", ""),
		"another profile": exec(`INSERT INTO federation_received_asset_metadata
			(revision_id, profile_token, snapshot, published_seconds, published_nanos, published_text,
			 origin_signer_key_id, origin_signature_algorithm, origin_signature_value, recipient_signer_key_id)
			VALUES ($1, 'archival-1', $2::jsonb, 1, 1, 'x', 'o', 'Ed25519', 's', 'r')`, r3, valid),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := exec(`UPDATE federation_received_asset_metadata SET origin_signature_value = 'other' WHERE revision_id = $1`, r1); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a snapshot rewritten: %v", err)
	}
	// A snapshot goes with its revision (here, with the revision's peer).
	if n := count(`SELECT count(*) FROM federation_received_asset_metadata`); n != 2 {
		t.Fatalf("%d snapshots, want 2", n)
	}

	// Down refuses while snapshots are kept, and changes nothing.
	if _, err := p.DownTo(ctx, 97); err == nil || !strings.Contains(err.Error(), "metadata is kept") {
		t.Fatalf("down to 97 with snapshots: %v", err)
	}
	if n := count(`SELECT count(*) FROM federation_received_asset_metadata`); n != 2 {
		t.Fatalf("the refused Down changed the snapshots (%d)", n)
	}
	if err := exec(`DELETE FROM federation_peers WHERE id = $1`, peer); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM federation_received_asset_metadata`); n != 0 {
		t.Fatalf("%d snapshots outlived their peer's revisions", n)
	}
	if _, err := p.DownTo(ctx, 97); err != nil {
		t.Fatalf("down to 97: %v", err)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_received_asset_metadata'`); n != 0 {
		t.Fatalf("after Down the table remains (%d)", n)
	}
	if n := count(`SELECT count(*) FROM pg_proc WHERE proname = 'federation_received_asset_metadata_immutable'`); n != 0 {
		t.Fatalf("after Down the immutability function remains (%d)", n)
	}
	// The revisions table of 00094 is untouched by this Down.
	if n := count(`SELECT count(*) FROM pg_trigger WHERE tgname = 'federation_received_asset_revisions_immutable_trg'`); n != 1 {
		t.Fatalf("Down 98 touched the revisions trigger (%d)", n)
	}
	if _, err := p.UpTo(ctx, 98); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := count(`SELECT count(*) FROM pg_trigger WHERE tgname = 'federation_received_asset_metadata_immutable_trg'`); n != 1 {
		t.Fatalf("after Up again the immutability trigger is missing (%d)", n)
	}
	if n := count(`SELECT count(*) FROM pg_constraint WHERE conname LIKE 'federation_received_asset_metadata_%'`); n != 6 {
		t.Fatalf("after Up again %d constraints, want 6 (pkey, fkey and 4 checks)", n)
	}
}
