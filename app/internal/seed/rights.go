// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// File-complete rights ledger for a CI selection report (#1514).
//
// A public CI package may contain only files somebody has reviewed. The
// selection report (export.go) names every file it could resolve for a
// package, companions included; this ledger gives each of those files
// exactly one row and a review state, bound to the exact report bytes it
// was drafted from. DraftRightsLedger writes every row as `pending`; a
// human edits rows to `hold` or `reviewed_allow`; CheckRightsLedger refuses
// any drift from the report and reports whether the ledger is eligible.
//
// ⚠️ The listed files are the complete closure ONLY when the report has no
// unknown_model_format assets. For such a model format3d cannot read the
// references, so files it needs may be missing from the report and from
// this ledger. A draft is still produced (it is a worklist), but the check
// is never eligible while counts.unknown_model_assets > 0, however many
// listed rows are reviewed.
//
// ⛔ STRUCTURAL ONLY. This code checks that the evidence fields a reviewer
// must fill are present and consistent with the report. It cannot check
// that a license says what the reviewer wrote, that a source is real, or
// that redistribution is lawful. A passing check is not legal clearance
// and not authorization to publish anything.
//
// Offline and read-only: no database, network or storage, no default
// input path, and nothing is written; the CLI prints to stdout.

package seed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// RightsLedgerSchema versions the ledger layout.
const RightsLedgerSchema = "artist-alley.rights-ledger/v1"

// RightsCheckSchema versions the check report.
const RightsCheckSchema = "artist-alley.rights-ledger-check/v1"

// RightsStructuralOnly is printed with every check result.
const RightsStructuralOnly = "STRUCTURAL ONLY: this checks that review fields are present and consistent " +
	"with the selection report. It is not legal clearance and not authorization to publish."

// Ledger row states. Anything else is refused.
const (
	// RightsPending: not reviewed. The draft writes every row this way.
	RightsPending = "pending"
	// RightsHold: reviewed and NOT to be packaged (replace or remove it).
	RightsHold = "hold"
	// RightsReviewedAllow: a human recorded the evidence and decided the
	// file may be redistributed. Requires a complete RightsReview.
	RightsReviewedAllow = "reviewed_allow"
)

// RedistributionAllowed is the only redistribution decision a
// reviewed_allow row may carry.
const RedistributionAllowed = "allowed"

// RightsLedger is the ledger document.
type RightsLedger struct {
	Schema    string                `json:"schema"`
	Notice    string                `json:"notice"`
	Selection RightsLedgerSelection `json:"selection"`
	Entries   []RightsLedgerEntry   `json:"entries"`
}

// RightsLedgerSelection binds the ledger to one exact selection report.
type RightsLedgerSelection struct {
	SHA256          string `json:"sha256"`
	Schema          string `json:"schema"`
	Profile         string `json:"profile"`
	CoverageDepth   int    `json:"coverage_depth"`
	Files           int    `json:"files"`
	UniqueFileBytes int64  `json:"unique_file_bytes"`
}

// RightsLedgerEntry is one closure file. Path, Bytes, SHA256, PrimaryFor
// and CompanionOf are copied from the report and must keep matching it;
// Status, Review and Note are the reviewer's.
type RightsLedgerEntry struct {
	Path        string        `json:"path"`
	Bytes       int64         `json:"bytes"`
	SHA256      string        `json:"sha256"`
	PrimaryFor  []string      `json:"primary_for"`
	CompanionOf []string      `json:"companion_of"`
	Status      string        `json:"status"`
	Review      *RightsReview `json:"review"`
	Note        string        `json:"note,omitempty"`
}

// RightsReview is the evidence a reviewed_allow row must carry.
//
//   - Sources: where these exact bytes came from (asset page, archive URL
//     and member, upstream commit, ...). At least one.
//   - License: the license or terms identifier, e.g. "CC0-1.0", "OFL-1.1".
//   - LicenseEvidence: where those terms were read (license file in the
//     pack, terms page, ...). At least one.
//   - Redistribution: the explicit decision; must be "allowed".
//   - Attribution: whether credit or a notice must ship, and what. Its
//     Required field must be stated either way.
//   - Reviewer, ReviewedOn: who decided, and when (YYYY-MM-DD).
type RightsReview struct {
	Sources         []string           `json:"sources"`
	License         string             `json:"license"`
	LicenseEvidence []string           `json:"license_evidence"`
	Redistribution  string             `json:"redistribution"`
	Attribution     *RightsAttribution `json:"attribution"`
	Reviewer        string             `json:"reviewer"`
	ReviewedOn      string             `json:"reviewed_on"`
}

