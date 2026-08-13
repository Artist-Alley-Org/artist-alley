// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1023 — the placeholder's owner name honours ADR 0024's opt-out.
//
// This is the reachable end of the defect, and it is worth being precise
// about why THIS surface. A restricted asset never reaches an anonymous
// caller through browse or /search: the EntityAsset predicate's anonymous
// branch floors those to `sensitivity='public'`, so the row is gone
// before any placeholder is built. It reaches them through a CONTAINER —
// a PUBLIC collection (or a public post) holding a restricted asset. The
// container is readable, the member is not, and #883's placeholder is
// what the caller gets: an id, a `restricted: true`, and the owner's
// name.
//
// That name was resolved by a hand-written SQL ladder in this file which
// never consulted `hide_from_anonymous`, so an owner who took the
// opt-out — whose profile 404s for that same caller — had their username
// handed out anyway, by a JOIN, on someone else's collection page.
//
// Two callers, one row, opposite verdicts. A test where both get the
// same answer would pass on "withhold the name from everybody", which
// breaks #881's request-access flow.
//
// Skips without AA_DB_PASSWORD.

package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	ooHiddenOwner  int64 = 10231001
	ooVisibleOwner int64 = 10231002
	ooStranger     int64 = 10231003
)

const (
	ooHiddenUsername  = "oo-opted-out"
	ooVisibleUsername = "oo-listed"
)

