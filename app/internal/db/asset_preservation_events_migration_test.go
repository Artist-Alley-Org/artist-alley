// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1598: migration 00083 adds asset_preservation_events and makes sweep
// findings retry-safe. Pinned against a real, fresh database:
//
//   - exact duplicate findings already present (a batch retried before
//     the migration) collapse to the earliest; distinct findings stay;
//   - afterwards a duplicate finding is refused, and the events table,
//     its append-only trigger and its idempotency key exist; an event
//     outlives its sweep run (the snapshot stays, the live reference is
//     cleared) and goes only with its asset;
//   - Down removes the table, the function and the findings key (a
//     duplicate is accepted again), and Up again restores them.

package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	preservationEventsBeforeVersion = 82 // 00082_storage_variant_fixity
	preservationEventsAtVersion     = 83 // 00083_asset_preservation_events
)

func TestMigration00083_AssetPreservationEvents_CollapsesDuplicateFindingsAndRoundTrips(t *testing.T) {
	cfg := freshDatabase(t)
	sqlDB := openCfg(t, cfg)
	ctx := t.Context()
	p := afvhProvider(t, sqlDB)
	if _, err := p.UpTo(ctx, preservationEventsBeforeVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", preservationEventsBeforeVersion, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run := uuid.New()
	hash := strings.Repeat("a", 64)
	exec(`INSERT INTO storage_sweep_runs (id, kind) VALUES ($1, 'checksum_verify')`, run)
	finding := func(id uuid.UUID, variant, kind, at string) {
		t.Helper()
		exec(`INSERT INTO storage_sweep_findings (id, run_id, finding, object_hash, variant_key, detected_at) VALUES ($1, $2, $3, $4, $5, $6::timestamptz)`,
			id, run, kind, hash, variant, at)
	}
	earliest := uuid.New()
	finding(uuid.New(), "preview", "checksum_mismatch", "2026-11-01T10:05:00Z")
	finding(earliest, "preview", "checksum_mismatch", "2026-11-01T10:00:00Z")
	finding(uuid.New(), "preview", "checksum_mismatch", "2026-11-01T10:09:00Z")
	finding(uuid.New(), "preview", "size_mismatch", "2026-11-01T10:01:00Z")   // another finding kind
	finding(uuid.New(), "hires", "checksum_mismatch", "2026-11-01T10:02:00Z") // another variant

	if _, err := p.UpTo(ctx, preservationEventsAtVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", preservationEventsAtVersion, err)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM storage_sweep_findings WHERE run_id = $1`, run); n != 3 {
		t.Fatalf("after Up: %d findings, want 3 (the duplicates collapsed)", n)
	}
	var kept uuid.UUID
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM storage_sweep_findings WHERE run_id = $1 AND variant_key = 'preview' AND finding = 'checksum_mismatch'`, run).Scan(&kept); err != nil || kept != earliest {
		t.Fatalf("kept %s (%v), want the earliest %s", kept, err, earliest)
	}
	dup := func() error {
		_, err := sqlDB.ExecContext(ctx, `INSERT INTO storage_sweep_findings (id, run_id, finding, object_hash, variant_key) VALUES ($1, $2, 'checksum_mismatch', $3, 'preview')`,
			uuid.New(), run, hash)
		return err
	}
	if err := dup(); err == nil {
		t.Fatal("after Up: a duplicate finding was accepted")
	}
	present := func(stage string, want bool) {
		t.Helper()
		objs := map[string]string{
			"table":   `SELECT count(*) FROM pg_class WHERE relname = 'asset_preservation_events' AND relkind = 'r'`,
			"key":     `SELECT count(*) FROM pg_indexes WHERE indexname = 'asset_preservation_events_sweep_item_key'`,
			"trigger": `SELECT count(*) FROM pg_trigger WHERE tgname = 'asset_preservation_events_append_only_trg'`,
			"func":    `SELECT count(*) FROM pg_proc WHERE proname = 'asset_preservation_events_append_only'`,
			"finding": `SELECT count(*) FROM pg_indexes WHERE indexname = 'storage_sweep_findings_item_key'`,
		}
		for name, q := range objs {
			if got := count(q) == 1; got != want {
				t.Errorf("%s: %s present = %v, want %v", stage, name, got, want)
			}
		}
	}
	present("after Up", true)
	appendOnly(t, sqlDB, hash)

	if _, err := p.DownTo(ctx, preservationEventsBeforeVersion); err != nil {
		t.Fatalf("migrate down to %d: %v", preservationEventsBeforeVersion, err)
	}
	present("after Down", false)
	if err := dup(); err != nil {
		t.Fatalf("after Down: a duplicate finding was refused: %v", err)
	}
	exec(`DELETE FROM storage_sweep_findings WHERE run_id = $1 AND variant_key = 'preview' AND finding = 'checksum_mismatch' AND id <> $2`, run, earliest)
	if _, err := p.UpTo(ctx, preservationEventsAtVersion); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	present("after Up again", true)
}

// appendOnly writes one event straight into the migrated table and
// confirms the trigger refuses UPDATE and DELETE, that deleting its run
// keeps it (snapshot kept, live reference cleared), and that deleting
// its asset removes it. Its own run, so the findings above are
// untouched.
func appendOnly(t *testing.T, sqlDB *sql.DB, hash string) {
	t.Helper()
	ctx := t.Context()
	run := uuid.New()
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO storage_sweep_runs (id, kind) VALUES ($1, 'checksum_verify')`, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	asset := uuid.New()
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO assets (id, title, asset_type) VALUES ($1, 'migration event', (SELECT MIN(ref) FROM asset_types))`, asset); err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO asset_preservation_events (asset_id, event_type, outcome, outcome_reason, object_hash, variant_key,
		observed_generation, fixity_status, sweep_run_id, sweep_run_ref, agent_name) VALUES ($1, 'fixity-check', 'failure', 'missing_object', $2, 'preview', 1, 'missing', $3, $3, 'artist-alley')`,
		asset, hash, run); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	for _, q := range []string{
		`UPDATE asset_preservation_events SET fixity_status = 'verified' WHERE asset_id = $1`,
		`DELETE FROM asset_preservation_events WHERE asset_id = $1`,
	} {
		if _, err := sqlDB.ExecContext(ctx, q, asset); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v, want the append-only refusal", q, err)
		}
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM storage_sweep_runs WHERE id = $1`, run); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var kept int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id = $1 AND sweep_run_id = $2 AND sweep_run_ref IS NULL`,
		asset, run).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("after deleting its run: %d events kept with the snapshot (%v), want 1", kept, err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DELETE FROM assets WHERE id = $1`, asset); err != nil {
		t.Fatalf("delete asset: %v", err)
	}
	var left int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM asset_preservation_events WHERE asset_id = $1`, asset).Scan(&left); err != nil || left != 0 {
		t.Fatalf("the asset's events survived it: %d (%v)", left, err)
	}
}