// RightsAttribution records the attribution or notice obligation.
type RightsAttribution struct {
	// Required is a pointer so that "not stated" is distinguishable from
	// "stated: none needed".
	Required *bool    `json:"required"`
	Text     string   `json:"text,omitempty"`
	Notices  []string `json:"notices,omitempty"`
}

// RightsCheck is the result of a structural check that passed.
type RightsCheck struct {
	Schema          string         `json:"schema"`
	Label           string         `json:"label"`
	SelectionSHA256 string         `json:"selection_sha256"`
	Entries         int            `json:"entries"`
	Counts          map[string]int `json:"counts"`
	// Eligible is true only when there is at least one row, every row is
	// reviewed_allow, AND the report's closure is complete (no
	// unknown_model_format assets).
	Eligible bool `json:"release_eligible_structurally"`
	// Blocking lists every row that is not reviewed_allow, by path.
	Blocking []RightsBlocking `json:"blocking"`
	// UnknownModelAssets is the report's count of selected models whose
	// companions could not be read, with their ids. Any makes the closure
	// incomplete, separately from the row states above.
	UnknownModelAssets   int      `json:"unknown_model_assets"`
	UnknownModelAssetIDs []string `json:"unknown_model_asset_ids"`
	// Ineligible gives every reason Eligible is false; empty when true.
	Ineligible []string `json:"ineligible_reasons"`
}

// Ineligibility reasons, in the order they are reported.
const (
	RightsReasonNoEntries        = "the ledger has no entries"
	RightsReasonIncompleteReview = "not every entry is reviewed_allow"
	RightsReasonIncompleteClose  = "incomplete companion closure"
)

// RightsBlocking is one row keeping the ledger from eligibility.
type RightsBlocking struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// LoadCISelectionReport reads a selection report and checks it is
// internally consistent. It returns the report and the SHA-256 of its raw
// bytes, which a ledger binds to.
func LoadCISelectionReport(p string) (*CISelection, string, error) {
	if p == "" {
		return nil, "", errors.New("rights: selection report path is required")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("rights: read selection report: %w", err)
	}
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, "", fmt.Errorf("rights: parse selection report: %w", err)
	}
	if head.Schema != CISelectionSchema && head.Schema != CISelectionIncludesSchema {
		return nil, "", fmt.Errorf("rights: selection report schema %q, want %q or %q",
			head.Schema, CISelectionSchema, CISelectionIncludesSchema)
	}
	var sel CISelection
	if err := strictDecode(raw, &sel); err != nil {
		return nil, "", fmt.Errorf("rights: parse selection report: %w", err)
	}
	if err := validateSelectionReport(&sel); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return &sel, hex.EncodeToString(sum[:]), nil
}

