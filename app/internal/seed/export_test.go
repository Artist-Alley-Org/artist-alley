// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the offline CI-selection export (#1514). Everything here is
// synthetic: the fixture is covFixture's catalogue written out as a real
// site root, so the export runs the same selector the seeder runs, over
// real model files whose companions format3d actually parses.

package seed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type exportFixture struct {
	t      *testing.T
	site   string
	catDir string
	outDir string
	assets []manifestAsset
	posts  []manifestPost
	// extra holds manifest keys the seeder does not model (license,
	// attribution), per asset id. Empty for the default fixture, whose
	// manifest bytes the golden report pins.
	extra map[string]map[string]any
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	c := covFixture(t)
	// The video is the costly asset in the fixture; give it real weight
	// on disk, since the export checks declared sizes against the file.
	mustWrite(t, filepath.Join(c.SiteRoot, "clip.mp4"), strings.Repeat("v", 4096))
	f := &exportFixture{
		t:      t,
		site:   c.SiteRoot,
		catDir: t.TempDir(),
		outDir: t.TempDir(),
		assets: c.Assets,
		posts:  c.Posts,
	}
	f.resize()
	cols, err := json.Marshal(c.Collections)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.catDir, "dataset.collections.json"), string(cols))
	for _, name := range []string{"users", "teams", "field_definitions"} {
		mustWrite(t, filepath.Join(f.catDir, "dataset."+name+".json"), "[]")
	}
	f.write()
	return f
}

// resize sets every row's declared size to its file's length on disk.
func (f *exportFixture) resize() {
	f.t.Helper()
	for i := range f.assets {
		st, err := os.Stat(filepath.Join(f.site, f.assets[i].FilePath))
		if err != nil {
			f.t.Fatal(err)
		}
		f.assets[i].FileSizeBytes = st.Size()
	}
}

// addSolo adds an asset with its own solo post, written to disk at file.
func (f *exportFixture) addSolo(id, typ, ext, file, content string) {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.site, file), content)
	a := manifestAsset{
		ID: id, AssetType: typ, Title: id, FilePath: file, FileExtension: ext,
		FileSizeBytes: int64(len(content)), SensitivityTier: "restricted",
		ArchiveState: "active", WorkflowState: "approved", OwnerUsername: "u1",
		CollectionName: "Alpha", TeamName: "Props", Tags: []string{"t-" + ext},
		FieldValues: map[string]any{"rating": 3},
	}
	f.assets = append(f.assets, a)
	f.posts = append(f.posts, manifestPost{
		ID: "p-" + id, Title: "post " + id, AssetIDs: []string{id}, AuthorUsername: "u1",
		CollectionName: "Alpha", TeamName: "Props", WorkflowState: "approved",
		PostKind: "solo_showcase", SensitivityTier: "restricted", Tags: []string{"p"},
	})
}

func (f *exportFixture) write() {
	f.t.Helper()
	m, err := json.Marshal(f.assets)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(f.extra) > 0 {
		var rows []map[string]any
		if err := json.Unmarshal(m, &rows); err != nil {
			f.t.Fatal(err)
		}
		for _, r := range rows {
			for k, v := range f.extra[r["id"].(string)] {
				r[k] = v
			}
		}
		if m, err = json.Marshal(rows); err != nil {
			f.t.Fatal(err)
		}
	}
	p, err := json.Marshal(f.posts)
	if err != nil {
		f.t.Fatal(err)
	}
	mustWrite(f.t, filepath.Join(f.site, "MANIFEST.json"), string(m))
	mustWrite(f.t, filepath.Join(f.site, "posts.json"), string(p))
}

func (f *exportFixture) opts(depth int) CISelectionOptions {
	return CISelectionOptions{SiteRoot: f.site, CatalogueRoot: f.catDir, CoverageDepth: depth}
}

func (f *exportFixture) out() string { return filepath.Join(f.outDir, "selection.json") }

func (f *exportFixture) export(depth int) (*CISelection, []byte) {
	f.t.Helper()
	if _, err := ExportCISelection(f.opts(depth), f.out()); err != nil {
		f.t.Fatalf("export: %v", err)
	}
	return readSelection(f.t, f.out())
}

