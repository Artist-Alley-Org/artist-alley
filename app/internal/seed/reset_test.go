// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Invariant tests for the `aa seed --reset` content reset (#569).
//
// Three properties, in increasing order of what they buy:
//
//  1. CoversSchema — every text `…kind` / `…type` column in the live
//     schema is classified in reset.go. This is the one that makes the
//     policy self-enforcing: a NEW polymorphic table fails here with
//     "classify me" instead of silently orphaning rows on every reset,
//     which is exactly how likes (#566) and featured_items got missed.
//
//  2. RefsAreUnenforced — every registered ref's id column really does
//     lack a foreign key. If someone adds one, CASCADE now covers the
//     table and the registry entry is misleading.
//
//  3. LeavesNoOrphans — the behavioural test. It plants a row in every
//     sweepable table, runs Reset, and asserts ZERO orphans across the
//     whole registry. The assertion loop is DERIVED from the registry,
//     so it starts covering a new table the moment that table is
//     classified — unlike a hardcoded table-name checklist.
//
// (3) fails on pre-#569 code with orphans in notifications,
// scheduled_actions, featured_items and workflow_audit.

package seed

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mscrnt/artist-alley/app/internal/db"
)

// Local copies of the two helpers admin_test.go defines — that file is
// in the external seed_test package, and this one must be in `seed`
// itself to see the unexported registry it verifies.
func resetEnvOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func resetSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
}

func openResetPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; integration test skipped")
	}
	dsn := fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable password=%s",
		resetEnvOr("AA_DB_HOST", "postgres"), resetEnvOr("AA_DB_PORT", "5432"),
		resetEnvOr("AA_DB_USER", "artist_alley"), resetEnvOr("AA_DB_NAME", "artist_alley"), pwd)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPolymorphicRegistry_CoversSchema is the guard against this bug
