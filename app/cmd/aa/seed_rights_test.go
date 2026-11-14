// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oneFileReport is a minimal, consistent synthetic selection report: one
// non-model asset whose file is the single byte "x".
const oneFileReport = `{
  "schema": "artist-alley.ci-selection/v1",
  "profile": "ci",
  "coverage_depth": 8,
  "inputs": [],
  "counts": {"assets_total": 1, "posts_total": 0, "assets_selected": 1, "posts_selected": 0,
    "companion_assets": 0, "unknown_model_assets": 0, "primary_files": 1, "companion_files": 0,
    "unique_files": 1, "selected_bytes": 1, "unique_file_bytes": 1,
    "observed_dimensions": 0, "covered_dimensions": 0},
  "dimensions": {"required": [], "covered": []},
  "assets": [{"id": "a", "file_path": "a.png", "companion_support": "not_a_model_format", "companions": []}],
  "post_ids": [],
  "files": [{"path": "a.png", "bytes": 1,
    "sha256": "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", "primary_for": ["a"]}]
}
`

func TestRunSeedRights(t *testing.T) {
	work := t.TempDir()
	t.Chdir(work)
	in := t.TempDir() // inputs live outside the working directory
	report := filepath.Join(in, "selection.json")
	if err := os.WriteFile(report, []byte(oneFileReport), 0o600); err != nil {
		t.Fatal(err)
	}

	// --draft prints the ledger, byte-stable, to stdout.
	var a, b, errs bytes.Buffer
	if err := runSeedRightsTo(&a, &errs, []string{"--selection", report, "--draft"}); err != nil {
		t.Fatal(err)
	}
	if err := runSeedRightsTo(&b, &errs, []string{"--selection", report, "--draft"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) || !strings.Contains(a.String(), `"status": "pending"`) {
		t.Fatalf("draft not stable or not pending:\n%s", a.String())
	}
	ledger := filepath.Join(in, "ledger.json")
	if err := os.WriteFile(ledger, a.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// --check on the all-pending draft: structure holds, eligibility fails.
	var out bytes.Buffer
	errs.Reset()
	err := runSeedRightsTo(&out, &errs, []string{"--selection", report, "--ledger", ledger, "--check"})
	if err == nil || err.Error() != "seed-rights: structure is consistent but NOT release-eligible: "+
		"not every entry is reviewed_allow: 1 pending, 0 hold of 1 entries" {
		t.Errorf("pending draft: err = %v", err)
	}
	if !strings.Contains(out.String(), "STRUCTURAL ONLY") || !strings.Contains(errs.String(), "not legal clearance") {
		t.Errorf("check output lacks the structural-only label:\n%s\n%s", out.String(), errs.String())
	}

	// A structurally complete synthetic review passes, still labelled.
	reviewed := strings.Replace(a.String(), `"status": "pending",
      "review": null`, `"status": "reviewed_allow",
      "review": {"sources": ["https://example.invalid/a"], "license": "CC0-1.0",
        "license_evidence": ["https://example.invalid/license"], "redistribution": "allowed",
        "attribution": {"required": false}, "reviewer": "synthetic", "reviewed_on": "2026-11-11"}`, 1)
	if reviewed == a.String() {
		t.Fatal("test could not edit the draft")
	}
	if err := os.WriteFile(ledger, []byte(reviewed), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSeedRightsTo(&out, &errs, []string{"--selection", report, "--ledger", ledger, "--check"}); err != nil {
		t.Errorf("complete synthetic ledger: %v", err)
	}
	if !strings.Contains(out.String(), `"release_eligible_structurally": true`) || !strings.Contains(out.String(), "STRUCTURAL ONLY") {
		t.Errorf("eligible output:\n%s", out.String())
	}

	// Every input is explicit, and the flags must make sense together.
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no selection":      {[]string{"--draft"}, "--selection is required"},
		"neither mode":      {[]string{"--selection", report}, "exactly one of --draft or --check"},
		"both modes":        {[]string{"--selection", report, "--draft", "--check"}, "exactly one of --draft or --check"},
		"ledger with draft": {[]string{"--selection", report, "--draft", "--ledger", ledger}, "not used with --draft"},
		"check, no ledger":  {[]string{"--selection", report, "--check"}, "needs --ledger"},
		"missing selection": {[]string{"--selection", filepath.Join(in, "nope.json"), "--draft"}, "read selection report"},
		"missing ledger":    {[]string{"--selection", report, "--check", "--ledger", filepath.Join(in, "no.json")}, "read ledger"},
	} {
		var o bytes.Buffer
		if err := runSeedRightsTo(&o, &errs, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if o.Len() != 0 {
			t.Errorf("%s: printed output on failure", name)
		}
	}

	// Nothing was written anywhere but the caller's own files.
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("seed-rights wrote %d file(s) into the working directory", len(entries))
	}
}

// unknownModelReport is oneFileReport whose single asset is a 3D model in
// a format format3d cannot read references from: a valid report whose
// closure is NOT known to be complete.
var unknownModelReport = strings.NewReplacer(
	`"unknown_model_assets": 0`, `"unknown_model_assets": 1`,
	`"file_path": "a.png", "companion_support": "not_a_model_format"`, `"file_path": "a.png", "companion_support": "unknown_model_format"`,
).Replace(oneFileReport)

func TestRunSeedRights_UnknownModelIsNeverEligible(t *testing.T) {
	if unknownModelReport == oneFileReport || !strings.Contains(unknownModelReport, `"unknown_model_format"`) {
		t.Fatal("test could not build the unknown-model report")
	}
	t.Chdir(t.TempDir())
	in := t.TempDir()
	report := filepath.Join(in, "selection.json")
	if err := os.WriteFile(report, []byte(unknownModelReport), 0o600); err != nil {
		t.Fatal(err)
	}
	var draft, errs bytes.Buffer
	if err := runSeedRightsTo(&draft, &errs, []string{"--selection", report, "--draft"}); err != nil {
		t.Fatalf("a report with an unknown model must still draft as a worklist: %v", err)
	}
	const closure = "incomplete companion closure: 1 selected model asset(s) use a format whose companions " +
		"cannot be read (unknown_model_format), so the listed files may not be every file the package needs"
	reviewed := strings.Replace(draft.String(), `"status": "pending",
      "review": null`, `"status": "reviewed_allow",
      "review": {"sources": ["https://example.invalid/a"], "license": "CC0-1.0",
        "license_evidence": ["https://example.invalid/license"], "redistribution": "allowed",
        "attribution": {"required": false}, "reviewer": "synthetic", "reviewed_on": "2026-11-11"}`, 1)
	for name, tc := range map[string]struct {
		ledger, want string
	}{
		"every row reviewed": {reviewed,
			"seed-rights: structure is consistent but NOT release-eligible: " + closure},
		"rows pending too": {draft.String(),
			"seed-rights: structure is consistent but NOT release-eligible: " +
				"not every entry is reviewed_allow: 1 pending, 0 hold of 1 entries; " + closure},
	} {
		ledger := filepath.Join(in, "ledger.json")
		if err := os.WriteFile(ledger, []byte(tc.ledger), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err := runSeedRightsTo(&out, &errs, []string{"--selection", report, "--ledger", ledger, "--check"})
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v\nwant %s", name, err, tc.want)
		}
		for _, frag := range []string{`"release_eligible_structurally": false`, `"unknown_model_assets": 1`,
			`"unknown_model_asset_ids": [`, closure, "STRUCTURAL ONLY"} {
			if !strings.Contains(out.String(), frag) {
				t.Errorf("%s: check output lacks %q:\n%s", name, frag, out.String())
			}
		}
	}
}
