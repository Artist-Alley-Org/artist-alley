// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Offline export of the `--profile ci` selection (#1514).
//
// A public CI dataset has to be packaged from the exact rows and files
// `aa seed --profile ci` would load, and a second selector would drift
// from the first. So this runs the same loadCatalogues and the same
// applyCoverageProfile, then describes the result: every catalogue input
// by digest, every selected row, and the complete file closure a package
// needs (each selected file plus every external model companion the
// format3d contract resolves for it), each with its length and SHA-256.
// It needs no database, config, master key or storage backend, and it
// never writes under the site root or the catalogue root.
//
// It is STRICTER than the seeder on purpose. The seeder soft-fails a
// missing or unreadable companion so the asset still seeds untextured
// (applyAssetCompanions); a release input that silently lost a texture
// would ship that damage to every consumer, so here the same gap is a
// refusal. The seeder's own behaviour is untouched.
//
// The report carries identifiers, site-relative paths, byte lengths,
// digests and dimension names. It carries no asset bytes, no absolute
// paths, and nothing about rights: a license label in a manifest is not
// a redistribution decision, and this file does not make one.

package seed

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
)

// CISelectionSchema versions the report layout. Bump it on any change to
// a field's name or meaning once a report has been consumed.
const CISelectionSchema = "artist-alley.ci-selection/v1"

// Companion-support values for CISelectionAsset.CompanionSupport.
//
// ⚠️ Only CompanionsResolved is a statement about the file's references.
// The other two say what was NOT read, and must never be read as "this
// asset needs no other file" (see format3d.CompanionUnsupported).
const (
	// CompanionsResolved: format3d read the model's references and every
	// one is listed in Companions (an empty list is then a real answer).
	CompanionsResolved = "resolved"
	// CompanionsUnknownModel: a 3D asset in a format format3d has no
	// reference reader for (STL, PLY, DAE, ...). Its companions are
	// UNKNOWN; a package built from this report may be missing some.
	CompanionsUnknownModel = "unknown_model_format"
	// CompanionsNotAModel: not a model format. The seeder registers no
	// companions for it, and neither does this export.
	CompanionsNotAModel = "not_a_model_format"
)

// CISelectionOptions names the inputs of an export.
type CISelectionOptions struct {
	// SiteRoot holds MANIFEST.json, posts.json and the asset bytes.
	SiteRoot string
	// CatalogueRoot holds the dataset.*.json profile files.
	CatalogueRoot string
	// CoverageDepth is the depth floor; 0 means the built-in default,
	// exactly as `aa seed --coverage-depth` treats it.
	CoverageDepth int
	// Includes, when set, adds one peer's overlap anchors after the
	// coverage profile (includes.go). The report is then
	// CISelectionIncludesSchema and pins the contract as an input.
	Includes *DeclaredIncludes
}

// CISelection is the exported report.
type CISelection struct {
	Schema        string `json:"schema"`
	Profile       string `json:"profile"`
	CoverageDepth int    `json:"coverage_depth"`
	// DeclaredIncludes is present exactly when the selection was asked to
	// include overlap anchors (schema CISelectionIncludesSchema).
	DeclaredIncludes *IncludePin `json:"declared_includes,omitempty"`

	// Inputs pins every file loadCatalogues read, sorted by name.
	Inputs     []CISelectionInput    `json:"inputs"`
	Counts     CISelectionCounts     `json:"counts"`
	Dimensions CISelectionDimensions `json:"dimensions"`

	// Assets are the selected manifest rows, sorted by id.
	Assets  []CISelectionAsset `json:"assets"`
	PostIDs []string           `json:"post_ids"`
	// Files is the package closure, sorted by path.
	Files []CISelectionFile `json:"files"`
}

