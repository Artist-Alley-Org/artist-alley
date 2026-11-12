// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for declared includes (#1514). Synthetic only: the export fixture
// plus two overlap anchors the coverage selector does not pick, which is
// the shape of today's real site_a. The public site_b does not exist, so
// its side runs on the same synthetic fixture under site_b's ids.

package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type incFix struct {
	*exportFixture
	peer         string
	contract     OverlapContract
	contractPath string
	anchorIDs    []string
	anchorPaths  []string
	anchorBytes  []string
}

func incAnchorBytes(i int) string {
	return fmt.Sprintf("overlap-anchor-%d:", i) + strings.Repeat("q", 40+i*7)
}

// newIncludeFixture adds two anchors, valid for the overlap check, to the
// export fixture as `peer` and writes a contract naming them.
// anchorExt[i] overrides anchor i's extension (and file suffix).
func newIncludeFixture(t *testing.T, peer string, anchorExt ...string) *incFix {
	t.Helper()
	f := &incFix{exportFixture: newExportFixture(t), peer: peer}
	f.extra = map[string]map[string]any{}
	f.contract = OverlapContract{Schema: OverlapContractSchema, Why: "test", Status: "proposed",
		Issue: "https://example.invalid/1514", Peers: []string{"site_a", "site_b"}}
	for i := 1; i <= 2; i++ {
		ext := "png"
		if len(anchorExt) >= i && anchorExt[i-1] != "" {
			ext = anchorExt[i-1]
		}
		content := incAnchorBytes(i)
		page := fmt.Sprintf("https://example.invalid/assets/pack%d", i)
		archive := fmt.Sprintf("https://example.invalid/pack%d.zip", i)
		member := fmt.Sprintf("Parts/anchor%d.%s", i, ext)
		aID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("test:inc:site_a:%d", i))).String()
		deriv := fmt.Sprintf("test:inc:public-site-b:overlap-image-%d", i)
		bID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(deriv)).String()
		aPath := fmt.Sprintf("images/anchor-%d.%s", i, ext)
		bPath := fmt.Sprintf("shared/b-%d.%s", i, ext)
		id, p := aID, aPath
		if peer == "site_b" {
			id, p = bID, bPath
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.site, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(f.site, p), content)
		meta, _ := json.Marshal(map[string]any{"fetched_from": page,
			"source_archive": map[string]any{"url": archive, "member": member, "sha256": ovSHA(content)}})
		f.assets = append(f.assets, manifestAsset{
			ID: id, AssetType: "image", Title: id, FilePath: p, FileExtension: ext,
			FileSizeBytes: int64(len(content)), SensitivityTier: "team", ArchiveState: "active",
			WorkflowState: "approved", OwnerUsername: "u1", CollectionName: "Alpha", TeamName: "Environment",
			Tags: []string{"t-png"}, FieldValues: map[string]any{"rating": 3}, Metadata: meta,
		})
		f.extra[id] = map[string]any{"license": ovLicense, "attribution": ovAttribution}
		f.anchorIDs = append(f.anchorIDs, id)
		f.anchorPaths = append(f.anchorPaths, p)
		f.anchorBytes = append(f.anchorBytes, content)
		f.contract.Fixtures = append(f.contract.Fixtures, OverlapFixture{
			Name: fmt.Sprintf("overlap-image-%d", i), SHA256: ovSHA(content), Bytes: int64(len(content)),
			Rights: "evidence recorded; not release-cleared",
			Assets: map[string]OverlapFixtureAsset{
				"site_a": {AssetID: aID, FilePath: aPath},
				"site_b": {AssetID: bID, IDDerivation: &OverlapDerivation{Method: "uuidv5", Namespace: "url", Name: deriv}},
			},
			Source: OverlapSource{AssetPage: page, ArchiveURL: archive, ArchiveMember: member, License: "CC0-1.0",
				ManifestLicense: ovLicense, LicenseEvidence: []string{"https://example.invalid/license"},
				Attribution: ovAttribution},
		})
	}
	f.contractPath = filepath.Join(t.TempDir(), "peer-overlap.v1.json")
	f.writeAll()
	return f
}

