// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1117 — the ROW plane on asset browse (ADR 0090 §3).
//
// # The gap this closes, stated plainly
//
// #1115 built the predicate and #1143 gave it callers on the post feed
// and the content plane. `GET /assets` got neither. So a disqualified
// viewer's browse feed correctly hid a mature POST while the asset
// browse listed the very same asset in full — the row, its title, its
// thumbhash. ADR 0090 §3 says the row plane means "the browse feed does
// not RETURN a disqualified viewer's mature rows", and it did.
//
// # Why the conjunct is UNCONDITIONAL here, and what that test proves
//
// Every other readability conjunct on this query lives inside the `?q=`
// branch, because ADR 0064 requires browse to keep LISTING a restricted
// asset as a placeholder — #881's request-access flow hangs off those
// placeholders. Mature is different: there is nothing to request, only a
// preference to change, and #921 measured what the placeholder
// alternative looks like (a feed of blurred plates nobody asked to be
// offered). So the mature row is ABSENT.
//
// The second arm below is the one that would have caught the plausible
// wrong fix. Folding the conjunct into AssetSearchMatchSQL — the tidier
// diff, and the one that keeps the rule in a single named function —
// puts it inside `($4::TEXT IS NULL OR …)`, so it applies only when the
// caller typed something. That version passes a `?q=` test and leaves
// unfiltered browse leaking, which is the shape this file exists to
// refuse.
//
// Skips without AA_DB_PASSWORD.

package assets

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/testdb"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

const (
	mrpOwner    int64 = 11170101 // owns BOTH assets, so ownership cannot explain a verdict
	mrpStranger int64 = 11170102 // signed in, no relationship, no capabilities
)

const mrpToken = "quibblethornmarsh"

func mrpPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	env := func(k, def string) string {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			return v
		}
		return def
	}
	dsn := "host=" + env("AA_DB_HOST", "postgres") +
		" port=" + env("AA_DB_PORT", "5432") +
		" user=" + env("AA_DB_USER", "artist_alley") +
		" dbname=" + testdb.Name(t) +
		" sslmode=disable password=" + pwd
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mrpSeed plants the pair: two PUBLIC, ACTIVE assets with the same owner
// and the same token, differing only in `mature`.
//
// Public and active deliberately — the mature axis has to be shown
// working on content the viewer is fully ENTITLED to (ADR 0090 §1: a
// public artwork can be mature). Seeding the mature one as `restricted`
// would let the row predicate do the hiding, and the test would pass
// with no mature conjunct anywhere in the file under test.
func mrpSeed(t *testing.T, pool *pgxpool.Pool) (matureID, controlID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	matureID, controlID = uuid.New(), uuid.New()
	for _, r := range []struct {
		id     uuid.UUID
		mature bool
		label  string
	}{{matureID, true, "mature"}, {controlID, false, "control"}} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO assets (id, title, description, owner_user_ref, asset_type,
			                    status, sensitivity, processing_status, mature)
			VALUES ($1, $2, $2, $3, (SELECT MIN(ref) FROM asset_types),
			        'active', 'public', 'ready', $4)`,
			r.id, mrpToken+" "+r.label, mrpOwner, r.mature); err != nil {
			t.Fatalf("seed asset (%s): %v", r.label, err)
		}
		id := r.id
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM assets WHERE id = $1`, id)
		})
	}
	return matureID, controlID
}

// mrpIDs runs one browse page for one arm and returns the ids it listed.
func mrpIDs(
	t *testing.T,
	pool *pgxpool.Pool,
	caller visibility.Caller,
	v visibility.MatureViewer,
	q *string,
) map[uuid.UUID]bool {
	t.Helper()
	limit := int32(500)
	rows, err := ListAssetsPageGated(context.Background(), pool, caller, nil,
		ListAssetsPageGatedParams{
			Q:            q,
			OwnerUserRef: &[]int64{mrpOwner}[0],
			RowLimit:     limit,
			Mature:       v,
		})
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, r := range rows {
		out[uuid.UUID(r.ID.Bytes)] = true
	}
	return out
}