// selector runs the seeder's own path, independently of the export.
func (f *exportFixture) selector(depth int) (*catalogues, *coverageReport) {
	f.t.Helper()
	c, err := loadCatalogues(f.catDir, f.site, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	rep, err := c.applyCoverageProfile(depth, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return c, rep
}

func readSelection(t *testing.T, p string) (*CISelection, []byte) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var r CISelection
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	return &r, b
}

func sha256Hex(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// treeSnapshot records every entry under root with its mode, size and
// mtime, and the content hash of regular files.
func treeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		line := fmt.Sprintf("%s|%v|%d|%d", rel, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			line += "|" + sha256Hex(t, p)
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assetIDsOf(r *CISelection) []string {
	out := make([]string, 0, len(r.Assets))
	for _, a := range r.Assets {
		out = append(out, a.ID)
	}
	return out
}

// covFixtureCompanions is what each covFixture model needs, written out
// by hand from the fixture's bytes rather than asked of format3d, so the
// closure is checked against an independent statement of the answer.
var covFixtureCompanions = map[string][]string{
	"ext.glb":  {"tex.png"},
	"ext.gltf": {"model.bin", "tex.png"},
	"ext.obj":  {"ext.mtl", "tex.png"},
	"ext.fbx":  {"tex.png"},
}

func TestExportCISelection_MatchesSeedSelector(t *testing.T) {
	f := newExportFixture(t)
	const depth = 2
	rep, raw := f.export(depth)

	// The reference is the seeder's own path: same loader, same selector.
	c, want := f.selector(depth)
	if want.Assets >= want.AssetsBefore {
		t.Fatalf("fixture does not shrink at depth %d; the test would prove nothing", depth)
	}
	var wantAssets, wantPosts []string
	for _, a := range c.Assets {
		wantAssets = append(wantAssets, a.ID)
	}
	for _, p := range c.Posts {
		wantPosts = append(wantPosts, p.ID)
	}
	sort.Strings(wantAssets)
	sort.Strings(wantPosts)
	if got := assetIDsOf(rep); strings.Join(got, ",") != strings.Join(wantAssets, ",") {
		t.Errorf("asset ids = %v, want %v", got, wantAssets)
	}
	if strings.Join(rep.PostIDs, ",") != strings.Join(wantPosts, ",") {
		t.Errorf("post ids = %v, want %v", rep.PostIDs, wantPosts)
	}

	if rep.Schema != CISelectionSchema || rep.Profile != ProfileCI || rep.CoverageDepth != depth {
		t.Errorf("header = %q %q %d", rep.Schema, rep.Profile, rep.CoverageDepth)
	}

	n := rep.Counts
	if n.AssetsSelected != want.Assets || n.PostsSelected != want.Posts ||
		n.AssetsTotal != want.AssetsBefore || n.PostsTotal != want.PostsBefore ||
		n.SelectedBytes != want.Bytes || n.CompanionAssets != want.Companions ||
		n.ObservedDimensions != want.Universe || n.CoveredDimensions != want.Covered {
		t.Errorf("counts = %+v, selector = %+v", n, want)
	}
	if n.AssetsSelected != len(rep.Assets) || n.PostsSelected != len(rep.PostIDs) {
		t.Errorf("counts disagree with the lists: %+v", n)
	}
	if n.CoveredDimensions != n.ObservedDimensions {
		t.Errorf("covered %d of %d dimensions", n.CoveredDimensions, n.ObservedDimensions)
	}
	if len(rep.Dimensions.Covered) != n.CoveredDimensions {
		t.Errorf("%d covered names for a count of %d", len(rep.Dimensions.Covered), n.CoveredDimensions)
	}
	if n.CompanionAssets == 0 || n.CompanionFiles == 0 {
		t.Errorf("fixture selected no companion-bearing asset: %+v", n)
	}
	if n.UnknownModelAssets != 0 {
		t.Errorf("covFixture has no unreadable model format, got %d", n.UnknownModelAssets)
	}

	// Every declared and every observed dimension is reported covered.
	for _, d := range requiredDims {
		if !contains(rep.Dimensions.Required, d.String()) {
			t.Errorf("required list lacks %s", d)
		}
		if !contains(rep.Dimensions.Covered, d.String()) {
			t.Errorf("required dimension %s not reported covered", d)
		}
	}
	if len(rep.Dimensions.Required) != len(requiredDims) {
		t.Errorf("required = %v", rep.Dimensions.Required)
	}
	for _, list := range [][]string{assetIDsOf(rep), rep.PostIDs, rep.Dimensions.Required, rep.Dimensions.Covered} {
		if !sort.StringsAreSorted(list) {
			t.Errorf("list not sorted: %v", list)
		}
	}

	// Closure: each selected asset names its primary file and exactly the
	// companions its model needs, and every one of them is a report file.
	files := map[string]CISelectionFile{}
	for i, file := range rep.Files {
		if i > 0 && rep.Files[i-1].Path >= file.Path {
			t.Errorf("files not strictly sorted at %q", file.Path)
		}
		files[file.Path] = file
	}
	byID := map[string]manifestAsset{}
	for _, a := range c.Assets {
		byID[a.ID] = a
	}
	for _, a := range rep.Assets {
		m := byID[a.ID]
		if a.FilePath != m.FilePath {
			t.Errorf("asset %q: file_path %q, manifest %q", a.ID, a.FilePath, m.FilePath)
		}
		if !contains(files[a.FilePath].PrimaryFor, a.ID) {
			t.Errorf("asset %q: primary %q missing from files", a.ID, a.FilePath)
		}
		wantComp, isModel := covFixtureCompanions[a.FilePath]
		switch {
		case isModel:
			if a.CompanionSupport != CompanionsResolved {
				t.Errorf("asset %q: support %q", a.ID, a.CompanionSupport)
			}
			if strings.Join(a.Companions, ",") != strings.Join(wantComp, ",") {
				t.Errorf("asset %q companions = %v, want %v", a.ID, a.Companions, wantComp)
			}
		default:
			if a.CompanionSupport != CompanionsNotAModel || len(a.Companions) != 0 {
				t.Errorf("asset %q: support %q companions %v", a.ID, a.CompanionSupport, a.Companions)
			}
		}
		for _, cp := range a.Companions {
			if !contains(files[cp].CompanionOf, a.ID) {
				t.Errorf("asset %q: companion %q is not tied back to it", a.ID, cp)
			}
		}
	}

	// Files: measured, digested from the bytes on disk, and every role
	// points at an asset that really needs the file.
	reported := map[string]CISelectionAsset{}
	for _, a := range rep.Assets {
		reported[a.ID] = a
	}
	var unique, selected int64
	for _, file := range rep.Files {
		p := filepath.Join(f.site, file.Path)
		st, err := os.Stat(p)
		if err != nil || st.Size() != file.Bytes {
			t.Errorf("file %q: reported %d bytes, on disk %v (%v)", file.Path, file.Bytes, st, err)
		}
		if file.SHA256 != sha256Hex(t, p) {
			t.Errorf("file %q: digest does not match its bytes", file.Path)
		}
		if len(file.PrimaryFor)+len(file.CompanionOf) == 0 {
			t.Errorf("file %q has no role", file.Path)
		}
		for _, id := range file.PrimaryFor {
			if reported[id].FilePath != file.Path {
				t.Errorf("file %q lists %q as primary, which points at %q", file.Path, id, reported[id].FilePath)
			}
			selected += file.Bytes
		}
		for _, id := range file.CompanionOf {
			if !contains(reported[id].Companions, file.Path) {
				t.Errorf("file %q lists %q as a companion user, which does not list it", file.Path, id)
			}
		}
		unique += file.Bytes
	}
	if n.UniqueFiles != len(rep.Files) || n.UniqueFileBytes != unique || n.SelectedBytes != selected {
		t.Errorf("file totals %+v vs unique=%d selected=%d", n, unique, selected)
	}
	if n.UniqueFileBytes >= n.SelectedBytes {
		t.Errorf("fixture shares files between assets; unique bytes %d should be below selected bytes %d",
			n.UniqueFileBytes, n.SelectedBytes)
	}
	// tex.png is a companion of every model format, and model.bin of the
	// glTF: a shared companion is one file with several users.
	if got := len(files["tex.png"].CompanionOf); got < 4 {
		t.Errorf("tex.png has %d companion users, want one per selected model", got)
	}

	// No site root path, no free text and no file content in the report.
	if bytes.Contains(raw, []byte(f.site)) {
		t.Errorf("report contains the absolute site root")
	}
	for _, leak := range []string{"title", "\"owner", "metadata", "license", "bytes-a.png", "newmtl"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("report contains %q", leak)
		}
	}
}

func TestExportCISelection_PinsEveryLoadedInput(t *testing.T) {
	f := newExportFixture(t)
	names := []string{
		"catalogue/dataset.collections.json",
		"catalogue/dataset.field_definitions.json",
		"catalogue/dataset.fixtures.json",
		"catalogue/dataset.teams.json",
		"catalogue/dataset.users.json",
		"site/MANIFEST.json",
		"site/posts.json",
	}
	diskPath := func(name string) string {
		dir, base, _ := strings.Cut(name, "/")
		if dir == "site" {
			return filepath.Join(f.site, base)
		}
		return filepath.Join(f.catDir, base)
	}
	check := func(rep *CISelection, fixturesPresent bool) {
		t.Helper()
		var got []string
		for _, in := range rep.Inputs {
			got = append(got, in.Name)
			want := in.Name != "catalogue/dataset.fixtures.json" || fixturesPresent
			if in.Present != want {
				t.Errorf("%s: present = %v, want %v", in.Name, in.Present, want)
				continue
			}
			if !in.Present {
				if in.SHA256 != "" || in.Bytes != 0 {
					t.Errorf("%s: absent input carries a digest %+v", in.Name, in)
				}
				continue
			}
			st, _ := os.Stat(diskPath(in.Name))
			if in.SHA256 != sha256Hex(t, diskPath(in.Name)) || st == nil || in.Bytes != st.Size() {
				t.Errorf("%s: pin %+v does not match the file", in.Name, in)
			}
		}
		if strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("inputs = %v, want %v", got, names)
		}
	}

	rep, _ := f.export(2)
	check(rep, false)

	// The optional fixtures catalogue is pinned once it exists, and a
	// change to any input changes its pin and only its pin.
	mustWrite(t, filepath.Join(f.catDir, "dataset.fixtures.json"), `{"principals":[]}`)
	withFix, _ := f.export(2)
	check(withFix, true)
	mustWrite(t, filepath.Join(f.catDir, "dataset.users.json"), "[ ]")
	edited, _ := f.export(2)
	check(edited, true)
	for i := range edited.Inputs {
		changed := edited.Inputs[i].SHA256 != withFix.Inputs[i].SHA256
		if changed != (edited.Inputs[i].Name == "catalogue/dataset.users.json") {
			t.Errorf("%s: changed = %v", edited.Inputs[i].Name, changed)
		}
	}
}

func TestExportCISelection_DefaultDepthIsTheSeedDefault(t *testing.T) {
	f := newExportFixture(t)
	rep, _ := f.export(0)
	if rep.CoverageDepth != defaultCoverageDepth {
		t.Errorf("depth = %d, want %d", rep.CoverageDepth, defaultCoverageDepth)
	}
	_, want := f.selector(defaultCoverageDepth)
	if rep.Counts.AssetsSelected != want.Assets || rep.Counts.PostsSelected != want.Posts {
		t.Errorf("default-depth counts %+v, selector %d/%d", rep.Counts, want.Assets, want.Posts)
	}
	shallow, _ := f.export(1)
	if shallow.Counts.AssetsSelected >= rep.Counts.AssetsSelected {
		t.Errorf("depth 1 kept %d assets, depth %d kept %d", shallow.Counts.AssetsSelected,
			defaultCoverageDepth, rep.Counts.AssetsSelected)
	}
}

func TestExportCISelection_Deterministic(t *testing.T) {
	f := newExportFixture(t)
	_, first := f.export(2)
	for i := 0; i < 5; i++ {
		_, again := f.export(2)
		if !bytes.Equal(first, again) {
			t.Fatalf("run %d differs from the first", i)
		}
	}
	other := filepath.Join(t.TempDir(), "elsewhere.json")
	if _, err := ExportCISelection(f.opts(2), other); err != nil {
		t.Fatal(err)
	}
	_, b := readSelection(t, other)
	if !bytes.Equal(first, b) {
		t.Fatalf("the report depends on its output path")
	}
	// Input row order is the selector's tie-break, so it is not varied
	// here; TestCIClosure_Cases proves the closure ignores it.
}

func TestExportCISelection_ReadsOnly(t *testing.T) {
	f := newExportFixture(t)
	site := treeSnapshot(t, f.site)
	cat := treeSnapshot(t, f.catDir)
	f.export(2)
	if got := treeSnapshot(t, f.site); strings.Join(got, "\n") != strings.Join(site, "\n") {
		t.Errorf("site root changed:\nbefore %v\nafter  %v", site, got)
	}
	if got := treeSnapshot(t, f.catDir); strings.Join(got, "\n") != strings.Join(cat, "\n") {
		t.Errorf("catalogue directory changed")
	}
}

func TestExportCISelection_OutputFile(t *testing.T) {
	f := newExportFixture(t)
	f.export(2)
	st, err := os.Stat(f.out())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(f.outDir)
	if len(entries) != 1 {
		t.Errorf("output directory holds %d entries, want just the report", len(entries))
	}

	// Replacing an existing report keeps the restrictive mode, even if
	// the old file was looser.
	if err := os.Chmod(f.out(), 0o644); err != nil {
		t.Fatal(err)
	}
	f.export(2)
	if st, _ := os.Stat(f.out()); st.Mode().Perm() != 0o600 {
		t.Errorf("replaced file mode = %v, want 0600", st.Mode().Perm())
	}

	// A failed export leaves the previous report intact and no temp file.
	before, _ := os.ReadFile(f.out())
	f.assets[0].ID = f.assets[1].ID
	f.write()
	if _, err := ExportCISelection(f.opts(2), f.out()); err == nil {
		t.Fatal("export of a duplicate-id manifest succeeded")
	}
	after, _ := os.ReadFile(f.out())
	if !bytes.Equal(before, after) {
		t.Errorf("a refused export modified the existing report")
	}
	if entries, _ := os.ReadDir(f.outDir); len(entries) != 1 {
		t.Errorf("refused export left %d entries behind", len(entries))
	}
}

func TestExportCISelection_RefusesBadOutputPath(t *testing.T) {
	f := newExportFixture(t)
	before := treeSnapshot(t, f.site)

	for name, out := range map[string]string{
		"empty":            "",
		"inside site":      filepath.Join(f.site, "selection.json"),
		"inside site dir":  filepath.Join(f.site, "sub", "selection.json"),
		"missing parent":   filepath.Join(f.outDir, "nope", "selection.json"),
		"existing dir":     f.outDir,
		"site root itself": f.site,
	} {
		if _, err := ExportCISelection(f.opts(2), out); err == nil {
			t.Errorf("%s: export succeeded", name)
		}
	}
	link := filepath.Join(f.outDir, "link.json")
	if err := os.Symlink(filepath.Join(f.site, "MANIFEST.json"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportCISelection(f.opts(2), link); err == nil {
		t.Errorf("export over a symlink succeeded")
	}
	// A symlinked directory that points into the site is still the site.
	dirLink := filepath.Join(f.outDir, "sitelink")
	if err := os.Symlink(f.site, dirLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportCISelection(f.opts(2), filepath.Join(dirLink, "selection.json")); err == nil {
		t.Errorf("export through a symlink into the site succeeded")
	}
	if got := treeSnapshot(t, f.site); strings.Join(got, "\n") != strings.Join(before, "\n") {
		t.Errorf("site root changed during refused exports")
	}
	if b, err := os.ReadFile(filepath.Join(f.site, "MANIFEST.json")); err != nil || len(b) == 0 {
		t.Errorf("manifest damaged: %v", err)
	}
}

// The catalogue root is an input tree exactly as the site root is: a
// report written over `seed/profiles/dataset.users.json` would replace an
// input right after the report pinned it (#1514 correction round 1).
func TestExportCISelection_RefusesCatalogueAndInputDestinations(t *testing.T) {
	f := newExportFixture(t)
	mustWrite(t, filepath.Join(f.catDir, "dataset.fixtures.json"), `{"principals":[]}`)
	if err := os.Mkdir(filepath.Join(f.catDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := t.TempDir() // aliases live outside both trees
	alias := func(name, target string) string {
		p := filepath.Join(links, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	catLink := alias("catlink", f.catDir)
	subLink := alias("sublink", filepath.Join(f.catDir, "sub"))
	siteLink := alias("sitelink", f.site)
	chain := alias("chain", catLink) // a symlink to a symlink to the catalogue

	siteBefore := treeSnapshot(t, f.site)
	catBefore := treeSnapshot(t, f.catDir)

	const inCat, inSite = "inside the catalogue root", "inside the site root"
	cases := []struct{ name, out, want string }{
		{"inside the catalogue", filepath.Join(f.catDir, "selection.json"), inCat},
		{"a catalogue input itself", filepath.Join(f.catDir, "dataset.users.json"), inCat},
		{"the optional catalogue input itself", filepath.Join(f.catDir, "dataset.fixtures.json"), inCat},
		{"a catalogue subdirectory", filepath.Join(f.catDir, "sub", "selection.json"), inCat},
		{"a symlinked catalogue alias", filepath.Join(catLink, "selection.json"), inCat},
		{"a catalogue input through an alias", filepath.Join(catLink, "dataset.users.json"), inCat},
		{"a catalogue input through a chained alias", filepath.Join(chain, "dataset.teams.json"), inCat},
		{"a symlinked catalogue subdirectory alias", filepath.Join(subLink, "selection.json"), inCat},
		{"the manifest itself", filepath.Join(f.site, "MANIFEST.json"), inSite},
		{"the posts file through a site alias", filepath.Join(siteLink, "posts.json"), inSite},
	}
	for _, tc := range cases {
		_, err := ExportCISelection(f.opts(2), tc.out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}

	// The same refusal for a relative path, as the CLI would receive it.
	t.Chdir(filepath.Dir(f.catDir))
	rel := filepath.Join(filepath.Base(f.catDir), "dataset.users.json")
	if _, err := ExportCISelection(f.opts(2), rel); err == nil || !strings.Contains(err.Error(), inCat) {
		t.Errorf("relative catalogue input: err = %v", err)
	}
	// And when the catalogue root is itself named through an alias.
	viaAlias := f.opts(2)
	viaAlias.CatalogueRoot = catLink
	if _, err := ExportCISelection(viaAlias, filepath.Join(f.catDir, "dataset.users.json")); err == nil ||
		!strings.Contains(err.Error(), inCat) {
		t.Errorf("catalogue root given as an alias: err = %v", err)
	}

	// Both input trees are byte-for-byte untouched: no replaced input, no
	// new file, no leftover temp file, no changed directory entry.
	if got := treeSnapshot(t, f.site); strings.Join(got, "\n") != strings.Join(siteBefore, "\n") {
		t.Errorf("site root changed:\nbefore %v\nafter  %v", siteBefore, got)
	}
	if got := treeSnapshot(t, f.catDir); strings.Join(got, "\n") != strings.Join(catBefore, "\n") {
		t.Errorf("catalogue root changed:\nbefore %v\nafter  %v", catBefore, got)
	}

	// A valid destination still gets the atomic 0600 report.
	if _, err := ExportCISelection(f.opts(2), f.out()); err != nil {
		t.Fatalf("valid destination: %v", err)
	}
	if st, err := os.Stat(f.out()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("valid destination: %v %v", st, err)
	}
}

// The destination is checked before the build and again just before the
// write, against the directory handle held since the first check.
func TestExportCISelection_OutputDirectorySwappedDuringBuild(t *testing.T) {
	cases := []struct {
		name    string
		swap    func(f *exportFixture, dest string)
		wantErr string
	}{
		{"swapped for an alias of the catalogue", func(f *exportFixture, dest string) {
			if err := os.Rename(dest, dest+".moved"); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(f.catDir, dest); err != nil {
				f.t.Fatal(err)
			}
		}, "inside the catalogue root"},
		{"swapped for a different directory", func(f *exportFixture, dest string) {
			if err := os.Rename(dest, dest+".moved"); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Mkdir(dest, 0o755); err != nil {
				f.t.Fatal(err)
			}
		}, "output directory changed while the report was being built"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			dest := filepath.Join(f.outDir, "dest")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			catBefore := treeSnapshot(t, f.catDir)
			ciSelectionAfterHash = func() { tc.swap(f, dest) }
			t.Cleanup(func() { ciSelectionAfterHash = nil })
			_, err := ExportCISelection(f.opts(2), filepath.Join(dest, "dataset.users.json"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if got := treeSnapshot(t, f.catDir); strings.Join(got, "\n") != strings.Join(catBefore, "\n") {
				t.Errorf("catalogue root changed")
			}
			// Neither the held directory nor whatever now sits at the path
			// may have gained the report or a temp file.
			if entries, err := os.ReadDir(dest + ".moved"); err != nil || len(entries) != 0 {
				t.Errorf("held directory: %d entries (%v); a refused write must leave nothing", len(entries), err)
			}
			if fi, err := os.Lstat(dest); err == nil && fi.IsDir() {
				if entries, _ := os.ReadDir(dest); len(entries) != 0 {
					t.Errorf("replacement directory gained %d entries", len(entries))
				}
			}
		})
	}
}

// skipIfRoot skips only the permission-denied cases, which cannot be
// produced for a process that ignores file modes.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode-000 file is still readable, so this case cannot be built")
	}
}

func TestExportCISelection_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		depth   int
		mutate  func(f *exportFixture)
		wantErr string
		// editsDisk: the case edits files itself; the fixture's in-memory
		// rows must not be written back over what it did.
		editsDisk bool
	}{
		{name: "duplicate asset id", depth: 2, mutate: func(f *exportFixture) { f.assets[3].ID = f.assets[2].ID }, wantErr: "duplicate asset id"},
		{name: "empty asset id", depth: 2, mutate: func(f *exportFixture) { f.assets[3].ID = "" }, wantErr: "no id"},
		{name: "duplicate post id", depth: 2, mutate: func(f *exportFixture) { f.posts[3].ID = f.posts[2].ID }, wantErr: "duplicate post id"},
		{name: "empty post id", depth: 2, mutate: func(f *exportFixture) { f.posts[3].ID = "" }, wantErr: "no id"},
		{name: "post of unknown asset", depth: 2, mutate: func(f *exportFixture) {
			f.posts[0].AssetIDs = append(f.posts[0].AssetIDs, "ghost")
		}, wantErr: "unknown asset"},
		{name: "post lists an asset twice", depth: 2, mutate: func(f *exportFixture) {
			f.posts[0].AssetIDs = append(f.posts[0].AssetIDs, f.posts[0].AssetIDs[0])
		}, wantErr: "twice"},
		{name: "parent traversal", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "../outside.png" }, wantErr: "unsafe"},
		{name: "nested traversal", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "a/../../outside.png" }, wantErr: "unsafe"},
		{name: "absolute path", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "/etc/hostname" }, wantErr: "unsafe"},
		{name: "dot segment", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "./a.png" }, wantErr: "unsafe"},
		{name: "double slash", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "sub//a.png" }, wantErr: "unsafe"},
		{name: "trailing slash", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "a.png/" }, wantErr: "unsafe"},
		{name: "backslash", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = `sub\a.png` }, wantErr: "unsafe"},
		{name: "NUL", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "a.png\x00x" }, wantErr: "unsafe"},
		{name: "drive letter", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "C:/a.png" }, wantErr: "unsafe"},
		{name: "empty path", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FilePath = "" }, wantErr: "empty file_path"},
		{name: "unsafe path on an unselected asset", depth: 1, mutate: func(f *exportFixture) {
			f.assets[len(f.assets)-1].FilePath = "../x.png"
		}, wantErr: "unsafe"},
		{name: "selected file missing", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "a.png"))
		}, wantErr: "missing or not readable"},
		{name: "selected file is a directory", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "c.jpg"))
			if err := os.Mkdir(filepath.Join(f.site, "c.jpg"), 0o755); err != nil {
				f.t.Fatal(err)
			}
		}, wantErr: "not a regular file"},
		{name: "selected file unreadable", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			skipIfRoot(f.t)
			chmodRestore(f.t, filepath.Join(f.site, "a.png"), 0)
		}, wantErr: "permission denied"},
		{name: "size disagrees with the manifest", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "a.png"), "different length bytes")
		}, wantErr: "manifest declares"},
		{name: "negative size", depth: 2, mutate: func(f *exportFixture) { f.assets[3].FileSizeBytes = -1 }, wantErr: "negative"},
		{name: "symlink loop", depth: 2, mutate: func(f *exportFixture) {
			// EvalSymlinks reports a loop with an error that wraps nothing,
			// which is exactly the case errors.Unwrap turned into nil.
			if err := os.Symlink("loop.png", filepath.Join(f.site, "loop.png")); err != nil {
				f.t.Fatal(err)
			}
			f.assets[3].FilePath = "loop.png"
		}, wantErr: "too many links"},
		{name: "symlink out of the site root", depth: 2, mutate: func(f *exportFixture) {
			outside := filepath.Join(f.t.TempDir(), "secret.png")
			mustWrite(f.t, outside, "outside")
			if err := os.Symlink(outside, filepath.Join(f.site, "link.png")); err != nil {
				f.t.Fatal(err)
			}
			f.assets[3].FilePath = "link.png"
			f.assets[3].FileSizeBytes = int64(len("outside"))
		}, wantErr: "outside the site root"},
		{name: "directory symlink out of the site root", depth: 2, mutate: func(f *exportFixture) {
			outside := f.t.TempDir()
			mustWrite(f.t, filepath.Join(outside, "x.png"), "outside")
			if err := os.Symlink(outside, filepath.Join(f.site, "dirlink")); err != nil {
				f.t.Fatal(err)
			}
			f.assets[3].FilePath = "dirlink/x.png"
			f.assets[3].FileSizeBytes = int64(len("outside"))
		}, wantErr: "outside the site root"},

		// Companion closure. The seeder soft-fails each of these; the
		// export must refuse, or the package ships a broken model.
		{name: "glTF buffer missing", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "model.bin"))
		}, wantErr: "declared companion(s) missing: model.bin"},
		{name: "MTL texture missing", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "ext.mtl"), "newmtl m\nmap_Kd tex.png\nbump gone.png\n")
		}, wantErr: "declared companion(s) missing: gone.png"},
		{name: "OBJ material library missing", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "ext.obj"), "mtllib ext.mtl lost.mtl\nv 0 0 0\n")
			f.resize()
			f.write()
		}, wantErr: "declared companion(s) missing: lost.mtl"},
		{name: "companion is a directory", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "model.bin"))
			if err := os.Mkdir(filepath.Join(f.site, "model.bin"), 0o755); err != nil {
				f.t.Fatal(err)
			}
		}, wantErr: `companion "model.bin": file is not a regular file`},
		{name: "companion symlink out of the site root", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			outside := filepath.Join(f.t.TempDir(), "model.bin")
			mustWrite(f.t, outside, "binbytes")
			mustRemove(f.t, filepath.Join(f.site, "model.bin"))
			if err := os.Symlink(outside, filepath.Join(f.site, "model.bin")); err != nil {
				f.t.Fatal(err)
			}
		}, wantErr: `companion "model.bin": file_path resolves outside the site root`},
		{name: "companion unreadable", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			skipIfRoot(f.t)
			chmodRestore(f.t, filepath.Join(f.site, "model.bin"), 0)
		}, wantErr: `file "model.bin": open`},
		{name: "selected model cannot be parsed", depth: 8, editsDisk: true, mutate: func(f *exportFixture) {
			// A fourth glTF that the depth-8 extension floor selects; the
			// other three still supply companions=gltf, so the selector
			// accepts the catalogue and the closure is what refuses it.
			f.addSolo("gltf-bad", "3d", "gltf", "bad.gltf", "{not json")
			f.write()
		}, wantErr: `"gltf-bad": companions could not be read`},

		{name: "catalogue without mature assets", depth: 2, mutate: func(f *exportFixture) {
			for i := range f.assets {
				f.assets[i].Mature = false
			}
		}, wantErr: "asset.mature=true"},
		{name: "catalogue without fonts", depth: 2, mutate: func(f *exportFixture) {
			var keep []manifestAsset
			for _, a := range f.assets {
				if a.AssetType != "font" {
					keep = append(keep, a)
				}
			}
			f.assets = keep
			var posts []manifestPost
			for _, p := range f.posts {
				if !strings.HasPrefix(p.ID, "p-fnt-") {
					posts = append(posts, p)
				}
			}
			f.posts = posts
		}, wantErr: "asset.type=font"},
		{name: "false AI declaration", depth: 2, mutate: func(f *exportFixture) {
			v := "generated"
			f.assets[3].AiProvenance = &v
			f.assets[3].Metadata = json.RawMessage(`{"acquisition_source":"Kenney (kenney.nl)"}`)
		}, wantErr: "ai_provenance"},
		{name: "negative depth", depth: -1, mutate: func(f *exportFixture) {}, wantErr: "negative"},
		{name: "missing manifest", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "MANIFEST.json"))
		}, wantErr: "MANIFEST.json"},
		{name: "missing posts", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "posts.json"))
		}, wantErr: "posts.json"},
		{name: "missing collections", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.catDir, "dataset.collections.json"))
		}, wantErr: "dataset.collections.json"},
		{name: "invalid fixtures catalogue", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.catDir, "dataset.fixtures.json"), `{"principals":`)
		}, wantErr: "dataset.fixtures.json"},
		{name: "invalid manifest", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "MANIFEST.json"), `[{"id":`)
		}, wantErr: "parse"},
		{name: "invalid posts", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "posts.json"), `{}`)
		}, wantErr: "parse"},
		{name: "empty manifest", depth: 2, editsDisk: true, mutate: func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "MANIFEST.json"), `[]`)
			mustWrite(f.t, filepath.Join(f.site, "posts.json"), `[]`)
		}, wantErr: "required dimension"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			tc.mutate(f)
			if !tc.editsDisk {
				f.write()
			}
			_, err := ExportCISelection(f.opts(tc.depth), f.out())
			if err == nil {
				t.Fatalf("export succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			// errors.Unwrap on a non-wrapping error used to print as
			// %!w(<nil>) and erase the cause; no message may carry that.
			if strings.Contains(err.Error(), "%!") {
				t.Errorf("error has a formatting fault: %v", err)
			}
			if _, statErr := os.Stat(f.out()); statErr == nil {
				t.Errorf("a refused export still wrote a report")
			}
			if entries, _ := os.ReadDir(f.outDir); len(entries) != 0 {
				t.Errorf("a refused export left %d file(s) behind", len(entries))
			}
		})
	}
}

