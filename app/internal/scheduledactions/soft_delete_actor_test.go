// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #931 — the scheduled `delete` action is the SECOND soft-delete path
// for assets. The interactive one (assets.Handler.DeleteAsset) is the
// obvious one; this executor is the one an audit of "who deletes
// assets" misses, and a row it removed with deleted_by_user_ref NULL
// would be un-restorable by the person who scheduled it.

package scheduledactions

import (
	"context"
	"testing"
	"time"
)

// A user-scheduled delete records the user who scheduled it, so that
// user can undo it through the restore gate.
func TestReaper_SoftDelete_RecordsSchedulingUserAsDeleter(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "public")

	var actor int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO "user" (username, approved) VALUES ('sa-deleter-931', 1)
		 ON CONFLICT (username) DO UPDATE SET approved = 1
		 RETURNING ref`,
	).Scan(&actor); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, actor)
	})

	schedule(t, store, ScheduleInput{
		Action: ActionDelete, TargetKind: TargetAsset, TargetID: asset.String(),
		ScheduledFor: at(-time.Second), CreatedBy: &actor,
	})

	drain(t, newReaper(t, pool, &fakeNotifier{}))

	var deletedAt *time.Time
	var deletedBy *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT deleted_at, deleted_by_user_ref FROM assets WHERE id = $1`, asset,
	).Scan(&deletedAt, &deletedBy); err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("scheduled delete did not soft-delete the asset")
	}
	if deletedBy == nil || *deletedBy != actor {
		t.Errorf("deleted_by_user_ref = %v, want %d — a scheduled delete must record who scheduled it, "+
			"or the person who asked for it cannot undo it", deletedBy, actor)
	}
}

// A SYSTEM-scheduled delete has no human behind it (created_by is
// nullable), and must leave deleted_by_user_ref NULL rather than
// inventing an actor. NULL fails closed at the restore gate: only
// system.admin can undo it.
func TestReaper_SoftDelete_SystemScheduledLeavesDeleterNull(t *testing.T) {
	pool := saPool(t)
	store := NewStore(pool)
	asset := seedAsset(t, pool, "public")

	schedule(t, store, ScheduleInput{
		Action: ActionDelete, TargetKind: TargetAsset, TargetID: asset.String(),
		ScheduledFor: at(-time.Second), CreatedBy: nil,
	})

	drain(t, newReaper(t, pool, &fakeNotifier{}))

	var deletedAt *time.Time
	var deletedBy *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT deleted_at, deleted_by_user_ref FROM assets WHERE id = $1`, asset,
	).Scan(&deletedAt, &deletedBy); err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("scheduled delete did not soft-delete the asset")
	}
	if deletedBy != nil {
		t.Errorf("deleted_by_user_ref = %d, want NULL — a system-scheduled delete has no actor "+
			"and must not claim one", *deletedBy)
	}
}
