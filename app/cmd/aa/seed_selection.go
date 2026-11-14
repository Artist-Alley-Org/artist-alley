// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"errors"
	"flag"
	"fmt"

	"github.com/mscrnt/artist-alley/app/internal/seed"
)

// `aa seed-selection ...` writes the exact selection `aa seed --profile ci`
// would load from a site root, as a versioned JSON report (#1514). It is
// offline: no config, database, master key or storage backend, and it
// writes nothing under the site root or the catalogue root. See
// internal/seed/export.go.
func runSeedSelection(args []string) error {
	fs := flag.NewFlagSet("seed-selection", flag.ContinueOnError)
	site := fs.String("site", "", "site root (MANIFEST.json + posts.json + bytes); read only")
	catalogue := fs.String("catalogue", "seed/profiles", "catalogue directory (seed/profiles)")
	coverageDepth := fs.Int("coverage-depth", 0,
		"minimum posts per collection and assets per extension (0 = the same built-in default `aa seed --profile ci` uses)")
	overlapContract := fs.String("overlap-contract", "",
		"also include --overlap-peer's overlap anchors from this contract (#1514); opt-in, no default. "+
			"The report is then schema "+seed.CISelectionIncludesSchema)
	overlapPeer := fs.String("overlap-peer", "", "with --overlap-contract: the contract peer this site is; never inferred")
	out := fs.String("out", "", "report path to write (JSON, mode 0600, replaced atomically); "+
		"must be outside both --site and --catalogue, including through symlinks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *site == "" {
		return errors.New("seed-selection: --site is required")
	}
	if *out == "" {
		return errors.New("seed-selection: --out is required")
	}
	includes, err := seed.CheckDeclaredIncludeFlags(*overlapContract, *overlapPeer, seed.ProfileCI)
	if err != nil {
		return fmt.Errorf("seed-selection: %w", err)
	}
	rep, err := seed.ExportCISelection(seed.CISelectionOptions{
		SiteRoot:      *site,
		CatalogueRoot: *catalogue,
		CoverageDepth: *coverageDepth,
		Includes:      includes,
	}, *out)
	if err != nil {
		return err
	}
	n := rep.Counts
	fmt.Printf("seed-selection: report written: %d/%d assets, %d/%d posts, %d files "+
		"(%d primary, %d companion), %d unique bytes\n",
		n.AssetsSelected, n.AssetsTotal, n.PostsSelected, n.PostsTotal,
		n.UniqueFiles, n.PrimaryFiles, n.CompanionFiles, n.UniqueFileBytes)
	if d := rep.DeclaredIncludes; d != nil {
		added := 0
		for _, a := range d.Anchors {
			if a.AddedByInclude {
				added++
			}
		}
		fmt.Printf("seed-selection: declared includes: %d %s anchor(s) from %s (%d added beyond coverage); "+
			"schema %s. Included is not reviewed.\n", len(d.Anchors), d.Peer, d.Contract, added, rep.Schema)
	}
	if n.UnknownModelAssets > 0 {
		// Not a failure, but the package gate has to see it: these
		// models' companions were not read, so the closure may be short.
		fmt.Printf("seed-selection: WARNING: %d selected 3D asset(s) use a format whose companions "+
			"cannot be read; see companion_support=%s in the report\n",
			n.UnknownModelAssets, seed.CompanionsUnknownModel)
	}
	return nil
}