// The cause of a refusal must survive wrapping, so a caller can still
// tell "not there" from "not allowed" (#1514 review: errors.Unwrap had
// replaced the cause with nil for errors that wrap nothing).
func TestExportCISelection_ErrorsKeepTheirCause(t *testing.T) {
	missing := func(mut func(f *exportFixture)) error {
		f := newExportFixture(t)
		mut(f)
		_, err := BuildCISelection(f.opts(2))
		return err
	}
	for name, err := range map[string]error{
		"manifest": missing(func(f *exportFixture) { mustRemove(t, filepath.Join(f.site, "MANIFEST.json")) }),
		"selected": missing(func(f *exportFixture) { mustRemove(t, filepath.Join(f.site, "a.png")) }),
		"site root": func() error {
			_, err := BuildCISelection(CISelectionOptions{SiteRoot: filepath.Join(t.TempDir(), "nope"), CatalogueRoot: t.TempDir()})
			return err
		}(),
	} {
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: error %v does not wrap fs.ErrNotExist", name, err)
		}
	}
	if os.Geteuid() != 0 {
		f := newExportFixture(t)
		chmodRestore(t, filepath.Join(f.site, "model.bin"), 0)
		if _, err := BuildCISelection(f.opts(2)); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("unreadable companion: error %v does not wrap fs.ErrPermission", err)
		}
	}
}

