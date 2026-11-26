// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1597: migration 00103 adds federation_inbound_grants.allow_access_copies,
// the receiver's record of a peer's access-copy grant. Pinned here against
// a real, fresh database:
//
//   - grants recorded before the migration, with and without
//     allow_original, read false after it, with allow_original unchanged;
//   - a new row that does not mention the column is false;
//   - only asset and collection grants may carry it (the CHECK);
//   - Down removes only the new column and its CHECK (allow_original and
//     its CHECK stay, with their values), and Up restores them.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	inboundAccessBeforeVersion = 102 // 00102_federation_share_allow_access_copies
	inboundAccessAtVersion     = 103 // 00103_federation_inbound_grant_allow_access_copies
)

func TestMigration00103_InboundGrantAllowAccessCopies_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.UpTo(ctx, inboundAccessBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", inboundAccessBeforeVersion, err)
	}
	if columnExists(t, sqlDB, "federation_inbound_grants", "allow_access_copies") {
		t.Fatal("federation_inbound_grants.allow_access_copies exists before 00103")
	}

	var userRef int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ('m103-user', 'M103', 1) RETURNING ref`,
	).Scan(&userRef); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	var peerID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO federation_peers (instance_url, display_name, instance_public_key, handshake_by_user_ref)
		 VALUES ('https://m103-peer.example', 'M103 peer', 'k', $1) RETURNING id`, userRef,
	).Scan(&peerID); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	n := 0
	insertGrant := func(kind string, allowOriginal bool) string {
		t.Helper()
		n++
		var id string
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO federation_inbound_grants (peer_id, grant_activity_uri, grantor_actor_uri, object_kind, object_uri,
			    scope, allow_original, state)
			 VALUES ($1, $2, 'https://m103-peer.example/users/a', $3, $4, 'remix', $5, 'live') RETURNING id`,
			peerID, "https://m103-peer.example/activities/"+string(rune('a'+n)), kind,
			"https://m103-peer.example/assets/"+string(rune('a'+n)), allowOriginal,
		).Scan(&id); err != nil {
			t.Fatalf("insert %s grant: %v", kind, err)
		}
		return id
	}
	oldOriginal := insertGrant("asset", true)
	oldPlain := insertGrant("collection", false)
	oldPost := insertGrant("post", false)

	readFlags := func(id string) (original, access bool) {
		t.Helper()
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT allow_original, allow_access_copies FROM federation_inbound_grants WHERE id = $1`, id).Scan(&original, &access); err != nil {
			t.Fatalf("read flags: %v", err)
		}
		return original, access
	}
	readOriginal := func(id string) bool {
		t.Helper()
		var v bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT allow_original FROM federation_inbound_grants WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatalf("read allow_original: %v", err)
		}
		return v
	}

	// Up.
	if _, err := p.UpTo(ctx, inboundAccessAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", inboundAccessAtVersion, err)
	}
	for _, c := range []struct {
		name         string
		id           string
		wantOriginal bool
	}{{"original asset grant", oldOriginal, true}, {"plain collection grant", oldPlain, false}, {"post grant", oldPost, false}} {
		original, access := readFlags(c.id)
		if access {
			t.Errorf("%s recorded before 00103 reads allow_access_copies = true", c.name)
		}
		if original != c.wantOriginal {
			t.Errorf("%s: allow_original = %v after 00103, want %v", c.name, original, c.wantOriginal)
		}
	}
	var nullable, def string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT is_nullable, COALESCE(column_default, '') FROM information_schema.columns
		  WHERE table_name = 'federation_inbound_grants' AND column_name = 'allow_access_copies'`,
	).Scan(&nullable, &def); err != nil {
		t.Fatalf("read column: %v", err)
	}
	if nullable != "NO" || def != "false" {
		t.Errorf("allow_access_copies nullable=%s default=%q, want NO / false", nullable, def)
	}
	if _, access := readFlags(insertGrant("asset", true)); access {
		t.Error("a new grant without allow_access_copies reads true")
	}

	// The CHECK: only asset and collection grants may carry it, with or
	// without allow_original.
	setFlag := func(kind string) error {
		n++
		_, err := sqlDB.ExecContext(ctx,
			`INSERT INTO federation_inbound_grants (peer_id, grant_activity_uri, grantor_actor_uri, object_kind, object_uri,
			    scope, allow_access_copies, state)
			 VALUES ($1, $2, 'https://m103-peer.example/users/a', $3, $4, 'view', TRUE, 'live')`,
			peerID, "https://m103-peer.example/activities/x"+string(rune('a'+n)), kind,
			"https://m103-peer.example/objects/x"+string(rune('a'+n)))
		return err
	}
	for _, kind := range []string{"asset", "collection"} {
		if err := setFlag(kind); err != nil {
			t.Errorf("allow_access_copies on a %s grant refused: %v", kind, err)
		}
	}
	for _, kind := range []string{"post", "workspace", "brand_kit", "user"} {
		err := setFlag(kind)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "federation_inbound_grants_allow_access_copies_kind_check" {
			t.Errorf("allow_access_copies on a %s grant: err = %v, want the kind CHECK violation", kind, err)
		}
	}

	// Down removes only the new column and CHECK; Up restores them.
	if _, err := p.DownTo(ctx, inboundAccessBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", inboundAccessBeforeVersion, err)
	}
	if columnExists(t, sqlDB, "federation_inbound_grants", "allow_access_copies") ||
		constraintExists(t, sqlDB, "federation_inbound_grants_allow_access_copies_kind_check") {
		t.Error("00103 Down left the column or its CHECK")
	}
	if !columnExists(t, sqlDB, "federation_inbound_grants", "allow_original") ||
		!constraintExists(t, sqlDB, "federation_inbound_grants_allow_original_kind_check") {
		t.Error("00103 Down removed allow_original or its CHECK")
	}
	if !readOriginal(oldOriginal) || readOriginal(oldPlain) {
		t.Error("00103 Down changed an existing allow_original value")
	}
	if _, err := p.UpTo(ctx, inboundAccessAtVersion); err != nil {
		t.Fatalf("re-apply 00103: %v", err)
	}
	if !columnExists(t, sqlDB, "federation_inbound_grants", "allow_access_copies") ||
		!constraintExists(t, sqlDB, "federation_inbound_grants_allow_access_copies_kind_check") {
		t.Error("00103 Up after Down did not restore the column and its CHECK")
	}
	if original, access := readFlags(oldOriginal); !original || access {
		t.Errorf("after 00103 Down and Up: allow_original=%v allow_access_copies=%v, want true / false", original, access)
	}
}
