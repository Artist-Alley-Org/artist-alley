// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestMigration00099_FederationMetadataWork_UpDownUp (#1577): Up keeps
// metadata work to its two legal shapes, with no foreign key, so a row
// outlives its asset and a peer cursor names no row; Down refuses while
// work is pending, changing nothing, and drops the table once none is
// left; Up again restores it.
func TestMigration00099_FederationMetadataWork_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 99); err != nil {
		t.Fatalf("up to 99: %v", err)
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
	work := func(kind string, asset, assetCursor, peerCursor any) error {
		return exec(`INSERT INTO federation_metadata_work (kind, asset_id, asset_cursor, peer_cursor) VALUES ($1, $2, $3, $4)`,
			kind, asset, assetCursor, peerCursor)
	}
	a, c := uuid.NewString(), uuid.NewString()
	for name, err := range map[string]error{
		"an asset row":                    work("asset", a, nil, nil),
		"an asset row with a peer cursor": work("asset", a, nil, c),
		"an all row":                      work("all", nil, nil, nil),
		"an all row with an asset cursor": work("all", nil, c, nil),
	} {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, err := range map[string]error{
		"an asset row without an asset":     work("asset", nil, nil, nil),
		"an asset row with an asset cursor": work("asset", a, c, nil),
		"an all row naming an asset":        work("all", a, nil, nil),
		"an all row with a peer cursor":     work("all", nil, nil, c),
		"another kind":                      work("peer", a, nil, nil),
		"no kind":                           work("", a, nil, nil),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// No foreign keys: the asset and the cursors name nothing that exists.
	if n := count(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.federation_metadata_work'::regclass AND contype = 'f'`); n != 0 {
		t.Fatalf("%d foreign keys", n)
	}
	if n := count(`SELECT count(*) FROM pg_indexes WHERE indexname = 'federation_metadata_work_oldest_idx'`); n != 1 {
		t.Fatalf("the oldest-work index is missing (%d)", n)
	}
	if n := count(`SELECT count(*) FROM federation_metadata_work`); n != 4 {
		t.Fatalf("%d rows, want 4", n)
	}

	// Down refuses while work is pending, and changes nothing.
	if _, err := p.DownTo(ctx, 98); err == nil || !strings.Contains(err.Error(), "metadata work is pending") {
		t.Fatalf("down to 98 with work: %v", err)
	}
	if n := count(`SELECT count(*) FROM federation_metadata_work`); n != 4 {
		t.Fatalf("the refused Down changed the work (%d)", n)
	}
	if err := exec(`DELETE FROM federation_metadata_work`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 98); err != nil {
		t.Fatalf("down to 98: %v", err)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_metadata_work'`); n != 0 {
		t.Fatalf("after Down the table remains (%d)", n)
	}
	// 00098 is untouched by this Down.
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name = 'federation_received_asset_metadata'`); n != 1 {
		t.Fatalf("Down 99 touched 00098 (%d)", n)
	}
	if _, err := p.UpTo(ctx, 99); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := count(`SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.federation_metadata_work'::regclass`); n != 3 {
		t.Fatalf("after Up again %d constraints, want 3", n)
	}
}

// TestMigration00099_DownDoesNotDiscardConcurrentWork (#1577): Down must
// not miss an obligation another transaction is still adding. An enqueue
// holds an uncommitted row while Down runs on its own connection; Down is
// observed BLOCKED on the work table by that enqueue (pg_locks and
// pg_blocking_pids, not timing). Once the enqueue commits, Down must
// refuse, leaving the table, the committed row and version 99 in place,
// rather than check an empty-looking table and then drop the new row.
func TestMigration00099_DownDoesNotDiscardConcurrentWork(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, 99); err != nil {
		t.Fatalf("up to 99: %v", err)
	}

	// The enqueue: one row, not yet committed, on its own connection.
	insConn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer insConn.Close()
	tx, err := insConn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op once committed
	var insPID int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&insPID); err != nil {
		t.Fatal(err)
	}
	var workID string
	if err := tx.QueryRowContext(ctx, `INSERT INTO federation_metadata_work (kind, asset_id) VALUES ('asset', $1) RETURNING id`,
		uuid.NewString()).Scan(&workID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Down, through a separate pool and provider.
	downDB := openCfg(t, cfg)
	pd := afvhProvider(t, downDB)
	downErr := make(chan error, 1)
	go func() {
		_, err := pd.DownTo(ctx, 98)
		downErr <- err
	}()

	// Observe Down waiting for a lock on the work table that the enqueue
	// holds. Bounded: a Down that never waits fails the test promptly.
	blocked := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		select {
		case err := <-downErr:
			t.Fatalf("Down returned while the enqueue was uncommitted (err %v): it did not wait for the enqueue", err)
		default:
		}
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks l
			WHERE l.locktype = 'relation' AND NOT l.granted
			  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND l.relation = 'public.federation_metadata_work'::regclass
			  AND $1 = ANY (pg_blocking_pids(l.pid))`, insPID).Scan(&n); err != nil {
			t.Fatalf("read pg_locks: %v", err)
		}
		if n == 1 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("Down never blocked on the work table behind the enqueue; the overlap this test asserts never happened")
	}
	t.Log("Down is observed blocked on federation_metadata_work by the uncommitted enqueue")

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the enqueue: %v", err)
	}
	select {
	case err = <-downErr:
	case <-ctx.Done():
		t.Fatal("Down never returned after the enqueue committed")
	}
	if err == nil || !strings.Contains(err.Error(), "metadata work is pending") {
		t.Errorf("Down with concurrently committed work: %v, want a refusal", err)
	}
	var exists bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public.federation_metadata_work') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("Down dropped the work table and the committed obligation with it")
	}
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM federation_metadata_work WHERE id = $1`, workID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the committed obligation is gone (%d rows)", n)
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != 99 {
		t.Fatalf("migration version %d after the refused Down, want 99", v)
	}
}
