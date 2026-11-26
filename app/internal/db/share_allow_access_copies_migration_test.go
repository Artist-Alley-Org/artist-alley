// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: migration 00102 adds federation_shares.allow_access_copies, the
// explicit access-copy grant. Pinned here against a real, fresh
// database:
//
//   - shares that existed before the migration, with and without the
//     original-file grant, read false after it (no earlier grant ever gave
//     access copies, and the original-file grant does not imply one);
//   - a new row that does not mention the column is false;
//   - only asset and collection shares may set it (the CHECK);
//   - Down removes the column and the constraint, Up restores them, and
//     neither step changes an existing original-file grant.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	shareAllowAccessCopiesBeforeVersion = 101 // 00101_federation_dispatch_work
	shareAllowAccessCopiesAtVersion     = 102 // 00102_federation_share_allow_access_copies
)

func TestMigration00102_ShareAllowAccessCopies_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.UpTo(ctx, shareAllowAccessCopiesBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", shareAllowAccessCopiesBeforeVersion, err)
	}
	if columnExists(t, sqlDB, "federation_shares", "allow_access_copies") {
		t.Fatal("federation_shares.allow_access_copies exists before 00102")
	}

	var userRef int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ('m102-grantor', 'M102', 1) RETURNING ref`,
	).Scan(&userRef); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var peerID, activityID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, handshake_by_user_ref)
		 VALUES ('https://m102-peer.example', 'M102 peer', 'k', $1) RETURNING id`, userRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO activities (activity_uri, activity_type, actor_uri, actor_user_ref, payload, source)
		 VALUES ('https://m102.example/activities/1', 'aa:Share', 'https://m102.example/users/m102-grantor', $1, '{}'::jsonb, 'local')
		 RETURNING id`, userRef,
	).Scan(&activityID); err != nil {
		t.Fatalf("insert activity: %v", err)
	}
	insertShare := func(kind string, allowOriginal bool) string {
		t.Helper()
		var id string
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_original)
			 VALUES ($1, $2, gen_random_uuid(), $3, 'remix', $4, $5) RETURNING id`,
			userRef, kind, peerID, activityID, allowOriginal,
		).Scan(&id); err != nil {
			t.Fatalf("insert %s share: %v", kind, err)
		}
		return id
	}
	// Shares made before the column existed: with the original-file grant,
	// without it, and on a kind that can carry neither.
	oldOriginal := insertShare("asset", true)
	oldPlain := insertShare("collection", false)
	oldPost := insertShare("post", false)

	readFlags := func(id string) (original, access bool) {
		t.Helper()
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT allow_original, allow_access_copies FROM federation_shares WHERE id = $1`, id).Scan(&original, &access); err != nil {
			t.Fatalf("read flags: %v", err)
		}
		return original, access
	}
	readOriginal := func(id string) bool {
		t.Helper()
		var v bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT allow_original FROM federation_shares WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("read allow_original: %v", err)
		}
		return v
	}

	// Up.
	if _, err := p.UpTo(ctx, shareAllowAccessCopiesAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", shareAllowAccessCopiesAtVersion, err)
	}
	for _, c := range []struct {
		name         string
		id           string
		wantOriginal bool
	}{{"original-granted asset share", oldOriginal, true}, {"plain collection share", oldPlain, false}, {"post share", oldPost, false}} {
		original, access := readFlags(c.id)
		if access {
			t.Errorf("%s made before 00102 reads allow_access_copies = true; every existing grant must stay false", c.name)
		}
		if original != c.wantOriginal {
			t.Errorf("%s: allow_original = %v after 00102, want %v (unchanged)", c.name, original, c.wantOriginal)
		}
	}

	// Column shape: NOT NULL, default false.
	var nullable, def string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT is_nullable, COALESCE(column_default, '') FROM information_schema.columns
		  WHERE table_name = 'federation_shares' AND column_name = 'allow_access_copies'`,
	).Scan(&nullable, &def); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if nullable != "NO" || def != "false" {
		t.Errorf("allow_access_copies nullable=%s default=%q, want NO / false", nullable, def)
	}

	// New rows that do not mention the column are false, with or without
	// the original-file grant (remix is the strongest scope: scope never
	// implies it, and neither does allow_original).
	if _, access := readFlags(insertShare("asset", true)); access {
		t.Error("a new original-granted share without allow_access_copies reads true")
	}
	if _, access := readFlags(insertShare("asset", false)); access {
		t.Error("a new share without allow_access_copies reads true")
	}

	// The CHECK: only asset and collection shares may set it, and setting
	// it needs no original-file grant.
	setFlag := func(kind string) error {
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO federation_shares (grantor_user_ref, object_kind, object_id, peer_id, scope, granted_activity_id, allow_access_copies)
			 VALUES ($1, $2, gen_random_uuid(), $3, 'view', $4, TRUE)`,
			userRef, kind, peerID, activityID)
		return err
	}
	for _, kind := range []string{"asset", "collection"} {
		if err := setFlag(kind); err != nil {
			t.Errorf("allow_access_copies on a %s share refused: %v", kind, err)
		}
	}
	for _, kind := range []string{"post", "workspace", "brand_kit", "user"} {
		err := setFlag(kind)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "federation_shares_allow_access_copies_kind_check" {
			t.Errorf("allow_access_copies on a %s share: err = %v, want the allow_access_copies kind CHECK violation", kind, err)
		}
	}

	// Down removes column and constraint, leaving the original-file grant;
	// Up restores them, again leaving it.
	if _, err := p.DownTo(ctx, shareAllowAccessCopiesBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", shareAllowAccessCopiesBeforeVersion, err)
	}
	if columnExists(t, sqlDB, "federation_shares", "allow_access_copies") {
		t.Error("allow_access_copies survived 00102 Down")
	}
	if constraintExists(t, sqlDB, "federation_shares_allow_access_copies_kind_check") {
		t.Error("the allow_access_copies kind CHECK survived 00102 Down")
	}
	if !columnExists(t, sqlDB, "federation_shares", "allow_original") ||
		!constraintExists(t, sqlDB, "federation_shares_allow_original_kind_check") {
		t.Error("00102 Down removed the original-file grant column or its CHECK")
	}
	if !readOriginal(oldOriginal) || readOriginal(oldPlain) {
		t.Error("00102 Down changed an existing original-file grant")
	}
	if _, err := p.UpTo(ctx, shareAllowAccessCopiesAtVersion); err != nil {
		t.Fatalf("re-apply 00102: %v", err)
	}
	if !columnExists(t, sqlDB, "federation_shares", "allow_access_copies") ||
		!constraintExists(t, sqlDB, "federation_shares_allow_access_copies_kind_check") {
		t.Error("00102 Up after Down did not restore the column and its CHECK")
	}
	if original, access := readFlags(oldOriginal); !original || access {
		t.Errorf("after 00102 Down and Up, the original-granted share reads allow_original=%v allow_access_copies=%v, want true / false", original, access)
	}
}
