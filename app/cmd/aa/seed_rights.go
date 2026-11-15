// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/seed"
)

// `aa seed-rights ...` drafts or checks the file-complete rights ledger
// for a CI selection report (#1514). It reads only the files it is given
// and writes only to stdout, so the caller chooses where a private draft
// lands. No config, database, network or default path.
//
//	aa seed-rights --selection <report> --draft            > ledger.json
//	aa seed-rights --selection <report> --ledger <file> --check
//
// --check exits non-zero unless every entry is reviewed_allow AND the
// report's companion closure is complete (no unknown_model_format
// assets). A passing --check is STRUCTURAL ONLY: never legal clearance or
// authorization to publish.
func runSeedRights(args []string) error {
	return runSeedRightsTo(os.Stdout, os.Stderr, args)
}

func runSeedRightsTo(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("seed-rights", flag.ContinueOnError)
	selection := fs.String("selection", "", "selection report from `aa seed-selection`; required")
	ledger := fs.String("ledger", "", "ledger to check (with --check)")
	draft := fs.Bool("draft", false, "print a new all-pending ledger for --selection to stdout")
	check := fs.Bool("check", false, "check --ledger against --selection; exits non-zero unless every row is "+
		"reviewed_allow and the report has no unknown_model_format assets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *selection == "" {
		return errors.New("seed-rights: --selection is required")
	}
	if *draft == *check {
		return errors.New("seed-rights: give exactly one of --draft or --check")
	}
	sel, sum, err := seed.LoadCISelectionReport(*selection)
	if err != nil {
		return err
	}
	if *draft {
		if *ledger != "" {
			return errors.New("seed-rights: --ledger is not used with --draft")
		}
		b, err := seed.DraftRightsLedger(sel, sum)
		if err != nil {
			return err
		}
		_, err = stdout.Write(b)
		return err
	}
	if *ledger == "" {
		return errors.New("seed-rights: --check needs --ledger")
	}
	l, err := seed.LoadRightsLedger(*ledger)
	if err != nil {
		return err
	}
	res, err := seed.CheckRightsLedger(sel, sum, l)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", b); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "seed-rights: "+seed.RightsStructuralOnly)
	if !res.Eligible {
		return fmt.Errorf("seed-rights: structure is consistent but NOT release-eligible: %s",
			strings.Join(res.Ineligible, "; "))
	}
	return nil
}
