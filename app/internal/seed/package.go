// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package seed

// The local CI seed-package builder (#1514).
//
// BuildCIPackage turns a strict selection report, its rights ledger and
// the site root they describe into one deterministic site-root tar.gz:
// the selected MANIFEST.json rows, the selected posts.json rows, exactly
// the report's closure files, and a small package manifest. It installs
// no new archive unless every binding holds:
//
//   - the report loads strictly and the ledger binds to it;
//   - CheckRightsLedger is structurally valid AND Eligible (every row
//     reviewed_allow, no unknown-model closure); pending, hold and
//     unknown-model reports fail closed;
//   - every pinned site/ and catalogue/ input is present or absent as
//     pinned, with the pinned size and SHA-256;
//   - every selected asset and post id exists exactly once in the pinned
//     MANIFEST.json / posts.json, and every selected post's members are
//     selected assets;
//   - every closure file is a regular file under the site root, reached
//     through no symlink, with the reported size and SHA-256 at the
//     moment its bytes are copied into the archive;
//   - immediately before the archive is installed, the selection report
//     and the ledger at their paths are still regular files with exactly
//     the SHA-256 of the bytes the eligibility decision was made from.
//
// On refusal or any other error no NEW archive is installed: the temp
// file is removed and an archive already at the output path is left as
// it was (the writer replaces it only by an atomic rename on success).
//
// ⚠️ Eligible is STRUCTURAL ONLY (RightsStructuralOnly). A package this
// builds is a LOCAL CANDIDATE ARTIFACT: it does not establish legal
// rights, is not release approval, and does not authorize publication.
// Rights are never inferred from manifest labels; the packager does not
// read them.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// CIPackageSchema versions the package manifest inside the archive.
const CIPackageSchema = "artist-alley.ci-package/v1"

// CIPackageManifestName is the package manifest's name at the archive
// root. The seeder's catalogue loader does not read it.
const CIPackageManifestName = "ci-package.json"

// CIPackageNotice is written into every package manifest.
const CIPackageNotice = "LOCAL CANDIDATE ARTIFACT. Built only because the rights ledger is structurally " +
	"eligible. " + RightsStructuralOnly + " This package is not release approval and does not authorize publication."

// ciPackageEpoch is the fixed timestamp of every archive entry.
var ciPackageEpoch = time.Unix(0, 0).UTC()

// ciPackageBeforeCopy, when set, runs for each closure file after it is
// opened and checked and before its bytes are copied. Tests use it to
// change the file at the one point a concurrent writer could hurt.
var ciPackageBeforeCopy func(rel string)

// CIPackageOptions names the inputs. All four are required; nothing is
// defaulted or discovered.
type CIPackageOptions struct {
	SiteRoot      string
	CatalogueRoot string
	SelectionPath string
	LedgerPath    string
}

// CIPackageResult describes a package that was written.
type CIPackageResult struct {
	SHA256      string
	Bytes       int64
	Entries     int
	Files       int
	Assets      int
	Posts       int
	ContentSize int64
}

// CIPackageManifest is the package's own record. It carries digests and
// counts only: no absolute paths and no review evidence.
type CIPackageManifest struct {
	Schema    string                `json:"schema"`
	Notice    string                `json:"notice"`
	Selection CIPackageSelectionPin `json:"selection"`
	Ledger    CIPackageLedgerPin    `json:"ledger"`
	// SiteInputs are the report's site/ pins the filtered rows were cut
	// from: the ORIGINAL MANIFEST.json and posts.json digests.
	SiteInputs []CISelectionInput `json:"site_inputs"`
	// Filtered describes the two filtered catalogue files in the archive.
	Filtered []CIPackageEntry `json:"filtered"`
	// Files is the closure, exactly the report's, in path order.
	Files []CIPackageEntry `json:"files"`
}

// CIPackageSelectionPin binds the package to its selection report.
type CIPackageSelectionPin struct {
	SHA256        string `json:"sha256"`
	Schema        string `json:"schema"`
	Profile       string `json:"profile"`
	CoverageDepth int    `json:"coverage_depth"`
	Assets        int    `json:"assets"`
	Posts         int    `json:"posts"`
}

// CIPackageLedgerPin binds the package to the ledger that made it
// eligible. Only counts are recorded, never the review evidence.
type CIPackageLedgerPin struct {
	SHA256        string `json:"sha256"`
	Schema        string `json:"schema"`
	Entries       int    `json:"entries"`
	ReviewedAllow int    `json:"reviewed_allow"`
	Eligibility   string `json:"eligibility"`
}

