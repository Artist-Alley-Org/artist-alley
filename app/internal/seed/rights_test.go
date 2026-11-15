// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the CI rights ledger (#1514). All inputs are synthetic: the
// main round trip runs on the real exporter's output for the synthetic
// export fixture, and the edge shapes on hand-built reports.

package seed

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// --- synthetic report builder ---------------------------------------------

type synthAsset struct {
	id, path   string
	support    string
	companions []string
}

// synthReport writes a selection report for assets, deriving files and
// counts the way the exporter does, and returns its path.
func synthReport(t *testing.T, assets []synthAsset, content map[string]string) string {
	t.Helper()
	sort.Slice(assets, func(i, j int) bool { return assets[i].id < assets[j].id })
	rep := CISelection{Schema: CISelectionSchema, Profile: ProfileCI, CoverageDepth: 8,
		Inputs: []CISelectionInput{}, Assets: []CISelectionAsset{}, PostIDs: []string{}, Files: []CISelectionFile{},
		Dimensions: CISelectionDimensions{Required: []string{}, Covered: []string{}}}
	roles := map[string]*CISelectionFile{}
	role := func(p string) *CISelectionFile {
		if roles[p] == nil {
			b := content[p]
			roles[p] = &CISelectionFile{Path: p, Bytes: int64(len(b)), SHA256: ovSHA(b)}
		}
		return roles[p]
	}
	for _, a := range assets {
		comps := append([]string{}, a.companions...)
		sort.Strings(comps)
		rep.Assets = append(rep.Assets, CISelectionAsset{ID: a.id, FilePath: a.path, CompanionSupport: a.support, Companions: comps})
		r := role(a.path)
		r.PrimaryFor = append(r.PrimaryFor, a.id)
		for _, c := range comps {
			r := role(c)
			r.CompanionOf = append(r.CompanionOf, a.id)
		}
		if len(comps) > 0 {
			rep.Counts.CompanionAssets++
		}
		if a.support == CompanionsUnknownModel {
			rep.Counts.UnknownModelAssets++
		}
	}
	for _, p := range sortedKeys(roles) {
		f := *roles[p]
		rep.Files = append(rep.Files, f)
		rep.Counts.UniqueFileBytes += f.Bytes
		rep.Counts.SelectedBytes += f.Bytes * int64(len(f.PrimaryFor))
		if len(f.PrimaryFor) > 0 {
			rep.Counts.PrimaryFiles++
		}
		if len(f.CompanionOf) > 0 {
			rep.Counts.CompanionFiles++
		}
	}
	rep.Counts.AssetsSelected = len(rep.Assets)
	rep.Counts.UniqueFiles = len(rep.Files)
	return writeReport(t, &rep)
}

func writeReport(t *testing.T, rep *CISelection) string {
	t.Helper()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "selection.json")
	mustWrite(t, p, string(b)+"\n")
	return p
}

// sharedShape: one texture is a companion of two models and the primary
// of a third asset; one buffer is a companion only; one image is
// primary only.
func sharedShape(t *testing.T) string {
	return synthReport(t, []synthAsset{
		{id: "m1", path: "m/one.gltf", support: CompanionsResolved, companions: []string{"m/buf.bin", "m/tex.png"}},
		{id: "m2", path: "m/two.gltf", support: CompanionsResolved, companions: []string{"m/tex.png"}},
		{id: "t", path: "m/tex.png", support: CompanionsNotAModel},
		{id: "img", path: "a.png", support: CompanionsNotAModel},
	}, map[string]string{"m/one.gltf": "one", "m/two.gltf": "two", "m/buf.bin": "buf", "m/tex.png": "tex", "a.png": "png"})
}

func loadReport(t *testing.T, p string) (*CISelection, string) {
	t.Helper()
	sel, sum, err := LoadCISelectionReport(p)
	if err != nil {
		t.Fatal(err)
	}
	return sel, sum
}

func draftLedger(t *testing.T, sel *CISelection, sum string) (*RightsLedger, []byte) {
	t.Helper()
	b, err := DraftRightsLedger(sel, sum)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "ledger.json")
	mustWrite(t, p, string(b))
	l, err := LoadRightsLedger(p)
	if err != nil {
		t.Fatalf("a draft does not load back: %v", err)
	}
	return l, b
}