// ooSeed plants two owners — one who took the opt-out and one who did
// not — a restricted asset each, and one PUBLIC collection holding both.
// Neither owner has a `display_name`, so the ladder falls through to the
// username: the rung that actually leaked, and the one most users are on.
func ooSeed(t *testing.T, pool *pgxpool.Pool) (colID uuid.UUID, hiddenAsset, visibleAsset uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	for _, o := range []struct {
		ref      int64
		username string
		hidden   bool
	}{
		{ooHiddenOwner, ooHiddenUsername, true},
		{ooVisibleOwner, ooVisibleUsername, false},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO "user" (ref, username) VALUES ($1, $2)
			 ON CONFLICT (ref) DO UPDATE SET username = EXCLUDED.username`,
			o.ref, o.username); err != nil {
			t.Fatalf("seed user %d: %v", o.ref, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO user_profiles (user_ref, display_name, hide_from_anonymous)
			 VALUES ($1, '', $2)
			 ON CONFLICT (user_ref) DO UPDATE
			    SET display_name = EXCLUDED.display_name,
			        hide_from_anonymous = EXCLUDED.hide_from_anonymous`,
			o.ref, o.hidden); err != nil {
			t.Fatalf("seed profile %d: %v", o.ref, err)
		}
	}

	asset := func(owner int64, title string) uuid.UUID {
		id := uuid.New()
		sum := sha256.Sum256(id[:])
		hash := hex.EncodeToString(sum[:])
		if _, err := pool.Exec(ctx,
			`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 11, 'fs')
			 ON CONFLICT (hash) DO NOTHING`, hash); err != nil {
			t.Fatalf("seed object: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, owner_user_ref, asset_type, status,
			                    sensitivity, processing_status, file_hash, file_extension)
			VALUES ($1,$2,$3,(SELECT MIN(ref) FROM asset_types),'active','restricted','ready',$4,'png')`,
			id, title, owner, hash); err != nil {
			t.Fatalf("seed asset %s: %v", title, err)
		}
		return id
	}
	hiddenAsset = asset(ooHiddenOwner, "oo hidden-owner asset")
	visibleAsset = asset(ooVisibleOwner, "oo listed-owner asset")

	colID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO collections (id, name, owner_user_ref, visibility) VALUES ($1, $2, $3, 'public')`,
		colID, "oo public collection", ooVisibleOwner); err != nil {
		t.Fatalf("seed collection: %v", err)
	}
	for i, m := range []uuid.UUID{hiddenAsset, visibleAsset} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO collection_resources (collection_id, asset_id, sort_order, pinned)
			 VALUES ($1, $2, $3, TRUE)`, colID, m, i); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}

	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM collection_resources WHERE collection_id = $1`, colID)
		_, _ = pool.Exec(c, `DELETE FROM collections WHERE id = $1`, colID)
		_, _ = pool.Exec(c, `DELETE FROM assets WHERE id = ANY($1::uuid[])`,
			[]uuid.UUID{hiddenAsset, visibleAsset})
		_, _ = pool.Exec(c, `DELETE FROM user_profiles WHERE user_ref = ANY($1::BIGINT[])`,
			[]int64{ooHiddenOwner, ooVisibleOwner})
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE ref = ANY($1::BIGINT[])`,
			[]int64{ooHiddenOwner, ooVisibleOwner})
	})
	return colID, hiddenAsset, visibleAsset
}

// ooNames lists the collection for one caller and returns
// asset_id -> owner_display_name, asserting that both members really did
// come back as PLACEHOLDERS. Without that check a change that dropped
// restricted members entirely would satisfy every absence below.
func ooNames(t *testing.T, pool *pgxpool.Pool, colID uuid.UUID, caller visibility.Caller) map[uuid.UUID]string {
	t.Helper()
	rows, err := ListCollectionResourcesPageGated(context.Background(), pool, caller, nil,
		ListCollectionResourcesPageGatedParams{
			CollectionID: pgtype.UUID{Bytes: colID, Valid: true},
			RowLimit:     50,
		})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := make(map[uuid.UUID]string, len(rows))
	for _, r := range rows {
		if !r.Restricted {
			t.Fatalf("member %v came back READABLE — the fixture is not exercising the placeholder "+
				"path and every assertion here is vacuous", uuid.UUID(r.AssetID.Bytes))
		}
		out[uuid.UUID(r.AssetID.Bytes)] = r.OwnerDisplayName
	}
	if len(out) != 2 {
		t.Fatalf("got %d members, want 2 — the fixture did not take", len(out))
	}
	return out
}

func TestCollectionPlaceholder_OwnerOptOut(t *testing.T) {
	pool := maPool(t)
	colID, hiddenAsset, visibleAsset := ooSeed(t, pool)

	anon := ooNames(t, pool, colID, visibility.NewCaller(nil))
	if got := anon[hiddenAsset]; got != "" {
		t.Errorf("anonymous caller got owner_display_name %q for an owner who set "+
			"hide_from_anonymous. ADR 0024's opt-out 404s their profile for this caller; a "+
			"placeholder must not hand out the same identity by a JOIN (#1023)", got)
	}
	// The non-vacuity control on the SAME response: an owner who did not
	// opt out is still named, or the fix is just "withhold everything"
	// and #881's request-access has nothing to address.
	if got := anon[visibleAsset]; got != ooVisibleUsername {
		t.Errorf("anonymous caller got owner_display_name %q for an owner who did NOT opt out, "+
			"want %q", got, ooVisibleUsername)
	}

	// The other verdict, same row. The opt-out is anonymous-only
	// (ADR 0070 §3) and must not withhold from a signed-in caller.
	strangerRef := ooStranger
	auth := ooNames(t, pool, colID, visibility.NewCaller(&strangerRef))
	if got := auth[hiddenAsset]; got != ooHiddenUsername {
		t.Errorf("authenticated caller got owner_display_name %q for an opted-out owner, want %q — "+
			"hide_from_anonymous withholds from ANONYMOUS callers only", got, ooHiddenUsername)
	}
	if got := auth[visibleAsset]; got != ooVisibleUsername {
		t.Errorf("authenticated caller got owner_display_name %q, want %q", got, ooVisibleUsername)
	}
}

// TestCollectionPlaceholder_FullnameRungIsAuthenticatedOnly pins the
// other rung the hand-written ladder had dropped: `fullname` is rung 2 of
// users.ResolveDisplayName and is AUTHENTICATED-ONLY (ADR 0070 §3).
//
// The old SQL skipped it entirely, so a signed-in caller saw a different
// name on a placeholder than on the same user's post header. Adopting it
// is a widening for authenticated callers and must NOT extend to
// anonymous ones — getting that backwards leaks the real name of every
// user who never set a display name, which is most of them.
func TestCollectionPlaceholder_FullnameRungIsAuthenticatedOnly(t *testing.T) {
	pool := maPool(t)
	colID, _, visibleAsset := ooSeed(t, pool)

	const real = "Wilhelmina Fensterwald"
	if _, err := pool.Exec(context.Background(),
		`UPDATE "user" SET fullname = $1 WHERE ref = $2`, real, ooVisibleOwner); err != nil {
		t.Fatalf("set fullname: %v", err)
	}

	if got := ooNames(t, pool, colID, visibility.NewCaller(nil))[visibleAsset]; got != ooVisibleUsername {
		t.Errorf("anonymous caller got %q, want the username %q — rung 2 is authenticated-only and "+
			"an anonymous ladder goes display_name → username (ADR 0070 §3)", got, ooVisibleUsername)
	}
	strangerRef := ooStranger
	if got := ooNames(t, pool, colID, visibility.NewCaller(&strangerRef))[visibleAsset]; got != real {
		t.Errorf("authenticated caller got %q, want the fullname %q — the placeholder now names its "+
			"owner the same way every other surface does", got, real)
	}
}
