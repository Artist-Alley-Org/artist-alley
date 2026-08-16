// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// SeedInsertFeatured's ON CONFLICT target, executed.
//
// # Why this test exists
//
// The target has silently gone stale TWICE, and both times the only
// thing that noticed was the Playwright job's `aa seed` step:
//
//	00010  widened featured_items_placement_unique from
//	       (subject_kind, subject_id) to (…, scope, team_id)
//	00053  widened it again with band_id (#1118)
//
// An ON CONFLICT target is matched to a real constraint by its exact
// column list. A list matching nothing is a RUNTIME error — 42P10,
// "there is no unique or exclusion constraint matching the ON CONFLICT
// specification" — and nothing upstream of the database catches it:
// it compiles, sqlc generates it happily, `go vet` is silent, and no
// other test in the tree reaches the statement. On #1118 the failure
// surfaced 13 minutes into CI, in a seed step, as a phase error whose
// text names neither the column that changed nor the migration that
// changed it.
//
// So this EXECUTES the generated query rather than asserting anything
// about its text. A string comparison against pg_constraint would pin
// the columns and still miss a target that names the right columns in a
// way Postgres cannot match; running the statement is the only oracle
// that answers the actual question, which is "does Postgres accept
// this".
//
// Both halves matter. The insert proves the target resolves at all; the
// SECOND insert proves it still does the job it is there for — a
// resumed seed run re-features the same collection and must be a silent
// no-op, not a 23505. A test that only inserted once would pass on a
// target that resolved to the wrong constraint.

package seed

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSeedInsertFeatured_OnConflictTargetResolves(t *testing.T) {
	pool := openCompanionTestPool(t)
	ctx := context.Background()
	q := New(pool)

	// A subject id that exists nowhere. featured_items deliberately has
	// no FK to its subject (migration 00002/00010), so a placement can
	// point at nothing — which is what makes this test cheap: it needs
	// no asset, no collection and no user fixture, only the constraint.
	subject := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM featured_items WHERE subject_id = $1`, subject)
	})

	params := SeedInsertFeaturedParams{SubjectKind: "collection", SubjectID: subject}

	if err := q.SeedInsertFeatured(ctx, params); err != nil {
		t.Fatalf("SeedInsertFeatured: %v\n\n"+
			"If this is 42P10 (\"no unique or exclusion constraint matching the ON "+
			"CONFLICT specification\"), a migration has changed "+
			"featured_items_placement_unique and the ON CONFLICT target in "+
			"internal/seed/queries.sql was not changed with it. That target must "+
			"name EVERY column of the constraint. This is the third time; the "+
			"first two were caught by CI's `aa seed` step 13 minutes in, which is "+
			"why the check now lives here.", err)
	}

	// The resumed-run case, and the whole reason the clause is there.
	if err := q.SeedInsertFeatured(ctx, params); err != nil {
		t.Fatalf("re-featuring the same subject failed: %v\n\n"+
			"ON CONFLICT ... DO NOTHING exists so a resumed `aa seed` is "+
			"idempotent. If this is 23505 the target resolved to a constraint "+
			"that does not cover this insert's real conflict.", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM featured_items WHERE subject_id = $1`, subject).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("re-seeding produced %d rows for one subject, want 1 — "+
			"the conflict was not caught and the seed is not idempotent", n)
	}

	// Seeded placements belong to the RAIL, never to a promo band
	// (#1118). The insert names no band_id, so this asserts the column's
	// default rather than a behaviour — which is the point: if a later
	// change gives band_id a non-NULL default, every seeded placement
	// silently becomes a band card on a surface the operator never
	// curated.
	var onRail bool
	if err := pool.QueryRow(ctx,
		`SELECT band_id IS NULL FROM featured_items WHERE subject_id = $1`, subject).Scan(&onRail); err != nil {
		t.Fatalf("band_id: %v", err)
	}
	if !onRail {
		t.Error("a seeded placement landed in a promo band; seeded curation is rail curation")
	}
}