func TestExportCISelection_RefusesMissingInputs(t *testing.T) {
	f := newExportFixture(t)
	for name, o := range map[string]CISelectionOptions{
		"no site":              {CatalogueRoot: f.catDir},
		"no catalogue":         {SiteRoot: f.site},
		"site does not exist":  {SiteRoot: filepath.Join(f.site, "nope"), CatalogueRoot: f.catDir},
		"site is a file":       {SiteRoot: filepath.Join(f.site, "a.png"), CatalogueRoot: f.catDir},
		"catalogue is missing": {SiteRoot: f.site, CatalogueRoot: filepath.Join(f.catDir, "nope")},
	} {
		if _, err := BuildCISelection(o); err == nil {
			t.Errorf("%s: build succeeded", name)
		}
	}
}

func TestExportCISelection_UnreadableModelFormatIsUnknownNotEmpty(t *testing.T) {
	// An STL is a model format format3d has no reference reader for. The
	// report must say its companions are unknown, never that it has none.
	f := newExportFixture(t)
	f.addSolo("stl-0", "3d", "stl", "part.stl", "solid x\nendsolid x\n")
	f.write()
	rep, _ := f.export(2)
	var got *CISelectionAsset
	for i := range rep.Assets {
		if rep.Assets[i].ID == "stl-0" {
			got = &rep.Assets[i]
		}
	}
	if got == nil {
		t.Fatal("the only .stl was not selected; ext=stl is a dimension the selector must cover")
	}
	if got.CompanionSupport != CompanionsUnknownModel || len(got.Companions) != 0 {
		t.Errorf("stl asset = %+v, want support %q", *got, CompanionsUnknownModel)
	}
	if rep.Counts.UnknownModelAssets != 1 {
		t.Errorf("unknown model assets = %d, want 1", rep.Counts.UnknownModelAssets)
	}
}