// CIPackageEntry is one archive member with its length and digest.
type CIPackageEntry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Rows   *int   `json:"rows,omitempty"`
}

// BuildCIPackage checks every binding, then streams the archive to a
// mode-0600 temporary file next to outPath and installs it atomically.
// outPath must lie outside both roots (through any symlink alias), and an
// existing outPath must be a regular file; a symlink there is refused,
// never followed. On any error no new archive is installed and a
// pre-existing archive at outPath is left untouched.
func BuildCIPackage(opts CIPackageOptions, outPath string) (*CIPackageResult, error) {
	for _, req := range []struct{ name, v string }{
		{"site root", opts.SiteRoot}, {"catalogue root", opts.CatalogueRoot},
		{"selection report", opts.SelectionPath}, {"rights ledger", opts.LedgerPath}, {"output path", outPath},
	} {
		if req.v == "" {
			return nil, fmt.Errorf("ci package: %s is required", req.name)
		}
	}
	protected := []protectedRoot{
		{label: "site root", path: opts.SiteRoot},
		{label: "catalogue root", path: opts.CatalogueRoot},
	}
	out, err := openOutputDir(protected, outPath)
	if err != nil {
		return nil, fmt.Errorf("ci package: %w", err)
	}
	defer out.Close()
	if err := refuseOutputAlias(outPath, opts.SelectionPath, opts.LedgerPath); err != nil {
		return nil, err
	}

	// 1. Report, ledger, structural eligibility. Fail closed before any
	// site byte is read.
	sel, selSHA, err := LoadCISelectionReport(opts.SelectionPath)
	if err != nil {
		return nil, fmt.Errorf("ci package: %w", err)
	}
	ledgerRaw, err := os.ReadFile(opts.LedgerPath)
	if err != nil {
		return nil, fmt.Errorf("ci package: read ledger: %w", err)
	}
	ledger, err := parseRightsLedger(ledgerRaw)
	if err != nil {
		return nil, fmt.Errorf("ci package: %w", err)
	}
	check, err := CheckRightsLedger(sel, selSHA, ledger)
	if err != nil {
		return nil, fmt.Errorf("ci package: %w", err)
	}
	if !check.Eligible {
		return nil, fmt.Errorf("ci package: refused: the rights ledger is not structurally eligible: %s",
			strings.Join(check.Ineligible, "; "))
	}

	// 2. Inputs, ids, closure files.
	site, err := os.OpenRoot(opts.SiteRoot)
	if err != nil {
		return nil, fmt.Errorf("ci package: site root: %w", err)
	}
	defer site.Close()
	cat, err := os.OpenRoot(opts.CatalogueRoot)
	if err != nil {
		return nil, fmt.Errorf("ci package: catalogue root: %w", err)
	}
	defer cat.Close()
	pinned, err := verifyCIPackageInputs(sel.Inputs, site, cat)
	if err != nil {
		return nil, err
	}
	manifestRows, err := filterCIRows("MANIFEST.json", pinned["site/MANIFEST.json"], sel.selectedAssetPaths(), nil)
	if err != nil {
		return nil, err
	}
	assetSet := map[string]bool{}
	for _, a := range sel.Assets {
		assetSet[a.ID] = true
	}
	postSet := map[string]string{}
	for _, id := range sel.PostIDs {
		postSet[id] = ""
	}
	postRows, err := filterCIRows("posts.json", pinned["site/posts.json"], postSet, assetSet)
	if err != nil {
		return nil, err
	}
	reserved := map[string]bool{CIPackageManifestName: true, "MANIFEST.json": true, "posts.json": true}
	for _, f := range sel.Files {
		if reserved[f.Path] {
			return nil, fmt.Errorf("ci package: closure file %q collides with a package member name", f.Path)
		}
		if err := checkCIPackageFile(site, f); err != nil {
			return nil, err
		}
	}

	// 3. Package manifest, from the report's digests: every closure file
	// is re-verified against them while it is copied.
	nAssets, nPosts := len(sel.Assets), len(sel.PostIDs)
	pm := CIPackageManifest{
		Schema: CIPackageSchema, Notice: CIPackageNotice,
		Selection: CIPackageSelectionPin{SHA256: selSHA, Schema: sel.Schema, Profile: sel.Profile,
			CoverageDepth: sel.CoverageDepth, Assets: nAssets, Posts: nPosts},
		Ledger: CIPackageLedgerPin{SHA256: hexSHA256(ledgerRaw), Schema: ledger.Schema, Entries: len(ledger.Entries),
			ReviewedAllow: check.Counts[RightsReviewedAllow], Eligibility: "structural only"},
		SiteInputs: []CISelectionInput{},
		Filtered: []CIPackageEntry{
			{Path: "MANIFEST.json", Bytes: int64(len(manifestRows)), SHA256: hexSHA256(manifestRows), Rows: &nAssets},
			{Path: "posts.json", Bytes: int64(len(postRows)), SHA256: hexSHA256(postRows), Rows: &nPosts},
		},
		Files: make([]CIPackageEntry, 0, len(sel.Files)),
	}
	for _, in := range sel.Inputs {
		if strings.HasPrefix(in.Name, "site/") {
			pm.SiteInputs = append(pm.SiteInputs, in)
		}
	}
	var content int64
	for _, f := range sel.Files {
		pm.Files = append(pm.Files, CIPackageEntry{Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256})
		content += f.Bytes
	}
	pmRaw, err := json.MarshalIndent(pm, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ci package: encode package manifest: %w", err)
	}
	pmRaw = append(pmRaw, '\n')

	// 4. Stream. Members: package manifest, the two filtered files, then
	// the closure in the report's (sorted) path order.
	if err := out.verify(protected, outPath); err != nil {
		return nil, fmt.Errorf("ci package: %w", err)
	}
	var sum hash.Hash
	var size int64
	fill := func(w io.Writer) error {
		sum = sha256.New()
		cw := &countingWriter{w: io.MultiWriter(w, sum)}
		if err := writeCIPackageArchive(cw, site, pmRaw, manifestRows, postRows, sel.Files); err != nil {
			return err
		}
		size = cw.n
		return nil
	}
	// Last-moment re-check, after the archive is complete and synced and
	// right before the rename: the pinned inputs must still be what the
	// rows were cut from, the destination must still be the held
	// directory, and the report and ledger the eligibility decision was
	// made from must still be exactly those bytes. A revoked or edited
	// ledger, or a changed report, refuses the install.
	ledgerSHA := hexSHA256(ledgerRaw)
	ready := func() error {
		if _, err := verifyCIPackageInputs(sel.Inputs, site, cat); err != nil {
			return fmt.Errorf("ci package: an input changed while the package was written: %w", err)
		}
		if err := out.verify(protected, outPath); err != nil {
			return fmt.Errorf("ci package: %w", err)
		}
		if err := verifyCIPackageControlFile("selection report", opts.SelectionPath, selSHA); err != nil {
			return err
		}
		return verifyCIPackageControlFile("rights ledger", opts.LedgerPath, ledgerSHA)
	}
	if err := out.writeAtomic0600(".ci-package-", fill, ready); err != nil {
		return nil, err
	}
	return &CIPackageResult{SHA256: hex.EncodeToString(sum.Sum(nil)), Bytes: size, Entries: 3 + len(sel.Files),
		Files: len(sel.Files), Assets: nAssets, Posts: nPosts, ContentSize: content}, nil
}