// validateSelectionReport refuses a report whose files, roles and counts
// do not describe each other exactly. A ledger drafted from an
// inconsistent report would inherit the inconsistency silently.
func validateSelectionReport(s *CISelection) error {
	if s.Profile != ProfileCI {
		return fmt.Errorf("rights: selection report profile %q, want %q", s.Profile, ProfileCI)
	}
	// v2 means declared includes and nothing else: the block must be there
	// exactly when the schema says so.
	if (s.Schema == CISelectionIncludesSchema) != (s.DeclaredIncludes != nil) {
		return fmt.Errorf("rights: selection report schema %q with declared_includes present=%v",
			s.Schema, s.DeclaredIncludes != nil)
	}
	assets := map[string]CISelectionAsset{}
	for i, a := range s.Assets {
		if a.ID == "" {
			return fmt.Errorf("rights: selection report asset %d has no id", i)
		}
		if i > 0 && s.Assets[i-1].ID >= a.ID {
			return fmt.Errorf("rights: selection report assets not strictly sorted at %q (duplicate or out of order)", a.ID)
		}
		if err := checkRelPath(a.FilePath); err != nil {
			return fmt.Errorf("rights: selection report asset %q: %w", a.ID, err)
		}
		switch a.CompanionSupport {
		case CompanionsResolved, CompanionsUnknownModel, CompanionsNotAModel:
		default:
			return fmt.Errorf("rights: selection report asset %q: unknown companion_support %q", a.ID, a.CompanionSupport)
		}
		if a.CompanionSupport != CompanionsResolved && len(a.Companions) > 0 {
			return fmt.Errorf("rights: selection report asset %q lists companions but support is %q", a.ID, a.CompanionSupport)
		}
		if err := sortedUnique(a.Companions); err != nil {
			return fmt.Errorf("rights: selection report asset %q companions: %w", a.ID, err)
		}
		assets[a.ID] = a
	}

	// Every role a file claims must be backed by the asset, and every
	// file an asset needs must be present with that role.
	wantPrimary := map[string][]string{}
	wantCompanion := map[string][]string{}
	for _, a := range s.Assets {
		wantPrimary[a.FilePath] = append(wantPrimary[a.FilePath], a.ID)
		for _, c := range a.Companions {
			wantCompanion[c] = append(wantCompanion[c], a.ID)
		}
	}
	var unique, selected int64
	primaryFiles, companionFiles := 0, 0
	for i, f := range s.Files {
		if i > 0 && s.Files[i-1].Path >= f.Path {
			return fmt.Errorf("rights: selection report files not strictly sorted at %q (duplicate or out of order)", f.Path)
		}
		if err := checkRelPath(f.Path); err != nil {
			return fmt.Errorf("rights: selection report file %q: %w", f.Path, err)
		}
		if f.Bytes < 0 {
			return fmt.Errorf("rights: selection report file %q has negative bytes", f.Path)
		}
		if !overlapSHA.MatchString(f.SHA256) {
			return fmt.Errorf("rights: selection report file %q: sha256 must be 64 lowercase hex digits", f.Path)
		}
		if len(f.PrimaryFor)+len(f.CompanionOf) == 0 {
			return fmt.Errorf("rights: selection report file %q has no role", f.Path)
		}
		for name, ids := range map[string][]string{"primary_for": f.PrimaryFor, "companion_of": f.CompanionOf} {
			if err := sortedUnique(ids); err != nil {
				return fmt.Errorf("rights: selection report file %q %s: %w", f.Path, name, err)
			}
		}
		if strings.Join(f.PrimaryFor, ",") != strings.Join(wantPrimary[f.Path], ",") {
			return fmt.Errorf("rights: selection report file %q primary_for %v, but the assets say %v",
				f.Path, f.PrimaryFor, wantPrimary[f.Path])
		}
		if strings.Join(f.CompanionOf, ",") != strings.Join(wantCompanion[f.Path], ",") {
			return fmt.Errorf("rights: selection report file %q companion_of %v, but the assets say %v",
				f.Path, f.CompanionOf, wantCompanion[f.Path])
		}
		delete(wantPrimary, f.Path)
		delete(wantCompanion, f.Path)
		unique += f.Bytes
		selected += f.Bytes * int64(len(f.PrimaryFor))
		if len(f.PrimaryFor) > 0 {
			primaryFiles++
		}
		if len(f.CompanionOf) > 0 {
			companionFiles++
		}
	}
	for _, m := range []map[string][]string{wantPrimary, wantCompanion} {
		if keys := sortedKeys(m); len(keys) > 0 {
			return fmt.Errorf("rights: selection report omits file %q needed by %v", keys[0], m[keys[0]])
		}
	}
	companionAssets, unknown := 0, 0
	for _, a := range s.Assets {
		if len(a.Companions) > 0 {
			companionAssets++
		}
		if a.CompanionSupport == CompanionsUnknownModel {
			unknown++
		}
	}
	if err := sortedUnique(s.PostIDs); err != nil {
		return fmt.Errorf("rights: selection report post_ids: %w", err)
	}
	if err := validateReportIncludes(s); err != nil {
		return err
	}
	n := s.Counts
	for _, c := range []struct {
		name      string
		got, want int64
	}{
		{"assets_selected", int64(n.AssetsSelected), int64(len(s.Assets))},
		{"posts_selected", int64(n.PostsSelected), int64(len(s.PostIDs))},
		{"unique_files", int64(n.UniqueFiles), int64(len(s.Files))},
		{"primary_files", int64(n.PrimaryFiles), int64(primaryFiles)},
		{"companion_files", int64(n.CompanionFiles), int64(companionFiles)},
		{"companion_assets", int64(n.CompanionAssets), int64(companionAssets)},
		{"unknown_model_assets", int64(n.UnknownModelAssets), int64(unknown)},
		{"unique_file_bytes", n.UniqueFileBytes, unique},
		{"selected_bytes", n.SelectedBytes, selected},
	} {
		if c.got != c.want {
			return fmt.Errorf("rights: selection report counts.%s is %d but the lists give %d", c.name, c.got, c.want)
		}
	}
	return nil
}