func completeReview() *RightsReview {
	yes := true
	return &RightsReview{
		Sources:         []string{"https://example.invalid/assets/pack", "https://example.invalid/pack.zip#a.png"},
		License:         "CC0-1.0",
		LicenseEvidence: []string{"License.txt in the pack"},
		Redistribution:  RedistributionAllowed,
		Attribution:     &RightsAttribution{Required: &yes, Text: "Example (example.invalid)"},
		Reviewer:        "synthetic reviewer",
		ReviewedOn:      "2026-11-11",
	}
}

// --- tests -----------------------------------------------------------------

func TestRightsLedger_DraftFromTheExporter(t *testing.T) {
	f := newExportFixture(t)
	f.export(2)
	sel, sum := loadReport(t, f.out())
	if sum != sha256Hex(t, f.out()) {
		t.Fatalf("bound sha %s is not the report file's", sum)
	}
	l, first := draftLedger(t, sel, sum)
	for i := 0; i < 3; i++ {
		again, _ := DraftRightsLedger(sel, sum)
		if !bytes.Equal(first, again) {
			t.Fatalf("draft %d differs", i)
		}
	}
	if len(l.Entries) != len(sel.Files) || l.Selection.Files != len(sel.Files) || l.Selection.SHA256 != sum {
		t.Fatalf("ledger has %d entries for %d files", len(l.Entries), len(sel.Files))
	}
	companions := 0
	for i, e := range l.Entries {
		fl := sel.Files[i]
		if e.Path != fl.Path || e.Bytes != fl.Bytes || e.SHA256 != fl.SHA256 ||
			strings.Join(e.PrimaryFor, ",") != strings.Join(fl.PrimaryFor, ",") ||
			strings.Join(e.CompanionOf, ",") != strings.Join(fl.CompanionOf, ",") {
			t.Errorf("entry %d = %+v, file %+v", i, e, fl)
		}
		if e.Status != RightsPending || e.Review != nil {
			t.Errorf("draft entry %q is %q with review %v", e.Path, e.Status, e.Review)
		}
		if len(e.CompanionOf) > 0 {
			companions++
		}
	}
	if companions == 0 || companions != sel.Counts.CompanionFiles {
		t.Errorf("draft carries %d companion entries, report has %d", companions, sel.Counts.CompanionFiles)
	}
	if !strings.Contains(string(first), "STRUCTURAL ONLY") {
		t.Errorf("draft lacks the structural-only notice")
	}
	res, err := CheckRightsLedger(sel, sum, l)
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligible || res.Counts[RightsPending] != len(l.Entries) || len(res.Blocking) != len(l.Entries) {
		t.Errorf("all-pending draft: %+v", res)
	}
}

func TestRightsLedger_Shapes(t *testing.T) {
	t.Run("N=0", func(t *testing.T) {
		sel, sum := loadReport(t, synthReport(t, nil, nil))
		l, _ := draftLedger(t, sel, sum)
		res, err := CheckRightsLedger(sel, sum, l)
		if err != nil {
			t.Fatal(err)
		}
		if len(l.Entries) != 0 || res.Eligible {
			t.Errorf("empty ledger: %d entries, eligible=%v; a package of nothing is not eligible", len(l.Entries), res.Eligible)
		}
	})
	t.Run("N=1 primary only", func(t *testing.T) {
		sel, sum := loadReport(t, synthReport(t, []synthAsset{{id: "a", path: "a.png", support: CompanionsNotAModel}},
			map[string]string{"a.png": "x"}))
		l, _ := draftLedger(t, sel, sum)
		if len(l.Entries) != 1 || strings.Join(l.Entries[0].PrimaryFor, ",") != "a" || len(l.Entries[0].CompanionOf) != 0 {
			t.Errorf("entries = %+v", l.Entries)
		}
		l.Entries[0].Status, l.Entries[0].Review = RightsReviewedAllow, completeReview()
		if res, err := CheckRightsLedger(sel, sum, l); err != nil || !res.Eligible {
			t.Errorf("one reviewed row: %+v %v", res, err)
		}
	})
	t.Run("N>=2 with companion-only and shared roles", func(t *testing.T) {
		sel, sum := loadReport(t, sharedShape(t))
		l, _ := draftLedger(t, sel, sum)
		want := map[string]string{
			"a.png":      "P:img|C:",
			"m/buf.bin":  "P:|C:m1",
			"m/one.gltf": "P:m1|C:",
			"m/tex.png":  "P:t|C:m1,m2",
			"m/two.gltf": "P:m2|C:",
		}
		got := map[string]string{}
		var order []string
		for _, e := range l.Entries {
			got[e.Path] = "P:" + strings.Join(e.PrimaryFor, ",") + "|C:" + strings.Join(e.CompanionOf, ",")
			order = append(order, e.Path)
		}
		if !jsonEqual(got, want) {
			t.Errorf("roles = %v, want %v", got, want)
		}
		if !sort.StringsAreSorted(order) {
			t.Errorf("entries not in path order: %v", order)
		}
		if res, err := CheckRightsLedger(sel, sum, l); err != nil || res.Eligible || res.Entries != 5 {
			t.Errorf("draft check: %+v %v", res, err)
		}
	})
}