func (f *incFix) writeAll() {
	f.t.Helper()
	b, err := json.Marshal(f.contract)
	if err != nil {
		f.t.Fatal(err)
	}
	mustWrite(f.t, f.contractPath, string(b))
	f.write()
}

func (f *incFix) incOpts(depth int) CISelectionOptions {
	o := f.opts(depth)
	o.Includes = &DeclaredIncludes{ContractPath: f.contractPath, Peer: f.peer}
	return o
}

func (f *incFix) exportWith(o CISelectionOptions) *CISelection {
	f.t.Helper()
	if _, err := ExportCISelection(o, f.out()); err != nil {
		f.t.Fatalf("export: %v", err)
	}
	r, _ := readSelection(f.t, f.out())
	return r
}

func (f *incFix) runner(o CISelectionOptions, pin *IncludePin) *Runner {
	return &Runner{log: slog.New(slog.NewTextHandler(io.Discard, nil)), opts: Options{
		SiteRoot: o.SiteRoot, CatalogueRoot: o.CatalogueRoot, Profile: ProfileCI, CoverageDepth: o.CoverageDepth,
		Includes: o.Includes, IncludePin: pin,
	}}
}

func idsOf(c *catalogues) ([]string, []string) {
	var a, p []string
	for _, x := range c.Assets {
		a = append(a, x.ID)
	}
	for _, x := range c.Posts {
		p = append(p, x.ID)
	}
	sort.Strings(a)
	sort.Strings(p)
	return a, p
}

func TestDeclaredIncludes_CoverageAloneOmitsTheAnchors(t *testing.T) {
	// The fixture mirrors real site_a: without the opt-in, neither anchor
	// is selected, and the report stays v1 with no includes block.
	f := newIncludeFixture(t, "site_a")
	rep := f.exportWith(f.opts(2))
	if rep.Schema != CISelectionSchema || rep.DeclaredIncludes != nil {
		t.Fatalf("default report: schema %q includes %v", rep.Schema, rep.DeclaredIncludes)
	}
	for _, id := range f.anchorIDs {
		if contains(assetIDsOf(rep), id) {
			t.Fatalf("coverage alone selected anchor %s; the fixture would prove nothing", id)
		}
	}
}

