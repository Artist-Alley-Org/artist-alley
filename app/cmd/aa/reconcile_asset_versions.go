// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mscrnt/artist-alley/app/internal/assets"
	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
)

// runReconcileAssetVersions is `aa reconcile-asset-versions` (#1596): the
// operator path for the versions migration 00084 backfilled as drafts.
//
// Dry run by default: it counts the backfilled drafts and splits their
// object hashes into those whose original is verified (#1597) and can be
// reconciled now, and those that cannot until `aa backfill-variant-digests
// -apply` has verified the original. Nothing is sealed over unverified
// bytes, so the second group is only reported.
//
// With -apply it requests one asset.version_reconcile job per eligible
// hash (coalesced with any already queued). Enqueue only: the server's
// worker pool does the work, at the type's concurrency cap.
func runReconcileAssetVersions(args []string) error {
	fs := flag.NewFlagSet("reconcile-asset-versions", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "request a reconcile for every eligible hash. Omit for a dry run that only counts")
	limit := fs.Int("limit", 0, "request at most this many hashes (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	eligible, ineligible, drafts, err := assets.BackfilledDrafts(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("%d backfilled draft version(s) on %d hash(es): %d with a verified original, %d without (run aa backfill-variant-digests -apply first)\n",
		drafts, len(eligible)+len(ineligible), len(eligible), len(ineligible))
	if !*apply {
		fmt.Println("dry run: rerun with -apply to request their reconciliation")
		return nil
	}
	n := 0
	for _, h := range eligible {
		if *limit > 0 && n >= *limit {
			break
		}
		if err := assets.EnqueueVersionReconcile(ctx, pool, h); err != nil {
			return fmt.Errorf("request reconcile of %s: %w", h, err)
		}
		n++
	}
	fmt.Printf("requested reconciliation of %d hash(es)\n", n)
	return nil
}