func TestRightsLedger_Eligibility(t *testing.T) {
	sel, sum := loadReport(t, sharedShape(t))
	reviewAll := func(l *RightsLedger) {
		for i := range l.Entries {
			l.Entries[i].Status, l.Entries[i].Review = RightsReviewedAllow, completeReview()
		}
	}
	l, _ := draftLedger(t, sel, sum)
	reviewAll(l)
	res, err := CheckRightsLedger(sel, sum, l)
	if err != nil || !res.Eligible || res.Counts[RightsReviewedAllow] != 5 || len(res.Blocking) != 0 {
		t.Fatalf("complete synthetic ledger: %+v %v", res, err)
	}
	if res.Label != RightsStructuralOnly || !strings.Contains(res.Label, "not legal clearance") {
		t.Errorf("label = %q", res.Label)
	}
	// An attribution that is explicitly not required is a complete record.
	no := false
	l.Entries[0].Review.Attribution = &RightsAttribution{Required: &no}
	if res, err := CheckRightsLedger(sel, sum, l); err != nil || !res.Eligible {
		t.Errorf("attribution not required: %+v %v", res, err)
	}

	for _, st := range []string{RightsPending, RightsHold} {
		l, _ := draftLedger(t, sel, sum)
		reviewAll(l)
		l.Entries[3].Status = st
		res, err := CheckRightsLedger(sel, sum, l)
		if err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		if res.Eligible || len(res.Blocking) != 1 || res.Blocking[0] != (RightsBlocking{Path: l.Entries[3].Path, Status: st}) {
			t.Errorf("one %s row: %+v", st, res)
		}
	}
}

