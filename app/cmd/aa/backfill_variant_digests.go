// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	aahttp "github.com/mscrnt/artist-alley/app/internal/http"
	"github.com/mscrnt/artist-alley/app/internal/storage"
)

// runBackfillVariantDigests is `aa backfill-variant-digests` (#1597): the
// explicit recheck of every storage_variants row that has no trustworthy
// digest (every row older than migration 00082, rows healed from a
// backend Stat, interrupted writes).
//
// Dry run by default: it counts the rows without a digest (unverifiable,
// or marked missing with none) and reads nothing.
// With -apply it rechecks each one under its write lock: the stored bytes
// are read back and their digest recorded (digest_source 'recheck') only
// when they agree with what the row already says (an original's address,
// a derived variant's recorded size); anything else stays unverifiable
// and is counted by outcome. It never overwrites a digest recorded at
// write time. Safe to stop and run again: it resumes from the start and
// skips rows it already verified.
func runBackfillVariantDigests(args []string) error {
	fs := flag.NewFlagSet("backfill-variant-digests", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "recheck and record digests. Omit for a dry run that only counts rows without a digest")
	batch := fs.Int("batch", 200, "rows per page")
	limit := fs.Int("limit", 0, "stop after this many rows (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *batch < 1 {
		return fmt.Errorf("-batch must be at least 1")
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

	afterHash, afterVariant := "", ""
	total := 0
	outcomes := map[string]int{}
	if !*apply {
		q := storage.New(pool)
		for {
			rows, err := q.ListVariantsWithoutDigest(ctx, storage.ListVariantsWithoutDigestParams{
				AfterHash: afterHash, AfterVariant: afterVariant, MaxRows: int32(*batch),
			})
			if err != nil {
				return err
			}
			total += len(rows)
			if len(rows) < *batch || (*limit > 0 && total >= *limit) {
				break
			}
			afterHash, afterVariant = rows[len(rows)-1].ObjectHash, rows[len(rows)-1].VariantKey
		}
		fmt.Printf("dry run: %d variant row(s) without a digest (unverifiable, or missing with none); rerun with -apply to recheck them\n", total)
		return nil
	}

	backend, err := aahttp.BuildStorageBackend(cfg)
	if err != nil {
		return fmt.Errorf("storage backend: %w", err)
	}
	svc := storage.NewService(backend, pool)
	for {
		n := *batch
		if *limit > 0 && *limit-total < n {
			n = *limit - total
		}
		if n <= 0 {
			break
		}
		b, err := svc.BackfillVariantDigests(ctx, afterHash, afterVariant, int32(n))
		if err != nil {
			return err
		}
		total += b.Scanned
		for k, v := range b.Outcomes {
			outcomes[k] += v
		}
		if b.Done || b.Scanned == 0 {
			break
		}
		afterHash, afterVariant = b.NextHash, b.NextVariant
	}
	keys := make([]string, 0, len(outcomes))
	for k := range outcomes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("rechecked %d variant row(s)\n", total)
	for _, k := range keys {
		fmt.Printf("  %-14s %d\n", k, outcomes[k])
	}
	return nil
}
