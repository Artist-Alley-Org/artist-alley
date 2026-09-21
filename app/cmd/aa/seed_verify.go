// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/mscrnt/artist-alley/app/internal/config"
	"github.com/mscrnt/artist-alley/app/internal/db"
	"github.com/mscrnt/artist-alley/app/internal/logging"
	"github.com/mscrnt/artist-alley/app/internal/seed"
)

// `aa seed-verify ...` is the read-only counterpart of `aa seed` (#1319):
// it reads the same site root and catalogue the seeder read and asks
// whether the database holds every value the catalogue carries, exactly
// as the seeder would have written it, and nothing seed-owned that the
// catalogue does not carry. See internal/seed/verify.go for the rules.
//
// It opens a pool and nothing else: no migrate, no bootstrap, no storage
// backend, no writes. Exit 1 when any invariant failed, after printing
// every verdict.

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// parseAssetExpectation reads `<id>:<ai_provenance>:<size_bytes>`, where
// ai_provenance may be `-` for undeclared and size_bytes `0` for
// unchecked.
func parseAssetExpectation(s string) (seed.AssetExpectation, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return seed.AssetExpectation{}, fmt.Errorf("expect-asset %q: want <id>:<ai_provenance|->:<size_bytes>", s)
	}
	size, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || size < 0 {
		return seed.AssetExpectation{}, fmt.Errorf("expect-asset %q: size_bytes must be a non-negative integer", s)
	}
	prov := parts[1]
	if prov == "-" {
		prov = ""
	}
	return seed.AssetExpectation{ID: parts[0], AiProvenance: prov, SizeBytes: size}, nil
}

func runSeedVerify(args []string) error {
	fs := flag.NewFlagSet("seed-verify", flag.ContinueOnError)
	site := fs.String("site", "", "seeded site root (MANIFEST.json + posts.json + bytes)")
	catalogue := fs.String("catalogue", "seed/profiles", "catalogue directory (seed/profiles)")
	migration := fs.String("migration", "",
		"post-id migration document (seed/upgrades/post-id-migration.<stem>.json); "+
			"every new_id must be live and every old_id must not be")
	var once, expectAssets stringList
	fs.Var(&once, "expect-once",
		"post id that must appear exactly once in posts.json and be live (repeatable)")
	fs.Var(&expectAssets, "expect-asset",
		"<id>:<ai_provenance|->:<size_bytes> that must hold in the database (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return errors.New("seed-verify: --site is required")
	}
	var expectations []seed.AssetExpectation
	for _, s := range expectAssets {
		e, err := parseAssetExpectation(s)
		if err != nil {
			return err
		}
		expectations = append(expectations, e)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.Setup(cfg.LogLevel, cfg.LogFormat)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := db.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	rep, err := seed.Verify(ctx, pool, seed.VerifyOptions{
		SiteRoot:          *site,
		CatalogueRoot:     *catalogue,
		MigrationDocument: *migration,
		ExpectOnce:        once,
		ExpectAssets:      expectations,
		Logger:            logger,
	})
	if err != nil {
		return err
	}
	for _, n := range rep.Notes {
		fmt.Printf("note: %s\n", n)
	}
	for _, f := range rep.Failures {
		fmt.Printf("FAIL: %s\n", f)
	}
	fmt.Print(rep.Summary())
	if !rep.OK() {
		return fmt.Errorf("seed-verify: %d invariant(s) failed", len(rep.Failures))
	}
	return nil
}
