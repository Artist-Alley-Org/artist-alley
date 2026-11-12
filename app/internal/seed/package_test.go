// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the local CI seed-package builder (#1514). Everything is
// synthetic: the site is the export fixture, the report is the real
// exporter's output for it, and the ledger is that report's draft with a
// synthetic structurally complete review on every row.

package seed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

type pkgFixture struct {
	t      *testing.T
	f      *exportFixture
	sel    *CISelection
	ledger string
	outDir string
}

func newPkgFixture(t *testing.T) *pkgFixture {
	t.Helper()
	return pkgFixtureFrom(t, newExportFixture(t))
}

func pkgFixtureFrom(t *testing.T, f *exportFixture) *pkgFixture {
	t.Helper()
	f.export(2)
	sel, sum := loadReport(t, f.out())
	p := &pkgFixture{t: t, f: f, sel: sel, outDir: t.TempDir()}
	p.ledger = filepath.Join(t.TempDir(), "ledger.json")
	p.writeLedger(sel, sum, func(i int, e *RightsLedgerEntry) {
		e.Status, e.Review = RightsReviewedAllow, completeReview()
	})
	return p
}

// writeLedger drafts the ledger for sel and applies edit to every entry.
func (p *pkgFixture) writeLedger(sel *CISelection, sum string, edit func(i int, e *RightsLedgerEntry)) {
	p.t.Helper()
	l, _ := draftLedger(p.t, sel, sum)
	for i := range l.Entries {
		edit(i, &l.Entries[i])
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		p.t.Fatal(err)
	}
	mustWrite(p.t, p.ledger, string(b)+"\n")
}

func (p *pkgFixture) opts() CIPackageOptions {
	return CIPackageOptions{SiteRoot: p.f.site, CatalogueRoot: p.f.catDir, SelectionPath: p.f.out(), LedgerPath: p.ledger}
}

func (p *pkgFixture) out() string { return filepath.Join(p.outDir, "seed.tar.gz") }

func (p *pkgFixture) build() *CIPackageResult {
	p.t.Helper()
	res, err := BuildCIPackage(p.opts(), p.out())
	if err != nil {
		p.t.Fatalf("build: %v", err)
	}
	return res
}

