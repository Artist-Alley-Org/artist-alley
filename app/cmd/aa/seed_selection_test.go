// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `aa seed-selection --out seed/profiles/dataset.users.json` must refuse
// before it reads anything, and leave the catalogue input as it was
// (#1514 correction round 1). The refusal needs no site content, so the
// site root here is an empty directory.
func TestRunSeedSelection_RefusesOutputOverACatalogueInput(t *testing.T) {
	work := t.TempDir()
	site := filepath.Join(work, "site")
	cat := filepath.Join(work, "seed", "profiles")
	for _, d := range []string{site, cat} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	input := filepath.Join(cat, "dataset.users.json")
	const original = `[{"username":"u1"}]`
	if err := os.WriteFile(input, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(input)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(work)
	err = runSeedSelection([]string{"--site", "site", "--catalogue", "seed/profiles",
		"--out", "seed/profiles/dataset.users.json"})
	if err == nil || !strings.Contains(err.Error(), "inside the catalogue root") {
		t.Fatalf("err = %v, want a catalogue-root refusal", err)
	}

	b, err := os.ReadFile(input)
	if err != nil || string(b) != original {
		t.Errorf("catalogue input changed: %q (%v)", b, err)
	}
	after, err := os.Stat(input)
	if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("catalogue input was replaced or touched")
	}
	entries, _ := os.ReadDir(cat)
	if len(entries) != 1 {
		t.Errorf("catalogue directory holds %d entries, want only the input", len(entries))
	}
}