func TestExportCISelection_DetectsConcurrentChange(t *testing.T) {
	cases := []struct {
		name    string
		change  func(f *exportFixture)
		wantErr string
	}{
		{"primary file rewritten at the same length", func(f *exportFixture) {
			b, _ := os.ReadFile(filepath.Join(f.site, "a.png"))
			mustWrite(f.t, filepath.Join(f.site, "a.png"), strings.Repeat("x", len(b)))
		}, `file "a.png" changed during the export`},
		{"companion rewritten at the same length", func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.site, "model.bin"), "BINBYTES")
		}, `file "model.bin" changed during the export`},
		{"companion replaced by an identical copy", func(f *exportFixture) {
			// Same bytes, new file: the digest alone cannot see this, the
			// file identity can. A package built later reads the new file.
			p := filepath.Join(f.site, "tex.png")
			b, _ := os.ReadFile(p)
			tmp := p + ".new"
			mustWrite(f.t, tmp, string(b))
			if err := os.Rename(tmp, p); err != nil {
				f.t.Fatal(err)
			}
		}, `file "tex.png" changed during the export`},
		{"selected file removed", func(f *exportFixture) {
			mustRemove(f.t, filepath.Join(f.site, "c.jpg"))
		}, "changed during the export"},
		{"catalogue input edited", func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.catDir, "dataset.users.json"), "[ ]")
		}, "a catalogue input changed during the export"},
		{"optional input appears", func(f *exportFixture) {
			mustWrite(f.t, filepath.Join(f.catDir, "dataset.fixtures.json"), `{"principals":[]}`)
		}, "a catalogue input changed during the export"},
		{"model gains a companion", func(f *exportFixture) {
			// The material now also names an existing file. Inputs and
			// selection are unchanged; only the closure moved.
			mustWrite(f.t, filepath.Join(f.site, "ext.mtl"), "newmtl m\nmap_Kd tex.png\nmap_Ks b.png\n")
		}, "the companion closure changed during the export"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newExportFixture(t)
			if _, err := ExportCISelection(f.opts(2), f.out()); err != nil {
				t.Fatalf("control export: %v", err)
			}
			mustRemove(t, f.out())
			ciSelectionAfterHash = func() { tc.change(f) }
			t.Cleanup(func() { ciSelectionAfterHash = nil })
			_, err := ExportCISelection(f.opts(2), f.out())
			if err == nil {
				t.Fatalf("export succeeded over a change, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(f.out()); statErr == nil {
				t.Errorf("an export over a change still wrote a report")
			}
		})
	}
}