func TestDeclaredIncludes_SiteA(t *testing.T) {
	f := newIncludeFixture(t, "site_a")
	base := f.exportWith(f.opts(2))
	rep := f.exportWith(f.incOpts(2))

	// Header: v2, the contract and peer named, the contract pinned.
	d := rep.DeclaredIncludes
	if rep.Schema != CISelectionIncludesSchema || d == nil {
		t.Fatalf("schema %q, includes %v", rep.Schema, d)
	}
	if d.Peer != "site_a" || d.Contract != "peer-overlap.v1.json" || d.ContractSchema != OverlapContractSchema ||
		d.ContractSHA256 != sha256Hex(t, f.contractPath) ||
		d.ManifestSHA256 != sha256Hex(t, filepath.Join(f.site, "MANIFEST.json")) {
		t.Errorf("declared_includes = %+v", *d)
	}
	pinned := false
	for _, in := range rep.Inputs {
		pinned = pinned || (in.Name == "overlap_contract/peer-overlap.v1.json" && in.SHA256 == d.ContractSHA256)
	}
	if !pinned {
		t.Errorf("contract is not a pinned input: %+v", rep.Inputs)
	}
	for i, a := range d.Anchors {
		if a.AssetID != f.anchorIDs[i] || a.FilePath != f.anchorPaths[i] || a.SHA256 != ovSHA(f.anchorBytes[i]) ||
			a.Bytes != int64(len(f.anchorBytes[i])) || !a.AddedByInclude {
			t.Errorf("anchor %d = %+v", i, a)
		}
	}

	// Additive only: every coverage row, post, dimension and file stays
	// exactly as it was; the two anchors are added once each.
	ids := assetIDsOf(rep)
	for _, id := range assetIDsOf(base) {
		if !contains(ids, id) {
			t.Errorf("coverage asset %s dropped", id)
		}
	}
	for _, id := range f.anchorIDs {
		n := 0
		for _, x := range ids {
			if x == id {
				n++
			}
		}
		if n != 1 {
			t.Errorf("anchor %s appears %d times", id, n)
		}
	}
	if len(ids) != len(base.Assets)+2 || rep.Counts.AssetsSelected != base.Counts.AssetsSelected+2 {
		t.Errorf("assets %d (counts %d), base %d", len(ids), rep.Counts.AssetsSelected, len(base.Assets))
	}
	if strings.Join(rep.PostIDs, ",") != strings.Join(base.PostIDs, ",") || rep.Counts.PostsSelected != base.Counts.PostsSelected {
		t.Errorf("posts changed")
	}
	if !jsonEqual(rep.Dimensions, base.Dimensions) || rep.Counts.CoveredDimensions != base.Counts.CoveredDimensions ||
		rep.Counts.ObservedDimensions != base.Counts.ObservedDimensions {
		t.Errorf("dimensions changed")
	}
	files := map[string]CISelectionFile{}
	for _, fl := range rep.Files {
		files[fl.Path] = fl
	}
	for _, fl := range base.Files {
		if !jsonEqual(files[fl.Path], fl) {
			t.Errorf("file %s changed: %+v -> %+v", fl.Path, fl, files[fl.Path])
		}
	}
	for i, p := range f.anchorPaths {
		fl := files[p]
		if fl.SHA256 != ovSHA(f.anchorBytes[i]) || strings.Join(fl.PrimaryFor, ",") != f.anchorIDs[i] || len(fl.CompanionOf) != 0 {
			t.Errorf("anchor file %s = %+v", p, fl)
		}
	}
	extra := int64(len(f.anchorBytes[0]) + len(f.anchorBytes[1]))
	if rep.Counts.SelectedBytes != base.Counts.SelectedBytes+extra || rep.Counts.UniqueFiles != base.Counts.UniqueFiles+2 ||
		rep.Counts.CompanionFiles != base.Counts.CompanionFiles || rep.Counts.CompanionAssets != base.Counts.CompanionAssets {
		t.Errorf("counts %+v vs base %+v", rep.Counts, base.Counts)
	}

	// Deterministic.
	_, first := readSelection(t, f.out())
	f.exportWith(f.incOpts(2))
	if _, again := readSelection(t, f.out()); !bytes.Equal(first, again) {
		t.Errorf("include export is not deterministic")
	}

	// The seeder's selection is the report's selection, with and without
	// the opt-in.
	for name, o := range map[string]CISelectionOptions{"with includes": f.incOpts(2), "without": f.opts(2)} {
		want := rep
		if o.Includes == nil {
			want = base
		}
		c, err := f.runner(o, nil).selectCatalogue()
		if err != nil {
			t.Fatalf("%s: seeder selection: %v", name, err)
		}
		a, p := idsOf(c)
		if strings.Join(a, ",") != strings.Join(assetIDsOf(want), ",") || strings.Join(p, ",") != strings.Join(want.PostIDs, ",") {
			t.Errorf("%s: seeder selection differs from the report", name)
		}
	}
}