// mustRefuse asserts the build fails with want in the error and leaves no
// archive and no temp file in the output directory.
func (p *pkgFixture) mustRefuse(want string) {
	p.t.Helper()
	_, err := BuildCIPackage(p.opts(), p.out())
	if err == nil {
		p.t.Fatalf("build succeeded, want an error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		p.t.Errorf("error = %v, want it to contain %q", err, want)
	}
	p.assertNoOutput()
}

func (p *pkgFixture) assertNoOutput() {
	p.t.Helper()
	ents, err := os.ReadDir(p.outDir)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, e := range ents {
		p.t.Errorf("output directory holds %q after a refused build", e.Name())
	}
}

type tarMember struct {
	hdr  *tar.Header
	body []byte
}

func readPackage(t *testing.T, p string) []tarMember {
	t.Helper()
	fh, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	gz, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() && gz.ModTime.Unix() != 0 {
		t.Errorf("gzip header carries name %q / mtime %v", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	var out []tarMember
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tarMember{h, b})
	}
	return out
}

// extractPackage writes the archive's regular members under dir, refusing
// anything else, as a consumer would.
func extractPackage(t *testing.T, archive, dir string) {
	t.Helper()
	for _, m := range readPackage(t, archive) {
		if m.hdr.Typeflag != tar.TypeReg || checkRelPath(m.hdr.Name) != nil {
			t.Fatalf("unsafe member %q type %v", m.hdr.Name, m.hdr.Typeflag)
		}
		dst := filepath.Join(dir, filepath.FromSlash(m.hdr.Name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, m.body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// rawRows returns the source rows of a JSON array, keyed by id.
func rawRows(t *testing.T, b []byte) ([]string, map[string]string) {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	m := map[string]string{}
	for _, r := range rows {
		var h struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r, &h); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, h.ID)
		m[h.ID] = string(r)
	}
	return ids, m
}

// --- tests -----------------------------------------------------------------

func TestCIPackage_ExactMembersAndFiltering(t *testing.T) {
	p := newPkgFixture(t)
	res := p.build()
	members := readPackage(t, p.out())

	want := []string{CIPackageManifestName, "MANIFEST.json", "posts.json"}
	for _, f := range p.sel.Files {
		want = append(want, f.Path)
	}
	var got []string
	for _, m := range members {
		got = append(got, m.hdr.Name)
		h := m.hdr
		if h.Typeflag != tar.TypeReg || h.Mode != 0o644 || h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" ||
			h.ModTime.Unix() != 0 {
			t.Errorf("member %q header not fixed: %+v", h.Name, h)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("members:\n%v\nwant:\n%v", got, want)
	}
	if res.Entries != len(want) || res.Files != len(p.sel.Files) || res.Assets != len(p.sel.Assets) || res.Posts != len(p.sel.PostIDs) {
		t.Errorf("result %+v", res)
	}
	byName := map[string][]byte{}
	for _, m := range members {
		byName[m.hdr.Name] = m.body
	}
	for _, f := range p.sel.Files {
		if b := byName[f.Path]; int64(len(b)) != f.Bytes || sha256Of(b) != f.SHA256 {
			t.Errorf("member %q is %d bytes / %s, report %d / %s", f.Path, len(b), sha256Of(b), f.Bytes, f.SHA256)
		}
	}

	// Filtered rows: exactly the selected ids, in source order, each row
	// byte-identical to the source row.
	for _, c := range []struct {
		name string
		ids  []string
	}{{"MANIFEST.json", assetIDsOf(p.sel)}, {"posts.json", p.sel.PostIDs}} {
		src, err := os.ReadFile(filepath.Join(p.f.site, c.name))
		if err != nil {
			t.Fatal(err)
		}
		srcOrder, srcRows := rawRows(t, src)
		gotOrder, gotRows := rawRows(t, byName[c.name])
		keep := map[string]bool{}
		for _, id := range c.ids {
			keep[id] = true
		}
		var wantOrder []string
		for _, id := range srcOrder {
			if keep[id] {
				wantOrder = append(wantOrder, id)
			}
		}
		if strings.Join(gotOrder, ",") != strings.Join(wantOrder, ",") {
			t.Errorf("%s ids %v, want %v", c.name, gotOrder, wantOrder)
		}
		if len(wantOrder) == 0 || len(wantOrder) == len(srcOrder) {
			t.Errorf("%s: fixture does not exercise filtering (%d of %d kept)", c.name, len(wantOrder), len(srcOrder))
		}
		for _, id := range gotOrder {
			if gotRows[id] != srcRows[id] {
				t.Errorf("%s row %q rewritten:\n%s\nsource:\n%s", c.name, id, gotRows[id], srcRows[id])
			}
		}
	}

	// Package manifest: strict shape, report and ledger digests, no
	// absolute paths, no review evidence.
	var pm CIPackageManifest
	dec := json.NewDecoder(bytes.NewReader(byName[CIPackageManifestName]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pm); err != nil {
		t.Fatal(err)
	}
	if pm.Schema != CIPackageSchema || pm.Selection.SHA256 != sha256Hex(t, p.f.out()) ||
		pm.Ledger.SHA256 != sha256Hex(t, p.ledger) || pm.Ledger.ReviewedAllow != len(p.sel.Files) ||
		len(pm.Files) != len(p.sel.Files) || !strings.Contains(pm.Notice, "STRUCTURAL ONLY") ||
		!strings.Contains(pm.Notice, "does not authorize publication") {
		t.Errorf("package manifest %+v", pm)
	}
	text := string(byName[CIPackageManifestName])
	rv := completeReview()
	for _, leak := range append([]string{p.f.site, p.f.catDir, p.outDir, filepath.Dir(p.ledger), rv.Reviewer, rv.License},
		rv.Sources...) {
		if strings.Contains(text, leak) {
			t.Errorf("package manifest leaks %q", leak)
		}
	}

	// The archive on disk: mode 0600, digest and size as reported, no temp.
	st, err := os.Stat(p.out())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || st.Size() != res.Bytes || sha256Hex(t, p.out()) != res.SHA256 {
		t.Errorf("archive mode %v size %d sha %s, result %+v", st.Mode().Perm(), st.Size(), sha256Hex(t, p.out()), res)
	}
	if ents, _ := os.ReadDir(p.outDir); len(ents) != 1 {
		t.Errorf("output directory holds %d entries", len(ents))
	}
}

func TestCIPackage_TwoRunsByteIdentical(t *testing.T) {
	p := newPkgFixture(t)
	first := p.build()
	again := filepath.Join(t.TempDir(), "again.tar.gz")
	second, err := BuildCIPackage(p.opts(), again)
	if err != nil {
		t.Fatal(err)
	}
	third := p.build() // replacing the existing regular output
	if first.SHA256 != second.SHA256 || first.SHA256 != third.SHA256 || first.Bytes != second.Bytes {
		t.Fatalf("runs differ: %+v / %+v / %+v", first, second, third)
	}
	a, _ := os.ReadFile(p.out())
	b, _ := os.ReadFile(again)
	if !bytes.Equal(a, b) {
		t.Fatal("archive bytes differ between runs")
	}
}

func TestCIPackage_ExtractsToALoadableSite(t *testing.T) {
	p := newPkgFixture(t)
	p.build()
	ext := t.TempDir()
	extractPackage(t, p.out(), ext)

	c, err := loadCatalogues(p.f.catDir, ext, nil)
	if err != nil {
		t.Fatalf("extracted site does not load: %v", err)
	}
	if err := validateCISelectionCatalogue(c, ext); err != nil {
		t.Fatalf("extracted catalogue invalid: %v", err)
	}
	var aids, pids []string
	for _, a := range c.Assets {
		aids = append(aids, a.ID)
	}
	for _, q := range c.Posts {
		pids = append(pids, q.ID)
	}
	sort.Strings(aids)
	sort.Strings(pids)
	if strings.Join(aids, ",") != strings.Join(assetIDsOf(p.sel), ",") || strings.Join(pids, ",") != strings.Join(p.sel.PostIDs, ",") {
		t.Fatalf("extracted ids %v / %v, report %v / %v", aids, pids, assetIDsOf(p.sel), p.sel.PostIDs)
	}
	// The seeder's own closure walk over the extracted tree finds exactly
	// the report's files with the report's digests.
	cl, err := resolveCIClosure(ext, c.Assets)
	if err != nil {
		t.Fatalf("closure over the extracted site: %v", err)
	}
	hashed, err := hashCIClosure(ext, cl)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashed) != len(p.sel.Files) {
		t.Fatalf("extracted closure has %d files, report %d", len(hashed), len(p.sel.Files))
	}
	for _, f := range p.sel.Files {
		if h, ok := hashed[f.Path]; !ok || h.sha != f.SHA256 {
			t.Errorf("extracted closure file %q missing or differs", f.Path)
		}
	}
}

func TestCIPackage_RefusesIneligibleLedgers(t *testing.T) {
	t.Run("all pending draft", func(t *testing.T) {
		p := newPkgFixture(t)
		sel, sum := loadReport(t, p.f.out())
		p.writeLedger(sel, sum, func(int, *RightsLedgerEntry) {})
		p.mustRefuse("not structurally eligible")
	})
	t.Run("one hold", func(t *testing.T) {
		p := newPkgFixture(t)
		sel, sum := loadReport(t, p.f.out())
		p.writeLedger(sel, sum, func(i int, e *RightsLedgerEntry) {
			e.Status, e.Review = RightsReviewedAllow, completeReview()
			if i == 0 {
				e.Status, e.Review = RightsHold, nil
			}
		})
		p.mustRefuse("not structurally eligible")
	})
	t.Run("reviewed_allow without evidence", func(t *testing.T) {
		p := newPkgFixture(t)
		sel, sum := loadReport(t, p.f.out())
		p.writeLedger(sel, sum, func(i int, e *RightsLedgerEntry) {
			e.Status, e.Review = RightsReviewedAllow, completeReview()
			if i == 0 {
				e.Review.License = ""
			}
		})
		p.mustRefuse("is reviewed_allow but")
	})
	t.Run("unknown-model closure", func(t *testing.T) {
		p := newPkgFixture(t)
		report := synthReport(t, []synthAsset{
			{id: "m", path: "m.fbx", support: CompanionsUnknownModel},
			{id: "a", path: "a.png", support: CompanionsNotAModel},
		}, map[string]string{"m.fbx": "fbx", "a.png": "png"})
		sel, sum := loadReport(t, report)
		p.writeLedger(sel, sum, func(_ int, e *RightsLedgerEntry) {
			e.Status, e.Review = RightsReviewedAllow, completeReview()
		})
		opts := p.opts()
		opts.SelectionPath = report
		_, err := BuildCIPackage(opts, p.out())
		if err == nil || !strings.Contains(err.Error(), "not structurally eligible") ||
			!strings.Contains(err.Error(), RightsReasonIncompleteClose) {
			t.Fatalf("err = %v", err)
		}
		p.assertNoOutput()
	})
}

func TestCIPackage_RefusesStaleBindings(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *pkgFixture)
		want   string
	}{
		{"ledger bound to another report", func(p *pkgFixture) {
			b, _ := os.ReadFile(p.f.out())
			mustWrite(p.t, p.f.out(), string(b)+" ")
		}, "stale ledger"},
		{"catalogue input edited", func(p *pkgFixture) {
			mustWrite(p.t, filepath.Join(p.f.catDir, "dataset.users.json"), "[ ]")
		}, `input "catalogue/dataset.users.json" is 3 bytes`},
		{"optional input appears", func(p *pkgFixture) {
			mustWrite(p.t, filepath.Join(p.f.catDir, "dataset.fixtures.json"), `{"principals":[]}`)
		}, "was pinned absent and now exists"},
		{"manifest edited", func(p *pkgFixture) {
			b, _ := os.ReadFile(filepath.Join(p.f.site, "MANIFEST.json"))
			mustWrite(p.t, filepath.Join(p.f.site, "MANIFEST.json"), string(b)+"\n")
		}, `input "site/MANIFEST.json"`},
		{"posts removed", func(p *pkgFixture) {
			mustRemove(p.t, filepath.Join(p.f.site, "posts.json"))
		}, `pinned input "site/posts.json" is missing`},
		{"closure file rewritten at the same length", func(p *pkgFixture) {
			path := filepath.Join(p.f.site, p.sel.Files[0].Path)
			mustWrite(p.t, path, strings.Repeat("z", int(p.sel.Files[0].Bytes)))
		}, "(changed or stale)"},
		{"closure file resized", func(p *pkgFixture) {
			mustWrite(p.t, filepath.Join(p.f.site, p.sel.Files[0].Path), "short")
		}, "the report has"},
		{"closure file removed", func(p *pkgFixture) {
			mustRemove(p.t, filepath.Join(p.f.site, p.sel.Files[len(p.sel.Files)-1].Path))
		}, "closure file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPkgFixture(t)
			tc.change(p)
			p.mustRefuse(tc.want)
		})
	}
}

func TestCIPackage_RefusesUnsafeSources(t *testing.T) {
	t.Run("closure file is an in-root symlink", func(t *testing.T) {
		p := newPkgFixture(t)
		f := p.sel.Files[0]
		src := filepath.Join(p.f.site, filepath.FromSlash(f.Path))
		b, _ := os.ReadFile(src)
		copyPath := filepath.Join(p.f.site, "copy-of-target")
		mustWrite(t, copyPath, string(b))
		mustRemove(t, src)
		if err := os.Symlink(copyPath, src); err != nil {
			t.Fatal(err)
		}
		p.mustRefuse("is a symlink")
	})
	t.Run("closure file links outside the root", func(t *testing.T) {
		p := newPkgFixture(t)
		f := p.sel.Files[0]
		src := filepath.Join(p.f.site, filepath.FromSlash(f.Path))
		b, _ := os.ReadFile(src)
		outside := filepath.Join(t.TempDir(), "outside")
		mustWrite(t, outside, string(b))
		mustRemove(t, src)
		if err := os.Symlink(outside, src); err != nil {
			t.Fatal(err)
		}
		p.mustRefuse("is a symlink")
	})
	t.Run("closure directory is a symlink", func(t *testing.T) {
		// The default fixture's closure is flat, so add a selected asset in
		// a subdirectory (a new extension, so coverage must select it).
		f := newExportFixture(t)
		if err := os.Mkdir(filepath.Join(f.site, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.addSolo("zz-nested", "image", "webp", "sub/nested.webp", "nested-bytes")
		f.write()
		p := pkgFixtureFrom(t, f)
		var nested *CISelectionFile
		for i := range p.sel.Files {
			if p.sel.Files[i].Path == "sub/nested.webp" {
				nested = &p.sel.Files[i]
			}
		}
		if nested == nil {
			t.Fatal("the nested file is not in the selection; the case would test nothing")
		}
		p.build() // control: the real directory packages
		mustRemove(t, p.out())
		dir := filepath.Join(p.f.site, "sub")
		moved := dir + ".real"
		if err := os.Rename(dir, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, dir); err != nil {
			t.Fatal(err)
		}
		p.mustRefuse("is a symlink")
	})
	t.Run("report with a traversal path", func(t *testing.T) {
		p := newPkgFixture(t)
		b, _ := os.ReadFile(p.f.out())
		f := p.sel.Files[0].Path
		bad := strings.ReplaceAll(string(b), `"`+f+`"`, `"../`+f+`"`)
		if bad == string(b) {
			t.Fatal("could not edit the report")
		}
		mustWrite(t, p.f.out(), bad)
		p.mustRefuse("unsafe file_path")
	})
}

func TestCIPackage_RefusesBadOutputs(t *testing.T) {
	cases := []struct {
		name  string
		setup func(p *pkgFixture) string
		want  string
	}{
		{"inside the site root", func(p *pkgFixture) string { return filepath.Join(p.f.site, "seed.tar.gz") },
			"inside the site root"},
		{"inside the catalogue root", func(p *pkgFixture) string { return filepath.Join(p.f.catDir, "x", "..", "seed.tar.gz") },
			"inside the catalogue root"},
		{"through a symlinked directory into the site", func(p *pkgFixture) string {
			link := filepath.Join(p.outDir, "alias")
			if err := os.Symlink(p.f.site, link); err != nil {
				p.t.Fatal(err)
			}
			return filepath.Join(link, "seed.tar.gz")
		}, "inside the site root"},
		{"output is a symlink", func(p *pkgFixture) string {
			target := filepath.Join(p.t.TempDir(), "victim")
			mustWrite(p.t, target, "keep")
			link := filepath.Join(p.outDir, "seed.tar.gz")
			if err := os.Symlink(target, link); err != nil {
				p.t.Fatal(err)
			}
			p.t.Cleanup(func() {
				if b, _ := os.ReadFile(target); string(b) != "keep" {
					p.t.Errorf("symlink target was overwritten")
				}
			})
			return link
		}, "not a regular file"},
		{"output is a directory", func(p *pkgFixture) string {
			d := filepath.Join(p.outDir, "seed.tar.gz")
			if err := os.Mkdir(d, 0o700); err != nil {
				p.t.Fatal(err)
			}
			return d
		}, "not a regular file"},
		{"output is the ledger", func(p *pkgFixture) string { return p.ledger }, "selection report or the rights ledger"},
		{"output directory missing", func(p *pkgFixture) string { return filepath.Join(p.outDir, "nope", "seed.tar.gz") },
			"output directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPkgFixture(t)
			out := tc.setup(p)
			before := treeSnapshot(t, p.f.site)
			_, err := BuildCIPackage(p.opts(), out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if after := treeSnapshot(t, p.f.site); strings.Join(before, "\n") != strings.Join(after, "\n") {
				t.Errorf("site root changed")
			}
			ents, _ := os.ReadDir(p.outDir)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".ci-package-") {
					t.Errorf("temp file %q left behind", e.Name())
				}
			}
		})
	}
}

func TestCIPackage_DetectsConcurrentChange(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *pkgFixture, rel string)
		want   string
	}{
		{"rewritten in place at the same length", func(p *pkgFixture, rel string) {
			full := filepath.Join(p.f.site, filepath.FromSlash(rel))
			b, _ := os.ReadFile(full)
			mustWrite(p.t, full, strings.Repeat("q", len(b)))
		}, "ci package: closure file"},
		{"replaced by an identical copy", func(p *pkgFixture, rel string) {
			full := filepath.Join(p.f.site, filepath.FromSlash(rel))
			b, _ := os.ReadFile(full)
			mustWrite(p.t, full+".new", string(b))
			if err := os.Rename(full+".new", full); err != nil {
				p.t.Fatal(err)
			}
		}, "changed while it was copied"},
		{"grows while being copied", func(p *pkgFixture, rel string) {
			fh, err := os.OpenFile(filepath.Join(p.f.site, filepath.FromSlash(rel)), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				p.t.Fatal(err)
			}
			defer fh.Close()
			if _, err := fh.WriteString("more"); err != nil {
				p.t.Fatal(err)
			}
		}, "grew while it was copied"},
		{"truncated", func(p *pkgFixture, rel string) {
			if err := os.Truncate(filepath.Join(p.f.site, filepath.FromSlash(rel)), 1); err != nil {
				p.t.Fatal(err)
			}
		}, "changed while it was copied"},
		{"catalogue input edited while writing", func(p *pkgFixture, _ string) {
			mustWrite(p.t, filepath.Join(p.f.catDir, "dataset.teams.json"), "[ ]")
		}, "an input changed while the package was written"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPkgFixture(t)
			target := p.sel.Files[len(p.sel.Files)/2].Path
			ciPackageBeforeCopy = func(rel string) {
				if rel == target {
					tc.change(p, rel)
				}
			}
			t.Cleanup(func() { ciPackageBeforeCopy = nil })
			p.mustRefuse(tc.want)
		})
	}
}