// verifyCIPackageControlFile re-reads a control file (the selection report
// or the rights ledger) at its path and requires a regular file whose
// bytes have exactly the SHA-256 the build used. Missing, unreadable,
// non-regular or changed all refuse. The path is followed as the first
// read followed it.
func verifyCIPackageControlFile(label, p, wantSHA string) error {
	fh, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("ci package: the %s changed while the package was written: %w", label, err)
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return fmt.Errorf("ci package: the %s changed while the package was written: %w", label, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("ci package: the %s changed while the package was written: not a regular file", label)
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return fmt.Errorf("ci package: the %s changed while the package was written: %w", label, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("ci package: the %s changed while the package was written: sha256 %s, the build used %s",
			label, got, wantSHA)
	}
	return nil
}

// refuseOutputAlias refuses an output that is the report or the ledger
// file itself (replacing an input right after reading it).
func refuseOutputAlias(outPath string, inputs ...string) error {
	ost, err := os.Stat(outPath)
	if err != nil {
		return nil // absent, or refused later as non-regular
	}
	for _, in := range inputs {
		if ist, err := os.Stat(in); err == nil && os.SameFile(ost, ist) {
			return errors.New("ci package: output path is the selection report or the rights ledger")
		}
	}
	return nil
}

// selectedAssetPaths maps each selected asset id to its report file_path.
func (s *CISelection) selectedAssetPaths() map[string]string {
	m := make(map[string]string, len(s.Assets))
	for _, a := range s.Assets {
		m[a.ID] = a.FilePath
	}
	return m
}

// verifyCIPackageInputs checks every site/ and catalogue/ pin against the
// file under its root (regular, not a symlink, pinned size and digest, or
// absent when pinned absent) and returns the verified bytes of present
// site/ inputs. Pins for other inputs (the v2 overlap contract) live
// outside both roots; the report loader already ties them to the report.
func verifyCIPackageInputs(pins []CISelectionInput, site, cat *os.Root) (map[string][]byte, error) {
	got := map[string][]byte{}
	seen := map[string]bool{}
	for _, in := range pins {
		if seen[in.Name] {
			return nil, fmt.Errorf("ci package: input %q is pinned twice", in.Name)
		}
		seen[in.Name] = true
		var r *os.Root
		var base string
		switch {
		case strings.HasPrefix(in.Name, "site/"):
			r, base = site, strings.TrimPrefix(in.Name, "site/")
		case strings.HasPrefix(in.Name, "catalogue/"):
			r, base = cat, strings.TrimPrefix(in.Name, "catalogue/")
		case strings.HasPrefix(in.Name, "overlap_contract/"):
			continue
		default:
			return nil, fmt.Errorf("ci package: input %q has an unknown root", in.Name)
		}
		if base == "" || strings.ContainsAny(base, "/\\\x00") || base == "." || base == ".." {
			return nil, fmt.Errorf("ci package: input %q is not a file directly under its root", in.Name)
		}
		st, err := r.Lstat(base)
		if errors.Is(err, fs.ErrNotExist) {
			if in.Present {
				return nil, fmt.Errorf("ci package: pinned input %q is missing (stale report)", in.Name)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("ci package: input %q: %w", in.Name, err)
		}
		if !in.Present {
			return nil, fmt.Errorf("ci package: input %q was pinned absent and now exists (stale report)", in.Name)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("ci package: input %q is not a regular file", in.Name)
		}
		b, err := readRootFile(r, base, in.Bytes)
		if err != nil {
			return nil, fmt.Errorf("ci package: input %q: %w", in.Name, err)
		}
		if int64(len(b)) != in.Bytes || hexSHA256(b) != in.SHA256 {
			return nil, fmt.Errorf("ci package: input %q is %d bytes / %s, pinned %d / %s (stale report)",
				in.Name, len(b), hexSHA256(b), in.Bytes, in.SHA256)
		}
		if r == site {
			got[in.Name] = b
		}
	}
	for _, need := range []string{"site/MANIFEST.json", "site/posts.json"} {
		if got[need] == nil {
			return nil, fmt.Errorf("ci package: the report does not pin %q as present; the package cannot be cut from it", need)
		}
	}
	return got, nil
}

// readRootFile reads one small input, refusing more than want+1 bytes so a
// file that grew is caught without reading it all.
func readRootFile(r *os.Root, name string, want int64) ([]byte, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, want+1))
}

