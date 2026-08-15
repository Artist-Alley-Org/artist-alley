// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1078 — CollectionReadableSQL must agree with CanReadCollection,
// always.
//
// Same exception, same price as ContentReadableSQL (#899): the rule has
// a Go form for the surfaces that reach one id and a SQL form for the
// surfaces that filter a SET, and two expressions of one authorization
// rule is the defect ADR 0063 exists to prevent. This drives every
// (visibility × owner × caller × caps) combination through BOTH forms
// against a real Postgres and fails on the first disagreement.
//
// If you edit CanReadCollection and this goes red, edit
// CollectionReadableSQL.
//
// ⚠️ WHAT IT CANNOT CATCH. It is a consistency check, so it passes
// whenever both forms are wrong in the same direction — an equality
// assertion can never detect a shared wrong rule. What makes it worth
// having anyway is that the two forms reach their answer by DIFFERENT
// routes: the Go form asks CanSee, which runs its own query, while the
// SQL form splices Predicate.ToSQL into the caller's statement. A
// divergence in either mechanism surfaces here; a wrong shared premise
// does not, and that is what the fixture's independently-stated `want`
// column below is for.
//
// Skips without AA_DB_PASSWORD.

package visibility

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const colSQLOwner int64 = 10782201
const colSQLStranger int64 = 10782202
const colSQLAdmin int64 = 10782203

// colSQLSeed plants one collection at `vis` and returns its id.
func colSQLSeed(t *testing.T, pool *pgxpool.Pool, vis string, deleted bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	del := "NULL"
	if deleted {
		del = "NOW()"
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO collections (id, name, description, owner_user_ref, visibility, membership, deleted_at)
		VALUES ($1,$2,'',$3,$4,'manual',`+del+`)`,
		id, "colsql-"+vis+"-"+id.String()[:8], colSQLOwner, vis); err != nil {
		t.Fatalf("seed %s collection: %v", vis, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM collections WHERE id = $1`, id)
	})
	return id
}

// colSQLAsk runs the SQL form for one collection and one caller, in the
// shape a real consumer uses it: spliced into a statement that already
// binds arguments of its own, so the argOffset contract is exercised
// rather than assumed.
func colSQLAsk(
	t *testing.T, pool *pgxpool.Pool, caller Caller, caps CapabilityChecker, id uuid.UUID,
) bool {
	t.Helper()
	frag, args, err := CollectionReadableSQL(context.Background(), "c", caller, caps, 1)
	if err != nil {
		t.Fatalf("CollectionReadableSQL: %v", err)
	}
	sql := `SELECT EXISTS (SELECT 1 FROM collections c WHERE c.id = $1` + frag + `)`
	var ok bool
	if err := pool.QueryRow(context.Background(), sql,
		append([]any{id}, args...)...).Scan(&ok); err != nil {
		t.Fatalf("CollectionReadableSQL query: %v\nSQL: %s", err, sql)
	}
	return ok
}

// TestCollectionReadableSQL_MatchesGo is the parity assertion.
//
// `want` is stated per case INDEPENDENTLY of either implementation, so
// the suite is not purely an equality check: a rule both forms get
// wrong together fails the `want` column even though the two agree.
func TestCollectionReadableSQL_MatchesGo(t *testing.T) {
	pool := contentPool(t)
	ctx := context.Background()

	pub := colSQLSeed(t, pool, "public", false)
	priv := colSQLSeed(t, pool, "private", false)
	deletedPub := colSQLSeed(t, pool, "public", true)

	owner := colSQLOwner
	stranger := colSQLStranger
	adminRef := colSQLAdmin
	admin := func(code string) bool { return code == SystemAdmin }
	none := func(string) bool { return false }

	cases := []struct {
		name string
		id   uuid.UUID
		ref  *int64
		caps CapabilityChecker
		want bool
	}{
		{"anonymous / public", pub, nil, nil, true},
		{"anonymous / private", priv, nil, nil, false},
		{"stranger / public", pub, &stranger, none, true},
		{"stranger / private", priv, &stranger, none, false},
		{"owner / private", priv, &owner, none, true},
		{"admin / private", priv, &adminRef, admin, true},
		{"admin / public", pub, &adminRef, admin, true},

		// Soft-delete. The row plane drops the tombstone for everyone
		// EXCEPT the admin arm, whose fragment is empty and so states
		// nothing about deleted_at. That asymmetry is deliberate —
		// GetCollection's Restore branch depends on it — and it is
		// pinned here so the day someone "fixes" it, the two callers
		// that rely on it fail loudly instead of quietly.
		{"anonymous / soft-deleted public", deletedPub, nil, nil, false},
		{"stranger / soft-deleted public", deletedPub, &stranger, none, false},
		{"owner / soft-deleted public", deletedPub, &owner, none, false},
		{"admin / soft-deleted public", deletedPub, &adminRef, admin, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			goAnswer, err := CanReadCollection(ctx, pool, NewCaller(c.ref), c.caps, c.id)
			if err != nil {
				t.Fatalf("CanReadCollection: %v", err)
			}
			sqlAnswer := colSQLAsk(t, pool, NewCaller(c.ref), c.caps, c.id)

			if goAnswer != sqlAnswer {
				t.Fatalf("SQL and Go disagree: CanReadCollection=%v CollectionReadableSQL=%v. "+
					"These are two expressions of ONE rule; edit the other one.",
					goAnswer, sqlAnswer)
			}
			if goAnswer != c.want {
				t.Errorf("both forms answered %v, want %v — they agree and are BOTH wrong, "+
					"which is exactly what an equality assertion cannot see", goAnswer, c.want)
			}
		})
	}
}
