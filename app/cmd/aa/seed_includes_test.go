// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every invalid declared-include request to `aa seed` must fail BEFORE
// config, migrate, the bootstrap admin or --reset (#1514). This test has
// no database and no config: each error below can only come from the
// include rules and the offline preflight, which run first. A request
// that slipped past them would instead fail later on config or the
// database, with a different message.
func TestRunSeed_DeclaredIncludesRefusedBeforeAnyChange(t *testing.T) {
	t.Chdir(t.TempDir())
	site := t.TempDir()
	emptyCatalogue := t.TempDir()
	contract := filepath.Join(t.TempDir(), "peer-overlap.v1.json") // never created
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"contract without peer": {[]string{"--site", site, "--profile", "ci", "--overlap-contract", contract},
			"must be given together"},
		"peer without contract": {[]string{"--site", site, "--profile", "ci", "--overlap-peer", "site_a"},
			"must be given together"},
		"full profile (the default)": {[]string{"--site", site, "--overlap-contract", contract, "--overlap-peer", "site_a"},
			"need --profile ci"},
		"with --limit-per-extension": {[]string{"--site", site, "--profile", "ci", "--limit-per-extension", "3",
			"--overlap-contract", contract, "--overlap-peer", "site_a"}, "excludes --limit-per-extension"},
		"invalid request with --reset": {[]string{"--site", site, "--catalogue", emptyCatalogue, "--profile", "ci",
			"--reset", "--overlap-contract", contract, "--overlap-peer", "site_a"}, "declared includes refused before any change"},
	} {
		err := runSeed(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestRunSeedSelection_DeclaredIncludeFlagsGoTogether(t *testing.T) {
	t.Chdir(t.TempDir())
	out := filepath.Join(t.TempDir(), "selection.json")
	for _, args := range [][]string{
		{"--site", t.TempDir(), "--out", out, "--overlap-peer", "site_a"},
		{"--site", t.TempDir(), "--out", out, "--overlap-contract", "c.json"},
	} {
		if err := runSeedSelection(args); err == nil || !strings.Contains(err.Error(), "must be given together") {
			t.Errorf("%v: err = %v", args, err)
		}
	}
}