// filterCIRows keeps, in source order and byte-for-byte, the rows of a
// pinned JSON array whose id is selected. keep maps id -> required
// file_path ("" = not checked). When members is non-nil, every kept row's
// asset_ids must be members. Every selected id must appear exactly once,
// and no id may repeat anywhere in the file.
func filterCIRows(name string, raw []byte, keep map[string]string, members map[string]bool) ([]byte, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("ci package: parse %s: %w", name, err)
	}
	seen := make(map[string]bool, len(rows))
	var buf bytes.Buffer
	buf.WriteString("[")
	kept := 0
	for i, row := range rows {
		var head struct {
			ID       *string  `json:"id"`
			FilePath *string  `json:"file_path"`
			AssetIDs []string `json:"asset_ids"`
		}
		if err := json.Unmarshal(row, &head); err != nil {
			return nil, fmt.Errorf("ci package: %s row %d: %w", name, i, err)
		}
		if head.ID == nil || *head.ID == "" {
			return nil, fmt.Errorf("ci package: %s row %d has no id", name, i)
		}
		id := *head.ID
		if seen[id] {
			return nil, fmt.Errorf("ci package: %s lists id %q twice", name, id)
		}
		seen[id] = true
		wantPath, ok := keep[id]
		if !ok {
			continue
		}
		if wantPath != "" && (head.FilePath == nil || *head.FilePath != wantPath) {
			return nil, fmt.Errorf("ci package: %s row %q file_path disagrees with the report's %q", name, id, wantPath)
		}
		for _, aid := range head.AssetIDs {
			if members != nil && !members[aid] {
				return nil, fmt.Errorf("ci package: selected post %q references asset %q, which is not selected", id, aid)
			}
		}
		if kept > 0 {
			buf.WriteString(",")
		}
		buf.WriteString("\n")
		buf.Write(row)
		kept++
	}
	for _, id := range sortedKeys(keep) {
		if !seen[id] {
			return nil, fmt.Errorf("ci package: selected id %q is not in %s (stale report)", id, name)
		}
	}
	buf.WriteString("\n]\n")
	return buf.Bytes(), nil
}

