// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1123 — migration 00050 creates `tag_follows`, and 00051 renames
// `user_preferences.team_rail` to `browse_rail`.
//
// The properties worth pinning are the ones where tag follows DIFFER
// from team follows, because the shape that rhymes is the shape someone
// will later "unify":
//
//   - the PK is (user_ref, tag) with the TAG as a text column, which is
//     what makes ON CONFLICT DO NOTHING idempotent for a corpus that has
//     no ids;
//   - there is exactly ONE foreign key, on user_ref, and it cascades.
//     There is deliberately no second FK, because there is no table to
//     point at — a test that asserted two would be asserting a design
//     this one rejected;
//   - the length CHECK exists and fires, since an unbounded text column
//     inside a btree PK is a runtime index-overflow error waiting for
//     the first pathological write;
//   - the rename in 00051 PRESERVES DATA, which is the entire reason a
//     rename was chosen over add-and-drop.

package db

import (
	"strings"
	"testing"
)

const (
	tagFollowsBeforeVersion = 49 // 00049_team_rail_preference
	tagFollowsAtVersion     = 50 // 00050_tag_follows
	browseRailAtVersion     = 51 // 00051_browse_rail_preference
)

func TestMigration00050_TagFollows_UpDown(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	// ── Before 00050 the table must not exist ────────────────────────
	if _, err := p.UpTo(ctx, tagFollowsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", tagFollowsBeforeVersion, err)
	}
	if tableExists(t, sqlDB, "tag_follows") {
		t.Fatalf("tag_follows exists before 00050 — the migration is redundant")
	}

	// ── Up ───────────────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, tagFollowsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", tagFollowsAtVersion, err)
	}
	if !tableExists(t, sqlDB, "tag_follows") {
		t.Fatalf("tag_follows missing after 00050 Up")
	}

	// The PK is the idempotence mechanism, so pin its exact columns and
	// their order — `ON CONFLICT (user_ref, tag)` in queries.sql names
	// this constraint by shape.
	var pkCols string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		   FROM pg_constraint c
		   JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON TRUE
		   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		  WHERE c.conname = 'tag_follows_pkey'`).Scan(&pkCols); err != nil {
		t.Fatalf("read tag_follows PK columns: %v", err)
	}
	if pkCols != "user_ref,tag" {
		t.Errorf("tag_follows PK = (%s), want (user_ref,tag)", pkCols)
	}

	// ONE cascading FK, on the user. Asserted alongside a count, because
	// "the user FK cascades" would still pass if somebody had also added
	// a second FK against some future tags table — and that would be a
	// design change (see 00050), not a refactor.
	if got := fkDeleteRule(t, sqlDB, "tag_follows_user_ref_fkey"); got != "CASCADE" {
		t.Errorf("tag_follows_user_ref_fkey delete rule = %q, want CASCADE", got)
	}
	var fkCount int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_constraint
		  WHERE conrelid = 'public.tag_follows'::regclass AND contype = 'f'`).
		Scan(&fkCount); err != nil {
		t.Fatalf("count tag_follows FKs: %v", err)
	}
	if fkCount != 1 {
		t.Errorf("tag_follows has %d foreign keys, want exactly 1 — a tag is a corpus "+
			"with no parent row, so a second FK means somebody gave tags a table "+
			"without revisiting this migration's argument", fkCount)
	}

	// ── Behaviour, not just declarations ─────────────────────────────
	var userA, userB int64
	for _, u := range []struct {
		name string
		out  *int64
	}{{"tagf-a", &userA}, {"tagf-b", &userB}} {
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO "user" (username, approved) VALUES ($1, 1) RETURNING ref`,
			u.name).Scan(u.out); err != nil {
			t.Fatalf("seed user %s: %v", u.name, err)
		}
	}
	countFollows := func() int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM tag_follows`).Scan(&n); err != nil {
			t.Fatalf("count tag_follows: %v", err)
		}
		return n
	}

	for _, pair := range [][2]any{{userA, "fantasy"}, {userB, "sci-fi"}} {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO tag_follows (user_ref, tag) VALUES ($1, $2)`,
			pair[0], pair[1]); err != nil {
			t.Fatalf("seed follow %v: %v", pair, err)
		}
	}
	if n := countFollows(); n != 2 {
		t.Fatalf("seeded %d follows, want 2", n)
	}

	// A tag that no post carries is a LEGAL follow. This is the property
	// that separates tag_follows from its two siblings — both of theirs
	// are FK-constrained and this one deliberately is not — so it gets an
	// assertion rather than a comment.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO tag_follows (user_ref, tag) VALUES ($1, $2)`,
		userA, "a-tag-no-post-has-ever-carried"); err != nil {
		t.Errorf("following an unused tag was refused (%v) — a tag is a corpus, not a "+
			"row, and following one before it exists must stay legal", err)
	}

	// The PK really rejects the duplicate the query's ON CONFLICT absorbs.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO tag_follows (user_ref, tag) VALUES ($1, $2)`,
		userA, "fantasy"); err == nil {
		t.Error("duplicate (user_ref, tag) accepted — the PK is not enforcing uniqueness")
	}

	// Case is NOT folded by the key: 'Fantasy' and 'fantasy' are two
	// follows. That mirrors post_tags and `?tag=`, and the whole product
	// agreeing on it is what keeps the rail chip and the feed consistent.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO tag_follows (user_ref, tag) VALUES ($1, $2)`,
		userA, "Fantasy"); err != nil {
		t.Errorf("'Fantasy' was rejected beside 'fantasy' (%v) — the PK must not fold "+
			"case, because post_tags and ?tag= do not", err)
	}

	// The length CHECK fires. Its job is to turn a btree index overflow
	// (a runtime ERROR with an opaque message) into a constraint the
	// handler can answer with a 400.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO tag_follows (user_ref, tag) VALUES ($1, $2)`,
		userA, strings.Repeat("x", 201)); err == nil {
		t.Error("a 201-character tag was accepted — the length CHECK is not enforced, " +
			"and an unbounded text column in a btree PK will fail at INSERT time instead")
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO tag_follows (user_ref, tag) VALUES ($1, '')`, userA); err == nil {
		t.Error("an empty tag was accepted — the CHECK's lower bound is not enforced")
	}

	// The user cascade FIRES.
	before := countFollows()
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM "user" WHERE ref = $1`, userA); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if n := countFollows(); n != 1 {
		t.Errorf("after deleting user A, %d follows remain, want 1 (had %d) — the user "+
			"cascade did not fire", n, before)
	}

	// ── Down drops it, re-Up is clean ────────────────────────────────
	if _, err := p.DownTo(ctx, tagFollowsBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", tagFollowsBeforeVersion, err)
	}
	if tableExists(t, sqlDB, "tag_follows") {
		t.Errorf("tag_follows survives 00050 Down")
	}
	if _, err := p.UpTo(ctx, tagFollowsAtVersion); err != nil {
		t.Fatalf("re-apply 00050 after down: %v", err)
	}
	if !tableExists(t, sqlDB, "tag_follows") {
		t.Errorf("tag_follows not restored after re-Up")
	}
}