// class recurring. Detection is derived from the live schema; only the
// CLASSIFICATION is hand-written.
func TestPolymorphicRegistry_CoversSchema(t *testing.T) {
	pool := openResetPool(t)
	ctx := t.Context()

	classified := map[string]bool{}
	for _, ref := range polymorphicRefs {
		classified[ref.Table+"."+ref.KindColumn] = true
	}
	for col := range nonReferenceKindColumns {
		if classified[col] {
			t.Errorf("%s is in BOTH polymorphicRefs and nonReferenceKindColumns", col)
		}
		classified[col] = true
	}

	rows, err := pool.Query(ctx, `
		SELECT c.table_name || '.' || c.column_name
		  FROM information_schema.columns c
		  JOIN information_schema.tables t
		    ON t.table_schema = c.table_schema
		   AND t.table_name = c.table_name
		   AND t.table_type = 'BASE TABLE'
		 WHERE c.table_schema = 'public'
		   AND c.data_type IN ('text', 'character varying')
		   AND c.column_name ~ '(^|_)(kind|type)$'
		 ORDER BY 1`)
	if err != nil {
		t.Fatalf("enumerate kind columns: %v", err)
	}
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found[col] = true
		if !classified[col] {
			t.Errorf("unclassified kind/type column %q — add it to polymorphicRefs "+
				"(with a disposition + reason) or to nonReferenceKindColumns in reset.go. "+
				"If it is a polymorphic reference, `aa seed --reset` is orphaning its rows "+
				"right now: TRUNCATE ... CASCADE cannot reach a column with no foreign key.", col)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	for col := range classified {
		if !found[col] {
			t.Errorf("classified column %q no longer exists in the schema — "+
				"remove it from reset.go", col)
		}
	}
}

// TestPolymorphicRegistry_TargetsResolve keeps the registry's target
// map honest: every (table, column) it names must exist, or the sweep
// would blow up mid-reset with a runtime SQL error.
func TestPolymorphicRegistry_TargetsResolve(t *testing.T) {
	pool := openResetPool(t)
	ctx := t.Context()

	check := func(table, column string) {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				 WHERE table_schema = 'public'
				   AND table_name = trim(both '"' from $1)
				   AND column_name = $2)`, table, column).Scan(&ok); err != nil {
			t.Fatalf("probe %s.%s: %v", table, column, err)
		}
		if !ok {
			t.Errorf("registry names %s.%s which does not exist", table, column)
		}
	}
	for _, ref := range polymorphicRefs {
		check(ref.Table, ref.KindColumn)
		check(ref.Table, ref.IDColumn)
		for kind, tgt := range ref.Targets {
			if tgt.Table == "" || tgt.Column == "" {
				t.Errorf("%s kind %q has an empty target", ref.Table, kind)
				continue
			}
			check(tgt.Table, tgt.Column)
		}
		if ref.Reason == "" {
			t.Errorf("%s.%s has no Reason — every disposition must be justified in place",
				ref.Table, ref.KindColumn)
		}
	}
	// The sweep must actually be executable.
	for _, stmt := range sweepStatements() {
		if _, err := pool.Exec(ctx, "EXPLAIN "+stmt.SQL, stmt.Kind); err != nil {
			t.Errorf("sweep statement is not valid SQL: %v\n%s", err, stmt.SQL)
		}
	}
}

// TestPolymorphicRefs_AreUnenforced asserts the premise of the whole
// registry: these id columns carry no FK, which is why CASCADE cannot
// reach them. If one gains an FK the entry should be reconsidered
// rather than left as dead weight.
func TestPolymorphicRefs_AreUnenforced(t *testing.T) {
	pool := openResetPool(t)
	ctx := t.Context()

	for _, ref := range polymorphicRefs {
		var hasFK bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				  FROM pg_constraint con
				  JOIN pg_class cl ON cl.oid = con.conrelid
				  JOIN LATERAL unnest(con.conkey) k(attnum) ON true
				  JOIN pg_attribute a
				    ON a.attrelid = con.conrelid AND a.attnum = k.attnum
				 WHERE con.contype = 'f'
				   AND cl.relname = trim(both '"' from $1)
				   AND a.attname = $2)`, ref.Table, ref.IDColumn).Scan(&hasFK); err != nil {
			t.Fatalf("fk probe %s.%s: %v", ref.Table, ref.IDColumn, err)
		}
		if hasFK {
			t.Errorf("%s.%s now has a FOREIGN KEY — CASCADE covers it, so it is no "+
				"longer polymorphic. Re-check its entry in reset.go.", ref.Table, ref.IDColumn)
		}
	}
}

// resetFixture is the content Reset is expected to destroy.
type resetFixture struct {
	adminUsername string
	adminRef      int64
	userRef       int64
	postID        uuid.UUID
	assetID       uuid.UUID
	collectionID  uuid.UUID
	commentID     uuid.UUID
	stateID       uuid.UUID
	roleID        uuid.UUID
	assetTypeRef  int64
}