func TestRightsLedger_ReportRefusals(t *testing.T) {
	type mut func(r *CISelection)
	cases := []struct {
		name    string
		raw     string // used instead of the report when set
		mutate  mut
		wantErr string
	}{
		{name: "malformed JSON", raw: `{"schema":`, wantErr: "parse selection report"},
		{name: "wrong schema", mutate: func(r *CISelection) { r.Schema = "artist-alley.ci-selection/v2" }, wantErr: "schema"},
		{name: "wrong profile", mutate: func(r *CISelection) { r.Profile = "full" }, wantErr: "profile"},
		{name: "unsafe path", mutate: func(r *CISelection) { r.Files[0].Path = "../a.png" }, wantErr: "unsafe"},
		{name: "duplicate path", mutate: func(r *CISelection) { r.Files[1] = r.Files[0] }, wantErr: "not strictly sorted"},
		{name: "unsorted files", mutate: func(r *CISelection) { r.Files[0], r.Files[1] = r.Files[1], r.Files[0] },
			wantErr: "not strictly sorted"},
		{name: "bad sha", mutate: func(r *CISelection) { r.Files[0].SHA256 = "abc" }, wantErr: "sha256"},
		{name: "negative bytes", mutate: func(r *CISelection) { r.Files[0].Bytes = -1 }, wantErr: "negative bytes"},
		{name: "file with no role", mutate: func(r *CISelection) { r.Files[0].PrimaryFor = nil }, wantErr: "no role"},
		{name: "primary role not backed by the asset", mutate: func(r *CISelection) {
			r.Files[0].PrimaryFor = []string{"ghost"}
		}, wantErr: "primary_for [ghost]"},
		{name: "shared companion role dropped", mutate: func(r *CISelection) {
			for i := range r.Files {
				if r.Files[i].Path == "m/tex.png" {
					r.Files[i].CompanionOf = []string{"m1"}
				}
			}
		}, wantErr: "companion_of [m1]"},
		{name: "companion file missing", mutate: func(r *CISelection) {
			var keep []CISelectionFile
			for _, f := range r.Files {
				if f.Path != "m/buf.bin" {
					keep = append(keep, f)
				}
			}
			r.Files = keep
		}, wantErr: `omits file "m/buf.bin"`},
		{name: "duplicate role id", mutate: func(r *CISelection) {
			for i := range r.Files {
				if r.Files[i].Path == "m/tex.png" {
					r.Files[i].CompanionOf = []string{"m1", "m1"}
				}
			}
		}, wantErr: "not strictly sorted"},
		{name: "duplicate asset id", mutate: func(r *CISelection) { r.Assets[1].ID = r.Assets[0].ID }, wantErr: "assets not strictly sorted"},
		{name: "companions on a non-model", mutate: func(r *CISelection) {
			for i := range r.Assets {
				if r.Assets[i].ID == "img" {
					r.Assets[i].Companions = []string{"m/tex.png"}
				}
			}
		}, wantErr: "lists companions but support"},
		{name: "unknown companion support", mutate: func(r *CISelection) { r.Assets[0].CompanionSupport = "maybe" },
			wantErr: "unknown companion_support"},
		{name: "unique_files count", mutate: func(r *CISelection) { r.Counts.UniqueFiles++ }, wantErr: "counts.unique_files"},
		{name: "companion_files count", mutate: func(r *CISelection) { r.Counts.CompanionFiles-- }, wantErr: "counts.companion_files"},
		{name: "selected_bytes count", mutate: func(r *CISelection) { r.Counts.SelectedBytes++ }, wantErr: "counts.selected_bytes"},
		{name: "unique_file_bytes count", mutate: func(r *CISelection) { r.Counts.UniqueFileBytes-- }, wantErr: "counts.unique_file_bytes"},
		{name: "assets_selected count", mutate: func(r *CISelection) { r.Counts.AssetsSelected++ }, wantErr: "counts.assets_selected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sharedShape(t)
			if tc.raw != "" {
				mustWrite(t, p, tc.raw)
			} else {
				b, _ := os.ReadFile(p)
				var r CISelection
				if err := json.Unmarshal(b, &r); err != nil {
					t.Fatal(err)
				}
				tc.mutate(&r)
				p = writeReport(t, &r)
			}
			_, _, err := LoadCISelectionReport(p)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
	// Unknown keys and trailing data are refused too.
	p := sharedShape(t)
	b, _ := os.ReadFile(p)
	for name, raw := range map[string]string{
		"unknown key":   strings.Replace(string(b), `"profile":`, `"extra":1,"profile":`, 1),
		"trailing data": string(b) + "{}",
	} {
		mustWrite(t, p, raw)
		if _, _, err := LoadCISelectionReport(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, _, err := LoadCISelectionReport(""); err == nil {
		t.Errorf("no path accepted")
	}
}

func TestRightsLedger_CheckRefusals(t *testing.T) {
	type mut func(t *testing.T, l *RightsLedger)
	at := func(l *RightsLedger, p string) *RightsLedgerEntry {
		for i := range l.Entries {
			if l.Entries[i].Path == p {
				return &l.Entries[i]
			}
		}
		return nil
	}
	allow := func(edit func(r *RightsReview)) mut {
		return func(t *testing.T, l *RightsLedger) {
			e := &l.Entries[0]
			e.Status, e.Review = RightsReviewedAllow, completeReview()
			edit(e.Review)
		}
	}
	cases := []struct {
		name    string
		mutate  mut
		wantErr string
	}{
		{"stale selection sha", func(t *testing.T, l *RightsLedger) { l.Selection.SHA256 = ovSHA("other report") }, "stale ledger"},
		{"header file count", func(t *testing.T, l *RightsLedger) { l.Selection.Files++ }, "header"},
		{"header depth", func(t *testing.T, l *RightsLedger) { l.Selection.CoverageDepth = 2 }, "header"},
		{"missing entry", func(t *testing.T, l *RightsLedger) { l.Entries = l.Entries[1:] }, "no entry for 1 selection file"},
		{"missing companion entry", func(t *testing.T, l *RightsLedger) {
			var keep []RightsLedgerEntry
			for _, e := range l.Entries {
				if e.Path != "m/buf.bin" {
					keep = append(keep, e)
				}
			}
			l.Entries = keep
		}, `first "m/buf.bin"`},
		{"extra entry", func(t *testing.T, l *RightsLedger) {
			l.Entries = append(l.Entries, RightsLedgerEntry{Path: "zz.png", Status: RightsPending})
		}, "extra entry"},
		{"duplicate entry", func(t *testing.T, l *RightsLedger) {
			l.Entries = append(l.Entries[:2], append([]RightsLedgerEntry{l.Entries[1]}, l.Entries[2:]...)...)
		}, "twice"},
		{"unsorted entries", func(t *testing.T, l *RightsLedger) { l.Entries[0], l.Entries[1] = l.Entries[1], l.Entries[0] }, "not sorted"},
		{"changed size", func(t *testing.T, l *RightsLedger) { l.Entries[0].Bytes++ }, "pins"},
		{"changed hash", func(t *testing.T, l *RightsLedger) { l.Entries[0].SHA256 = ovSHA("x") }, "pins"},
		{"dropped shared companion role", func(t *testing.T, l *RightsLedger) {
			at(l, "m/tex.png").CompanionOf = []string{"m2"}
		}, "roles"},
		{"dropped primary role on a primary+companion path", func(t *testing.T, l *RightsLedger) {
			at(l, "m/tex.png").PrimaryFor = []string{}
		}, "roles"},
		{"unknown status", func(t *testing.T, l *RightsLedger) { l.Entries[0].Status = "approved" }, "unknown status"},
		{"empty status", func(t *testing.T, l *RightsLedger) { l.Entries[0].Status = "" }, "unknown status"},
		{"reviewed_allow without review", func(t *testing.T, l *RightsLedger) { l.Entries[0].Status = RightsReviewedAllow },
			"has no review"},
		{"no sources", allow(func(r *RightsReview) { r.Sources = nil }), "review.sources"},
		{"blank source", allow(func(r *RightsReview) { r.Sources = []string{" "} }), "review.sources"},
		{"no license", allow(func(r *RightsReview) { r.License = "" }), "review.license is empty"},
		{"no license evidence", allow(func(r *RightsReview) { r.LicenseEvidence = nil }), "review.license_evidence"},
		{"no redistribution decision", allow(func(r *RightsReview) { r.Redistribution = "" }), "review.redistribution"},
		{"redistribution not allowed", allow(func(r *RightsReview) { r.Redistribution = "not_allowed" }), "review.redistribution"},
		{"attribution unstated", allow(func(r *RightsReview) { r.Attribution = nil }), "attribution.required is not stated"},
		{"attribution required unstated", allow(func(r *RightsReview) { r.Attribution = &RightsAttribution{Text: "x"} }),
			"attribution.required is not stated"},
		{"attribution required without text", allow(func(r *RightsReview) { r.Attribution.Text = "" }), "no text or notice"},
		{"blank notice", allow(func(r *RightsReview) { r.Attribution.Notices = []string{""} }), "notices has a blank"},
		{"no reviewer", allow(func(r *RightsReview) { r.Reviewer = "" }), "review.reviewer"},
		{"no date", allow(func(r *RightsReview) { r.ReviewedOn = "" }), "reviewed_on"},
		{"bad date", allow(func(r *RightsReview) { r.ReviewedOn = "11/11/2026" }), "reviewed_on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, sum := loadReport(t, sharedShape(t))
			l, _ := draftLedger(t, sel, sum)
			tc.mutate(t, l)
			_, err := CheckRightsLedger(sel, sum, l)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}

	// A ledger checked against a different (even if similar) report is stale.
	sel, sum := loadReport(t, sharedShape(t))
	l, _ := draftLedger(t, sel, sum)
	other, otherSum := loadReport(t, synthReport(t, []synthAsset{{id: "a", path: "a.png", support: CompanionsNotAModel}},
		map[string]string{"a.png": "y"}))
	if _, err := CheckRightsLedger(other, otherSum, l); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Errorf("other report: %v", err)
	}

	// The ledger file itself is read strictly.
	_, raw := draftLedger(t, sel, sum)
	p := filepath.Join(t.TempDir(), "l.json")
	for name, body := range map[string]string{
		"unknown schema": strings.Replace(string(raw), RightsLedgerSchema, "artist-alley.rights-ledger/v9", 1),
		"unknown key":    strings.Replace(string(raw), `"notice":`, `"approved_by_legal":true,"notice":`, 1),
		"trailing data":  string(raw) + "[]",
	} {
		mustWrite(t, p, body)
		if _, err := LoadRightsLedger(p); err == nil {
			t.Errorf("%s: ledger accepted", name)
		}
	}
}

// A report with an unknown_model_format asset is a valid worklist, but its
// closure is not known to be complete, so no ledger over it is eligible
// (#1514 rights-ledger correction round 1).
func TestRightsLedger_UnknownModelClosureIsNeverEligible(t *testing.T) {
	withUnknown := func(t *testing.T) string {
		return synthReport(t, []synthAsset{
			{id: "img", path: "a.png", support: CompanionsNotAModel},
			{id: "part", path: "m/part.stl", support: CompanionsUnknownModel},
		}, map[string]string{"a.png": "png", "m/part.stl": "solid x"})
	}
	reviewAll := func(l *RightsLedger) {
		for i := range l.Entries {
			l.Entries[i].Status, l.Entries[i].Review = RightsReviewedAllow, completeReview()
		}
	}
	closureReason := func(res *RightsCheck) string {
		for _, r := range res.Ineligible {
			if strings.HasPrefix(r, RightsReasonIncompleteClose+":") {
				return r
			}
		}
		return ""
	}

	t.Run("complete reviews, unknown model", func(t *testing.T) {
		sel, sum := loadReport(t, withUnknown(t))
		if sel.Counts.UnknownModelAssets != 1 {
			t.Fatalf("fixture has %d unknown models", sel.Counts.UnknownModelAssets)
		}
		l, _ := draftLedger(t, sel, sum)
		if len(l.Entries) != 2 {
			t.Fatalf("draft has %d rows; the worklist must still be drafted", len(l.Entries))
		}
		reviewAll(l)
		res, err := CheckRightsLedger(sel, sum, l)
		if err != nil {
			t.Fatal(err)
		}
		if res.Eligible || res.UnknownModelAssets != 1 || strings.Join(res.UnknownModelAssetIDs, ",") != "part" {
			t.Errorf("result = %+v", res)
		}
		if len(res.Blocking) != 0 || res.Counts[RightsReviewedAllow] != 2 {
			t.Errorf("the closure reason must be separate from row states: %+v", res)
		}
		if len(res.Ineligible) != 1 || closureReason(res) == "" ||
			!strings.Contains(res.Ineligible[0], "1 selected model asset(s)") {
			t.Errorf("reasons = %q", res.Ineligible)
		}
	})
	t.Run("pending rows and unknown model", func(t *testing.T) {
		sel, sum := loadReport(t, withUnknown(t))
		l, _ := draftLedger(t, sel, sum)
		res, err := CheckRightsLedger(sel, sum, l)
		if err != nil {
			t.Fatal(err)
		}
		if res.Eligible || len(res.Ineligible) != 2 ||
			res.Ineligible[0] != RightsReasonIncompleteReview+": 2 pending, 0 hold of 2 entries" ||
			closureReason(res) != res.Ineligible[1] {
			t.Errorf("reasons = %q", res.Ineligible)
		}
	})
	t.Run("complete reviews, no unknown model", func(t *testing.T) {
		sel, sum := loadReport(t, sharedShape(t))
		l, _ := draftLedger(t, sel, sum)
		reviewAll(l)
		res, err := CheckRightsLedger(sel, sum, l)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Eligible || len(res.Ineligible) != 0 || res.UnknownModelAssets != 0 || len(res.UnknownModelAssetIDs) != 0 {
			t.Errorf("result = %+v", res)
		}
	})
	t.Run("the exporter's own unknown-model report", func(t *testing.T) {
		// An STL selected by the real exporter: the report is valid, the
		// draft is a worklist, the ledger is never eligible.
		f := newExportFixture(t)
		f.addSolo("stl-0", "3d", "stl", "part.stl", "solid x\nendsolid x\n")
		f.write()
		f.export(2)
		sel, sum := loadReport(t, f.out())
		l, _ := draftLedger(t, sel, sum)
		reviewAll(l)
		res, err := CheckRightsLedger(sel, sum, l)
		if err != nil {
			t.Fatal(err)
		}
		if res.Eligible || res.UnknownModelAssets != 1 || closureReason(res) == "" {
			t.Errorf("result: eligible=%v unknown=%d reasons=%q", res.Eligible, res.UnknownModelAssets, res.Ineligible)
		}
	})
}