// CISelectionInput is one catalogue input, named by its root ("site/" or
// "catalogue/") and file name. An optional input that does not exist is
// pinned as absent, so it appearing mid-export is a detected change.
type CISelectionInput struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Bytes   int64  `json:"bytes,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// CISelectionCounts are the totals. SelectedBytes is what the seeder
// reads for primary files: the sum over selected assets, so two assets
// that share one file count it twice. UniqueFileBytes counts each closure
// path once, companions included.
type CISelectionCounts struct {
	AssetsTotal    int `json:"assets_total"`
	PostsTotal     int `json:"posts_total"`
	AssetsSelected int `json:"assets_selected"`
	PostsSelected  int `json:"posts_selected"`
	// CompanionAssets are selected assets with at least one companion.
	CompanionAssets int `json:"companion_assets"`
	// UnknownModelAssets are selected 3D assets whose companions could
	// not be read (CompanionsUnknownModel).
	UnknownModelAssets int   `json:"unknown_model_assets"`
	PrimaryFiles       int   `json:"primary_files"`
	CompanionFiles     int   `json:"companion_files"`
	UniqueFiles        int   `json:"unique_files"`
	SelectedBytes      int64 `json:"selected_bytes"`
	UniqueFileBytes    int64 `json:"unique_file_bytes"`
	ObservedDimensions int   `json:"observed_dimensions"`
	CoveredDimensions  int   `json:"covered_dimensions"`
}

// CISelectionDimensions names the declared floor and what the selection
// covers. An export is only produced when every required dimension is
// covered and no observed dimension is left uncovered.
type CISelectionDimensions struct {
	Required []string `json:"required"`
	Covered  []string `json:"covered"`
}

// CISelectionAsset is one selected manifest row and the closure it needs.
type CISelectionAsset struct {
	ID               string   `json:"id"`
	FilePath         string   `json:"file_path"`
	CompanionSupport string   `json:"companion_support"`
	Companions       []string `json:"companions"`
}

// CISelectionFile is one closure file: its path relative to the site
// root, its measured length and digest, and the selected assets that
// need it as their primary file or as a companion. One path can be both.
type CISelectionFile struct {
	Path        string   `json:"path"`
	Bytes       int64    `json:"bytes"`
	SHA256      string   `json:"sha256"`
	PrimaryFor  []string `json:"primary_for,omitempty"`
	CompanionOf []string `json:"companion_of,omitempty"`
}

// ciSelectionAfterHash, when set, runs after the closure is hashed and
// before the export re-checks that nothing moved. Tests use it to change
// files at the one point a real concurrent writer could hurt.
var ciSelectionAfterHash func()

// BuildCISelection runs the CI coverage profile over the site root and
// describes the selection and its file closure. It only reads.
func BuildCISelection(opts CISelectionOptions) (*CISelection, error) {
	if opts.SiteRoot == "" {
		return nil, errors.New("ci selection: site root is required")
	}
	if opts.CatalogueRoot == "" {
		return nil, errors.New("ci selection: catalogue root is required")
	}
	depth := opts.CoverageDepth
	if depth < 0 {
		return nil, fmt.Errorf("ci selection: coverage depth %d is negative", depth)
	}
	if depth == 0 {
		depth = defaultCoverageDepth
	}
	info, err := os.Stat(opts.SiteRoot)
	if err != nil {
		return nil, fmt.Errorf("ci selection: site root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("ci selection: site root is not a directory")
	}
	root, err := filepath.EvalSymlinks(opts.SiteRoot)
	if err != nil {
		return nil, fmt.Errorf("ci selection: site root: %w", err)
	}

	first, err := runCISelectionPass(opts, depth, root)
	if err != nil {
		return nil, err
	}
	hashed, err := hashCIClosure(root, first.closure)
	if err != nil {
		return nil, err
	}

	if ciSelectionAfterHash != nil {
		ciSelectionAfterHash()
	}

	// Nothing the report describes may have moved while it was measured.
	// A second full pass re-reads every input and re-runs the selector and
	// the companion walk; it must agree with the first exactly. Then every
	// hashed file must still be the same file, length and mtime.
	second, err := runCISelectionPass(opts, depth, root)
	if err != nil {
		return nil, fmt.Errorf("ci selection: the re-check after hashing failed, so the site or catalogue changed during the export: %w", err)
	}
	if err := first.sameAs(second); err != nil {
		return nil, err
	}
	if err := recheckCIClosure(root, hashed); err != nil {
		return nil, err
	}
	return first.report(depth, hashed)
}

// ExportCISelection builds the report and writes it to outPath as JSON,
// atomically and readable only by its owner. outPath may not lie inside
// the site root or the catalogue root, through any symlink alias (see
// outputDir.verify for the limit of that guarantee under a concurrent
// rename). It returns the report it wrote.
func ExportCISelection(opts CISelectionOptions, outPath string) (*CISelection, error) {
	if outPath == "" {
		return nil, errors.New("ci selection: output path is required")
	}
	protected := []protectedRoot{
		{label: "site root", path: opts.SiteRoot},
		{label: "catalogue root", path: opts.CatalogueRoot},
	}
	// Checked before the build so a bad destination fails fast, and again
	// just before the write, against the directory handle held since.
	out, err := openOutputDir(protected, outPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	rep, err := BuildCISelection(opts)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("ci selection: encode: %w", err)
	}
	data = append(data, '\n')
	if err := out.verify(protected, outPath); err != nil {
		return nil, err
	}
	if err := out.writeFileAtomic0600(data); err != nil {
		return nil, err
	}
	return rep, nil
}

// ciPass is one read of the inputs, one selection and one companion walk.
type ciPass struct {
	inputs  []CISelectionInput
	cat     *catalogues
	rep     *coverageReport
	closure *ciClosure
	inc     *resolvedIncludes
}

func runCISelectionPass(opts CISelectionOptions, depth int, root string) (*ciPass, error) {
	rec := newInputRecorder(opts.CatalogueRoot, opts.SiteRoot)
	cat, err := loadCatalogues(opts.CatalogueRoot, opts.SiteRoot, rec.read)
	if rec.err != nil {
		return nil, rec.err
	}
	if err != nil {
		return nil, err
	}
	// The seeder refuses a manifest with a false AI declaration before it
	// selects anything; an export that accepted one would describe a seed
	// that cannot run.
	if err := cat.validateAIDeclarations(); err != nil {
		return nil, err
	}
	if err := validateCISelectionCatalogue(cat, root); err != nil {
		return nil, err
	}
	pins := rec.pins()
	var inc *resolvedIncludes
	if opts.Includes != nil {
		if inc, err = resolveDeclaredIncludes(opts.Includes, opts.SiteRoot); err != nil {
			return nil, err
		}
		// The contract is an input like any catalogue file: a change to
		// it between the two passes is a detected change.
		pins = append(pins, inc.input)
		sort.Slice(pins, func(i, j int) bool { return pins[i].Name < pins[j].Name })
	}
	rep, err := cat.selectCIProfile(depth, inc, rec.seen["site/MANIFEST.json"].SHA256, nil)
	if err != nil {
		return nil, err
	}
	closure, err := resolveCIClosure(root, cat.Assets)
	if err != nil {
		return nil, err
	}
	return &ciPass{inputs: pins, cat: cat, rep: rep, closure: closure, inc: inc}, nil
}

// sameAs refuses a second pass that disagrees with the first in any
// input digest, selected row, or closure entry.
func (p *ciPass) sameAs(q *ciPass) error {
	if !jsonEqual(p.inputs, q.inputs) {
		return errors.New("ci selection: a catalogue input changed during the export")
	}
	if !jsonEqual(p.selectedIDs(), q.selectedIDs()) {
		return errors.New("ci selection: the selection changed during the export")
	}
	if !jsonEqual(p.closure.assets, q.closure.assets) || !jsonEqual(p.closure.files, q.closure.files) {
		return errors.New("ci selection: the companion closure changed during the export")
	}
	if (p.inc == nil) != (q.inc == nil) || (p.inc != nil && !jsonEqual(p.inc.pin, q.inc.pin)) {
		return errors.New("ci selection: the declared includes changed during the export")
	}
	return nil
}

func (p *ciPass) selectedIDs() [2][]string {
	var a, q []string
	for _, x := range p.cat.Assets {
		a = append(a, x.ID)
	}
	for _, x := range p.cat.Posts {
		q = append(q, x.ID)
	}
	sort.Strings(a)
	sort.Strings(q)
	return [2][]string{a, q}
}

func (p *ciPass) report(depth int, hashed map[string]hashedFile) (*CISelection, error) {
	rep := p.rep
	out := &CISelection{
		Schema:        CISelectionSchema,
		Profile:       ProfileCI,
		CoverageDepth: depth,
		Inputs:        p.inputs,
		Assets:        p.closure.assets,
		PostIDs:       make([]string, 0, len(p.cat.Posts)),
		Files:         make([]CISelectionFile, 0, len(p.closure.files)),
		Dimensions: CISelectionDimensions{
			Required: make([]string, 0, len(requiredDims)),
			Covered:  make([]string, 0, len(rep.CoveredDims)),
		},
	}
	if p.inc != nil {
		out.Schema = CISelectionIncludesSchema
		pin := p.inc.pin
		out.DeclaredIncludes = &pin
	}
	for _, d := range requiredDims {
		out.Dimensions.Required = append(out.Dimensions.Required, d.String())
	}
	sort.Strings(out.Dimensions.Required)
	for _, d := range rep.CoveredDims {
		out.Dimensions.Covered = append(out.Dimensions.Covered, d.String())
	}
	sort.Strings(out.Dimensions.Covered)
	for _, x := range p.cat.Posts {
		out.PostIDs = append(out.PostIDs, x.ID)
	}
	sort.Strings(out.PostIDs)

	n := CISelectionCounts{
		AssetsTotal:        rep.AssetsBefore,
		PostsTotal:         rep.PostsBefore,
		AssetsSelected:     rep.Assets,
		PostsSelected:      rep.Posts,
		ObservedDimensions: rep.Universe,
		CoveredDimensions:  rep.Covered,
	}
	for _, fp := range sortedKeys(p.closure.files) {
		roles := p.closure.files[fp]
		h := hashed[fp]
		out.Files = append(out.Files, CISelectionFile{
			Path: fp, Bytes: h.info.Size(), SHA256: h.sha,
			PrimaryFor: roles.PrimaryFor, CompanionOf: roles.CompanionOf,
		})
		n.UniqueFileBytes += h.info.Size()
		n.SelectedBytes += h.info.Size() * int64(len(roles.PrimaryFor))
		if len(roles.PrimaryFor) > 0 {
			n.PrimaryFiles++
		}
		if len(roles.CompanionOf) > 0 {
			n.CompanionFiles++
		}
	}
	n.UniqueFiles = len(out.Files)
	for _, a := range out.Assets {
		if len(a.Companions) > 0 {
			n.CompanionAssets++
		}
		if a.CompanionSupport == CompanionsUnknownModel {
			n.UnknownModelAssets++
		}
	}
	out.Counts = n

	// The report's own totals must agree with the selector's, or the
	// export does not describe the selection the seeder would load.
	if len(out.Assets) != rep.Assets {
		return nil, fmt.Errorf("ci selection: closure holds %d assets, selector kept %d", len(out.Assets), rep.Assets)
	}
	if n.SelectedBytes != rep.Bytes {
		return nil, fmt.Errorf("ci selection: measured %d selected bytes, selector counted %d",
			n.SelectedBytes, rep.Bytes)
	}
	if n.CompanionAssets != rep.Companions {
		return nil, fmt.Errorf("ci selection: closure found %d companion-bearing assets, selector counted %d",
			n.CompanionAssets, rep.Companions)
	}
	return out, nil
}

// --- inputs -------------------------------------------------------------

// inputRecorder is the reader handed to loadCatalogues: it returns the
// file's bytes and pins their digest, so a pin is by construction the
// digest of what was parsed.
type inputRecorder struct {
	catRoot, siteRoot string
	seen              map[string]CISelectionInput
	err               error
}

func newInputRecorder(catRoot, siteRoot string) *inputRecorder {
	return &inputRecorder{catRoot: filepath.Clean(catRoot), siteRoot: filepath.Clean(siteRoot),
		seen: map[string]CISelectionInput{}}
}

func (r *inputRecorder) read(p string) ([]byte, error) {
	var name string
	switch dir := filepath.Dir(p); dir {
	case r.siteRoot:
		name = "site/" + filepath.Base(p)
	case r.catRoot:
		name = "catalogue/" + filepath.Base(p)
	default:
		// loadCatalogues reads only directly under its two roots; a read
		// elsewhere means the loader grew an input this export does not
		// know how to name.
		if r.err == nil {
			r.err = fmt.Errorf("ci selection: catalogue loader read an input outside both roots: %s",
				filepath.Base(p))
		}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.seen[name] = CISelectionInput{Name: name}
		}
		return nil, err
	}
	sum := sha256.Sum256(b)
	r.seen[name] = CISelectionInput{Name: name, Present: true, Bytes: int64(len(b)),
		SHA256: hex.EncodeToString(sum[:])}
	return b, nil
}

func (r *inputRecorder) pins() []CISelectionInput {
	out := make([]CISelectionInput, 0, len(r.seen))
	for _, k := range sortedKeys(r.seen) {
		out = append(out, r.seen[k])
	}
	return out
}

// --- catalogue validation ---------------------------------------------

// validateCISelectionCatalogue refuses a catalogue the export cannot
// describe unambiguously. It runs before selection, because selection
// opens model files by path to look for companions.
func validateCISelectionCatalogue(c *catalogues, root string) error {
	assetIDs := make(map[string]bool, len(c.Assets))
	for i, a := range c.Assets {
		if a.ID == "" {
			return fmt.Errorf("ci selection: manifest row %d has no id", i)
		}
		if assetIDs[a.ID] {
			return fmt.Errorf("ci selection: duplicate asset id %q", a.ID)
		}
		assetIDs[a.ID] = true
		if a.FileSizeBytes < 0 {
			return fmt.Errorf("ci selection: asset %q has a negative file_size_bytes", a.ID)
		}
		if err := checkRelPath(a.FilePath); err != nil {
			return fmt.Errorf("ci selection: asset %q: %w", a.ID, err)
		}
		if err := checkContained(root, a.FilePath); err != nil {
			return fmt.Errorf("ci selection: asset %q: %w", a.ID, err)
		}
	}
	postIDs := make(map[string]bool, len(c.Posts))
	for i, p := range c.Posts {
		if p.ID == "" {
			return fmt.Errorf("ci selection: post row %d has no id", i)
		}
		if postIDs[p.ID] {
			return fmt.Errorf("ci selection: duplicate post id %q", p.ID)
		}
		postIDs[p.ID] = true
		seen := make(map[string]bool, len(p.AssetIDs))
		for _, aid := range p.AssetIDs {
			if !assetIDs[aid] {
				return fmt.Errorf("ci selection: post %q references unknown asset %q", p.ID, aid)
			}
			if seen[aid] {
				return fmt.Errorf("ci selection: post %q lists asset %q twice", p.ID, aid)
			}
			seen[aid] = true
		}
	}
	return nil
}

// checkRelPath accepts only a clean, slash-separated path that stays
// below the site root.
func checkRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("empty file_path")
	case strings.ContainsAny(p, "\x00\\"):
		return errors.New("unsafe file_path (NUL or backslash)")
	case strings.HasPrefix(p, "/"), len(p) >= 2 && p[1] == ':':
		return errors.New("unsafe file_path (absolute)")
	case path.Clean(p) != p, !filepath.IsLocal(filepath.FromSlash(p)):
		return errors.New("unsafe file_path (not a clean path below the site root)")
	}
	return nil
}

// checkContained refuses a path that resolves, through symlinks, outside
// the site root. A path that does not exist passes here; selected ones
// are required to exist by checkSiteFile.
func checkContained(root, rel string) error {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("resolve file_path: %w", err)
	}
	if !within(root, resolved) {
		return errors.New("file_path resolves outside the site root")
	}
	return nil
}

func within(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != "." && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// checkSiteFile requires rel to name a readable regular file inside the
// site root, through any symlinks, and returns its length.
func checkSiteFile(root, rel string) (int64, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return 0, fmt.Errorf("file is missing or not readable: %w", err)
	}
	if !within(root, resolved) {
		return 0, errors.New("file_path resolves outside the site root")
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return 0, fmt.Errorf("file is missing or not readable: %w", err)
	}
	if !st.Mode().IsRegular() {
		return 0, errors.New("file is not a regular file")
	}
	return st.Size(), nil
}

// --- closure ------------------------------------------------------------

// ciClosure is the package closure: the selected assets and, per
// site-relative path, which of them need that file and how.
type ciClosure struct {
	assets []CISelectionAsset
	files  map[string]*ciFileRoles
}

type ciFileRoles struct {
	PrimaryFor  []string `json:"primary_for,omitempty"`
	CompanionOf []string `json:"companion_of,omitempty"`
}

// resolveCIClosure walks the selected assets' primary files and model
// companions. Companion discovery is format3d.ResolveCompanions, the
// same contract Runner.applyAssetCompanions seeds from, called on the
// same path; what the export adds is strictness, because a package that
// quietly lacks a texture is broken for every consumer:
//
//   - a primary file must exist, be a regular file inside the site root,
//     and have the length the manifest declares (the size is a selection
//     input, so a wrong one means a different selection);
//   - a model whose references cannot be parsed is refused, where the
//     seeder logs and seeds it untextured;
//   - every declared companion must exist and be a regular file inside
//     the site root, where the seeder logs and skips a missing one.
//
// References the format3d contract itself drops (data: URIs, remote
// URLs, absolute paths, and parent-escaping `..` paths) are not part of
// the closure, exactly as they are not part of a seeded asset.
func resolveCIClosure(root string, assets []manifestAsset) (*ciClosure, error) {
	sorted := append([]manifestAsset(nil), assets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	cl := &ciClosure{assets: make([]CISelectionAsset, 0, len(sorted)), files: map[string]*ciFileRoles{}}
	roles := func(p string) *ciFileRoles {
		r := cl.files[p]
		if r == nil {
			r = &ciFileRoles{}
			cl.files[p] = r
		}
		return r
	}
	for _, a := range sorted {
		size, err := checkSiteFile(root, a.FilePath)
		if err != nil {
			return nil, fmt.Errorf("ci selection: selected asset %q: %w", a.ID, err)
		}
		if size != a.FileSizeBytes {
			return nil, fmt.Errorf("ci selection: selected asset %q: file is %d bytes but the manifest declares %d",
				a.ID, size, a.FileSizeBytes)
		}
		r := roles(a.FilePath)
		r.PrimaryFor = append(r.PrimaryFor, a.ID)

		entry := CISelectionAsset{ID: a.ID, FilePath: a.FilePath, Companions: []string{}}
		// ResolveCompanions decides support from the path's extension,
		// and so does the seeder that calls it; ask the same question.
		switch {
		case format3d.CompanionSupportFor(filepath.Ext(a.FilePath)) != format3d.CompanionUnsupported:
			entry.CompanionSupport = CompanionsResolved
			found, missing, err := format3d.ResolveCompanions(filepath.Join(root, filepath.FromSlash(a.FilePath)))
			if err != nil {
				return nil, fmt.Errorf("ci selection: selected asset %q: companions could not be read: %w", a.ID, err)
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				return nil, fmt.Errorf("ci selection: selected asset %q: declared companion(s) missing: %s",
					a.ID, strings.Join(missing, ", "))
			}
			seen := map[string]bool{}
			for _, rel := range found {
				p := path.Join(path.Dir(a.FilePath), rel)
				if seen[p] {
					continue
				}
				seen[p] = true
				if err := checkRelPath(p); err != nil {
					return nil, fmt.Errorf("ci selection: selected asset %q: companion %q: %w", a.ID, rel, err)
				}
				if _, err := checkSiteFile(root, p); err != nil {
					return nil, fmt.Errorf("ci selection: selected asset %q: companion %q: %w", a.ID, p, err)
				}
				entry.Companions = append(entry.Companions, p)
				cr := roles(p)
				cr.CompanionOf = append(cr.CompanionOf, a.ID)
			}
			sort.Strings(entry.Companions)
		case a.AssetType == "3d":
			entry.CompanionSupport = CompanionsUnknownModel
		default:
			entry.CompanionSupport = CompanionsNotAModel
		}
		cl.assets = append(cl.assets, entry)
	}
	return cl, nil
}

// hashedFile is a closure file as it was when hashed.
type hashedFile struct {
	info fs.FileInfo
	sha  string
}

// hashCIClosure hashes every closure file through an os.Root, so a
// symlink swapped in after resolution still cannot lead outside the site
// root, and refuses a file whose length or mtime moved while it was read.
func hashCIClosure(root string, cl *ciClosure) (map[string]hashedFile, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("ci selection: site root: %w", err)
	}
	defer r.Close()
	out := make(map[string]hashedFile, len(cl.files))
	for _, p := range sortedKeys(cl.files) {
		h, err := hashRootFile(r, p)
		if err != nil {
			return nil, fmt.Errorf("ci selection: file %q: %w", p, err)
		}
		out[p] = h
	}
	return out, nil
}

func hashRootFile(r *os.Root, p string) (hashedFile, error) {
	f, err := r.Open(filepath.FromSlash(p))
	if err != nil {
		return hashedFile{}, fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return hashedFile{}, fmt.Errorf("stat: %w", err)
	}
	if !before.Mode().IsRegular() {
		return hashedFile{}, errors.New("not a regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return hashedFile{}, fmt.Errorf("read: %w", err)
	}
	after, err := f.Stat()
	if err != nil {
		return hashedFile{}, fmt.Errorf("stat: %w", err)
	}
	if n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return hashedFile{}, errors.New("changed while it was being hashed")
	}
	return hashedFile{info: before, sha: hex.EncodeToString(h.Sum(nil))}, nil
}

// recheckCIClosure re-hashes every closure file and refuses the export
// unless each is still the same file with the same digest. Length and
// mtime alone are not enough: file timestamps can tick as coarsely as a
// kernel jiffy, so a same-length rewrite right after the first hash can
// keep its mtime. This doubles the bytes read, which is the price of a
// digest that describes the file rather than a moment.
func recheckCIClosure(root string, hashed map[string]hashedFile) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("ci selection: site root: %w", err)
	}
	defer r.Close()
	for _, p := range sortedKeys(hashed) {
		h := hashed[p]
		again, err := hashRootFile(r, p)
		if err != nil {
			return fmt.Errorf("ci selection: file %q changed during the export: %w", p, err)
		}
		if !os.SameFile(again.info, h.info) || again.sha != h.sha {
			return fmt.Errorf("ci selection: file %q changed during the export", p)
		}
	}
	return nil
}

// --- output -------------------------------------------------------------

// protectedRoot is an input tree the export must never write into: the
// site root (the dataset it describes) and the catalogue root (whose
// files it pins). Writing the report over `seed/profiles/dataset.users.json`
// would replace an input right after reporting its digest.
type protectedRoot struct {
	label, path string
}

// outputDir is the checked directory a report is written into. It is held
// open, and the write goes through that handle, so the report lands in the
// directory that was checked rather than wherever the path points later.
type outputDir struct {
	base string
	root *os.Root
	info fs.FileInfo
}

// openOutputDir refuses an output path whose directory, after resolving
// every symlink in it, is or lies inside any protected root, then opens
// that directory and binds the handle to it. An output path equal to an
// input file is refused too, since every input lives inside a root.
func openOutputDir(protected []protectedRoot, outPath string) (*outputDir, error) {
	base := filepath.Base(outPath)
	if outPath == "" || base == "." || base == ".." || base == string(filepath.Separator) ||
		strings.HasSuffix(outPath, string(filepath.Separator)) {
		return nil, errors.New("ci selection: output path must name a file")
	}
	dir, err := resolveOutputDir(protected, outPath)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("ci selection: output directory: %w", err)
	}
	info, err := r.Stat(".")
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("ci selection: output directory: %w", err)
	}
	o := &outputDir{base: base, root: r, info: info}
	if err := o.verify(protected, outPath); err != nil {
		_ = r.Close()
		return nil, err
	}
	return o, nil
}

// resolveOutputDir resolves outPath's directory through every symlink and
// refuses it if it is, or lies under, a protected root.
func resolveOutputDir(protected []protectedRoot, outPath string) (string, error) {
	dir, err := filepath.Abs(filepath.Dir(outPath))
	if err != nil {
		return "", fmt.Errorf("ci selection: output path: %w", err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("ci selection: output directory: %w", err)
	}
	for _, p := range protected {
		if p.path == "" {
			return "", fmt.Errorf("ci selection: %s is required", p.label)
		}
		root, err := filepath.EvalSymlinks(p.path)
		if err != nil {
			return "", fmt.Errorf("ci selection: %s: %w", p.label, err)
		}
		if root, err = filepath.Abs(root); err != nil {
			return "", fmt.Errorf("ci selection: %s: %w", p.label, err)
		}
		if dir == root || within(root, dir) {
			return "", fmt.Errorf("ci selection: output path is inside the %s", p.label)
		}
	}
	return dir, nil
}

// verify re-runs the path check and requires the path to still name the
// held directory, and the held directory to be none of the protected roots
// by identity, which no path alias can fake.
//
// ⚠️ The guarantee is bounded. Between this check and the rename that
// installs the report, a concurrent process that RENAMES the held
// directory into a protected tree can still place the report there. It
// cannot make the handle BE a protected root, so it cannot replace a
// catalogue input or a top-level site file, but it can reach a site
// subdirectory. Closing that needs a lock on the input trees, which the
// export does not take.
func (o *outputDir) verify(protected []protectedRoot, outPath string) error {
	dir, err := resolveOutputDir(protected, outPath)
	if err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("ci selection: output directory: %w", err)
	}
	if !os.SameFile(st, o.info) {
		return errors.New("ci selection: output directory changed while the report was being built")
	}
	for _, p := range protected {
		rs, err := os.Stat(p.path)
		if err != nil {
			return fmt.Errorf("ci selection: %s: %w", p.label, err)
		}
		if os.SameFile(rs, o.info) {
			return fmt.Errorf("ci selection: output path is inside the %s", p.label)
		}
	}
	return nil
}

func (o *outputDir) Close() error { return o.root.Close() }

// writeFileAtomic0600 writes data to a temp file in the held directory,
// syncs it, and renames it over the output name, so a reader sees the old
// file or the whole new one and a failed write leaves nothing behind.
// Replacing anything other than a regular file is refused. A rename
// replaces the directory entry, so an existing output that is a hard link
// to some other file leaves that file's bytes alone.
func (o *outputDir) writeFileAtomic0600(data []byte) error {
	return o.writeAtomic0600(".ci-selection-", func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}, nil)
}

// writeAtomic0600 is writeFileAtomic0600 for a streamed body: fill writes
// the content to w, and nothing is installed unless it returns nil. When
// ready is non-nil it runs after the temp file is complete and synced and
// before the rename, so a caller can re-verify its inputs at the last
// moment; an error from it discards the temp file too.
func (o *outputDir) writeAtomic0600(prefix string, fill func(w io.Writer) error, ready func() error) (err error) {
	if st, lerr := o.root.Lstat(o.base); lerr == nil {
		if !st.Mode().IsRegular() {
			return errors.New("ci selection: output path exists and is not a regular file")
		}
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		return fmt.Errorf("ci selection: output path: %w", lerr)
	}
	tmpName := prefix + rand.Text() + ".tmp"
	tmp, err := o.root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("ci selection: create output: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = o.root.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("ci selection: chmod output: %w", err)
	}
	if err = fill(tmp); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("ci selection: sync output: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("ci selection: close output: %w", err)
	}
	if ready != nil {
		if err = ready(); err != nil {
			return err
		}
	}
	// The final name may have been replaced by a symlink or a directory
	// while the body was written; rename would follow neither, but refuse
	// rather than silently replace a non-regular entry.
	if st, lerr := o.root.Lstat(o.base); lerr == nil && !st.Mode().IsRegular() {
		return errors.New("ci selection: output path exists and is not a regular file")
	}
	if err = o.root.Rename(tmpName, o.base); err != nil {
		return fmt.Errorf("ci selection: install output: %w", err)
	}
	if d, derr := o.root.Open("."); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// --- helpers --------------------------------------------------------------

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}