func mrpAssert(t *testing.T, arm string, got map[uuid.UUID]bool, matureID, controlID uuid.UUID, wantMature bool) {
	t.Helper()
	if !got[controlID] {
		t.Fatalf("%s: the CONTROL asset is missing. The query is refusing everything, "+
			"which makes the mature assertion below pass for the wrong reason", arm)
	}
	switch {
	case wantMature && !got[matureID]:
		t.Errorf("%s: the mature asset is missing for a QUALIFIED viewer — the conjunct "+
			"is refusing everyone, which an absence-only test would call a pass", arm)
	case !wantMature && got[matureID]:
		t.Errorf("%s: LEAK — a mature asset was listed to a disqualified viewer. "+
			"ADR 0090 §3 puts this on the row plane: absent, not placeholdered", arm)
	}
}

// TestMatureRowPlane_UnfilteredBrowse is the arm a `?q=`-only fix fails.
func TestMatureRowPlane_UnfilteredBrowse(t *testing.T) {
	pool := mrpPool(t)
	matureID, controlID := mrpSeed(t, pool)
	stranger := visibility.NewCaller(&[]int64{mrpStranger}[0])

	disq := visibility.MatureViewer{SignedIn: true, OptedIn: false, InstanceAllows: true}
	qual := visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}

	mrpAssert(t, "unfiltered browse / opted-out",
		mrpIDs(t, pool, stranger, disq, nil), matureID, controlID, false)
	mrpAssert(t, "unfiltered browse / opted-in",
		mrpIDs(t, pool, stranger, qual, nil), matureID, controlID, true)
}

// TestMatureRowPlane_TextFiltered covers the `?q=` branch, so a
// regression that moves the conjunct back inside the match is caught by
// the sibling above rather than by both passing.
func TestMatureRowPlane_TextFiltered(t *testing.T) {
	pool := mrpPool(t)
	matureID, controlID := mrpSeed(t, pool)
	stranger := visibility.NewCaller(&[]int64{mrpStranger}[0])
	q := mrpToken

	mrpAssert(t, "?q= / opted-out",
		mrpIDs(t, pool, stranger,
			visibility.MatureViewer{SignedIn: true, InstanceAllows: true}, &q),
		matureID, controlID, false)
	mrpAssert(t, "?q= / opted-in",
		mrpIDs(t, pool, stranger,
			visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: true}, &q),
		matureID, controlID, true)
}

// TestMatureRowPlane_AnonymousAndOwnerAndInstanceOff drives the three
// remaining arms of ADR 0090 §2 through the SQL form, so the conjunct's
// exemptions are proven where they are actually composed rather than
// only in the Go predicate's table test.
func TestMatureRowPlane_AnonymousAndOwnerAndInstanceOff(t *testing.T) {
	pool := mrpPool(t)
	matureID, controlID := mrpSeed(t, pool)
	stranger := visibility.NewCaller(&[]int64{mrpStranger}[0])
	owner := visibility.NewCaller(&[]int64{mrpOwner}[0])

	// ANONYMOUS fails the first conjunct and can never opt in. The
	// MatureViewer zero value IS this arm.
	mrpAssert(t, "anonymous",
		mrpIDs(t, pool, visibility.NewCaller(nil), visibility.AnonymousMatureViewer, nil),
		matureID, controlID, false)

	// THE OWNER sees their own work whatever the axis says. This is the
	// asymmetry ADR 0090 §2 makes deliberately: an artist must not lose
	// access to their own library because of a display preference.
	mrpAssert(t, "owner, opted out",
		mrpIDs(t, pool, owner,
			visibility.MatureViewer{SignedIn: true, OptedIn: false, InstanceAllows: true}, nil),
		matureID, controlID, true)

	// THE INSTANCE SWITCH OFF outranks an opted-in reader's own
	// preference — the operator's answer is about the install.
	mrpAssert(t, "opted in, instance off",
		mrpIDs(t, pool, stranger,
			visibility.MatureViewer{SignedIn: true, OptedIn: true, InstanceAllows: false}, nil),
		matureID, controlID, false)

	// …and the owner still sees it with the switch off, which is the
	// case a "qualified OR owner" spelling gets right by luck rather
	// than by construction.
	mrpAssert(t, "owner, instance off",
		mrpIDs(t, pool, owner,
			visibility.MatureViewer{SignedIn: true, OptedIn: false, InstanceAllows: false}, nil),
		matureID, controlID, true)
}
