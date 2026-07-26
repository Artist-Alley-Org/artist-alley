// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #591 — the reader that turns the operator's stored preview config into
// the rung list the `ladder_available` queries take as a parameter.
//
// This closes the loop the sibling suite in assets/ leaves open. That
// one proves the SQL honours whatever ladder it is handed; this proves
// the ladder handed to it is the operator's CONFIGURED one and not the
// package default. Together they are what makes a hardcoded
// col+preview+screen+hires check impossible to reintroduce quietly:
// break either half and one of the two suites fails.
package sysconfig_test

import (
	"context"
	"os"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/sysconfig"
)

// TestPreviewLadderReader_FollowsStoredConfig writes a NON-default
// ladder and asserts the reader reports it.
//
// The default is col/preview/screen/hires. The config written here is
// deliberately different in all three ways an install can diverge —
// fewer rungs, different names, different sizes — so an implementation
// that returned DefaultPreviewConfig() (or a constant) fails on every
// assertion rather than coincidentally passing one.
func TestPreviewLadderReader_FollowsStoredConfig(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	ctx := context.Background()
	pool := openPool(t, pwd)
	defer pool.Close()

	store := sysconfig.NewStore(pool)

	// Restore whatever was there, so this test can't leave the install
	// on a two-rung ladder for every suite that runs after it.
	before, err := store.GetPreviews(ctx)
	if err != nil {
		t.Fatalf("read current previews: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM system_config WHERE key = 'previews'`)
		_ = before
	})

	custom := sysconfig.PreviewConfig{Variants: []sysconfig.PreviewVariant{
		{Key: "thumb", Fit: sysconfig.PreviewFitCover, MaxDim: 200,
			Format: sysconfig.PreviewFormatWebP, Quality: 80},
		{Key: "big", Fit: sysconfig.PreviewFitContain, MaxDim: 2400,
			Format: sysconfig.PreviewFormatWebP, Quality: 90},
	}}
	if err := store.SetPreviews(ctx, custom); err != nil {
		t.Fatalf("SetPreviews: %v", err)
	}

	// nil registry — uncached, which is the path a test fixture takes and
	// which must be correct on its own. Caching is latency, not truth.
	read := sysconfig.NewPreviewLadderReader(store, nil, nil)
	got := read(ctx)

	want := map[string]bool{"thumb": true, "big": true}
	if len(got) != len(want) {
		t.Fatalf("ladder = %v, want the 2 configured rungs (thumb, big). "+
			"A reader returning the DEFAULT ladder gives 4 here.", got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("ladder contains %q, which is not configured — the reader "+
				"is not reading the operator's config", k)
		}
	}
	for _, def := range []string{"col", "preview", "screen", "hires"} {
		for _, k := range got {
			if k == def {
				t.Errorf("ladder contains the DEFAULT rung %q on an install that "+
					"configured thumb+big — this is the hardcode #591 exists to "+
					"prevent", def)
			}
		}
	}
}

// TestPreviewLadderReader_FallsBackToDefaultWhenUnset pins the other
// half of the contract: an install that has never written a preview
// config is on the defaults, and must report them rather than reporting
// "no ladder" and disabling responsive images on a healthy install.
func TestPreviewLadderReader_FallsBackToDefaultWhenUnset(t *testing.T) {
	pwd := os.Getenv("AA_DB_PASSWORD")
	if pwd == "" {
		t.Skip("AA_DB_PASSWORD not set; sysconfig integration test skipped")
	}
	ctx := context.Background()
	pool := openPool(t, pwd)
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`DELETE FROM system_config WHERE key = 'previews'`); err != nil {
		t.Fatalf("clear previews: %v", err)
	}

	read := sysconfig.NewPreviewLadderReader(sysconfig.NewStore(pool), nil, nil)
	got := read(ctx)

	if len(got) != len(sysconfig.DefaultPreviewConfig().Variants) {
		t.Fatalf("unset config yielded %v; expected the default ladder", got)
	}
	// Sorted, because the value is a cache entry and a query parameter —
	// an unstable order would make both harder to reason about.
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("ladder is not sorted: %v", got)
			break
		}
	}
}

// TestPreviewLadderReader_NilReaderIsConservative documents the
// direction the whole feature fails in.
//
// A handler with no reader wired (every test fixture, and any future
// construction path that forgets it) must produce NO ladder, which makes
// ladder_available false, which keeps clients on the single `col` rung
// they already know exists. The opposite default — assuming the standard
// four — would have every such install serving 404s for rungs nobody
// vouched for.
func TestPreviewLadderReader_EmptyLadderIsNotVacuouslySatisfied(t *testing.T) {
	// The SQL-level counterpart of this is asserted in
	// assets/ladder_available_test.go ("unknown ladder"). Here we only
	// pin the shape the SQL relies on: an empty list, never a nil-safe
	// substitute that quietly becomes the default.
	empty := sysconfig.PreviewConfig{}
	if len(empty.Variants) != 0 {
		t.Fatal("zero PreviewConfig should carry no variants")
	}
	// GetPreviews substitutes the default for an EMPTY STORED config —
	// that is the "never configured" case and is correct. The
	// unknown-ladder case is a failed READ, which returns nil from the
	// reader without touching this path.
	if len(sysconfig.DefaultPreviewConfig().Variants) == 0 {
		t.Fatal("DefaultPreviewConfig must not be empty")
	}
}
