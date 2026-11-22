// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00094_FederationReceivedAssetRevisions_UpDownUp (#1575): Up
// keeps each received aa:asset's flags as an immutable revision, one per
// (peer, activity). Down refuses while a revision is kept, changing
// nothing; once none is left it drops the table; Up again restores it.
func TestMigration00094_FederationReceivedAssetRevisions_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 94); err != nil {
		t.Fatalf("up to 94: %v", err)
	}
	exec := func(q string, args ...any) error {
		_, err := sqlDB.ExecContext(ctx, q, args...)
		return err
	}
	var ref int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'm94', 1) RETURNING ref`,
		"m94-"+uuid.NewString()[:8]).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	var peer string
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO federation_peers (instance_url, display_name, instance_public_key, enabled, status, handshake_by_user_ref)
		VALUES ($1, 'm94', 'k', TRUE, 'connected', $2) RETURNING id`, "https://m94-"+uuid.NewString()[:8]+".example", ref).Scan(&peer); err != nil {
		t.Fatal(err)
	}
	asset := "https://m94.example/assets/" + uuid.NewString()
	revision := func(activity, typ, ai string) error {
		var aiArg any
		if ai != "" {
			aiArg = ai
		}
		return exec(`INSERT INTO federation_received_asset_revisions
			(peer_id, origin_asset_uri, activity_uri, activity_type, actor_uri, published, version_uri, file_set_sha256, mature, ai_provenance, record)
			VALUES ($1, $2, $3, $4, 'https://m94.example/users/a', now(), $5, $6, TRUE, $7, '{}'::jsonb)`,
			peer, asset, "https://m94.example/activities/"+activity, typ, asset+"/versions/1", strings.Repeat("0", 64), aiArg)
	}
	if err := revision("a1", "aa:AssetVersion", ""); err != nil {
		t.Fatalf("a revision after Up: %v", err)
	}
	if err := revision("u1", "Update", "generated"); err != nil {
		t.Fatalf("an Update's revision: %v", err)
	}
	for name, err := range map[string]error{
		"the same activity again":  revision("u1", "Update", ""),
		"another activity type":    revision("x1", "Create", ""),
		"an unknown ai_provenance": revision("x2", "Update", "machine"),
	} {
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if err := exec(`UPDATE federation_received_asset_revisions SET mature = FALSE WHERE activity_uri LIKE '%/u1'`); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a revision rewritten: %v", err)
	}

	// Down refuses while revisions are kept.
	if _, err := p.DownTo(ctx, 93); err == nil || !strings.Contains(err.Error(), "revisions are kept") {
		t.Fatalf("down to 93 with revisions: %v", err)
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_received_asset_revisions`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the refused Down changed the revisions (%d, %v)", n, err)
	}
	// Rows go with their peer; then Down goes through.
	if err := exec(`DELETE FROM federation_peers WHERE id = $1`, peer); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 93); err != nil {
		t.Fatalf("down to 93: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_received_asset_revisions'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after Down the table remains (%d, %v)", n, err)
	}
	if _, err := p.UpTo(ctx, 94); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname = 'federation_received_asset_revisions_immutable_trg'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("after Up again the immutability trigger is missing (%d, %v)", n, err)
	}
}
