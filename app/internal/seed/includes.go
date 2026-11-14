// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Declared includes for the CI selection (#1514).
//
// The cross-peer overlap anchors (seed/contracts/peer-overlap.v1.json)
// must be in each peer's CI seed, but the coverage selector does not pick
// them: on today's site_a it omits both. This adds them, opt-in, after
// the coverage profile has run:
//
//   - explicitly requested: a contract path AND a peer, together, and only
//     with the ci profile. There is no default for either, and the peer is
//     never inferred from a directory name;
//   - checked before use with the same per-peer check `aa seed-overlap`
//     runs (exact asset id, path, measured length and SHA-256, mandatory
//     provenance, exactly one row with those bytes), against the same
//     manifest bytes the selection parsed;
//   - additive only: the anchor rows join the selected assets in manifest
//     order, de-duplicated if coverage already chose them. Every
//     coverage-selected post and asset stays; no post is added.
//
// selectCIProfile is the ONE selection path: Runner.selectCatalogue (the
// seeder) and runCISelectionPass (the exporter) both call it, so the seed
// and the report cannot drift.
//
// This includes rows. It reviews nothing: a contract is a fixture
// declaration, not rights clearance, and an included anchor is as
// unreviewed as any other selected file.

package seed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// CISelectionIncludesSchema is the report layout used when declared
// includes are requested: v1 plus a required declared_includes block, and
// assets/counts that include the anchors. A report without includes stays
// CISelectionSchema, byte for byte.
const CISelectionIncludesSchema = "artist-alley.ci-selection/v2"

// DeclaredIncludes requests one peer's overlap anchors in the CI selection.
type DeclaredIncludes struct {
	ContractPath string
	Peer         string
}

// IncludePin is what one resolution of the includes measured. The seed's
// preflight takes one before any mutation and the run must reproduce it
// exactly; the report carries it as declared_includes.
type IncludePin struct {
	Contract       string           `json:"contract"`
	ContractSchema string           `json:"contract_schema"`
	ContractSHA256 string           `json:"contract_sha256"`
	Peer           string           `json:"peer"`
	ManifestSHA256 string           `json:"manifest_sha256"`
	Anchors        []IncludedAnchor `json:"anchors"`
}

