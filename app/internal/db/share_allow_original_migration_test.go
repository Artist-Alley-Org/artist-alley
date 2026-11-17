// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1573: migration 00075 adds federation_shares.allow_original, the
// explicit original-byte grant. Pinned here against a real, fresh
// database:
//
//   - a share that existed before the migration reads false after it
//     (no earlier grant ever gave original bytes, and none does now);
//   - a new row that does not mention the column is false;
//   - only asset and collection shares may set it (the CHECK);
//   - Down removes both the column and the constraint, and Up again
//     restores them.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	shareAllowOriginalBeforeVersion = 74 // 00074_remote_actor_signing_key
	shareAllowOriginalAtVersion     = 75 // 00075_federation_share_allow_original
)

func TestMigration00075_ShareAllowOriginal_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.UpTo(ctx, shareAllowOriginalBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", shareAllowOriginalBeforeVersion, err)
	}
	if columnExists(t, sqlDB, "federation_shares", "allow_original") {
		t.Fatal("federation_shares.allow_original exists before 00075")
	}

	// A share made before the column existed.
	var userRef int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ('m75-grantor', 'M75', 1) RETURNING ref`,
	).Scan(&userRef); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var peerID, activityID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, handshake_by_user_ref)
		 VALUES ('https://m75-peer.example', 'M75 peer', 'k', $1) RETURNING id`, userRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source)
		 VALUES ('https://m75.example/activities/1', 'aa:Share', 'https://m75.example/users/m75-grantor', $1, '{}'::jsonb, 'local')
		 RETURNING id`, userRef,
	).Scan(&activityID); err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	insertShare := func(kind string) string {
		t.Helper()
		var id string
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id)
			 VALUES ($1, $2, gen_random_uuid(), $3, 'remix', $4) RETURNING id`,
			userRef, kind, peerID, activityID,
		).Scan(&id); err != nil {
			t.Fatalf("insert %s share: %v", kind, err)
		}
		return id
	}
	oldAsset := insertShare("asset")
	oldPost := insertShare("post")

	// Up.
	if _, err := p.UpTo(ctx, shareAllowOriginalAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", shareAllowOriginalAtVersion, err)
	}
	readFlag := func(id string) bool {
		t.Helper()
		var v bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT allow_original FROM federation_shares WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("read allow_original: %v", err)
		}
		return v
	}
	if readFlag(oldAsset) || readFlag(oldPost) {
		t.Fatal("a share made before 00075 reads allow_original = true; every existing grant must stay false")
	}

	// Column shape: NOT NULL, default false.
	var nullable, def string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT is_nullable, COALESCE(column_default, '') FROM information_schema.columns
		  WHERE table_name = 'federation_shares' AND column_name = 'allow_original'`,
	).Scan(&nullable, &def); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if nullable != "NO" || def != "false" {
		t.Errorf("allow_original nullable=%s default=%q, want NO / false", nullable, def)
	}

	// A new row that does not mention the column is false (the
	// remix share is the strongest scope: scope never implies it).
	if readFlag(insertShare("asset")) {
		t.Error("a new share without allow_original reads true")
	}

	// The CHECK: only asset and collection shares may set it.
	setFlag := func(kind string) error {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			 VALUES ($1, $2, gen_random_uuid(), $3, 'view', $4, TRUE)`,
			userRef, kind, peerID, activityID)
		return err
	}
	for _, kind := range []string{"asset", "collection"} {
		if err := setFlag(kind); err != nil {
			t.Errorf("allow_original on a %s share refused: %v", kind, err)
		}
	}
	for _, kind := range []string{"post", "workspace", "brand_kit", "user"} {
		err := setFlag(kind)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "federation_shares_allow_original_kind_check" {
			t.Errorf("allow_original on a %s share: err = %v, want the allow_original kind CHECK violation", kind, err)
		}
	}

	// Down removes column and constraint; Up restores them.
	if _, err := p.DownTo(ctx, shareAllowOriginalBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", shareAllowOriginalBeforeVersion, err)
	}
	if columnExists(t, sqlDB, "federation_shares", "allow_original") {
		t.Error("allow_original survived 00075 Down")
	}
	if constraintExists(t, sqlDB, "federation_shares_allow_original_kind_check") {
		t.Error("the allow_original kind CHECK survived 00075 Down")
	}
	if _, err := p.UpTo(ctx, shareAllowOriginalAtVersion); err != nil {
		t.Fatalf("re-apply 00075: %v", err)
	}
	if !columnExists(t, sqlDB, "federation_shares", "allow_original") ||
		!constraintExists(t, sqlDB, "federation_shares_allow_original_kind_check") {
		t.Error("00075 Up after Down did not restore the column and its CHECK")
	}
}

func constraintExists(t *testing.T, sqlDB *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(t.Context(),
		`SELECT count(*) FROM pg_constraint WHERE conname = $1`, name).Scan(&n); err != nil {
		t.Fatalf("read constraint %s: %v", name, err)
	}
	return n > 0
}
