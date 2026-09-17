// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1417, sprint 24: migration 00071 on REAL ROWS, both directions.
//
// 00071 replaces two document builders and backfills every asset and
// post document inline. A fresh-database migration run proves the
// statements parse; it says nothing about whether the backfill touched
// the rows it had to, or whether Down puts the old semantics back rather
// than leaving a mixed-version corpus. So this plants rows at v70, walks
// Up, Down and Up again on the same database, and reads the documents
// after each step.
//
// The witness for the fold correction is the marker junk the old fold
// manufactured: serialising a member's tsvector to text and
// re-tokenising it turned `'alpha':1A 'beta':2A 'gamma':3` into the
// lexemes `1a`, `2a` and `3`. At v70 the post matches those through the
// product's own plainto_tsquery predicate; after Up it does not; after
// Down it does again, because stored documents must agree with the
// restored functions.
package db

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
)

const (
	kvmBeforeVersion = 70 // 00070_post_comments_enabled
	kvmAtVersion     = 71 // 00071_kind_is_searchable_vocabulary
)

func kvmProvider(t *testing.T, sqlDB *sql.DB) *goose.Provider {
	t.Helper()
	migrations, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("sub fs: %v", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	return p
}

// kvmMatches is the product predicate on one row.
func kvmMatches(t *testing.T, sqlDB *sql.DB, table string, id uuid.UUID, word string) bool {
	t.Helper()
	var ok bool
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT search_text @@ plainto_tsquery('english', $2) FROM `+table+` WHERE id = $1`,
		id, word).Scan(&ok); err != nil {
		t.Fatalf("match %s %q: %v", table, word, err)
	}
	return ok
}

// kvmWeights returns the weight letters of one lexeme in a row's
// document ("" when absent). Weight D prints as no letter.
func kvmWeights(t *testing.T, sqlDB *sql.DB, table string, id uuid.UUID, lexeme string) string {
	t.Helper()
	var w sql.NullString
	if err := sqlDB.QueryRowContext(context.Background(), `
		SELECT string_agg(COALESCE(NULLIF(substring(pos FROM '[A-C]$'), ''), 'D'), '' ORDER BY pos)
		  FROM `+table+` r,
		       unnest(string_to_array(
		           substring(r.search_text::text FROM '''' || $2 || ''':([0-9A-D,]+)'), ',')) AS pos
		 WHERE r.id = $1`, id, lexeme).Scan(&w); err != nil {
		t.Fatalf("weights %s %q: %v", table, lexeme, err)
	}
	return w.String
}

func kvmFunctionBody(t *testing.T, sqlDB *sql.DB, name string) (string, bool) {
	t.Helper()
	var body string
	err := sqlDB.QueryRowContext(context.Background(), `
		SELECT p.prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'public' AND p.proname = $1`, name).Scan(&body)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body, true
}

func TestMigration00071_KindVocabulary_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := kvmProvider(t, sqlDB)

	// ── v70: the state 00071 has to convert ─────────────────────────
	if _, err := p.UpTo(ctx, kvmBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", kvmBeforeVersion, err)
	}

	// An epub with no kind word anywhere, and a second public member
	// whose title is two lexemes (A, positions 1 and 2) and whose one
	// searchable field value sits at D, position 3: the shape the old
	// fold turns into `1a`, `2a` and `3`. Both inside one public post.
	epub, member, post := uuid.New(), uuid.New(), uuid.New()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status, sensitivity, processing_status, file_extension)
	      VALUES ($1, 'morvantide', '', 14170099, 1, 'active', 'public', 'ready', 'epub')`, epub)
	exec(`INSERT INTO assets (id, title, description, owner_user_ref, asset_type, status, sensitivity, processing_status, file_extension)
	      VALUES ($1, 'brindlewax quorrel', '', 14170099, 1, 'active', 'public', 'ready', 'png')`, member)
	var fieldID uuid.UUID
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT id FROM field_definition
		 WHERE searchable = TRUE AND status = 'active' AND type = 'text' AND mirrors_column IS NULL
		 ORDER BY id LIMIT 1`).Scan(&fieldID); err != nil {
		t.Fatalf("no searchable text field at v%d: %v", kvmBeforeVersion, err)
	}
	exec(`INSERT INTO asset_field_value (asset_id, field_id, value_text) VALUES ($1, $2, 'gorbulent')`, member, fieldID)
	exec(`INSERT INTO posts (id, author_user_ref, title, description, visibility, cover_asset_id)
	      VALUES ($1, 14170099, 'thessaly', '', 'public', $2)`, post, epub)
	exec(`INSERT INTO post_assets (post_id, asset_id, sort_order) VALUES ($1, $2, 0), ($1, $3, 1)`, post, epub, member)

	// The v70 semantics, asserted rather than assumed: no kind anywhere,
	// and the old fold's marker junk is what makes the post searchable
	// by words nobody wrote.
	if w := kvmWeights(t, sqlDB, "assets", epub, "ebook"); w != "" {
		t.Fatalf("at v%d the asset document already carries `ebook` (%s)", kvmBeforeVersion, w)
	}
	if kvmMatches(t, sqlDB, "posts", post, "ebook") {
		t.Fatalf("at v%d the post already matches `ebook`", kvmBeforeVersion)
	}
	for _, junk := range []string{"1a", "2a", "3"} {
		if !kvmMatches(t, sqlDB, "posts", post, junk) {
			t.Fatalf("at v%d the post does not match `%s`; the old fold's marker junk is the witness this test relies on", kvmBeforeVersion, junk)
		}
	}

	// ── Up: 00071 ───────────────────────────────────────────────────
	if _, err := p.UpTo(ctx, kvmAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", kvmAtVersion, err)
	}
	assertSprint24 := func(stage string) {
		t.Helper()
		if w := kvmWeights(t, sqlDB, "assets", epub, "ebook"); w != "D" {
			t.Errorf("%s: asset document carries `ebook` at %q, want D (no reindex was run)", stage, w)
		}
		if w := kvmWeights(t, sqlDB, "posts", post, "ebook"); w != "D" {
			t.Errorf("%s: post document carries `ebook` at %q, want D", stage, w)
		}
		if w := kvmWeights(t, sqlDB, "assets", member, "imag"); w != "D" {
			t.Errorf("%s: the png member's document carries `imag` at %q, want D", stage, w)
		}
		for _, junk := range []string{"1a", "2a", "3"} {
			if kvmMatches(t, sqlDB, "posts", post, junk) {
				t.Errorf("%s: post still matches the manufactured `%s`", stage, junk)
			}
		}
		for _, word := range []string{"ebook", "image", "brindlewax", "quorrel", "gorbulent", "morvantide", "thessaly"} {
			if !kvmMatches(t, sqlDB, "posts", post, word) {
				t.Errorf("%s: post does not match `%s`", stage, word)
			}
		}
		if _, ok := kvmFunctionBody(t, sqlDB, "asset_view_kind"); !ok {
			t.Errorf("%s: public.asset_view_kind is absent", stage)
		}
		if body, _ := kvmFunctionBody(t, sqlDB, "rebuild_post_search_text"); !strings.Contains(body, "tsvector_agg") || !strings.Contains(body, "FOR NO KEY UPDATE") {
			t.Errorf("%s: rebuild_post_search_text is not the corrected fold with 00067's entry lock", stage)
		}
	}
	assertSprint24("after Up")

	// ── Down: back to v70 ───────────────────────────────────────────
	if _, err := p.DownTo(ctx, kvmBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", kvmBeforeVersion, err)
	}
	if _, ok := kvmFunctionBody(t, sqlDB, "asset_view_kind"); ok {
		t.Errorf("after Down: public.asset_view_kind survived")
	}
	var aggCount int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'tsvector_agg'`).Scan(&aggCount); err != nil {
		t.Fatalf("aggregate lookup: %v", err)
	}
	if aggCount != 0 {
		t.Errorf("after Down: tsvector_agg survived")
	}
	if body, _ := kvmFunctionBody(t, sqlDB, "rebuild_asset_search_text"); strings.Contains(body, "asset_view_kind") {
		t.Errorf("after Down: rebuild_asset_search_text still derives a kind")
	}
	if body, _ := kvmFunctionBody(t, sqlDB, "rebuild_post_search_text"); !strings.Contains(body, "string_agg(COALESCE(a.search_text::text") || !strings.Contains(body, "FOR NO KEY UPDATE") || strings.Contains(body, "tsvector_agg") {
		t.Errorf("after Down: rebuild_post_search_text is not 00067's definition (text fold, entry lock first)")
	}
	if body, _ := kvmFunctionBody(t, sqlDB, "asset_changed_trigger"); strings.Contains(body, "file_extension") {
		t.Errorf("after Down: asset_changed_trigger still watches file_extension")
	}
	// Documents rebuilt under the restored builders: no kind lexeme
	// anywhere, and the old fold's marker junk is back.
	for _, row := range []struct {
		table string
		id    uuid.UUID
		lex   string
	}{{"assets", epub, "ebook"}, {"assets", member, "imag"}, {"posts", post, "ebook"}, {"posts", post, "imag"}} {
		if w := kvmWeights(t, sqlDB, row.table, row.id, row.lex); w != "" {
			t.Errorf("after Down: %s document retains the Sprint 24 lexeme %q (%s)", row.table, row.lex, w)
		}
	}
	for _, junk := range []string{"1a", "2a", "3"} {
		if !kvmMatches(t, sqlDB, "posts", post, junk) {
			t.Errorf("after Down: post does not match `%s`; stored documents must carry the restored fold's semantics, junk included", junk)
		}
	}
	for _, word := range []string{"brindlewax", "quorrel", "gorbulent", "morvantide", "thessaly"} {
		if !kvmMatches(t, sqlDB, "posts", post, word) {
			t.Errorf("after Down: post does not match `%s`", word)
		}
	}

	// ── Up again: Sprint 24 returns ─────────────────────────────────
	if _, err := p.UpTo(ctx, kvmAtVersion); err != nil {
		t.Fatalf("migrate up to %d again: %v", kvmAtVersion, err)
	}
	assertSprint24("after second Up")
}