// IncludedAnchor is one anchor as found on the peer.
type IncludedAnchor struct {
	Fixture  string `json:"fixture"`
	AssetID  string `json:"asset_id"`
	FilePath string `json:"file_path"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	// AddedByInclude is false when the coverage profile had already
	// selected the row; it is then included once, not twice.
	AddedByInclude bool `json:"added_by_include"`
}

// CheckDeclaredIncludeFlags turns the two CLI values into a request. Both
// empty means no request; one without the other, or either without the
// ci profile, is refused.
func CheckDeclaredIncludeFlags(contract, peer, profile string) (*DeclaredIncludes, error) {
	if contract == "" && peer == "" {
		return nil, nil
	}
	if contract == "" || peer == "" {
		return nil, errors.New("declared includes: --overlap-contract and --overlap-peer must be given together")
	}
	if profile != ProfileCI {
		return nil, fmt.Errorf("declared includes need --profile %s (got %q); the full profile already seeds every row",
			ProfileCI, profile)
	}
	return &DeclaredIncludes{ContractPath: contract, Peer: peer}, nil
}

// resolvedIncludes is a checked request: the pin plus the contract as a
// pinned selection input.
type resolvedIncludes struct {
	pin   IncludePin
	input CISelectionInput
}

// resolveDeclaredIncludes reads the contract once, checks the peer is in
// it, and runs the per-peer overlap check against the site root. It only
// reads.
func resolveDeclaredIncludes(d *DeclaredIncludes, siteRoot string) (*resolvedIncludes, error) {
	if d.ContractPath == "" || d.Peer == "" {
		return nil, errors.New("declared includes: contract path and peer are both required")
	}
	raw, err := os.ReadFile(d.ContractPath)
	if err != nil {
		return nil, fmt.Errorf("declared includes: read contract: %w", err)
	}
	c, err := parseOverlapContract(raw)
	if err != nil {
		return nil, fmt.Errorf("declared includes: %w", err)
	}
	known := false
	for _, p := range c.Peers {
		known = known || p == d.Peer
	}
	if !known {
		return nil, fmt.Errorf("declared includes: peer %q is not in the contract (peers: %v)", d.Peer, c.Peers)
	}
	root, err := filepath.EvalSymlinks(siteRoot)
	if err != nil {
		return nil, fmt.Errorf("declared includes: site root: %w", err)
	}
	res, err := checkOverlapPeer(c, d.Peer, root)
	if err != nil {
		return nil, fmt.Errorf("declared includes: %w", err)
	}
	sum := sha256.Sum256(raw)
	base := filepath.Base(d.ContractPath)
	ri := &resolvedIncludes{
		pin: IncludePin{Contract: base, ContractSchema: c.Schema, ContractSHA256: hex.EncodeToString(sum[:]),
			Peer: d.Peer, ManifestSHA256: res.ManifestSHA256},
		input: CISelectionInput{Name: "overlap_contract/" + base, Present: true, Bytes: int64(len(raw)),
			SHA256: hex.EncodeToString(sum[:])},
	}
	for _, a := range res.Anchors {
		ri.pin.Anchors = append(ri.pin.Anchors, IncludedAnchor{Fixture: a.Fixture, AssetID: a.AssetID,
			FilePath: a.FilePath, Bytes: a.Bytes, SHA256: a.SHA256})
	}
	return ri, nil
}

// selectCIProfile is the CI selection: the coverage profile, then the
// declared includes when requested. parsedManifestSHA256 is the digest of
// the MANIFEST.json bytes loadCatalogues parsed; it must be the manifest
// the anchors were checked in.
func (c *catalogues) selectCIProfile(depth int, inc *resolvedIncludes, parsedManifestSHA256 string, log *slog.Logger) (*coverageReport, error) {
	if inc == nil {
		return c.applyCoverageProfile(depth, log)
	}
	if parsedManifestSHA256 != inc.pin.ManifestSHA256 {
		return nil, errors.New("declared includes: MANIFEST.json changed between the anchor check and the catalogue load")
	}
	full := append([]manifestAsset(nil), c.Assets...)
	rep, err := c.applyCoverageProfile(depth, log)
	if err != nil {
		return rep, err
	}
	if err := c.applyDeclaredIncludes(full, inc, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// applyDeclaredIncludes adds the anchor rows to an already-selected
// catalogue and brings the coverage report's totals with it.
func (c *catalogues) applyDeclaredIncludes(full []manifestAsset, inc *resolvedIncludes, rep *coverageReport) error {
	byID := make(map[string]manifestAsset, len(full))
	for _, a := range full {
		if _, dup := byID[a.ID]; dup {
			return fmt.Errorf("declared includes: duplicate manifest id %s", a.ID)
		}
		byID[a.ID] = a
	}
	kept := make(map[string]bool, len(c.Assets))
	for _, a := range c.Assets {
		kept[a.ID] = true
	}
	add := map[string]bool{}
	for i := range inc.pin.Anchors {
		a := &inc.pin.Anchors[i]
		row, ok := byID[a.AssetID]
		if !ok {
			return fmt.Errorf("declared includes: anchor %s (%s) is not in the parsed manifest", a.AssetID, a.Fixture)
		}
		// The overlap check measured the file; the parsed row must name
		// that same file and declare that same length, or the selection
		// would seed a row the check did not look at.
		if row.FilePath != a.FilePath || row.FileSizeBytes != a.Bytes {
			return fmt.Errorf("declared includes: anchor %s: parsed row %s (%d bytes) disagrees with the checked file %s (%d bytes)",
				a.AssetID, row.FilePath, row.FileSizeBytes, a.FilePath, a.Bytes)
		}
		if add[a.AssetID] {
			return fmt.Errorf("declared includes: anchor %s requested twice", a.AssetID)
		}
		a.AddedByInclude = !kept[a.AssetID]
		if a.AddedByInclude {
			add[a.AssetID] = true
		}
	}
	if len(add) == 0 {
		return nil
	}
	// Rebuild in manifest order, exactly as applyCoverageProfile
	// materialises its selection, so row order does not depend on
	// whether a row came from coverage or from the contract.
	var added []manifestAsset
	next := make([]manifestAsset, 0, len(c.Assets)+len(add))
	for _, a := range full {
		switch {
		case kept[a.ID]:
			next = append(next, a)
		case add[a.ID]:
			next = append(next, a)
			added = append(added, a)
		}
	}
	c.Assets = next
	companions := resolveCompanionAssets(added, c.SiteRoot)
	for _, a := range added {
		rep.Bytes += a.FileSizeBytes
		rep.PerExtension[a.FileExtension]++
		if companions[a.ID] {
			rep.Companions++
		}
		rep.DeclaredIncludesAdded++
	}
	rep.Assets = len(c.Assets)
	return nil
}

// PreflightCISelection runs the CI selection with declared includes,
// offline and read-only, and returns what it measured. `aa seed` calls it
// before migrating, bootstrapping or resetting anything, so an invalid
// request costs nothing; the seeder's run must then reproduce the pin.
func PreflightCISelection(opts CISelectionOptions) (*IncludePin, error) {
	if opts.Includes == nil {
		return nil, errors.New("declared includes: preflight needs a request")
	}
	depth := opts.CoverageDepth
	if depth <= 0 {
		depth = defaultCoverageDepth
	}
	rec := newInputRecorder(opts.CatalogueRoot, opts.SiteRoot)
	cat, err := loadCatalogues(opts.CatalogueRoot, opts.SiteRoot, rec.read)
	if rec.err != nil {
		return nil, rec.err
	}
	if err != nil {
		return nil, err
	}
	if err := cat.validateAIDeclarations(); err != nil {
		return nil, err
	}
	inc, err := resolveDeclaredIncludes(opts.Includes, opts.SiteRoot)
	if err != nil {
		return nil, err
	}
	if _, err := cat.selectCIProfile(depth, inc, rec.seen["site/MANIFEST.json"].SHA256, nil); err != nil {
		return nil, err
	}
	pin := inc.pin
	return &pin, nil
}