// validateReportIncludes ties a v2 report's declared_includes to the rest
// of the report: each anchor is a selected asset at its path, its file is
// in the closure with that length and hash, and the contract is a pinned
// input with that digest.
func validateReportIncludes(s *CISelection) error {
	d := s.DeclaredIncludes
	if d == nil {
		return nil
	}
	if d.Peer == "" || d.Contract == "" || d.ContractSchema != OverlapContractSchema ||
		!overlapSHA.MatchString(d.ContractSHA256) || !overlapSHA.MatchString(d.ManifestSHA256) {
		return fmt.Errorf("rights: selection report declared_includes header is incomplete: %+v", *d)
	}
	// Both bindings must be pinned exactly once: a missing manifest pin
	// would leave manifest_sha256 bound to nothing, and a duplicate would
	// let one matching entry vouch for a mismatching one.
	if err := requireOnePin(s.Inputs, "site/MANIFEST.json", d.ManifestSHA256); err != nil {
		return err
	}
	if err := requireOnePin(s.Inputs, "overlap_contract/"+d.Contract, d.ContractSHA256); err != nil {
		return err
	}
	if len(d.Anchors) == 0 {
		return errors.New("rights: selection report declared_includes has no anchors")
	}
	assets := map[string]CISelectionAsset{}
	for _, a := range s.Assets {
		assets[a.ID] = a
	}
	files := map[string]CISelectionFile{}
	for _, f := range s.Files {
		files[f.Path] = f
	}
	seen := map[string]bool{}
	for _, a := range d.Anchors {
		if seen[a.AssetID] {
			return fmt.Errorf("rights: selection report declares anchor %s twice", a.AssetID)
		}
		seen[a.AssetID] = true
		row, ok := assets[a.AssetID]
		if !ok || row.FilePath != a.FilePath {
			return fmt.Errorf("rights: selection report anchor %s (%s) is not a selected asset at %s", a.AssetID, a.Fixture, a.FilePath)
		}
		f, ok := files[a.FilePath]
		if !ok || f.Bytes != a.Bytes || f.SHA256 != a.SHA256 {
			return fmt.Errorf("rights: selection report anchor %s file %s disagrees with the closure entry", a.AssetID, a.FilePath)
		}
	}
	return nil
}

// requireOnePin requires exactly one input named name, present, with a
// 64-hex SHA-256 equal to want.
func requireOnePin(inputs []CISelectionInput, name, want string) error {
	var found []CISelectionInput
	for _, in := range inputs {
		if in.Name == name {
			found = append(found, in)
		}
	}
	switch {
	case len(found) == 0:
		return fmt.Errorf("rights: selection report declared_includes needs the input %q pinned, and it is missing", name)
	case len(found) > 1:
		return fmt.Errorf("rights: selection report pins the input %q %d times; it must be pinned exactly once", name, len(found))
	}
	in := found[0]
	if !in.Present {
		return fmt.Errorf("rights: selection report input %q is recorded as absent, so declared_includes is bound to nothing", name)
	}
	if !overlapSHA.MatchString(in.SHA256) {
		return fmt.Errorf("rights: selection report input %q has a malformed sha256", name)
	}
	if in.SHA256 != want {
		return fmt.Errorf("rights: selection report input %q digest %s disagrees with declared_includes %s", name, in.SHA256, want)
	}
	return nil
}

// DraftRightsLedger returns the draft ledger for a report: one pending row
// per closure file, in the report's path order, with no review. The same
// report always drafts the same bytes.
func DraftRightsLedger(sel *CISelection, selectionSHA256 string) ([]byte, error) {
	if !overlapSHA.MatchString(selectionSHA256) {
		return nil, errors.New("rights: selection sha256 must be 64 lowercase hex digits")
	}
	l := RightsLedger{
		Schema: RightsLedgerSchema,
		Notice: "DRAFT. Every row is pending. " + RightsStructuralOnly,
		Selection: RightsLedgerSelection{
			SHA256: selectionSHA256, Schema: sel.Schema, Profile: sel.Profile, CoverageDepth: sel.CoverageDepth,
			Files: len(sel.Files), UniqueFileBytes: sel.Counts.UniqueFileBytes,
		},
		Entries: make([]RightsLedgerEntry, 0, len(sel.Files)),
	}
	for _, f := range sel.Files {
		l.Entries = append(l.Entries, RightsLedgerEntry{
			Path: f.Path, Bytes: f.Bytes, SHA256: f.SHA256,
			PrimaryFor: nonNil(f.PrimaryFor), CompanionOf: nonNil(f.CompanionOf),
			Status: RightsPending,
		})
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("rights: encode ledger: %w", err)
	}
	return append(b, '\n'), nil
}

