// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1595: migration 00081 adds write-once ingest provenance to assets and
// the upload_receipts table. Pinned here against a real, fresh database:
//
//   - an asset made before the migration keeps metadata.original_filename
//     as it was, and gets it as original_basename ONLY when it is already
//     a safe bare base name, labelled client_reported (never
//     upload_request); a path, a control or bidi mark, an empty, dot or
//     overlong value, a non-string or an absent key is unknown with no
//     name (a path is not reduced to its last component);
//   - after the migration the pair cannot be changed by an UPDATE, and a
//     new row cannot claim an unsafe name or an unlisted source;
//   - Down removes the columns, trigger and table; Up again restores
//     them and backfills the same way.
//
// Skips without AA_DB_PASSWORD, like the other migration tests.

package db

import (
	"strings"
	"testing"
)

const (
	originalBasenameBeforeVersion = 80 // 00080_federation_remote_comment_edits
	originalBasenameAtVersion     = 81 // 00081_asset_original_basename
)

func TestMigration00081_AssetOriginalBasename_BackfillAndWriteOnce(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, originalBasenameBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", originalBasenameBeforeVersion, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// title -> metadata JSON; the title is the key the assertions use.
	legacy := map[string]string{
		"good":     `{"original_filename": "sketch one.png"}`,
		"unicode":  `{"original_filename": "写真.png"}`,
		"path":     `{"original_filename": "C:\\Users\\bob\\sketch.png"}`,
		"slash":    `{"original_filename": "dir/sketch.png"}`,
		"control":  `{"original_filename": "a\u0001b.png"}`,
		"bidi":     `{"original_filename": "photo\u202egpj.exe"}`,
		"empty":    `{"original_filename": ""}`,
		"dotdot":   `{"original_filename": ".."}`,
		"overlong": `{"original_filename": "` + strings.Repeat("x", 256) + `"}`,
		"number":   `{"original_filename": 5}`,
		"absent":   `{"original_size_bytes": 12}`,
	}
	for title, meta := range legacy {
		exec(`INSERT INTO assets (title, asset_type, metadata) VALUES ($1, 1, $2::jsonb)`, "m81-"+title, meta)
	}

	if _, err := p.UpTo(ctx, originalBasenameAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", originalBasenameAtVersion, err)
	}
	want := map[string]string{
		"good":     "sketch one.png/client_reported",
		"unicode":  "写真.png/client_reported",
		"path":     "<nil>/unknown",
		"slash":    "<nil>/unknown",
		"control":  "<nil>/unknown",
		"bidi":     "<nil>/unknown",
		"empty":    "<nil>/unknown",
		"dotdot":   "<nil>/unknown",
		"overlong": "<nil>/unknown",
		"number":   "<nil>/unknown",
		"absent":   "<nil>/unknown",
	}
	check := func(stage string) {
		t.Helper()
		rows, err := sqlDB.QueryContext(ctx, `SELECT title, coalesce(original_basename, '<nil>') || '/' || original_basename_source, metadata::text FROM assets WHERE title LIKE 'm81-%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var title, got, meta string
			if err := rows.Scan(&title, &got, &meta); err != nil {
				t.Fatal(err)
			}
			key := strings.TrimPrefix(title, "m81-")
			seen++
			if got != want[key] {
				t.Errorf("%s: %s = %q, want %q", stage, key, got, want[key])
			}
			if !strings.Contains(meta, "original_") {
				t.Errorf("%s: %s metadata lost its claim: %s", stage, key, meta)
			}
		}
		if seen != len(want) {
			t.Fatalf("%s: %d rows, want %d", stage, seen, len(want))
		}
	}
	check("after Up")

	// The guard fires on EVERY UPDATE, not UPDATE OF its two columns. An
	// OF trigger fires only when the UPDATE command names those columns,
	// so another BEFORE UPDATE trigger, ordered first by name, could
	// rewrite NEW.original_basename during an UPDATE that names only
	// title, and the stored provenance would change unguarded.
	var triggerDef string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname = 'assets_original_basename_write_once_trg'`).Scan(&triggerDef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(triggerDef, "BEFORE UPDATE ON public.assets") || strings.Contains(triggerDef, " OF ") {
		// Errorf, not Fatalf: the behaviour below must be checked on its own.
		t.Errorf("write-once trigger is not a plain BEFORE UPDATE trigger: %s", triggerDef)
	}
	exec(`CREATE FUNCTION public.m81_rewrite_basename() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.original_basename := 'rewritten.png';
    NEW.original_basename_source := 'client_reported';
    RETURN NEW;
END;
$$`)
	// "assets_aaa_..." sorts before "assets_original_basename_...", and
	// Postgres fires same-event BEFORE triggers in name order.
	exec(`CREATE TRIGGER assets_aaa_m81_rewrite_basename_trg BEFORE UPDATE ON public.assets
		FOR EACH ROW EXECUTE FUNCTION public.m81_rewrite_basename()`)
	for _, title := range []string{"m81-good", "m81-absent"} {
		_, err := sqlDB.ExecContext(ctx, `UPDATE assets SET title = title || '-retitled' WHERE title = $1`, title)
		if err == nil || !strings.Contains(err.Error(), "write-once") {
			t.Fatalf("%s: a title-only UPDATE that another trigger turned into a provenance change was not refused by the write-once guard (err %v)", title, err)
		}
	}
	exec(`DROP TRIGGER assets_aaa_m81_rewrite_basename_trg ON public.assets`)
	exec(`DROP FUNCTION public.m81_rewrite_basename()`)
	check("after the rewriting trigger was refused")

	refused := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err == nil {
			t.Fatalf("accepted: %s %v", q, args)
		}
	}
	// Write-once: no UPDATE of either column, in either direction.
	refused(`UPDATE assets SET original_basename = 'x.png' WHERE title = 'm81-good'`)
	refused(`UPDATE assets SET original_basename_source = 'upload_request' WHERE title = 'm81-good'`)
	refused(`UPDATE assets SET original_basename = 'x.png', original_basename_source = 'client_reported' WHERE title = 'm81-absent'`)
	// Metadata stays editable and does not reach the typed pair.
	exec(`UPDATE assets SET metadata = '{"original_filename": "renamed.png"}' WHERE title = 'm81-good'`)
	// New rows: the checks hold.
	refused(`INSERT INTO assets (title, asset_type, original_basename, original_basename_source) VALUES ('m81-x', 1, 'a/b.png', 'upload_request')`)
	refused(`INSERT INTO assets (title, asset_type, original_basename, original_basename_source) VALUES ('m81-x', 1, 'a.png', 'unknown')`)
	refused(`INSERT INTO assets (title, asset_type, original_basename_source) VALUES ('m81-x', 1, 'upload_request')`)
	refused(`INSERT INTO assets (title, asset_type, original_basename_source) VALUES ('m81-x', 1, 'server_verified')`)
	refused(`INSERT INTO assets (title, asset_type, original_basename, original_basename_source) VALUES ('m81-x', 1, E'a\x7fb.png', 'upload_request')`)
	exec(`INSERT INTO storage_objects (hash, size_bytes, backend) VALUES ($1, 1, 'fs')`, strings.Repeat("a", 64))
	refused(`INSERT INTO upload_receipts (user_ref, file_hash, original_basename, original_basename_source, expires_at) VALUES (1, $1, '..', 'upload_request', now() + interval '1 hour')`, strings.Repeat("a", 64))
	refused(`INSERT INTO upload_receipts (user_ref, file_hash, original_basename_source, expires_at) VALUES (1, $1, 'client_reported', now() + interval '1 hour')`, strings.Repeat("a", 64))
	refused(`INSERT INTO upload_receipts (user_ref, file_hash, original_basename_source, expires_at) VALUES (1, $1, 'unknown', now() - interval '1 hour')`, strings.Repeat("a", 64))
	exec(`INSERT INTO upload_receipts (user_ref, file_hash, original_basename, original_basename_source, expires_at) VALUES (1, $1, 'ok.png', 'upload_request', now() + interval '1 hour')`, strings.Repeat("a", 64))
	// A receipt goes with its storage object.
	exec(`DELETE FROM storage_objects WHERE hash = $1`, strings.Repeat("a", 64))
	var receipts int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM upload_receipts`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("receipts left after their object went: %d (%v)", receipts, err)
	}

	// Down removes everything; Up again backfills again from the (edited)
	// metadata, still only as client_reported.
	if _, err := p.DownTo(ctx, originalBasenameBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", originalBasenameBeforeVersion, err)
	}
	var cols, tables, triggers int
	_ = sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'assets' AND column_name LIKE 'original_basename%'`).Scan(&cols)
	_ = sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'upload_receipts'`).Scan(&tables)
	_ = sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname = 'assets_original_basename_write_once_trg'`).Scan(&triggers)
	if cols != 0 || tables != 0 || triggers != 0 {
		t.Fatalf("after Down: %d columns, %d tables, %d triggers remain", cols, tables, triggers)
	}
	if _, err := p.UpTo(ctx, originalBasenameAtVersion); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	want["good"] = "renamed.png/client_reported"
	check("after Up again")
}
