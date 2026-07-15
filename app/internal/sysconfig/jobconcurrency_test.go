// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #278 — the boot path loads the seeded jobs.type_concurrency.<type>
// caps into the worker Pool. This exercises the sysconfig accessor
// that reads those scalar rows, including the parse/skip semantics.
package sysconfig_test

import (
	"context"
	"os"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

func TestGetJobTypeConcurrency(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	ctx := context.Background()
	pool := openPool(t, pwd)
	defer pool.Close()

	// The baseline migration seeds ai.tag=4, ai.caption=2, ai.embed=8,
	// ai.transcribe=1. Add a 0 (uncapped) row and a non-numeric row to
	// prove those are skipped rather than surfaced as caps.
	extras := []string{
		"jobs.type_concurrency.preview.raster",
		"jobs.type_concurrency.ai.badval",
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO system_config (key, value, updated_at) VALUES
			('jobs.type_concurrency.preview.raster', '0', now()),
			('jobs.type_concurrency.ai.badval', '"nope"', now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("seed extras: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM system_config WHERE key = ANY($1)`, extras)
	})

	store := sysconfig.NewStore(pool)
	caps, err := store.GetJobTypeConcurrency(ctx)
	if err != nil {
		t.Fatalf("GetJobTypeConcurrency: %v", err)
	}

	// Seeded caps come through with their values.
	for typ, want := range map[string]int{
		"ai.tag": 4, "ai.caption": 2, "ai.embed": 8, "ai.transcribe": 1,
	} {
		if caps[typ] != want {
			t.Errorf("caps[%q] = %d, want %d", typ, caps[typ], want)
		}
	}
	// 0 = uncapped → absent; non-numeric → skipped.
	if _, ok := caps["preview.raster"]; ok {
		t.Errorf("0-value cap must be treated as uncapped/absent, got %d", caps["preview.raster"])
	}
	if _, ok := caps["ai.badval"]; ok {
		t.Error("non-numeric cap value must be skipped")
	}
}