// LoadRightsLedger reads a ledger strictly: unknown schema and unknown
// keys are refused.
func LoadRightsLedger(p string) (*RightsLedger, error) {
	if p == "" {
		return nil, errors.New("rights: ledger path is required")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("rights: read ledger: %w", err)
	}
	return parseRightsLedger(raw)
}

// parseRightsLedger is LoadRightsLedger over bytes already read, so a
// caller that also pins the ledger's digest hashes exactly what it parsed.
func parseRightsLedger(raw []byte) (*RightsLedger, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("rights: parse ledger: %w", err)
	}
	if head.Schema != RightsLedgerSchema {
		return nil, fmt.Errorf("rights: unknown ledger schema %q (this tool reads %q)", head.Schema, RightsLedgerSchema)
	}
	var l RightsLedger
	if err := strictDecode(raw, &l); err != nil {
		return nil, fmt.Errorf("rights: parse ledger: %w", err)
	}
	return &l, nil
}

// CheckRightsLedger compares a ledger to the selection report it claims to
// describe. Any structural disagreement is an error. When the structure
// holds, the result says whether every row is reviewed_allow; that is the
// most it can say (see RightsStructuralOnly).
func CheckRightsLedger(sel *CISelection, selectionSHA256 string, l *RightsLedger) (*RightsCheck, error) {
	b := l.Selection
	if b.SHA256 != selectionSHA256 {
		return nil, fmt.Errorf("rights: ledger is bound to selection %s, not the supplied report %s (stale ledger)",
			b.SHA256, selectionSHA256)
	}
	if b.Schema != sel.Schema || b.Profile != sel.Profile || b.CoverageDepth != sel.CoverageDepth ||
		b.Files != len(sel.Files) || b.UniqueFileBytes != sel.Counts.UniqueFileBytes {
		return nil, fmt.Errorf("rights: ledger selection header %+v disagrees with the report", b)
	}

	files := map[string]CISelectionFile{}
	for _, f := range sel.Files {
		files[f.Path] = f
	}
	seen := map[string]bool{}
	res := &RightsCheck{Schema: RightsCheckSchema, Label: RightsStructuralOnly, SelectionSHA256: selectionSHA256,
		Entries: len(l.Entries), Counts: map[string]int{RightsPending: 0, RightsHold: 0, RightsReviewedAllow: 0},
		Blocking: []RightsBlocking{}, UnknownModelAssetIDs: []string{}, Ineligible: []string{}}
	for i, e := range l.Entries {
		if seen[e.Path] {
			return nil, fmt.Errorf("rights: ledger lists %q twice", e.Path)
		}
		seen[e.Path] = true
		if i > 0 && l.Entries[i-1].Path >= e.Path {
			return nil, fmt.Errorf("rights: ledger entries not sorted by path at %q", e.Path)
		}
		f, ok := files[e.Path]
		if !ok {
			return nil, fmt.Errorf("rights: ledger entry %q is not a file of the selection (extra entry)", e.Path)
		}
		if e.Bytes != f.Bytes || e.SHA256 != f.SHA256 {
			return nil, fmt.Errorf("rights: ledger entry %q pins %d bytes / %s, the report has %d / %s",
				e.Path, e.Bytes, e.SHA256, f.Bytes, f.SHA256)
		}
		if strings.Join(e.PrimaryFor, ",") != strings.Join(f.PrimaryFor, ",") ||
			strings.Join(e.CompanionOf, ",") != strings.Join(f.CompanionOf, ",") {
			return nil, fmt.Errorf("rights: ledger entry %q roles primary_for=%v companion_of=%v, the report has %v / %v",
				e.Path, e.PrimaryFor, e.CompanionOf, f.PrimaryFor, f.CompanionOf)
		}
		switch e.Status {
		case RightsPending, RightsHold:
			res.Blocking = append(res.Blocking, RightsBlocking{Path: e.Path, Status: e.Status})
		case RightsReviewedAllow:
			if err := e.Review.complete(); err != nil {
				return nil, fmt.Errorf("rights: ledger entry %q is reviewed_allow but %w", e.Path, err)
			}
		default:
			return nil, fmt.Errorf("rights: ledger entry %q has unknown status %q (want %s, %s or %s)",
				e.Path, e.Status, RightsPending, RightsHold, RightsReviewedAllow)
		}
		res.Counts[e.Status]++
	}
	var missing []string
	for _, f := range sel.Files {
		if !seen[f.Path] {
			missing = append(missing, f.Path)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("rights: ledger has no entry for %d selection file(s), first %q", len(missing), missing[0])
	}
	for _, a := range sel.Assets {
		if a.CompanionSupport == CompanionsUnknownModel {
			res.UnknownModelAssetIDs = append(res.UnknownModelAssetIDs, a.ID)
		}
	}
	res.UnknownModelAssets = len(res.UnknownModelAssetIDs)

	// An empty ledger is never eligible: a package of nothing has nothing
	// reviewed, and eligibility must not be vacuously true.
	if len(l.Entries) == 0 {
		res.Ineligible = append(res.Ineligible, RightsReasonNoEntries)
	}
	if len(res.Blocking) > 0 {
		res.Ineligible = append(res.Ineligible, fmt.Sprintf("%s: %d pending, %d hold of %d entries",
			RightsReasonIncompleteReview, res.Counts[RightsPending], res.Counts[RightsHold], len(l.Entries)))
	}
	// Reviewing every listed row cannot cover a file the report could not
	// list. The draft stays usable as a worklist; eligibility does not.
	if res.UnknownModelAssets > 0 {
		res.Ineligible = append(res.Ineligible, fmt.Sprintf(
			"%s: %d selected model asset(s) use a format whose companions cannot be read (%s), "+
				"so the listed files may not be every file the package needs",
			RightsReasonIncompleteClose, res.UnknownModelAssets, CompanionsUnknownModel))
	}
	res.Eligible = len(res.Ineligible) == 0
	return res, nil
}

// complete reports the first missing or inconsistent evidence field.
func (r *RightsReview) complete() error {
	if r == nil {
		return errors.New("has no review")
	}
	if !allNonBlank(r.Sources) {
		return errors.New("review.sources is empty or has a blank entry")
	}
	if strings.TrimSpace(r.License) == "" {
		return errors.New("review.license is empty")
	}
	if !allNonBlank(r.LicenseEvidence) {
		return errors.New("review.license_evidence is empty or has a blank entry")
	}
	if r.Redistribution != RedistributionAllowed {
		return fmt.Errorf("review.redistribution is %q; a reviewed_allow row must record %q", r.Redistribution, RedistributionAllowed)
	}
	if r.Attribution == nil || r.Attribution.Required == nil {
		return errors.New("review.attribution.required is not stated")
	}
	if *r.Attribution.Required && strings.TrimSpace(r.Attribution.Text) == "" && !allNonBlank(r.Attribution.Notices) {
		return errors.New("review.attribution is required but has no text or notice record")
	}
	if len(r.Attribution.Notices) > 0 && !allNonBlank(r.Attribution.Notices) {
		return errors.New("review.attribution.notices has a blank entry")
	}
	if strings.TrimSpace(r.Reviewer) == "" {
		return errors.New("review.reviewer is empty")
	}
	if _, err := time.Parse("2006-01-02", r.ReviewedOn); err != nil {
		return fmt.Errorf("review.reviewed_on %q is not a YYYY-MM-DD date", r.ReviewedOn)
	}
	return nil
}

func allNonBlank(ss []string) bool {
	if len(ss) == 0 {
		return false
	}
	for _, s := range ss {
		if strings.TrimSpace(s) == "" {
			return false
		}
	}
	return true
}

func sortedUnique(ss []string) error {
	for i, s := range ss {
		if s == "" {
			return errors.New("empty id")
		}
		if i > 0 && ss[i-1] >= s {
			return fmt.Errorf("not strictly sorted at %q (duplicate or out of order)", s)
		}
	}
	return nil
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// strictDecode decodes one JSON value, refusing unknown keys and
// trailing data.
func strictDecode(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}
