// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/mscrnt/artist-alley/app/internal/seed"
)

// `aa seed-package ...` builds the LOCAL CI seed package (#1514): a
// deterministic site-root tar.gz of the selected MANIFEST.json and
// posts.json rows and exactly the selection's closure files. Offline: no
// config, database, network or storage backend, and no default paths.
//
//	aa seed-package --site <root> --catalogue <root> --selection <report> \
//	    --ledger <ledger> --output <archive.tar.gz>
//
// It installs no new archive unless the rights ledger is structurally
// eligible and every pinned input and closure file still matches the
// report, and the report and ledger are unchanged when the archive is
// installed. On refusal an existing archive at --output is left as it was.
// Structural eligibility is NOT legal clearance: the archive is a local
// candidate artifact, not release approval or permission to publish.
func runSeedPackage(args []string) error {
	return runSeedPackageTo(os.Stdout, args)
}

func runSeedPackageTo(stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("seed-package", flag.ContinueOnError)
	site := fs.String("site", "", "site root the report describes; read only; required")
	catalogue := fs.String("catalogue", "", "catalogue directory whose files the report pins; read only; required")
	selection := fs.String("selection", "", "selection report from `aa seed-selection`; required")
	ledger := fs.String("ledger", "", "rights ledger bound to --selection; every row must be reviewed_allow; required")
	output := fs.String("output", "", "archive to write (tar.gz, mode 0600, installed atomically); "+
		"must be outside both --site and --catalogue, including through symlinks; required")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("seed-package: unexpected argument %q", fs.Arg(0))
	}
	for _, f := range []struct{ flag, v string }{
		{"--site", *site}, {"--catalogue", *catalogue}, {"--selection", *selection}, {"--ledger", *ledger}, {"--output", *output},
	} {
		if f.v == "" {
			return errors.New("seed-package: " + f.flag + " is required")
		}
	}
	res, err := seed.BuildCIPackage(seed.CIPackageOptions{
		SiteRoot: *site, CatalogueRoot: *catalogue, SelectionPath: *selection, LedgerPath: *ledger,
	}, *output)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "seed-package: wrote %s: %d entries (%d closure files, %d assets, %d posts), "+
		"%d bytes, sha256 %s\n", *output, res.Entries, res.Files, res.Assets, res.Posts, res.Bytes, res.SHA256)
	fmt.Fprintln(stdout, "seed-package: "+seed.CIPackageNotice)
	return nil
}
