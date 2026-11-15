// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Cross-peer overlap fixtures (#1514).
//
// The public site_a and the new public site_b must deliberately share at
// least two different byte-identical assets, so a paired-stack test can
// check that one byte stream on two federation peers keeps two separate
// peer-local identities. seed/contracts/peer-overlap.v1.json declares
// them; this file checks a contract and, given explicit peer roots, that
// each peer really carries exactly those bytes under exactly that row.
//
// It is read-only and offline: no database, network or writes, and no
// default peer root. In particular nothing here knows where the current
// private site_b lives, and nothing may: the replacement peer is a new
// corpus, and its root is always passed in.
//
// What it does NOT establish: rights clearance (a license label is not a
// redistribution decision), that the coverage selector includes these
// rows (it does not today), or that a seeded database keeps them apart.
// Those are later gates.

package seed

import (
	"bytes"
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
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// OverlapContractSchema is the only contract layout this validator reads.
const OverlapContractSchema = "artist-alley.peer-overlap/v1"

// OverlapReportSchema versions the validator's own report.
const OverlapReportSchema = "artist-alley.peer-overlap-check/v1"

// overlapMinFixtures is the owner's floor: at least two distinct pairs,
// so a validator that collapses everything onto one hash cannot pass.
const overlapMinFixtures = 2

// OverlapContract is the decoded contract. Unknown keys are refused.
type OverlapContract struct {
	Schema    string            `json:"schema"`
	Why       string            `json:"_why"`
	Status    string            `json:"status"`
	Issue     string            `json:"issue"`
	Peers     []string          `json:"peers"`
	PeerNotes map[string]string `json:"peer_notes"`
	Fixtures  []OverlapFixture  `json:"fixtures"`
}

// OverlapFixture is one deliberately shared byte stream.
type OverlapFixture struct {
	Name   string                         `json:"name"`
	SHA256 string                         `json:"sha256"`
	Bytes  int64                          `json:"bytes"`
	Assets map[string]OverlapFixtureAsset `json:"assets"`
	Source OverlapSource                  `json:"source"`
	Rights string                         `json:"rights_status"`
}

// OverlapFixtureAsset is the row that carries the bytes on one peer.
// FilePath is optional: a peer may store the bytes under its own name,
// and the validator then takes the path from that peer's manifest row.
type OverlapFixtureAsset struct {
	AssetID      string             `json:"asset_id"`
	FilePath     string             `json:"file_path,omitempty"`
	IDDerivation *OverlapDerivation `json:"id_derivation,omitempty"`
}

// OverlapDerivation records how a new peer-local ID was minted, so the
// validator can recompute it instead of trusting it.
type OverlapDerivation struct {
	Method    string `json:"method"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// OverlapSource is where the bytes come from. Every anchor row on every
// checked peer must carry the same asset page (metadata.fetched_from) and
// archive url, member and hash (metadata.source_archive).
type OverlapSource struct {
	AssetPage       string   `json:"asset_page"`
	ArchiveURL      string   `json:"archive_url"`
	ArchiveMember   string   `json:"archive_member"`
	License         string   `json:"license"`
	ManifestLicense string   `json:"manifest_license"`
	LicenseEvidence []string `json:"license_evidence"`
	Attribution     string   `json:"attribution"`
}

// OverlapOptions names the inputs of a check. Peers maps a contract peer
// name to its site root; there is no default for any peer.
type OverlapOptions struct {
	ContractPath string
	Peers        map[string]string
	// AllowPartial accepts a subset of the contract's peers. The pair
	// checks are then NOT run and the report says so.
	AllowPartial bool
}

// OverlapReport is the validator's result.
type OverlapReport struct {
	Schema         string              `json:"schema"`
	ContractSchema string              `json:"contract_schema"`
	ContractSHA256 string              `json:"contract_sha256"`
	PairsChecked   bool                `json:"pairs_checked"`
	Fixtures       []string            `json:"fixtures"`
	Peers          []OverlapPeerResult `json:"peers"`
}

// OverlapPeerResult is what one peer was checked against.
type OverlapPeerResult struct {
	Peer           string          `json:"peer"`
	ManifestSHA256 string          `json:"manifest_sha256"`
	Rows           int             `json:"rows"`
	LengthMatched  int             `json:"length_matched_rows"`
	FilesHashed    int             `json:"files_hashed"`
	Anchors        []OverlapAnchor `json:"anchors"`
}

// OverlapAnchor is a fixture as found on one peer.
type OverlapAnchor struct {
	Fixture  string `json:"fixture"`
	AssetID  string `json:"asset_id"`
	FilePath string `json:"file_path"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
}

// overlapAfterScan, when set, runs after a peer is scanned and before its
// manifest and anchors are re-read. Tests use it to change files.
var overlapAfterScan func(peer string)

var (
	overlapPeerName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	overlapSHA      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	overlapFixture  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
)

// LoadOverlapContract reads and validates a contract. It checks only the
// contract itself; it opens no peer.
func LoadOverlapContract(p string) (*OverlapContract, string, error) {
	if p == "" {
		return nil, "", errors.New("overlap: contract path is required")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, "", fmt.Errorf("overlap: read contract: %w", err)
	}
	c, err := parseOverlapContract(raw)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return c, hex.EncodeToString(sum[:]), nil
}

// parseOverlapContract decodes and validates contract bytes. Callers that
// must pin the exact bytes they parsed (the declared-include path) read
// the file once and hand the bytes here.
func parseOverlapContract(raw []byte) (*OverlapContract, error) {
	// The schema is checked first and on its own, so a future layout is
	// reported as an unknown schema rather than as an unknown field.
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("overlap: parse contract: %w", err)
	}
	if head.Schema != OverlapContractSchema {
		return nil, fmt.Errorf("overlap: unknown contract schema %q (this validator reads %q)",
			head.Schema, OverlapContractSchema)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c OverlapContract
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("overlap: parse contract: %w", err)
	}
	if dec.More() {
		return nil, errors.New("overlap: parse contract: trailing data after the contract object")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *OverlapContract) validate() error {
	if len(c.Peers) < 2 {
		return fmt.Errorf("overlap: contract names %d peer(s); a cross-peer fixture needs at least 2", len(c.Peers))
	}
	peers := map[string]bool{}
	for _, p := range c.Peers {
		if !overlapPeerName.MatchString(p) {
			return fmt.Errorf("overlap: bad peer name %q", p)
		}
		if peers[p] {
			return fmt.Errorf("overlap: duplicate peer %q", p)
		}
		peers[p] = true
	}
	if len(c.Fixtures) < overlapMinFixtures {
		return fmt.Errorf("overlap: contract declares %d fixture(s); at least %d distinct pairs are required",
			len(c.Fixtures), overlapMinFixtures)
	}
	names := map[string]bool{}
	hashes := map[string]string{}
	ids := map[string]string{} // asset id -> "fixture/peer"
	for _, f := range c.Fixtures {
		if !overlapFixture.MatchString(f.Name) {
			return fmt.Errorf("overlap: bad fixture name %q", f.Name)
		}
		if names[f.Name] {
			return fmt.Errorf("overlap: duplicate fixture name %q", f.Name)
		}
		names[f.Name] = true
		if !overlapSHA.MatchString(f.SHA256) {
			return fmt.Errorf("overlap: fixture %q: sha256 must be 64 lowercase hex digits", f.Name)
		}
		if other, dup := hashes[f.SHA256]; dup {
			return fmt.Errorf("overlap: fixtures %q and %q declare the same sha256; the anchors must be different bytes",
				other, f.Name)
		}
		hashes[f.SHA256] = f.Name
		if f.Bytes <= 0 {
			return fmt.Errorf("overlap: fixture %q: bytes must be positive", f.Name)
		}
		if f.Rights == "" {
			return fmt.Errorf("overlap: fixture %q: rights_status is required", f.Name)
		}
		if err := f.Source.validate(); err != nil {
			return fmt.Errorf("overlap: fixture %q: %w", f.Name, err)
		}
		if len(f.Assets) != len(c.Peers) {
			return fmt.Errorf("overlap: fixture %q names %d peer(s), the contract has %d",
				f.Name, len(f.Assets), len(c.Peers))
		}
		for _, p := range c.Peers {
			a, ok := f.Assets[p]
			if !ok {
				return fmt.Errorf("overlap: fixture %q has no asset on peer %q", f.Name, p)
			}
			if err := a.validate(); err != nil {
				return fmt.Errorf("overlap: fixture %q peer %q: %w", f.Name, p, err)
			}
			// One asset id may carry one fixture on one peer. The same id on
			// two peers is exactly the collapse the fixture exists to catch.
			if other, dup := ids[a.AssetID]; dup {
				return fmt.Errorf("overlap: asset id %s is used by both %s and %s/%s; peer-local ids must differ",
					a.AssetID, other, f.Name, p)
			}
			ids[a.AssetID] = f.Name + "/" + p
		}
	}
	return nil
}

func (a OverlapFixtureAsset) validate() error {
	id, err := uuid.Parse(a.AssetID)
	if err != nil || id.String() != a.AssetID {
		return fmt.Errorf("asset_id %q is not a canonical lowercase UUID", a.AssetID)
	}
	if a.FilePath != "" {
		if err := checkRelPath(a.FilePath); err != nil {
			return err
		}
	}
	if d := a.IDDerivation; d != nil {
		if d.Method != "uuidv5" || d.Namespace != "url" || d.Name == "" {
			return fmt.Errorf("unsupported id_derivation %+v (only uuidv5 over the url namespace)", *d)
		}
		if want := uuid.NewSHA1(uuid.NameSpaceURL, []byte(d.Name)).String(); want != a.AssetID {
			return fmt.Errorf("asset_id %s does not match its derivation %q (want %s)", a.AssetID, d.Name, want)
		}
	}
	return nil
}

func (s OverlapSource) validate() error {
	for name, v := range map[string]string{"asset_page": s.AssetPage, "archive_url": s.ArchiveURL} {
		if !strings.HasPrefix(v, "https://") || strings.ContainsAny(v, " \t\n") {
			return fmt.Errorf("source.%s must be an https URL, got %q", name, v)
		}
	}
	if err := checkRelPath(s.ArchiveMember); err != nil {
		return fmt.Errorf("source.archive_member: %w", err)
	}
	if s.License == "" || s.ManifestLicense == "" || s.Attribution == "" || len(s.LicenseEvidence) == 0 {
		return errors.New("source needs license, manifest_license, attribution and license_evidence")
	}
	return nil
}

// ValidatePeerOverlap checks the contract and every named peer. With all
// of the contract's peers it also runs the pair checks.
func ValidatePeerOverlap(opts OverlapOptions) (*OverlapReport, error) {
	c, sum, err := LoadOverlapContract(opts.ContractPath)
	if err != nil {
		return nil, err
	}
	if len(opts.Peers) == 0 {
		return nil, errors.New("overlap: no peer roots given; every peer root must be passed explicitly")
	}
	known := map[string]bool{}
	for _, p := range c.Peers {
		known[p] = true
	}
	for p := range opts.Peers {
		if !known[p] {
			return nil, fmt.Errorf("overlap: peer %q is not in the contract (peers: %s)", p, strings.Join(c.Peers, ", "))
		}
	}
	complete := len(opts.Peers) == len(c.Peers)
	if !complete && !opts.AllowPartial {
		return nil, fmt.Errorf("overlap: %d of %d peers given; the pair checks need all of them (or allow a partial check)",
			len(opts.Peers), len(c.Peers))
	}

	// Two names for one directory would let one peer's rows stand in for
	// the other's.
	roots := map[string]string{}
	var infos []struct {
		peer string
		fi   fs.FileInfo
	}
	for _, p := range c.Peers {
		r, ok := opts.Peers[p]
		if !ok {
			continue
		}
		if r == "" {
			return nil, fmt.Errorf("overlap: peer %q: empty root", p)
		}
		resolved, err := filepath.EvalSymlinks(r)
		if err != nil {
			return nil, fmt.Errorf("overlap: peer %q root: %w", p, err)
		}
		fi, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("overlap: peer %q root: %w", p, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("overlap: peer %q root is not a directory", p)
		}
		for _, o := range infos {
			if os.SameFile(o.fi, fi) {
				return nil, fmt.Errorf("overlap: peers %q and %q are the same directory", o.peer, p)
			}
		}
		infos = append(infos, struct {
			peer string
			fi   fs.FileInfo
		}{p, fi})
		roots[p] = resolved
	}

	rep := &OverlapReport{Schema: OverlapReportSchema, ContractSchema: c.Schema, ContractSHA256: sum,
		PairsChecked: complete}
	for _, f := range c.Fixtures {
		rep.Fixtures = append(rep.Fixtures, f.Name)
	}
	for _, p := range c.Peers {
		root, ok := roots[p]
		if !ok {
			continue
		}
		res, err := checkOverlapPeer(c, p, root)
		if err != nil {
			return nil, err
		}
		rep.Peers = append(rep.Peers, *res)
	}

	if complete {
		// Every peer was already held to the contract's hash and length;
		// state the pair property directly anyway, so it does not rest on
		// that implication.
		for i, f := range c.Fixtures {
			seen := map[string]string{}
			for _, pr := range rep.Peers {
				a := pr.Anchors[i]
				if a.SHA256 != f.SHA256 || a.Bytes != f.Bytes {
					return nil, fmt.Errorf("overlap: fixture %q: peer %q bytes differ from the pair", f.Name, pr.Peer)
				}
				if other, dup := seen[a.AssetID]; dup {
					return nil, fmt.Errorf("overlap: fixture %q: peers %q and %q use the same asset id", f.Name, other, pr.Peer)
				}
				seen[a.AssetID] = pr.Peer
			}
		}
	}
	return rep, nil
}

// overlapRow is the narrow manifest row this check reads. Metadata stays
// raw: only anchor rows' metadata is decoded, so an odd metadata value on
// an unrelated row cannot fail the scan.
type overlapRow struct {
	ID            string          `json:"id"`
	FilePath      string          `json:"file_path"`
	FileSizeBytes int64           `json:"file_size_bytes"`
	License       string          `json:"license"`
	Attribution   string          `json:"attribution"`
	Metadata      json.RawMessage `json:"metadata"`
}

// checkOverlapPeer holds one peer to the contract:
//
//   - each fixture's asset id is exactly one manifest row, whose path is
//     safe, has no symlink in any component, matches the contract's path
//     when the contract names one, and whose declared size, measured
//     length and SHA-256 equal the fixture's;
//   - the row's license label and attribution equal the contract's, and
//     its metadata MUST carry fetched_from and source_archive equal to the
//     contract source (checkOverlapProvenance);
//   - NO OTHER manifest row's primary file has those bytes. Every row's
//     file is measured, and every file whose length equals an anchor's is
//     hashed, so the scan is complete over primary files without hashing
//     the whole peer. A row whose file cannot be measured fails the
//     check, because uniqueness cannot then be shown.
//
// Then the manifest and the anchors are read again and must not have moved.
func checkOverlapPeer(c *OverlapContract, peer, root string) (*OverlapPeerResult, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("overlap: peer %q root: %w", peer, err)
	}
	defer r.Close()

	manifestSum, rows, err := readOverlapManifest(r, peer)
	if err != nil {
		return nil, err
	}
	byID := map[string]int{}
	for i, row := range rows {
		if row.ID == "" {
			return nil, fmt.Errorf("overlap: peer %q: manifest row %d has no id", peer, i)
		}
		if _, dup := byID[row.ID]; dup {
			return nil, fmt.Errorf("overlap: peer %q: duplicate manifest id %s", peer, row.ID)
		}
		byID[row.ID] = i
	}

	res := &OverlapPeerResult{Peer: peer, ManifestSHA256: manifestSum, Rows: len(rows)}
	anchorRow := map[int]bool{}
	anchorBySHA := map[string]string{} // sha -> fixture
	lengths := map[int64]bool{}
	for _, f := range c.Fixtures {
		want := f.Assets[peer]
		i, ok := byID[want.AssetID]
		if !ok {
			return nil, fmt.Errorf("overlap: peer %q: fixture %q: no manifest row with id %s", peer, f.Name, want.AssetID)
		}
		row := rows[i]
		a, err := checkOverlapAnchor(r, peer, f, want, row)
		if err != nil {
			return nil, err
		}
		anchorRow[i] = true
		anchorBySHA[f.SHA256] = f.Name
		lengths[f.Bytes] = true
		res.Anchors = append(res.Anchors, a)
	}

	// Uniqueness over every other row's primary file.
	hashedPaths := map[string]string{}
	for i, row := range rows {
		if anchorRow[i] {
			continue
		}
		if err := checkRelPath(row.FilePath); err != nil {
			return nil, fmt.Errorf("overlap: peer %q: row %s: %w; cannot show the anchors are unique", peer, row.ID, err)
		}
		st, err := r.Stat(filepath.FromSlash(row.FilePath))
		if err != nil {
			return nil, fmt.Errorf("overlap: peer %q: row %s: file cannot be measured, so the anchors cannot be shown unique: %w",
				peer, row.ID, err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("overlap: peer %q: row %s: file is not a regular file", peer, row.ID)
		}
		if !lengths[st.Size()] {
			continue
		}
		res.LengthMatched++
		sum, done := hashedPaths[row.FilePath]
		if !done {
			if sum, _, err = hashOverlapFile(r, row.FilePath); err != nil {
				return nil, fmt.Errorf("overlap: peer %q: row %s: %w", peer, row.ID, err)
			}
			hashedPaths[row.FilePath] = sum
			res.FilesHashed++
		}
		if name, hit := anchorBySHA[sum]; hit {
			return nil, fmt.Errorf("overlap: peer %q: row %s (%s) has the same bytes as fixture %q; "+
				"each declared hash must be carried by exactly one row per peer", peer, row.ID, row.FilePath, name)
		}
	}

	if overlapAfterScan != nil {
		overlapAfterScan(peer)
	}

	// Nothing the result describes may have moved during the scan.
	again, _, err := readOverlapManifest(r, peer)
	if err != nil {
		return nil, err
	}
	if again != manifestSum {
		return nil, fmt.Errorf("overlap: peer %q: MANIFEST.json changed during the check", peer)
	}
	for _, a := range res.Anchors {
		sum, n, err := hashOverlapFile(r, a.FilePath)
		if err != nil || sum != a.SHA256 || n != a.Bytes {
			return nil, fmt.Errorf("overlap: peer %q: fixture %q file changed during the check", peer, a.Fixture)
		}
	}
	return res, nil
}

func checkOverlapAnchor(r *os.Root, peer string, f OverlapFixture, want OverlapFixtureAsset, row overlapRow) (OverlapAnchor, error) {
	where := fmt.Sprintf("overlap: peer %q: fixture %q (row %s)", peer, f.Name, row.ID)
	if err := checkRelPath(row.FilePath); err != nil {
		return OverlapAnchor{}, fmt.Errorf("%s: %w", where, err)
	}
	if want.FilePath != "" && row.FilePath != want.FilePath {
		return OverlapAnchor{}, fmt.Errorf("%s: manifest path %q, contract path %q", where, row.FilePath, want.FilePath)
	}
	// No symlink anywhere on the anchor's path: the fixture is about these
	// exact bytes under this exact name, not whatever a link points at.
	parts := strings.Split(row.FilePath, "/")
	for i := range parts {
		prefix := filepath.FromSlash(path.Join(parts[:i+1]...))
		st, err := r.Lstat(prefix)
		if err != nil {
			return OverlapAnchor{}, fmt.Errorf("%s: file is missing or not readable: %w", where, err)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return OverlapAnchor{}, fmt.Errorf("%s: %q is a symlink; an anchor must be a real file", where, path.Join(parts[:i+1]...))
		}
	}
	if row.FileSizeBytes != f.Bytes {
		return OverlapAnchor{}, fmt.Errorf("%s: manifest declares %d bytes, the fixture is %d", where, row.FileSizeBytes, f.Bytes)
	}
	sum, n, err := hashOverlapFile(r, row.FilePath)
	if err != nil {
		return OverlapAnchor{}, fmt.Errorf("%s: %w", where, err)
	}
	if n != f.Bytes || sum != f.SHA256 {
		return OverlapAnchor{}, fmt.Errorf("%s: file is %d bytes with sha256 %s; the fixture is %d bytes with sha256 %s",
			where, n, sum, f.Bytes, f.SHA256)
	}
	if row.License != f.Source.ManifestLicense || row.Attribution != f.Source.Attribution {
		return OverlapAnchor{}, fmt.Errorf("%s: manifest license/attribution %q/%q, contract %q/%q",
			where, row.License, row.Attribution, f.Source.ManifestLicense, f.Source.Attribution)
	}
	if err := checkOverlapProvenance(row.Metadata, f); err != nil {
		return OverlapAnchor{}, fmt.Errorf("%s: %w", where, err)
	}
	return OverlapAnchor{Fixture: f.Name, AssetID: row.ID, FilePath: row.FilePath, Bytes: n, SHA256: sum}, nil
}

// checkOverlapProvenance requires an anchor row to carry its source, and
// that source to be the contract's: metadata must be a JSON object whose
// fetched_from equals source.asset_page, and whose source_archive is an
// object with url, member and sha256 equal to the contract's archive URL,
// member and fixture hash. Absent, null, mistyped and different values
// are all refused. An anchor row with no provenance would let a peer pass
// with bytes nobody can trace.
//
// This is consistency between the row and the contract. It is not a
// rights clearance.
func checkOverlapProvenance(raw json.RawMessage, f OverlapFixture) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return errors.New("metadata is missing; an anchor row must carry fetched_from and source_archive")
	}
	if trimmed[0] != '{' {
		return errors.New("metadata is not a JSON object")
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &meta); err != nil {
		return fmt.Errorf("metadata: %w", err)
	}

	fetched, err := overlapMetaString(meta, "fetched_from")
	if err != nil {
		return err
	}
	if fetched != f.Source.AssetPage {
		return fmt.Errorf("metadata.fetched_from %q disagrees with the contract asset_page %q", fetched, f.Source.AssetPage)
	}

	sa, ok := meta["source_archive"]
	sa = bytes.TrimSpace(sa)
	if !ok || len(sa) == 0 || string(sa) == "null" {
		return errors.New("metadata.source_archive is missing")
	}
	if sa[0] != '{' {
		return errors.New("metadata.source_archive is not a JSON object")
	}
	var archive map[string]json.RawMessage
	if err := json.Unmarshal(sa, &archive); err != nil {
		return fmt.Errorf("metadata.source_archive: %w", err)
	}
	for _, want := range []struct{ key, value string }{
		{"url", f.Source.ArchiveURL},
		{"member", f.Source.ArchiveMember},
		{"sha256", f.SHA256},
	} {
		got, err := overlapMetaString(archive, want.key)
		if err != nil {
			return fmt.Errorf("metadata.source_archive: %w", err)
		}
		if got != want.value {
			return fmt.Errorf("metadata.source_archive.%s %q disagrees with the contract %q", want.key, got, want.value)
		}
	}
	return nil
}