// plantOrphans writes one row into every SWEEPABLE polymorphic table,
// each pointing at content the reset destroys. Keyed by table so the
// coverage check below can name what is missing.
func plantOrphans(t *testing.T, pool *pgxpool.Pool, f resetFixture) {
	t.Helper()
	ctx := t.Context()

	planted := map[string]func() error{
		"comments": func() error {
			id := uuid.New()
			_, err := pool.Exec(ctx,
				`INSERT INTO comments (id, root_id, target_kind, target_id, body, author_user_ref)
				 VALUES ($1, $1, 'post', $2, 'reset fixture', $3)`, id, f.postID, f.userRef)
			return err
		},
		"likes": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO likes (target_kind, target_id, user_ref) VALUES ('post', $1, $2)`,
				f.postID, f.userRef)
			return err
		},
		"featured_items": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO featured_items (subject_kind, subject_id, position, created_by_user_ref, scope)
				 VALUES ('collection', $1, 9001, $2, 'public')`, f.collectionID, f.adminRef)
			return err
		},
		"notifications": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO notifications (recipient_user_ref, verb, target_kind, target_id)
				 VALUES ($1, 'post.commented', 'post', $2)`, f.adminRef, f.postID.String())
			return err
		},
		"scheduled_actions": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO scheduled_actions (action, target_kind, target_id, scheduled_for, created_by)
				 VALUES ('restrict', 'asset', $1, now() + interval '30 days', $2)`,
				f.assetID.String(), f.adminRef)
			return err
		},
		"workflow_audit": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO workflow_audit (resource_kind, resource_id, to_state_id, actor_user_ref)
				 VALUES ('asset', $1, $2, $3)`, f.assetID, f.stateID, f.adminRef)
			return err
		},
		"post_acls": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO post_acls (post_id, principal_type, principal_id, permission)
				 VALUES ($1, 'user', $2, 'read')`, f.postID, strconv.FormatInt(f.userRef, 10))
			return err
		},
		"collection_acls": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO collection_acls (collection_id, principal_type, principal_id, permission)
				 VALUES ($1, 'user', $2, 'read')`, f.collectionID, strconv.FormatInt(f.userRef, 10))
			return err
		},
		"asset_type_acls": func() error {
			_, err := pool.Exec(ctx,
				`INSERT INTO asset_type_acls (asset_type_ref, principal_type, principal_id, permission)
				 VALUES ($1, 'user', $2, 'read')`, f.assetTypeRef, strconv.FormatInt(f.userRef, 10))
			return err
		},
	}

	// Coverage: a newly-classified sweepable table with no fixture is a
	// gap in this test, so say so loudly rather than passing vacuously.
	for _, ref := range polymorphicRefs {
		if ref.Sweep && planted[ref.Table] == nil {
			t.Errorf("no orphan fixture for sweepable table %q — add one to plantOrphans "+
				"so the invariant below actually exercises it", ref.Table)
		}
	}
	for table, plant := range planted {
		if err := plant(); err != nil {
			t.Fatalf("plant %s: %v", table, err)
		}
	}
}

// countOrphans applies the registry's own resolve rule to a table and
// returns how many rows dangle. This is the assertion form the issue
// asked for: it is derived from the registry, so it keeps working for
// tables that do not exist yet.
func countOrphans(t *testing.T, pool *pgxpool.Pool, ref polymorphicRef) int {
	t.Helper()
	total := 0
	for kind, tgt := range ref.Targets {
		q := fmt.Sprintf(
			`SELECT count(*) FROM %s src WHERE src.%s = $1 AND NOT EXISTS (`+
				`SELECT 1 FROM %s tgt WHERE tgt.%s::text = src.%s::text)`,
			ref.Table, ref.KindColumn, tgt.Table, tgt.Column, ref.IDColumn)
		var n int
		if err := pool.QueryRow(t.Context(), q, kind).Scan(&n); err != nil {
			t.Fatalf("count orphans %s/%s: %v", ref.Table, kind, err)
		}
		total += n
	}
	return total
}

func TestReset_LeavesNoPolymorphicOrphans(t *testing.T) {
	pool := openResetPool(t)
	ctx := t.Context()

	f := resetFixture{adminUsername: "reset-admin-" + resetSuffix()}

	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Reset Admin', 1) RETURNING ref`,
		f.adminUsername).Scan(&f.adminRef); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Reset Victim', 1) RETURNING ref`,
		"reset-victim-"+resetSuffix()).Scan(&f.userRef); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM workflow_states ORDER BY id LIMIT 1`).Scan(&f.stateID); err != nil {
		t.Fatalf("workflow state: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT ref FROM asset_types ORDER BY ref LIMIT 1`).Scan(&f.assetTypeRef); err != nil {
		t.Fatalf("asset type: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM roles ORDER BY id LIMIT 1`).Scan(&f.roleID); err != nil {
		t.Fatalf("role: %v", err)
	}

	f.postID, f.assetID, f.collectionID, f.commentID = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO posts (id, author_user_ref, title, visibility) VALUES ($1, $2, 'Reset post', 'org-only')`,
		f.postID, f.userRef); err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, owner_user_ref, title, asset_type) VALUES ($1, $2, 'Reset asset', $3)`,
		f.assetID, f.userRef, f.assetTypeRef); err != nil {
		t.Fatalf("asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO collections (id, owner_user_ref, name) VALUES ($1, $2, 'Reset collection')`,
		f.collectionID, f.userRef); err != nil {
		t.Fatalf("collection: %v", err)
	}

	// user_follows is not polymorphic, but carries no FK to "user"
	// either, so it is the same "CASCADE cannot reach me" class.
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_follows (follower_user_ref, followee_user_ref) VALUES ($1, $2)`,
		f.userRef, f.adminRef); err != nil {
		t.Fatalf("follow: %v", err)
	}

	plantOrphans(t, pool, f)

	// A scheduled action against the SURVIVING admin. #569 assumed
	// scheduled_actions had to be exempted wholesale to protect rows
	// like this; sweeping instead of truncating protects it precisely.
	var keeperID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO scheduled_actions (action, target_kind, target_id, scheduled_for, created_by)
		 VALUES ('notify', 'user', $1, now() + interval '30 days', $2) RETURNING id`,
		strconv.FormatInt(f.adminRef, 10), f.adminRef).Scan(&keeperID); err != nil {
		t.Fatalf("keeper action: %v", err)
	}

	// Every sweepable table must have something to lose, or the
	// invariant below would pass without proving anything.
	before := map[string]int{}
	for _, ref := range polymorphicRefs {
		if !ref.Sweep {
			continue
		}
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+ref.Table).Scan(&n); err != nil {
			t.Fatalf("pre-count %s: %v", ref.Table, err)
		}
		before[ref.Table] = n
		if n == 0 {
			t.Fatalf("%s is empty before Reset — the fixture did not land", ref.Table)
		}
	}

	if err := Reset(ctx, pool, f.adminUsername); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	// THE invariant: no row in any swept polymorphic table names a
	// target that no longer exists.
	for _, ref := range polymorphicRefs {
		if !ref.Sweep {
			continue
		}
		if n := countOrphans(t, pool, ref); n != 0 {
			t.Errorf("%s has %d orphan row(s) after Reset (had %d rows before) — "+
				"its (%s, %s) pair survived a reset that deleted the targets",
				ref.Table, n, before[ref.Table], ref.KindColumn, ref.IDColumn)
		}
	}

	// Sweep, not blanket truncate: rows whose target the reset KEPT
	// must still be there.
	var kept int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_actions WHERE id = $1`, keeperID).Scan(&kept); err != nil {
		t.Fatalf("keeper probe: %v", err)
	}
	if kept != 1 {
		t.Errorf("scheduled_action against the surviving admin was deleted; the reset must "+
			"sweep unresolvable rows, not truncate the table (found %d)", kept)
	}

	// user_follows is the same no-FK class without being polymorphic:
	// nothing cascades from "user", so seeded follow edges used to
	// accumulate one full dataset per reset.
	var follows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_follows`).Scan(&follows); err != nil {
		t.Fatalf("user_follows: %v", err)
	}
	if follows != 0 {
		t.Errorf("user_follows still holds %d row(s) after Reset", follows)
	}

	t.Cleanup(func() {
		c := context.Background()
		ctxT, cancel := context.WithTimeout(c, 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctxT, `DELETE FROM scheduled_actions WHERE id = $1`, keeperID)
		_, _ = pool.Exec(ctxT, `DELETE FROM "user" WHERE username = $1`, f.adminUsername)
	})
}