// checkCIPackageFile is the preflight for one closure file: a clean
// relative path, no symlink on any component, a regular file of the
// reported size. Its bytes are verified while they are copied.
func checkCIPackageFile(r *os.Root, f CISelectionFile) error {
	if err := checkRelPath(f.Path); err != nil {
		return fmt.Errorf("ci package: closure file %q: %w", f.Path, err)
	}
	parts := strings.Split(f.Path, "/")
	for i := range parts {
		prefix := path.Join(parts[:i+1]...)
		st, err := r.Lstat(filepath.FromSlash(prefix))
		if err != nil {
			return fmt.Errorf("ci package: closure file %q: %w", f.Path, err)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("ci package: closure file %q: %q is a symlink; the package holds real files only", f.Path, prefix)
		}
		if i == len(parts)-1 {
			if !st.Mode().IsRegular() {
				return fmt.Errorf("ci package: closure file %q is not a regular file", f.Path)
			}
			if st.Size() != f.Bytes {
				return fmt.Errorf("ci package: closure file %q is %d bytes, the report has %d (stale report)",
					f.Path, st.Size(), f.Bytes)
			}
		} else if !st.IsDir() {
			return fmt.Errorf("ci package: closure file %q: %q is not a directory", f.Path, prefix)
		}
	}
	return nil
}

// writeCIPackageArchive writes the deterministic tar.gz: fixed gzip
// header (no name, zero mtime) at a fixed level; tar members in a fixed
// order with fixed times, uid/gid 0, empty owner names and mode 0644; no
// directory or link entries.
func writeCIPackageArchive(w io.Writer, site *os.Root, pm, manifest, posts []byte, files []CISelectionFile) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("ci package: gzip: %w", err)
	}
	tw := tar.NewWriter(gz)
	for _, m := range []struct {
		name string
		body []byte
	}{{CIPackageManifestName, pm}, {"MANIFEST.json", manifest}, {"posts.json", posts}} {
		if err := tw.WriteHeader(ciTarHeader(m.name, int64(len(m.body)))); err != nil {
			return fmt.Errorf("ci package: archive %s: %w", m.name, err)
		}
		if _, err := tw.Write(m.body); err != nil {
			return fmt.Errorf("ci package: archive %s: %w", m.name, err)
		}
	}
	for _, f := range files {
		if err := copyCIPackageFile(tw, site, f); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("ci package: close archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("ci package: close gzip: %w", err)
	}
	return nil
}

func ciTarHeader(name string, size int64) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o644,
		ModTime: ciPackageEpoch, Uid: 0, Gid: 0, Uname: "", Gname: "", Format: tar.FormatPAX}
}

// copyCIPackageFile streams one closure file into the archive through
// SHA-256 and refuses it, failing the whole package, unless the bytes
// copied are exactly the reported length and digest, the file did not
// grow, and the path still names the same file it was read from.
func copyCIPackageFile(tw *tar.Writer, r *os.Root, f CISelectionFile) error {
	where := fmt.Sprintf("ci package: closure file %q", f.Path)
	fh, err := r.Open(filepath.FromSlash(f.Path))
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	defer fh.Close()
	before, err := fh.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	if !before.Mode().IsRegular() || before.Size() != f.Bytes {
		return fmt.Errorf("%s changed before it was copied", where)
	}
	if ciPackageBeforeCopy != nil {
		ciPackageBeforeCopy(f.Path)
	}
	if err := tw.WriteHeader(ciTarHeader(f.Path, f.Bytes)); err != nil {
		return fmt.Errorf("%s: archive header: %w", where, err)
	}
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(tw, h), fh, f.Bytes)
	if err != nil {
		return fmt.Errorf("%s changed while it was copied (%d of %d bytes): %w", where, n, f.Bytes, err)
	}
	var extra [1]byte
	if k, _ := fh.Read(extra[:]); k > 0 {
		return fmt.Errorf("%s grew while it was copied", where)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s: copied bytes have sha256 %s, the report has %s (changed or stale)", where, got, f.SHA256)
	}
	after, err := fh.Stat()
	if err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}
	now, err := r.Lstat(filepath.FromSlash(f.Path))
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, now) {
		return fmt.Errorf("%s changed while it was copied", where)
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	k, err := c.w.Write(p)
	c.n += int64(k)
	return k, err
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
