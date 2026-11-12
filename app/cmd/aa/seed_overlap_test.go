// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// Every input of `aa seed-overlap` is explicit: there is no default
// contract and, above all, no default peer root (#1514). It also writes
// nothing: the report goes to the writer it is given.
func TestRunSeedOverlap_RequiresExplicitInputs(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no arguments":      {nil, "--contract is required"},
		"contract, no peer": {[]string{"--contract", "c.json"}, "at least one --peer"},
		"peer without root": {[]string{"--contract", "c.json", "--peer", "site_b"}, "want name=root"},
		"peer given twice": {[]string{"--contract", "c.json", "--peer", "site_a=" + work,
			"--peer", "site_a=" + work}, "given twice"},
		"contract missing": {[]string{"--contract", "c.json", "--peer", "site_a=" + work}, "read contract"},
	} {
		var out bytes.Buffer
		err := runSeedOverlapTo(&out, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if out.Len() != 0 {
			t.Errorf("%s: printed a report on failure", name)
		}
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("seed-overlap wrote %d file(s)", len(entries))
	}
}