// TestCIPackage_RechecksControlFilesBeforeInstall changes the report or
// the ledger while the archive is being written (after eligibility was
// decided). The install must be refused, an archive already at the output
// path must keep its bytes, and no temp file may remain.
func TestCIPackage_RechecksControlFilesBeforeInstall(t *testing.T) {
	cases := []struct {
		name   string
		change func(p *pkgFixture)
		want   string // "" = the install must succeed
	}{
		{"ledger revoked to hold", func(p *pkgFixture) {
			sel, sum := loadReport(p.t, p.f.out())
			p.writeLedger(sel, sum, func(i int, e *RightsLedgerEntry) {
				e.Status, e.Review = RightsReviewedAllow, completeReview()
				if i == 0 {
					e.Status, e.Review = RightsHold, nil
				}
			})
		}, "the rights ledger changed while the package was written: sha256"},
		{"ledger bytes edited", func(p *pkgFixture) {
			b, _ := os.ReadFile(p.ledger)
			mustWrite(p.t, p.ledger, string(b)+"\n")
		}, "the rights ledger changed while the package was written: sha256"},
		{"ledger removed", func(p *pkgFixture) {
			mustRemove(p.t, p.ledger)
		}, "the rights ledger changed while the package was written"},
		{"ledger replaced by a directory", func(p *pkgFixture) {
			mustRemove(p.t, p.ledger)
			if err := os.Mkdir(p.ledger, 0o700); err != nil {
				p.t.Fatal(err)
			}
		}, "the rights ledger changed while the package was written: not a regular file"},
		{"ledger unreadable", func(p *pkgFixture) {
			skipIfRoot(p.t)
			chmodRestore(p.t, p.ledger, 0o000)
		}, "the rights ledger changed while the package was written"},
		{"selection report changed", func(p *pkgFixture) {
			b, _ := os.ReadFile(p.f.out())
			mustWrite(p.t, p.f.out(), string(b)+" ")
		}, "the selection report changed while the package was written: sha256"},
		{"selection report removed", func(p *pkgFixture) {
			mustRemove(p.t, p.f.out())
		}, "the selection report changed while the package was written"},
		{"control: ledger rewritten with identical bytes", func(p *pkgFixture) {
			b, _ := os.ReadFile(p.ledger)
			mustWrite(p.t, p.ledger+".new", string(b))
			if err := os.Rename(p.ledger+".new", p.ledger); err != nil {
				p.t.Fatal(err)
			}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPkgFixture(t)
			first := p.build() // a previous archive at the output path
			prev, err := os.ReadFile(p.out())
			if err != nil {
				t.Fatal(err)
			}
			fired := false
			ciPackageBeforeCopy = func(string) {
				if !fired {
					fired = true
					tc.change(p)
				}
			}
			t.Cleanup(func() { ciPackageBeforeCopy = nil })
			res, err := BuildCIPackage(p.opts(), p.out())
			if !fired {
				t.Fatal("the hook never ran; the case tested nothing")
			}
			if tc.want == "" {
				if err != nil || res.SHA256 != first.SHA256 {
					t.Fatalf("control build: err = %v, res = %+v", err, res)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			now, err := os.ReadFile(p.out())
			if err != nil || !bytes.Equal(now, prev) {
				t.Errorf("the previous archive was replaced or removed (err %v)", err)
			}
			ents, _ := os.ReadDir(p.outDir)
			if len(ents) != 1 || ents[0].Name() != filepath.Base(p.out()) {
				var names []string
				for _, e := range ents {
					names = append(names, e.Name())
				}
				t.Errorf("output directory holds %v, want only the previous archive", names)
			}
		})
	}
}

func TestCIPackage_StreamsLargeFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 64 MiB synthetic file")
	}
	f := newExportFixture(t)
	const big = 64 << 20
	f.addSolo("zz-big", "image", "tif", "big.tif", strings.Repeat("B", big))
	f.write()
	p := pkgFixtureFrom(t, f)
	found := false
	for _, fl := range p.sel.Files {
		found = found || fl.Path == "big.tif"
	}
	if !found {
		t.Fatal("selection does not include the large file")
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p.build()
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > big/2 {
		t.Errorf("building allocated %d bytes for a %d-byte file; the copy is not streamed", alloc, big)
	}
}

func TestFilterCIRows(t *testing.T) {
	src := []byte(`[ {"id":"a", "file_path":"a.png",  "z":1},
  {"id":"b","file_path":"b.png"},
  {"id": "p", "asset_ids": ["a"], "odd":  [1, 2]} ]`)
	got, err := filterCIRows("t", src, map[string]string{"a": "a.png", "p": ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n" + `{"id":"a", "file_path":"a.png",  "z":1}` + ",\n" + `{"id": "p", "asset_ids": ["a"], "odd":  [1, 2]}` + "\n]\n"
	if string(got) != want {
		t.Errorf("filtered:\n%s\nwant:\n%s", got, want)
	}
	for _, c := range []struct {
		name    string
		src     string
		keep    map[string]string
		members map[string]bool
		want    string
	}{
		{"duplicate id", `[{"id":"a"},{"id":"a"}]`, map[string]string{"a": ""}, nil, "twice"},
		{"missing selected id", `[{"id":"a"}]`, map[string]string{"b": ""}, nil, "is not in"},
		{"file_path disagrees", `[{"id":"a","file_path":"x"}]`, map[string]string{"a": "a.png"}, nil, "file_path disagrees"},
		{"post references unselected asset", `[{"id":"p","asset_ids":["a","b"]}]`, map[string]string{"p": ""},
			map[string]bool{"a": true}, "not selected"},
		{"row without id", `[{"x":1}]`, map[string]string{}, nil, "has no id"},
		{"not an array", `{"id":"a"}`, map[string]string{}, nil, "parse"},
	} {
		if _, err := filterCIRows("t", []byte(c.src), c.keep, c.members); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}