// TestCIClosure_Cases drives the closure walk directly over hand-built
// trees: empty, single, multiple and duplicate paths, nested companion
// directories, and the three support outcomes.
func TestCIClosure_Cases(t *testing.T) {
	type want struct {
		assets map[string][]string // id -> companions
		files  map[string]string   // path -> "P:ids|C:ids"
	}
	asset := func(id, typ, ext, file string) manifestAsset {
		return manifestAsset{ID: id, AssetType: typ, FileExtension: ext, FilePath: file}
	}
	cases := []struct {
		name   string
		tree   map[string]string
		assets []manifestAsset
		want   want
		supp   map[string]string
	}{
		{name: "empty", want: want{assets: map[string][]string{}, files: map[string]string{}}},
		{
			name:   "single non-model file",
			tree:   map[string]string{"a.png": "png"},
			assets: []manifestAsset{asset("a", "image", "png", "a.png")},
			want:   want{assets: map[string][]string{"a": {}}, files: map[string]string{"a.png": "P:a|C:"}},
			supp:   map[string]string{"a": CompanionsNotAModel},
		},
		{
			name:   "self-contained glb resolves to an empty, real answer",
			tree:   map[string]string{},
			assets: []manifestAsset{asset("g", "3d", "glb", "m/solid.glb")},
			want:   want{assets: map[string][]string{"g": {}}, files: map[string]string{"m/solid.glb": "P:g|C:"}},
			supp:   map[string]string{"g": CompanionsResolved},
		},
		{
			name: "multiple companions in nested directories",
			tree: map[string]string{
				"models/m.obj":         "mtllib mat/m.mtl\nv 0 0 0\n",
				"models/mat/m.mtl":     "newmtl m\nmap_Kd tex/d.png\nbump -bm 0.5 tex/n.png\n",
				"models/mat/tex/d.png": "d",
				"models/mat/tex/n.png": "n",
			},
			assets: []manifestAsset{asset("o", "3d", "obj", "models/m.obj")},
			want: want{
				assets: map[string][]string{"o": {"models/mat/m.mtl", "models/mat/tex/d.png", "models/mat/tex/n.png"}},
				files: map[string]string{
					"models/m.obj":         "P:o|C:",
					"models/mat/m.mtl":     "P:|C:o",
					"models/mat/tex/d.png": "P:|C:o",
					"models/mat/tex/n.png": "P:|C:o",
				},
			},
			supp: map[string]string{"o": CompanionsResolved},
		},
		{
			name: "duplicate paths: shared primary, shared companion, companion that is also a primary",
			tree: map[string]string{
				"a.gltf":  `{"buffers":[{"uri":"buf.bin"}],"images":[{"uri":"tex.png"}]}`,
				"b.gltf":  `{"images":[{"uri":"tex.png"},{"uri":"tex.png"}]}`,
				"buf.bin": "b",
				"tex.png": "t",
			},
			assets: []manifestAsset{
				asset("a1", "3d", "gltf", "a.gltf"),
				asset("a2", "3d", "gltf", "a.gltf"),
				asset("b", "3d", "gltf", "b.gltf"),
				asset("t", "image", "png", "tex.png"),
			},
			want: want{
				assets: map[string][]string{
					"a1": {"buf.bin", "tex.png"}, "a2": {"buf.bin", "tex.png"},
					"b": {"tex.png"}, "t": {},
				},
				files: map[string]string{
					"a.gltf":  "P:a1,a2|C:",
					"b.gltf":  "P:b|C:",
					"buf.bin": "P:|C:a1,a2",
					"tex.png": "P:t|C:a1,a2,b",
				},
			},
			supp: map[string]string{"a1": CompanionsResolved, "a2": CompanionsResolved,
				"b": CompanionsResolved, "t": CompanionsNotAModel},
		},
		{
			name:   "model format with no reference reader",
			tree:   map[string]string{"p.ply": "ply\n"},
			assets: []manifestAsset{asset("p", "3d", "ply", "p.ply")},
			want:   want{assets: map[string][]string{"p": {}}, files: map[string]string{"p.ply": "P:p|C:"}},
			supp:   map[string]string{"p": CompanionsUnknownModel},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for p, content := range tc.tree {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(root, p), content)
			}
			if tc.name == "self-contained glb resolves to an empty, real answer" {
				if err := os.MkdirAll(filepath.Join(root, "m"), 0o755); err != nil {
					t.Fatal(err)
				}
				mustWriteGLB(t, filepath.Join(root, "m", "solid.glb"), `{"asset":{"version":"2.0"},"buffers":[{"byteLength":4}]}`)
			}
			for i := range tc.assets {
				st, err := os.Stat(filepath.Join(root, tc.assets[i].FilePath))
				if err != nil {
					t.Fatal(err)
				}
				tc.assets[i].FileSizeBytes = st.Size()
			}
			// Reverse the input order: the closure must not depend on it.
			rev := append([]manifestAsset(nil), tc.assets...)
			for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
				rev[i], rev[j] = rev[j], rev[i]
			}
			var prev []byte
			for _, in := range [][]manifestAsset{tc.assets, rev} {
				cl, err := resolveCIClosure(root, in)
				if err != nil {
					t.Fatal(err)
				}
				gotAssets := map[string][]string{}
				for i, a := range cl.assets {
					if i > 0 && cl.assets[i-1].ID >= a.ID {
						t.Errorf("assets not sorted at %q", a.ID)
					}
					gotAssets[a.ID] = a.Companions
					if s := tc.supp[a.ID]; a.CompanionSupport != s {
						t.Errorf("%s: support %q, want %q", a.ID, a.CompanionSupport, s)
					}
				}
				gotFiles := map[string]string{}
				for p, r := range cl.files {
					gotFiles[p] = "P:" + strings.Join(r.PrimaryFor, ",") + "|C:" + strings.Join(r.CompanionOf, ",")
				}
				if !jsonEqual(gotAssets, tc.want.assets) {
					t.Errorf("assets = %v, want %v", gotAssets, tc.want.assets)
				}
				if !jsonEqual(gotFiles, tc.want.files) {
					t.Errorf("files = %v, want %v", gotFiles, tc.want.files)
				}
				hashed, err := hashCIClosure(root, cl)
				if err != nil {
					t.Fatal(err)
				}
				for p := range cl.files {
					if hashed[p].sha != sha256Hex(t, filepath.Join(root, p)) {
						t.Errorf("%s: digest mismatch", p)
					}
				}
				b, _ := json.Marshal([]any{cl.assets, cl.files})
				if prev != nil && !bytes.Equal(prev, b) {
					t.Errorf("closure depends on input order")
				}
				prev = b
			}
		})
	}
}