// TestMigration00051_BrowseRailRename_PreservesCuration is the reason
// 00051 is a RENAME and not an add-column-plus-backfill-plus-drop.
//
// A reader who curated their rail during sprint 24's two-day window must
// still have that curation afterwards. Nothing else in the migration can
// go wrong — a rename either happens or the migration fails — so the
// data is the whole subject.
func TestMigration00051_BrowseRailRename_PreservesCuration(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)

	if _, err := p.UpTo(ctx, tagFollowsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", tagFollowsAtVersion, err)
	}

	var ref int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO "user" (username, approved) VALUES ('rail-rename', 1) RETURNING ref`).
		Scan(&ref); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// A curation written under the OLD column name, exactly as sprint
	// 24's manage panel would have written it.
	const curation = `{"hidden_team_ids": ["11111111-1111-1111-1111-111111111111"], "team_order": ["22222222-2222-2222-2222-222222222222"]}`
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO user_preferences (user_ref, team_rail) VALUES ($1, $2::jsonb)`,
		ref, curation); err != nil {
		t.Fatalf("seed pre-rename curation: %v", err)
	}

	if _, err := p.UpTo(ctx, browseRailAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", browseRailAtVersion, err)
	}

	var hidden, ordered string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT browse_rail->'hidden_team_ids'->>0, browse_rail->'team_order'->>0
		   FROM user_preferences WHERE user_ref = $1`, ref).Scan(&hidden, &ordered); err != nil {
		t.Fatalf("read browse_rail after rename: %v", err)
	}
	if hidden != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("hidden_team_ids[0] = %q after the rename, want the seeded id — the "+
			"rename dropped the reader's curation", hidden)
	}
	if ordered != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("team_order[0] = %q after the rename, want the seeded id", ordered)
	}

	// The old name is gone. Otherwise the two would drift, and a reader
	// on one code path would curate a column the other never reads.
	if columnExists(t, sqlDB, "user_preferences", "team_rail") {
		t.Error("user_preferences.team_rail still exists after 00051 — the rename left " +
			"two columns and a reader's curation can now land in the wrong one")
	}

	// Down restores the old name WITH the data, so a rollback is not a
	// data-loss event either.
	if _, err := p.DownTo(ctx, tagFollowsAtVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", tagFollowsAtVersion, err)
	}
	var back string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT team_rail->'hidden_team_ids'->>0 FROM user_preferences WHERE user_ref = $1`,
		ref).Scan(&back); err != nil {
		t.Fatalf("read team_rail after Down: %v", err)
	}
	if back != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("after 00051 Down, team_rail[0] = %q, want the seeded id", back)
	}
}