func TestDeclaredIncludes_SyntheticPublicSiteB(t *testing.T) {
	f := newIncludeFixture(t, "site_b")
	rep := f.exportWith(f.incOpts(2))
	d := rep.DeclaredIncludes
	if d == nil || d.Peer != "site_b" || len(d.Anchors) != 2 {
		t.Fatalf("declared_includes = %+v", d)
	}
	for i, a := range d.Anchors {
		want := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("test:inc:public-site-b:overlap-image-%d", i+1))).String()
		if a.AssetID != want || a.FilePath != f.anchorPaths[i] || !contains(assetIDsOf(rep), want) {
			t.Errorf("site_b anchor %d = %+v, want id %s", i, a, want)
		}
		if contains(assetIDsOf(rep), f.contract.Fixtures[i].Assets["site_a"].AssetID) {
			t.Errorf("site_a's id appeared in the site_b selection")
		}
	}
}

func TestDeclaredIncludes_AnchorAlreadySelectedIsIncludedOnce(t *testing.T) {
	// Anchor 1 is the catalogue's only .gif, so coverage must select it
	// for the ext=gif dimension; the include must not add it again.
	f := newIncludeFixture(t, "site_a", "gif")
	base := f.exportWith(f.opts(2))
	if !contains(assetIDsOf(base), f.anchorIDs[0]) {
		t.Fatal("coverage did not select the only .gif; the test would prove nothing")
	}
	rep := f.exportWith(f.incOpts(2))
	d := rep.DeclaredIncludes
	if d.Anchors[0].AddedByInclude || !d.Anchors[1].AddedByInclude {
		t.Errorf("added_by_include = %v, %v", d.Anchors[0].AddedByInclude, d.Anchors[1].AddedByInclude)
	}
	if len(rep.Assets) != len(base.Assets)+1 {
		t.Errorf("assets %d, base %d: the already-selected anchor was added twice", len(rep.Assets), len(base.Assets))
	}
}

func TestDeclaredIncludes_FlagRules(t *testing.T) {
	for name, tc := range map[string]struct {
		contract, peer, profile, want string
	}{
		"contract only": {"c.json", "", ProfileCI, "must be given together"},
		"peer only":     {"", "site_a", ProfileCI, "must be given together"},
		"full profile":  {"c.json", "site_a", ProfileFull, "need --profile ci"},
		"empty profile": {"c.json", "site_a", "", "need --profile ci"},
	} {
		if _, err := CheckDeclaredIncludeFlags(tc.contract, tc.peer, tc.profile); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if d, err := CheckDeclaredIncludeFlags("", "", ProfileFull); d != nil || err != nil {
		t.Errorf("neither flag: %v %v", d, err)
	}
	if d, err := CheckDeclaredIncludeFlags("c.json", "site_a", ProfileCI); err != nil || d.Peer != "site_a" || d.ContractPath != "c.json" {
		t.Errorf("both flags: %+v %v", d, err)
	}
}

func TestDeclaredIncludes_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(f *incFix) CISelectionOptions
		wantErr string
	}{
		{"unknown peer", func(f *incFix) CISelectionOptions {
			o := f.incOpts(2)
			o.Includes.Peer = "site_c"
			return o
		}, `peer "site_c" is not in the contract`},
		{"missing contract", func(f *incFix) CISelectionOptions {
			o := f.incOpts(2)
			o.Includes.ContractPath = filepath.Join(f.t.TempDir(), "nope.json")
			return o
		}, "read contract"},
		{"unknown contract schema", func(f *incFix) CISelectionOptions {
			f.contract.Schema = "artist-alley.peer-overlap/v9"
			f.writeAll()
			return f.incOpts(2)
		}, "unknown contract schema"},
		{"anchor id missing from the manifest", func(f *incFix) CISelectionOptions {
			f.assets = f.assets[:len(f.assets)-1]
			f.write()
			return f.incOpts(2)
		}, "no manifest row with id"},
		{"duplicate manifest id", func(f *incFix) CISelectionOptions {
			f.assets[0].ID = f.anchorIDs[0]
			f.write()
			return f.incOpts(2)
		}, "duplicate"},
		{"wrong bytes at the same length", func(f *incFix) CISelectionOptions {
			mustWrite(f.t, filepath.Join(f.site, f.anchorPaths[1]), strings.Repeat("z", len(f.anchorBytes[1])))
			return f.incOpts(2)
		}, "the fixture is"},
		{"wrong explicit path", func(f *incFix) CISelectionOptions {
			a := f.contract.Fixtures[0].Assets["site_a"]
			a.FilePath = "images/elsewhere.png"
			f.contract.Fixtures[0].Assets["site_a"] = a
			f.writeAll()
			return f.incOpts(2)
		}, "contract path"},
		{"unsafe contract path", func(f *incFix) CISelectionOptions {
			a := f.contract.Fixtures[0].Assets["site_a"]
			a.FilePath = "../escape.png"
			f.contract.Fixtures[0].Assets["site_a"] = a
			f.writeAll()
			return f.incOpts(2)
		}, "unsafe"},
		{"anchor without provenance", func(f *incFix) CISelectionOptions {
			f.assets[len(f.assets)-1].Metadata = nil
			f.write()
			return f.incOpts(2)
		}, "metadata is missing"},
		{"extra identical bytes", func(f *incFix) CISelectionOptions {
			mustWrite(f.t, filepath.Join(f.site, "dupe.png"), f.anchorBytes[0])
			dup := f.assets[0]
			dup.ID, dup.FilePath, dup.FileSizeBytes = "dupe", "dupe.png", int64(len(f.anchorBytes[0]))
			f.assets = append(f.assets, dup)
			f.write()
			return f.incOpts(2)
		}, "has the same bytes as fixture"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newIncludeFixture(t, "site_a")
			o := tc.mutate(f)
			_, err := ExportCISelection(o, f.out())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("export: err = %v, want %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(f.out()); statErr == nil {
				t.Errorf("a refused include export wrote a report")
			}
			// The seeder's preflight refuses the same request.
			if _, err := PreflightCISelection(o); err == nil {
				t.Errorf("preflight accepted what the export refused")
			}
		})
	}
}

