// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMigration00100_FederationMetadataProducers_UpDownUp (#1577): at 99
// an asset edit queues nothing; Up adds the five producers and ONE
// bootstrap 'all' row, and an edit then queues its asset in its own
// transaction (rolled back, nothing). Down removes only the producers:
// the pending work stays, and 00099's Down still refuses to discard it.
// Up again restores the producers and the bootstrap row.
func TestMigration00100_FederationMetadataProducers_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const producers = `SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgname LIKE 'federation_metadata_%'`
	const work = `SELECT count(*) FROM federation_metadata_work`

	if _, err := p.UpTo(ctx, 99); err != nil {
		t.Fatalf("up to 99: %v", err)
	}
	var owner int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO "user" (username, fullname, approved) VALUES ('mp-owner', 'Owner', 1) RETURNING ref`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	asset := uuid.NewString()
	exec(`INSERT INTO assets (id, title, asset_type, owner_user_ref, sensitivity) VALUES ($1, 'One', 1, $2, 'public')`, asset, owner)
	exec(`UPDATE assets SET title = 'Two' WHERE id = $1`, asset)
	if n := count(work); n != 0 {
		t.Fatalf("at 99 an edit queued %d rows", n)
	}

	if _, err := p.UpTo(ctx, 100); err != nil {
		t.Fatalf("up to 100: %v", err)
	}
	if n := count(producers); n != 5 {
		t.Fatalf("%d producers, want 5", n)
	}
	if n := count(`SELECT count(*) FROM federation_metadata_work WHERE kind = 'all' AND asset_cursor IS NULL`); n != 1 || count(work) != 1 {
		t.Fatalf("after Up: %d bootstrap rows of %d", n, count(work))
	}
	// SECURITY INVOKER, plain plpgsql.
	if n := count(`SELECT count(*) FROM pg_proc WHERE proname LIKE 'federation_metadata_%' AND NOT prosecdef`); n != 5 {
		t.Fatalf("%d invoker functions, want 5", n)
	}

	edit := func(title string, commit bool) {
		t.Helper()
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE assets SET title = $2 WHERE id = $1`, asset, title); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	edit("Rolled back", false)
	if n := count(`SELECT count(*) FROM federation_metadata_work WHERE asset_id = $1`, asset); n != 0 {
		t.Fatalf("a rolled-back edit queued %d rows", n)
	}
	edit("Three", true)
	if n := count(`SELECT count(*) FROM federation_metadata_work WHERE kind = 'asset' AND asset_id = $1`, asset); n != 1 {
		t.Fatalf("a committed edit queued %d rows", n)
	}

	// Down 100 removes the producers only; the work stays.
	if _, err := p.DownTo(ctx, 99); err != nil {
		t.Fatalf("down to 99: %v", err)
	}
	if n := count(producers); n != 0 {
		t.Fatalf("after Down %d producers remain", n)
	}
	if n := count(`SELECT count(*) FROM pg_proc WHERE proname LIKE 'federation_metadata_%'`); n != 0 {
		t.Fatalf("after Down %d producer functions remain", n)
	}
	if n := count(work); n != 2 {
		t.Fatalf("Down 100 changed the pending work (%d rows, want 2)", n)
	}
	edit("Four", true)
	if n := count(work); n != 2 {
		t.Fatalf("with no producers an edit queued work (%d rows)", n)
	}
	// 00099's Down still refuses to discard it.
	if _, err := p.DownTo(ctx, 98); err == nil || !strings.Contains(err.Error(), "metadata work is pending") {
		t.Fatalf("down to 98 with work: %v", err)
	}
	if n := count(work); n != 2 {
		t.Fatalf("the refused Down changed the work (%d)", n)
	}
	if v, err := p.GetDBVersion(ctx); err != nil || v != 99 {
		t.Fatalf("version %d, %v after the refused Down", v, err)
	}

	// Up again: the producers and one new bootstrap row.
	exec(`DELETE FROM federation_metadata_work`)
	if _, err := p.UpTo(ctx, 100); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if count(producers) != 5 || count(work) != 1 {
		t.Fatalf("after Up again: %d producers, %d rows", count(producers), count(work))
	}
}
