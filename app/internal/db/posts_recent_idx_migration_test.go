// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25b: migration 00072 walks 71 -> 72 -> 71 -> 72 and the
// index it adds is exactly the keyset the recent window's post arm
// orders and positions on.

package db

import (
	"context"
	"database/sql"
	"testing"
)

const (
	priBeforeVersion = 71 // 00071_kind_is_searchable_vocabulary
	priAtVersion     = 72 // 00072_posts_recent_idx
)

func priIndexDef(t *testing.T, sqlDB *sql.DB) (string, bool) {
	t.Helper()
	var def string
	err := sqlDB.QueryRowContext(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'posts_recent_idx'`).Scan(&def)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	return def, true
}

func TestMigration00072_PostsRecentIdx_UpDownUp(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := kvmProvider(t, sqlDB)

	if _, err := p.UpTo(ctx, priBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", priBeforeVersion, err)
	}
	if _, ok := priIndexDef(t, sqlDB); ok {
		t.Fatalf("posts_recent_idx exists at v%d", priBeforeVersion)
	}

	const want = "CREATE INDEX posts_recent_idx ON public.posts USING btree (posted_at DESC, id DESC) WHERE (deleted_at IS NULL)"
	for round := 0; round < 2; round++ {
		if _, err := p.UpTo(ctx, priAtVersion); err != nil {
			t.Fatalf("round %d: migrate up to %d: %v", round, priAtVersion, err)
		}
		def, ok := priIndexDef(t, sqlDB)
		if !ok {
			t.Fatalf("round %d: posts_recent_idx missing at v%d", round, priAtVersion)
		}
		if def != want {
			t.Errorf("round %d: index is\n%s\nwant\n%s", round, def, want)
		}
		if round == 1 {
			break
		}
		if _, err := p.DownTo(ctx, priBeforeVersion); err != nil {
			t.Fatalf("migrate down to %d: %v", priBeforeVersion, err)
		}
		if _, ok := priIndexDef(t, sqlDB); ok {
			t.Fatalf("posts_recent_idx survived the Down")
		}
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil || v != priAtVersion {
		t.Errorf("version %d %v, want %d", v, err, priAtVersion)
	}
}