func TestExportCISelection_DoesNotChangeSeedSelection(t *testing.T) {
	// Exporting must leave the seeder's selector exactly as it was: the
	// same catalogue, selected before and after an export, agrees.
	f := newExportFixture(t)
	sel := func() string {
		c, rep := f.selector(2)
		var ids []string
		for _, a := range c.Assets {
			ids = append(ids, a.ID)
		}
		return fmt.Sprintf("%v|%d|%d", ids, rep.Bytes, rep.Covered)
	}
	before := sel()
	f.export(2)
	if after := sel(); after != before {
		t.Errorf("selection changed:\n%s\n%s", before, after)
	}
}

func mustRemove(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
}

// chmodRestore sets p's mode and restores a readable one at cleanup, so
// t.TempDir can still remove the tree.
func chmodRestore(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
}

// ciSelectionGoldenSHA256 pins the `aa seed-selection` report for the
// fixed synthetic export fixture at depth 2. The overlap work must not
// change that output; a deliberate report change updates this digest in
// the same change and says why.
const ciSelectionGoldenSHA256 = "460058cb2856baf3964bece528f9a047227bb5f363a170f077e7a0eab172659e"

func TestExportCISelection_GoldenReportUnchanged(t *testing.T) {
	f := newExportFixture(t)
	_, raw := f.export(2)
	if got := sha256Of(raw); got != ciSelectionGoldenSHA256 {
		t.Errorf("seed-selection report digest = %s, want %s", got, ciSelectionGoldenSHA256)
	}
}

func sha256Of(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
