// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	"github.com/mscrnt/artist-alley/app/internal/fixturesweep"
)

// runSweepFixtures is `aa sweep-fixtures` (#1245): remove the rows that
// dogfood and integration runs have left in a long-lived development
// database, without touching the seeded corpus.
//
// It exists because the shared coding stack is deliberately persistent —
// reseeding costs 10-15 minutes — so litter accumulates permanently.
// By 2026-08-20 it was 44% of the asset table, and it had displaced the
// newest-200 window far enough that nine dogfood specs could not pass
// there for reasons that had nothing to do with what they assert.
//
// DRY RUN IS THE DEFAULT. Deleting requires -apply, and even then the
// sweep aborts before its first DELETE if any rule matches a row it also
// classifies as real. See the fixturesweep package for the rules and the
// evidence behind each one.
func runSweepFixtures(args []string) error {
	fs := flag.NewFlagSet("sweep-fixtures", flag.ContinueOnError)
	apply := fs.Bool("apply", false,
		"actually delete. Omit for a dry run, which reports what WOULD be removed "+
			"and rolls back without writing")
	cataloguePath := fs.String("catalogue", "seed/profiles",
		"catalogue directory; dataset.collections.json names the real "+
			"collections and studio-*.posts.json the real posts")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cat, err := loadSeedCatalogue(*cataloguePath)
	if err != nil {
		return err
	}

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	fmt.Printf("database : %s\n", cfg.DBName)
	fmt.Printf("mode     : %s\n", map[bool]string{true: "APPLY (deletes rows)", false: "DRY RUN (no writes)"}[*apply])
	fmt.Printf("catalogue: %s (%d real collection names, %d real post ids)\n\n",
		*cataloguePath, len(cat.CollectionNames), len(cat.PostIDs))

	rep, rErr := fixturesweep.Run(ctx, pool, cat, *apply)
	if rep != nil {
		fmt.Print(rep.String())
	}
	if rErr != nil {
		var ce *fixturesweep.ErrContradiction
		if errors.As(rErr, &ce) {
			fmt.Fprintf(os.Stderr, "\nABORTED: %v\n", ce)
		}
		return rErr
	}

	if !*apply {
		fmt.Printf("\nDry run — nothing was written. Re-run with -apply to delete.\n")
	} else {
		fmt.Printf("\nApplied. Blobs in object storage are NOT removed here; the storage\n" +
			"sweep (storage_sweep_runs) is what reclaims orphaned bytes.\n")
	}
	return nil
}

// loadSeedCatalogue reads what the seed profiles say is REAL, rather
// than hardcoding it, so the rules track the dataset instead of drifting
// from it — which is precisely how the posts rule came to claim that
// "fixture posts are all authored by the bootstrap admin" long after
// #1270 stopped making that true (#1276).
func loadSeedCatalogue(dir string) (fixturesweep.Catalogue, error) {
	names, err := loadSeedCollectionNames(dir)
	if err != nil {
		return fixturesweep.Catalogue{}, err
	}
	ids, err := loadSeedPostIDs(dir)
	if err != nil {
		return fixturesweep.Catalogue{}, err
	}
	return fixturesweep.Catalogue{CollectionNames: names, PostIDs: ids}, nil
}

// loadSeedPostIDs reads the real post ids out of the per-studio post
// profiles. Both studios, because a stack may hold either site and the
// protected set has to cover whichever it holds — a post id missing from
// the set is a REAL post the sweep would stop protecting.
//
// ⛔ It refuses on an empty result for the same reason the collections
// loader does: sweeping with an empty protected set classifies the whole
// table as fixtures. That is the shape of an unrecoverable mistake, and
// "the file moved" must not be able to cause it.
func loadSeedPostIDs(dir string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	var read int
	for _, name := range []string{"studio-a.posts.json", "studio-b.posts.json"} {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// One site's profile may legitimately be absent on a
				// trimmed checkout; ALL of them absent is not.
				continue
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		read++
		var entries []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, e := range entries {
			if e.ID != "" && !seen[e.ID] {
				seen[e.ID] = true
				out = append(out, e.ID)
			}
		}
	}
	if read == 0 {
		return nil, fmt.Errorf(
			"no studio-*.posts.json under %s; refusing to sweep with an empty "+
				"protected set (pass -catalogue if the seed profiles live elsewhere)", dir)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"%s named no posts; refusing to sweep with an empty protected set", dir)
	}
	return out, nil
}

// loadSeedCollectionNames reads the real collection names from the seed
// catalogue rather than hardcoding them, so the rule tracks the dataset
// instead of drifting from it.
func loadSeedCollectionNames(dir string) ([]string, error) {
	path := filepath.Join(dir, "dataset.collections.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (pass -catalogue if the seed profiles live elsewhere)", path, err)
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name != "" {
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		// Refuse rather than sweep with an empty protected set, which
		// would classify every collection as a fixture.
		return nil, fmt.Errorf("%s named no collections; refusing to sweep with an empty protected set", path)
	}
	return names, nil
}
