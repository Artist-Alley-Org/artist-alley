// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #931 — a collection's owner could delete it and then not get it
// back: DeleteCollection gated on canMutateCollection, RestoreCollection
// on system.admin alone. Restoring now keys on WHO deleted it.
//
// Skips without AA_DB_PASSWORD.

package collections

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

func TestCanRestoreCollection_DeleterOrSystemAdminOnly(t *testing.T) {
	owner := &auth.Identity{UserRef: 7001, AuthMethod: "session"}
	admin := &auth.Identity{
		UserRef: 7002, AuthMethod: "session",
		Capabilities: []string{CapSystemAdmin},
	}
	ownerRef := int64(7001)
	otherRef := int64(7003)

	if !auth.CanRestoreDeleted(owner, &ownerRef) {
		t.Error("you must be able to undo your own delete")
	}
	if auth.CanRestoreDeleted(owner, &otherRef) {
		t.Error("you must NOT be able to undo someone else's delete")
	}
	if auth.CanRestoreDeleted(owner, nil) {
		t.Error("a NULL deleter must fail closed, not open")
	}
	if !auth.CanRestoreDeleted(admin, nil) {
		t.Error("system.admin must be able to restore a row with no recorded deleter")
	}
	if !auth.CanRestoreDeleted(admin, &otherRef) {
		t.Error("system.admin must be able to restore anything")
	}

	anon := &auth.Identity{UserRef: 0, AuthMethod: "anonymous"}
	var zero int64
	if auth.CanRestoreDeleted(anon, &zero) {
		t.Error("an anonymous caller must not be matched against a ref-0 deleter")
	}
	if auth.CanRestoreDeleted(nil, &ownerRef) {
		t.Error("a nil identity must never be authorised")
	}
}

// The soft-delete query must persist the deleter — the gate above
// decides nothing if the column is never written.
func TestDeleteCollection_WritesDeletedBy(t *testing.T) {
	pool := listCollPool(t)
	ctx := context.Background()

	var owner int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
		"cr-owner-"+uuid.NewString()[:8],
	).Scan(&owner); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE ref = $1`, owner) })

	var id pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO collections (owner_user_ref, name, description, visibility, membership)
		 VALUES ($1, 'cr-delete-931', '', 'private', 'manual') RETURNING id`,
		owner,
	).Scan(&id); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id) })

	if err := New(pool).DeleteCollection(ctx, DeleteCollectionParams{
		ID:               id,
		DeletedByUserRef: &owner,
	}); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	var deletedBy *int64
	if err := pool.QueryRow(ctx,
		`SELECT deleted_by_user_ref FROM collections WHERE id = $1`, id,
	).Scan(&deletedBy); err != nil {
		t.Fatalf("read deleted_by_user_ref: %v", err)
	}
	if deletedBy == nil || *deletedBy != owner {
		t.Errorf("deleted_by_user_ref = %v, want %d", deletedBy, owner)
	}
}
