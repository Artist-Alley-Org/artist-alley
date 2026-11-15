// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// packageInputs writes a two-asset synthetic site (one asset selected), an
// empty catalogue, a report pinning the site's MANIFEST.json and posts.json,
// and that report's ledger with every row set by status.
func packageInputs(t *testing.T, reviewed bool) (site, cat, report, ledger string) {
	t.Helper()
	site, cat, in := t.TempDir(), t.TempDir(), t.TempDir()
	manifest := `[{"id":"a","file_path":"a.png","file_size_bytes":1,"license":"label is not evidence"},` +
		`{"id":"b","file_path":"b.png","file_size_bytes":1}]`
	posts := `[]`
	for name, body := range map[string]string{"a.png": "x", "b.png": "y", "MANIFEST.json": manifest, "posts.json": posts} {
		if err := os.WriteFile(filepath.Join(site, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pin := func(name, body string) string {
		return fmt.Sprintf(`{"name": %q, "present": true, "bytes": %d, "sha256": %q}`, name, len(body), hexSum([]byte(body)))
	}
	rep := strings.Replace(oneFileReport, `"inputs": [],`,
		`"inputs": [`+pin("site/MANIFEST.json", manifest)+`, `+pin("site/posts.json", posts)+`],`, 1)
	report = filepath.Join(in, "selection.json")
	if err := os.WriteFile(report, []byte(rep), 0o600); err != nil {
		t.Fatal(err)
	}
	var draft, errs bytes.Buffer
	if err := runSeedRightsTo(&draft, &errs, []string{"--selection", report, "--draft"}); err != nil {
		t.Fatal(err)
	}
	l := draft.String()
	if reviewed {
		l = strings.Replace(l, `"status": "pending",
      "review": null`, `"status": "reviewed_allow",
      "review": {"sources": ["https://example.invalid/a"], "license": "CC0-1.0",
        "license_evidence": ["https://example.invalid/license"], "redistribution": "allowed",
        "attribution": {"required": false}, "reviewer": "synthetic", "reviewed_on": "2026-11-11"}`, 1)
		if l == draft.String() {
			t.Fatal("could not edit the draft")
		}
	}
	ledger = filepath.Join(in, "ledger.json")
	if err := os.WriteFile(ledger, []byte(l), 0o600); err != nil {
		t.Fatal(err)
	}
	return site, cat, report, ledger
}

func TestRunSeedPackage(t *testing.T) {
	site, cat, report, ledger := packageInputs(t, true)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "seed.tar.gz")
	args := []string{"--site", site, "--catalogue", cat, "--selection", report, "--ledger", ledger, "--output", out}

	var first, second bytes.Buffer
	if err := runSeedPackageTo(&first, args); err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.String(), "sha256 "+hexSum(a)) || !strings.Contains(first.String(), fmt.Sprintf("%d bytes", len(a))) ||
		!strings.Contains(first.String(), "4 entries (1 closure files, 1 assets, 0 posts)") ||
		!strings.Contains(first.String(), "STRUCTURAL ONLY") || !strings.Contains(first.String(), "does not authorize publication") {
		t.Errorf("output:\n%s", first.String())
	}
	if err := runSeedPackageTo(&second, args); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if !bytes.Equal(a, b) || first.String() != second.String() {
		t.Error("two runs differ")
	}
}

func TestRunSeedPackage_Refusals(t *testing.T) {
	site, cat, report, pending := packageInputs(t, false)
	out := filepath.Join(t.TempDir(), "seed.tar.gz")
	full := []string{"--site", site, "--catalogue", cat, "--selection", report, "--ledger", pending, "--output", out}
	var sink bytes.Buffer
	err := runSeedPackageTo(&sink, full)
	if err == nil || !strings.Contains(err.Error(), "not structurally eligible") {
		t.Errorf("pending ledger: err = %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a refused build wrote the archive")
	}
	for i := 0; i < len(full); i += 2 {
		args := append(append([]string{}, full[:i]...), full[i+2:]...)
		if err := runSeedPackageTo(&sink, args); err == nil || err.Error() != "seed-package: "+full[i]+" is required" {
			t.Errorf("without %s: err = %v", full[i], err)
		}
	}
	if err := runSeedPackageTo(&sink, append(full, "extra")); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("stray argument: err = %v", err)
	}
}