func TestDeclaredIncludes_ContractChangedDuringExport(t *testing.T) {
	f := newIncludeFixture(t, "site_a")
	ciSelectionAfterHash = func() {
		b, _ := os.ReadFile(f.contractPath)
		mustWrite(f.t, f.contractPath, string(b)+"\n") // still valid, different bytes
	}
	t.Cleanup(func() { ciSelectionAfterHash = nil })
	if _, err := ExportCISelection(f.incOpts(2), f.out()); err == nil ||
		!strings.Contains(err.Error(), "a catalogue input changed during the export") {
		t.Errorf("err = %v", err)
	}
}

func TestDeclaredIncludes_ManifestMustBeTheCheckedOne(t *testing.T) {
	f := newIncludeFixture(t, "site_a")
	inc, err := resolveDeclaredIncludes(f.incOpts(2).Includes, f.site)
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadCatalogues(f.catDir, f.site, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.selectCIProfile(2, inc, ovSHA("some other manifest"), nil); err == nil ||
		!strings.Contains(err.Error(), "MANIFEST.json changed between the anchor check and the catalogue load") {
		t.Errorf("err = %v", err)
	}
}

func TestDeclaredIncludes_SeederRunMustMatchThePreflight(t *testing.T) {
	f := newIncludeFixture(t, "site_a")
	o := f.incOpts(2)
	pin, err := PreflightCISelection(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(pin.Anchors) != 2 || !pin.Anchors[0].AddedByInclude {
		t.Fatalf("pin = %+v", pin)
	}
	if _, err := f.runner(o, pin).selectCatalogue(); err != nil {
		t.Fatalf("unchanged inputs: %v", err)
	}
	// Still a valid contract, but not the one the preflight checked.
	b, _ := os.ReadFile(f.contractPath)
	mustWrite(t, f.contractPath, string(b)+" ")
	if _, err := f.runner(o, pin).selectCatalogue(); err == nil ||
		!strings.Contains(err.Error(), "changed between the preflight and the seed") {
		t.Errorf("changed contract: err = %v", err)
	}
	// Anchor bytes replaced after the preflight: the run's own check refuses.
	mustWrite(t, f.contractPath, string(b))
	mustWrite(t, filepath.Join(f.site, f.anchorPaths[0]), strings.Repeat("w", len(f.anchorBytes[0])))
	if _, err := f.runner(o, pin).selectCatalogue(); err == nil {
		t.Errorf("changed anchor bytes were seeded")
	}
	// Includes outside the ci profile are refused by the seeder itself.
	r := f.runner(o, nil)
	r.opts.Profile = ProfileFull
	if _, err := r.selectCatalogue(); err == nil || !strings.Contains(err.Error(), "need --profile ci") {
		t.Errorf("full profile with includes: err = %v", err)
	}
}

func TestDeclaredIncludes_RightsLedgerOverAV2Report(t *testing.T) {
	f := newIncludeFixture(t, "site_a")
	f.exportWith(f.incOpts(2))
	sel, sum := loadReport(t, f.out())
	l, _ := draftLedger(t, sel, sum)
	if l.Selection.Schema != CISelectionIncludesSchema {
		t.Errorf("ledger binds schema %q", l.Selection.Schema)
	}
	for _, p := range f.anchorPaths {
		found := false
		for _, e := range l.Entries {
			found = found || (e.Path == p && e.Status == RightsPending)
		}
		if !found {
			t.Errorf("anchor %s has no pending ledger row; an included file is not a reviewed one", p)
		}
	}
	if res, err := CheckRightsLedger(sel, sum, l); err != nil || res.Eligible {
		t.Errorf("check: %+v %v", res, err)
	}

	// The reader ties declared_includes to the rest of the report.
	raw, _ := os.ReadFile(f.out())
	var r CISelection
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mutate func(r *CISelection)
		want   string
	}{
		"v2 without the block": {func(r *CISelection) { r.DeclaredIncludes = nil }, "declared_includes present=false"},
		"v1 with the block":    {func(r *CISelection) { r.Schema = CISelectionSchema }, "declared_includes present=true"},
		"anchor path not selected": {func(r *CISelection) {
			r.DeclaredIncludes.Anchors[0].FilePath = "images/other.png"
		}, "is not a selected asset"},
		"anchor hash differs": {func(r *CISelection) { r.DeclaredIncludes.Anchors[1].SHA256 = ovSHA("x") }, "disagrees with the closure"},
		"contract not pinned": {func(r *CISelection) {
			var keep []CISelectionInput
			for _, in := range r.Inputs {
				if !strings.HasPrefix(in.Name, "overlap_contract/") {
					keep = append(keep, in)
				}
			}
			r.Inputs = keep
		}, `"overlap_contract/peer-overlap.v1.json" pinned, and it is missing`},
		"duplicate anchor": {func(r *CISelection) {
			r.DeclaredIncludes.Anchors[1] = r.DeclaredIncludes.Anchors[0]
		}, "twice"},
	} {
		var c CISelection
		_ = json.Unmarshal(raw, &c)
		tc.mutate(&c)
		if _, _, err := LoadCISelectionReport(writeReport(t, &c)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// A v2 report's two bindings must each be pinned exactly once, present,
// with the declared digest. Edited or malformed reports that drop,
// duplicate or blank a pin are refused (#1514 declared-includes
// correction round 1).
func TestDeclaredIncludes_ReportPinsAreExact(t *testing.T) {
	f := newIncludeFixture(t, "site_a")
	f.exportWith(f.incOpts(2))
	raw, _ := os.ReadFile(f.out())
	if _, _, err := LoadCISelectionReport(f.out()); err != nil {
		t.Fatalf("the generated v2 report must load: %v", err)
	}
	const manifest, contract = "site/MANIFEST.json", "overlap_contract/peer-overlap.v1.json"
	pick := func(r *CISelection, name string) CISelectionInput {
		for _, in := range r.Inputs {
			if in.Name == name {
				return in
			}
		}
		t.Fatalf("generated report lacks %s", name)
		return CISelectionInput{}
	}
	without := func(r *CISelection, name string) []CISelectionInput {
		var keep []CISelectionInput
		for _, in := range r.Inputs {
			if in.Name != name {
				keep = append(keep, in)
			}
		}
		return keep
	}
	edit := func(name string, fn func(in *CISelectionInput)) func(r *CISelection) {
		return func(r *CISelection) {
			for i := range r.Inputs {
				if r.Inputs[i].Name == name {
					fn(&r.Inputs[i])
				}
			}
		}
	}
	cases := []struct {
		name   string
		mutate func(r *CISelection)
		want   string
	}{
		{"manifest missing", func(r *CISelection) { r.Inputs = without(r, manifest) },
			`"site/MANIFEST.json" pinned, and it is missing`},
		{"manifest duplicated, both matching", func(r *CISelection) { r.Inputs = append(r.Inputs, pick(r, manifest)) },
			`"site/MANIFEST.json" 2 times`},
		{"manifest duplicated, one matching and one not", func(r *CISelection) {
			bad := pick(r, manifest)
			bad.SHA256 = ovSHA("another manifest")
			r.Inputs = append(r.Inputs, bad)
		}, `"site/MANIFEST.json" 2 times`},
		{"manifest absent", edit(manifest, func(in *CISelectionInput) { in.Present, in.Bytes, in.SHA256 = false, 0, "" }),
			`"site/MANIFEST.json" is recorded as absent`},
		{"manifest digest mismatched", edit(manifest, func(in *CISelectionInput) { in.SHA256 = ovSHA("x") }),
			`"site/MANIFEST.json" digest`},
		{"manifest digest malformed", edit(manifest, func(in *CISelectionInput) { in.SHA256 = "abc" }),
			`"site/MANIFEST.json" has a malformed sha256`},
		{"contract missing", func(r *CISelection) { r.Inputs = without(r, contract) },
			`"overlap_contract/peer-overlap.v1.json" pinned, and it is missing`},
		{"contract duplicated", func(r *CISelection) { r.Inputs = append(r.Inputs, pick(r, contract)) },
			`"overlap_contract/peer-overlap.v1.json" 2 times`},
		{"contract duplicated, one matching and one not", func(r *CISelection) {
			bad := pick(r, contract)
			bad.SHA256 = ovSHA("another contract")
			r.Inputs = append([]CISelectionInput{bad}, r.Inputs...)
		}, `"overlap_contract/peer-overlap.v1.json" 2 times`},
		{"contract absent", edit(contract, func(in *CISelectionInput) { in.Present, in.Bytes, in.SHA256 = false, 0, "" }),
			`"overlap_contract/peer-overlap.v1.json" is recorded as absent`},
		{"contract digest mismatched", edit(contract, func(in *CISelectionInput) { in.SHA256 = ovSHA("y") }),
			`"overlap_contract/peer-overlap.v1.json" digest`},
		{"contract pinned under another name", func(r *CISelection) {
			in := pick(r, contract)
			in.Name = "overlap_contract/other.json"
			r.Inputs = append(without(r, contract), in)
		}, `"overlap_contract/peer-overlap.v1.json" pinned, and it is missing`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r CISelection
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&r)
			_, _, err := LoadCISelectionReport(writeReport(t, &r))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}

	// v1 is untouched: a default report with no includes and no contract
	// input still loads, whatever its inputs say about a contract.
	f.exportWith(f.opts(2))
	if _, _, err := LoadCISelectionReport(f.out()); err != nil {
		t.Errorf("v1 report: %v", err)
	}
}