// overlapMetaString reads a required, nonempty string field.
func overlapMetaString(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return "", fmt.Errorf("%s is missing", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("%s is not a string", key)
	}
	if s == "" {
		return "", fmt.Errorf("%s is empty", key)
	}
	return s, nil
}

// readOverlapManifest reads MANIFEST.json, which must be a real file at
// the peer root, and returns its digest and rows.
func readOverlapManifest(r *os.Root, peer string) (string, []overlapRow, error) {
	st, err := r.Lstat("MANIFEST.json")
	if err != nil {
		return "", nil, fmt.Errorf("overlap: peer %q: MANIFEST.json: %w", peer, err)
	}
	if !st.Mode().IsRegular() {
		return "", nil, fmt.Errorf("overlap: peer %q: MANIFEST.json is not a regular file", peer)
	}
	raw, err := r.ReadFile("MANIFEST.json")
	if err != nil {
		return "", nil, fmt.Errorf("overlap: peer %q: MANIFEST.json: %w", peer, err)
	}
	var rows []overlapRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return "", nil, fmt.Errorf("overlap: peer %q: parse MANIFEST.json: %w", peer, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), rows, nil
}

// hashOverlapFile hashes a file through the peer's os.Root, so no symlink
// can lead the read outside the peer.
func hashOverlapFile(r *os.Root, rel string) (string, int64, error) {
	f, err := r.Open(filepath.FromSlash(rel))
	if err != nil {
		return "", 0, fmt.Errorf("open %s: %w", rel, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat %s: %w", rel, err)
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", rel)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