// TestReset_SweepsFieldDefinitionButKeepsShipped is the behavioural
// half of #812. The reset must leave a field_definition table holding
// exactly the shipped catalogue: the studio fields a seed added are
// content and go, the definitions the migrations ship are the product
// and stay.
//
// Both directions are asserted, because each has its own failure. If
// the sweep becomes a no-op, a `--reset` between two datasets leaves
// the previous dataset's studio fields behind and the metadata panel
// grows a field per reset. If it goes back to being a TRUNCATE, the
// instance loses the catalogue every operator has — which is the bug
// this test exists to keep fixed, and which no row count caught for
// the life of the project.
func TestReset_SweepsFieldDefinitionButKeepsShipped(t *testing.T) {
	pool := openResetPool(t)
	ctx := t.Context()

	admin := "reset-fieldadmin-" + resetSuffix()
	var adminRef int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (username, fullname, approved) VALUES ($1, 'Field Admin', 1) RETURNING ref`,
		admin).Scan(&adminRef); err != nil {
		t.Fatalf("admin: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = pool.Exec(c, `DELETE FROM "user" WHERE username = $1`, admin)
	})

	// The shipped catalogue must be there to begin with, or the
	// "survives" assertion below would pass vacuously.
	shipped := db.ShippedFieldCodes()
	var present int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM field_definition WHERE code = ANY($1::text[])`,
		shipped).Scan(&present); err != nil {
		t.Fatalf("count shipped: %v", err)
	}
	if present != len(shipped) {
		t.Fatalf("precondition failed: %d of %d shipped field definitions are in the "+
			"database. Something already deleted them.", present, len(shipped))
	}

	// A seed-added studio field, with a value and a history row hanging
	// off it. asset_field_value has an ON DELETE CASCADE FK;
	// asset_field_value_history has no constraint at all, so only the
	// explicit sweep can reach it.
	code := "reset_studio_field_" + resetSuffix()
	var fieldID, assetID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO field_definition (code, label, type, subject_kind)
		 VALUES ($1, 'Reset studio field', 'text', 'asset') RETURNING id`, code).Scan(&fieldID); err != nil {
		t.Fatalf("plant field: %v", err)
	}
	var assetTypeRef int64
	if err := pool.QueryRow(ctx, `SELECT ref FROM asset_types ORDER BY ref LIMIT 1`).Scan(&assetTypeRef); err != nil {
		t.Fatalf("asset type: %v", err)
	}
	assetID = uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO assets (id, owner_user_ref, title, asset_type) VALUES ($1, $2, 'Field asset', $3)`,
		assetID, adminRef, assetTypeRef); err != nil {
		t.Fatalf("asset: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, 'v')`,
		assetID, fieldID); err != nil {
		t.Fatalf("asset_field_value: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_field_value_history (asset_id, field_id, new_value) VALUES ($1, $2, '"v"'::jsonb)`,
		assetID, fieldID); err != nil {
		t.Fatalf("asset_field_value_history: %v", err)
	}
	// A history row against a SHIPPED field on the same doomed asset:
	// the asset goes, so this must go too, and it proves the sweep does
	// not stop at the field_id predicate.
	var shippedFieldID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM field_definition WHERE code = $1`, shipped[0]).Scan(&shippedFieldID); err != nil {
		t.Fatalf("shipped field id: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO asset_field_value_history (asset_id, field_id, new_value) VALUES ($1, $2, '"s"'::jsonb)`,
		assetID, shippedFieldID); err != nil {
		t.Fatalf("shipped history row: %v", err)
	}

	if err := Reset(ctx, pool, admin); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	var codes []string
	rows, err := pool.Query(ctx, `SELECT code FROM field_definition ORDER BY code`)
	if err != nil {
		t.Fatalf("read field_definition: %v", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		codes = append(codes, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if diff := strings.Join(codes, ","); diff != strings.Join(shipped, ",") {
		t.Errorf("field_definition after Reset is [%s], want exactly the shipped catalogue "+
			"[%s]. Extra codes mean the sweep is a no-op and studio fields accumulate on "+
			"every reset; missing ones mean the shipped catalogue was truncated again (#812).",
			diff, strings.Join(shipped, ","))
	}

	var history int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM asset_field_value_history WHERE asset_id = $1`, assetID).Scan(&history); err != nil {
		t.Fatalf("history probe: %v", err)
	}
	if history != 0 {
		t.Errorf("asset_field_value_history kept %d row(s) for a truncated asset. The table "+
			"has no foreign key on either asset_id or field_id, so nothing but the explicit "+
			"sweep in Reset can reach it.", history)
	}
}
