// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the cross-peer overlap contract and validator (#1514). All
// peers here are synthetic public peers built in temp directories; no
// test reads a real corpus.

package seed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type ovRow struct {
	ID            string `json:"id"`
	FilePath      string `json:"file_path"`
	FileSizeBytes int64  `json:"file_size_bytes"`
	License       string `json:"license,omitempty"`
	Attribution   string `json:"attribution,omitempty"`
	Metadata      any    `json:"metadata,omitempty"`
}

type ovFix struct {
	t        *testing.T
	dir      string // holds the contract
	contract OverlapContract
	roots    map[string]string
	rows     map[string][]ovRow
	// rawContract, when set, is written instead of contract.
	rawContract string
}

const ovLicense, ovAttribution = "CC0 1.0", "Kenney (kenney.nl)"

// ovBytes is fixture i's content. All anchors have the SAME length, so
// the length pre-filter alone can never tell them apart: only the hash can.
func ovBytes(i int) string { return fmt.Sprintf("anchor-%02d:", i) + strings.Repeat("x", 54) }

func ovPage(i int) string    { return fmt.Sprintf("https://example.invalid/assets/pack%d", i) }
func ovArchive(i int) string { return fmt.Sprintf("https://example.invalid/pack%d.zip", i) }
func ovMember(i int) string  { return fmt.Sprintf("Parts/anchor%d.png", i) }

// ovMeta is a FRESH copy of fixture i's correct anchor metadata, so a test
// that edits one peer's row cannot reach the other peer's through a shared
// map.
func ovMeta(i int) map[string]any {
	return map[string]any{
		"fetched_from": ovPage(i),
		"source_archive": map[string]any{
			"url": ovArchive(i), "member": ovMember(i), "sha256": ovSHA(ovBytes(i)),
		},
	}
}

func ovSHA(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// newOvFix builds n fixtures over two synthetic public peers. site_a
// stores each anchor under one name and site_b under another; each peer
// also holds a same-length file with other bytes and an unrelated file.
func newOvFix(t *testing.T, n int) *ovFix {
	t.Helper()
	f := &ovFix{
		t:     t,
		dir:   t.TempDir(),
		roots: map[string]string{"site_a": t.TempDir(), "site_b": t.TempDir()},
		rows:  map[string][]ovRow{},
		contract: OverlapContract{
			Schema: OverlapContractSchema, Why: "test", Status: "proposed",
			Issue: "https://example.invalid/1514", Peers: []string{"site_a", "site_b"},
		},
	}
	for i := 1; i <= n; i++ {
		content := ovBytes(i)
		name := fmt.Sprintf("overlap-image-%d", i)
		archive, member := ovArchive(i), ovMember(i)
		aPath := fmt.Sprintf("images/pack%d/anchor%d.png", i, i)
		bPath := fmt.Sprintf("shared/b-side-%d.png", i)
		aID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("test:site-a:%d", i))).String()
		deriv := fmt.Sprintf("test:public-site-b:overlap-image-%d", i)
		bID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(deriv)).String()
		f.put("site_a", aPath, content)
		f.put("site_b", bPath, content)
		f.rows["site_a"] = append(f.rows["site_a"], ovRow{ID: aID, FilePath: aPath, FileSizeBytes: int64(len(content)),
			License: ovLicense, Attribution: ovAttribution, Metadata: ovMeta(i)})
		f.rows["site_b"] = append(f.rows["site_b"], ovRow{ID: bID, FilePath: bPath, FileSizeBytes: int64(len(content)),
			License: ovLicense, Attribution: ovAttribution, Metadata: ovMeta(i)})
		f.contract.Fixtures = append(f.contract.Fixtures, OverlapFixture{
			Name: name, SHA256: ovSHA(content), Bytes: int64(len(content)), Rights: "evidence recorded; not release-cleared",
			Assets: map[string]OverlapFixtureAsset{
				"site_a": {AssetID: aID, FilePath: aPath},
				"site_b": {AssetID: bID, IDDerivation: &OverlapDerivation{Method: "uuidv5", Namespace: "url", Name: deriv}},
			},
			Source: OverlapSource{
				AssetPage: ovPage(i), ArchiveURL: archive,
				ArchiveMember: member, License: "CC0-1.0", ManifestLicense: ovLicense,
				LicenseEvidence: []string{"https://example.invalid/license"}, Attribution: ovAttribution,
			},
		})
	}
	for _, peer := range []string{"site_a", "site_b"} {
		other := strings.Repeat("y", len(ovBytes(1))) // same length, other bytes: hashed, accepted
		f.put(peer, "other/same-length.png", other)
		f.put(peer, "other/notes.txt", "unrelated")
		f.rows[peer] = append(f.rows[peer],
			ovRow{ID: peer + "-same-length", FilePath: "other/same-length.png", FileSizeBytes: int64(len(other))},
			ovRow{ID: peer + "-notes", FilePath: "other/notes.txt", FileSizeBytes: 9})
	}
	f.write()
	return f
}

func (f *ovFix) put(peer, rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.roots[peer], filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	mustWrite(f.t, p, content)
}

func (f *ovFix) contractPath() string { return filepath.Join(f.dir, "peer-overlap.v1.json") }

func (f *ovFix) write() {
	f.t.Helper()
	raw := f.rawContract
	if raw == "" {
		b, err := json.Marshal(f.contract)
		if err != nil {
			f.t.Fatal(err)
		}
		raw = string(b)
	}
	mustWrite(f.t, f.contractPath(), raw)
	for peer, rows := range f.rows {
		b, err := json.Marshal(rows)
		if err != nil {
			f.t.Fatal(err)
		}
		mustWrite(f.t, filepath.Join(f.roots[peer], "MANIFEST.json"), string(b))
	}
}

func (f *ovFix) run() (*OverlapReport, error) {
	return ValidatePeerOverlap(OverlapOptions{ContractPath: f.contractPath(), Peers: f.roots})
}

func TestPeerOverlap_TwoDistinctPairsPass(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			f := newOvFix(t, n)
			rep, err := f.run()
			if err != nil {
				t.Fatal(err)
			}
			if !rep.PairsChecked || len(rep.Peers) != 2 || len(rep.Fixtures) != n {
				t.Fatalf("report = %+v", rep)
			}
			a, b := rep.Peers[0], rep.Peers[1]
			if a.Peer != "site_a" || b.Peer != "site_b" {
				t.Fatalf("peer order %q %q", a.Peer, b.Peer)
			}
			hashes := map[string]bool{}
			for i := 0; i < n; i++ {
				x, y := a.Anchors[i], b.Anchors[i]
				if x.SHA256 != y.SHA256 || x.SHA256 != ovSHA(ovBytes(i+1)) {
					t.Errorf("fixture %d: hashes %s / %s", i, x.SHA256, y.SHA256)
				}
				if x.AssetID == y.AssetID {
					t.Errorf("fixture %d: one asset id on both peers", i)
				}
				if x.FilePath == y.FilePath {
					t.Errorf("fixture %d: this fixture stores different filenames per peer", i)
				}
				hashes[x.SHA256] = true
			}
			if len(hashes) != n {
				t.Errorf("anchors share a hash: %v", hashes)
			}
			// The same-length decoy on each peer was hashed, not skipped.
			for _, p := range rep.Peers {
				if p.LengthMatched != 1 || p.FilesHashed != 1 || p.Rows != n+2 {
					t.Errorf("%s: rows=%d length_matched=%d hashed=%d", p.Peer, p.Rows, p.LengthMatched, p.FilesHashed)
				}
			}
		})
	}
}

func TestPeerOverlap_SameFilenamesOnBothPeersPass(t *testing.T) {
	f := newOvFix(t, 2)
	for i := range f.contract.Fixtures {
		aPath := f.rows["site_a"][i].FilePath
		f.put("site_b", aPath, ovBytes(i+1))
		f.rows["site_b"][i].FilePath = aPath
		fx := f.contract.Fixtures[i]
		b := fx.Assets["site_b"]
		b.FilePath = aPath
		fx.Assets["site_b"] = b
	}
	f.write()
	rep, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Peers[0].Anchors[0].FilePath != rep.Peers[1].Anchors[0].FilePath {
		t.Errorf("filenames differ")
	}
}

func TestPeerOverlap_Partial(t *testing.T) {
	f := newOvFix(t, 2)
	only := map[string]string{"site_a": f.roots["site_a"]}
	if _, err := ValidatePeerOverlap(OverlapOptions{ContractPath: f.contractPath(), Peers: only}); err == nil ||
		!strings.Contains(err.Error(), "pair checks need all") {
		t.Errorf("one peer without AllowPartial: err = %v", err)
	}
	rep, err := ValidatePeerOverlap(OverlapOptions{ContractPath: f.contractPath(), Peers: only, AllowPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.PairsChecked || len(rep.Peers) != 1 {
		t.Errorf("partial report claims pairs checked: %+v", rep)
	}
}

func TestPeerOverlap_ReadsOnly(t *testing.T) {
	f := newOvFix(t, 2)
	before := map[string][]string{}
	for p, r := range f.roots {
		before[p] = treeSnapshot(t, r)
	}
	contractDir := treeSnapshot(t, f.dir)
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	for p, r := range f.roots {
		if strings.Join(treeSnapshot(t, r), "\n") != strings.Join(before[p], "\n") {
			t.Errorf("%s changed", p)
		}
	}
	if strings.Join(treeSnapshot(t, f.dir), "\n") != strings.Join(contractDir, "\n") {
		t.Errorf("contract directory changed")
	}
}

func TestPeerOverlap_Refusals(t *testing.T) {
	type mut func(f *ovFix)
	fx := func(f *ovFix, i int) *OverlapFixture { return &f.contract.Fixtures[i] }
	setAsset := func(f *ovFix, i int, peer string, edit func(*OverlapFixtureAsset)) {
		a := fx(f, i).Assets[peer]
		edit(&a)
		fx(f, i).Assets[peer] = a
	}
	symlink := func(f *ovFix, target, link string) {
		if err := os.Symlink(target, link); err != nil {
			f.t.Fatal(err)
		}
	}
	cases := []struct {
		name    string
		n       int
		mutate  mut
		opts    func(f *ovFix) OverlapOptions
		wantErr string
	}{
		// --- the contract --------------------------------------------------
		{name: "N=0", n: 0, wantErr: "at least 2 distinct pairs"},
		{name: "N=1", n: 1, wantErr: "at least 2 distinct pairs"},
		{name: "unknown schema", n: 2, mutate: func(f *ovFix) { f.contract.Schema = "artist-alley.peer-overlap/v2" },
			wantErr: "unknown contract schema"},
		{name: "unknown field", n: 2, mutate: func(f *ovFix) {
			b, _ := json.Marshal(f.contract)
			f.rawContract = strings.Replace(string(b), `"status":`, `"surprise":1,"status":`, 1)
		}, wantErr: "unknown field"},
		{name: "one peer", n: 2, mutate: func(f *ovFix) { f.contract.Peers = []string{"site_a"} }, wantErr: "at least 2"},
		{name: "duplicate peer", n: 2, mutate: func(f *ovFix) { f.contract.Peers = []string{"site_a", "site_a"} },
			wantErr: "duplicate peer"},
		{name: "duplicate fixture name", n: 2, mutate: func(f *ovFix) { fx(f, 1).Name = fx(f, 0).Name },
			wantErr: "duplicate fixture name"},
		{name: "two anchors with equal hashes", n: 2, mutate: func(f *ovFix) { fx(f, 1).SHA256 = fx(f, 0).SHA256 },
			wantErr: "declare the same sha256"},
		{name: "malformed hash", n: 2, mutate: func(f *ovFix) { fx(f, 0).SHA256 = strings.ToUpper(fx(f, 0).SHA256) },
			wantErr: "64 lowercase hex"},
		{name: "matching peer-local ids", n: 2, mutate: func(f *ovFix) {
			setAsset(f, 0, "site_b", func(a *OverlapFixtureAsset) {
				a.AssetID = fx(f, 0).Assets["site_a"].AssetID
				a.IDDerivation = nil
			})
		}, wantErr: "peer-local ids must differ"},
		{name: "duplicate id across fixtures", n: 2, mutate: func(f *ovFix) {
			setAsset(f, 1, "site_a", func(a *OverlapFixtureAsset) { a.AssetID = fx(f, 0).Assets["site_a"].AssetID })
		}, wantErr: "is used by both"},
		{name: "id not its derivation", n: 2, mutate: func(f *ovFix) {
			setAsset(f, 0, "site_b", func(a *OverlapFixtureAsset) { a.IDDerivation.Name += "-x" })
		}, wantErr: "does not match its derivation"},
		{name: "non-canonical id", n: 2, mutate: func(f *ovFix) {
			setAsset(f, 0, "site_a", func(a *OverlapFixtureAsset) { a.AssetID = strings.ToUpper(a.AssetID) })
		}, wantErr: "canonical lowercase UUID"},
		{name: "fixture missing a peer", n: 2, mutate: func(f *ovFix) { delete(fx(f, 0).Assets, "site_b") },
			wantErr: "names 1 peer(s)"},
		{name: "unsafe contract path", n: 2, mutate: func(f *ovFix) {
			setAsset(f, 0, "site_a", func(a *OverlapFixtureAsset) { a.FilePath = "../escape.png" })
		}, wantErr: "unsafe"},
		{name: "non-https source", n: 2, mutate: func(f *ovFix) { fx(f, 0).Source.ArchiveURL = "http://x/p.zip" },
			wantErr: "https URL"},
		{name: "no license evidence", n: 2, mutate: func(f *ovFix) { fx(f, 0).Source.LicenseEvidence = nil },
			wantErr: "license_evidence"},

		// --- the options ---------------------------------------------------
		{name: "no peer roots", n: 2, opts: func(f *ovFix) OverlapOptions {
			return OverlapOptions{ContractPath: f.contractPath()}
		}, wantErr: "no peer roots"},
		{name: "peer not in the contract", n: 2, opts: func(f *ovFix) OverlapOptions {
			return OverlapOptions{ContractPath: f.contractPath(), Peers: map[string]string{
				"site_a": f.roots["site_a"], "site_c": f.roots["site_b"]}}
		}, wantErr: "not in the contract"},
		{name: "both peers one directory", n: 2, opts: func(f *ovFix) OverlapOptions {
			link := filepath.Join(f.t.TempDir(), "alias")
			symlink(f, f.roots["site_a"], link)
			return OverlapOptions{ContractPath: f.contractPath(), Peers: map[string]string{
				"site_a": f.roots["site_a"], "site_b": link}}
		}, wantErr: "same directory"},
		{name: "missing peer root", n: 2, opts: func(f *ovFix) OverlapOptions {
			return OverlapOptions{ContractPath: f.contractPath(), Peers: map[string]string{
				"site_a": f.roots["site_a"], "site_b": filepath.Join(f.dir, "nope")}}
		}, wantErr: "site_b\" root"},
		{name: "no contract path", n: 2, opts: func(f *ovFix) OverlapOptions {
			return OverlapOptions{Peers: f.roots}
		}, wantErr: "contract path is required"},

		// --- the manifests and bytes --------------------------------------
		{name: "anchor row missing", n: 2, mutate: func(f *ovFix) { f.rows["site_b"] = f.rows["site_b"][1:] },
			wantErr: "no manifest row with id"},
		{name: "duplicate manifest id", n: 2, mutate: func(f *ovFix) {
			f.rows["site_a"][3].ID = f.rows["site_a"][2].ID
		}, wantErr: "duplicate manifest id"},
		{name: "row path differs from the contract", n: 2, mutate: func(f *ovFix) {
			f.put("site_a", "images/moved.png", ovBytes(1))
			mustRemove(f.t, filepath.Join(f.roots["site_a"], "images", "pack1", "anchor1.png"))
			f.rows["site_a"][0].FilePath = "images/moved.png"
		}, wantErr: "contract path"},
		{name: "declared size differs", n: 2, mutate: func(f *ovFix) { f.rows["site_b"][1].FileSizeBytes++ },
			wantErr: "manifest declares"},
		{name: "anchor file absent", n: 2, mutate: func(f *ovFix) {
			mustRemove(f.t, filepath.Join(f.roots["site_b"], "shared", "b-side-2.png"))
		}, wantErr: "missing or not readable"},
		{name: "anchor bytes corrupt at the same length", n: 2, mutate: func(f *ovFix) {
			f.put("site_b", "shared/b-side-1.png", strings.Repeat("z", len(ovBytes(1))))
		}, wantErr: "the fixture is"},
		{name: "anchor truncated", n: 2, mutate: func(f *ovFix) {
			f.put("site_a", "images/pack2/anchor2.png", "short")
		}, wantErr: "is 5 bytes"},
		{name: "hash disagreement within a pair", n: 2, mutate: func(f *ovFix) {
			// site_b carries fixture 2's bytes under fixture 1's row: a valid
			// file, the wrong half of the pair.
			f.put("site_b", "shared/b-side-1.png", ovBytes(2))
		}, wantErr: `"site_b": fixture "overlap-image-1"`},
		{name: "source archive disagrees", n: 2, mutate: func(f *ovFix) {
			m := ovMeta(1)
			m["source_archive"] = map[string]any{"url": "https://example.invalid/other.zip", "member": "x.png",
				"sha256": ovSHA(ovBytes(1))}
			f.rows["site_a"][0].Metadata = m
		}, wantErr: "metadata.source_archive.url"},
		{name: "attribution differs", n: 2, mutate: func(f *ovFix) { f.rows["site_b"][0].Attribution = "someone else" },
			wantErr: "license/attribution"},
		{name: "license label differs", n: 2, mutate: func(f *ovFix) { f.rows["site_a"][1].License = "Internal" },
			wantErr: "license/attribution"},
		{name: "unsafe anchor path", n: 2, mutate: func(f *ovFix) {
			f.rows["site_b"][0].FilePath = "../outside.png"
		}, wantErr: "unsafe"},
		{name: "anchor is a symlink", n: 2, mutate: func(f *ovFix) {
			p := filepath.Join(f.roots["site_b"], "shared", "b-side-1.png")
			f.put("site_b", "real/copy.png", ovBytes(1))
			mustRemove(f.t, p)
			symlink(f, filepath.Join(f.roots["site_b"], "real", "copy.png"), p)
			// The real copy is an unlisted file, so only the symlink rule
			// can catch this.
		}, wantErr: "is a symlink"},
		{name: "anchor under a symlinked directory", n: 2, mutate: func(f *ovFix) {
			root := f.roots["site_b"]
			if err := os.Rename(filepath.Join(root, "shared"), filepath.Join(root, "real-shared")); err != nil {
				f.t.Fatal(err)
			}
			symlink(f, filepath.Join(root, "real-shared"), filepath.Join(root, "shared"))
		}, wantErr: `"shared" is a symlink`},
		{name: "other row escapes through a symlink", n: 2, mutate: func(f *ovFix) {
			outside := filepath.Join(f.t.TempDir(), "x.png")
			mustWrite(f.t, outside, "outside")
			p := filepath.Join(f.roots["site_a"], "other", "notes.txt")
			mustRemove(f.t, p)
			symlink(f, outside, p)
		}, wantErr: "cannot be measured"},
		{name: "other row has an unsafe path", n: 2, mutate: func(f *ovFix) {
			f.rows["site_a"][3].FilePath = "a/../../x.png"
		}, wantErr: "cannot show the anchors are unique"},
		{name: "other row's file is missing", n: 2, mutate: func(f *ovFix) {
			mustRemove(f.t, filepath.Join(f.roots["site_b"], "other", "notes.txt"))
		}, wantErr: "cannot be measured"},
		{name: "extra identical bytes under another name", n: 2, mutate: func(f *ovFix) {
			f.put("site_b", "dupes/again.png", ovBytes(2))
			f.rows["site_b"] = append(f.rows["site_b"], ovRow{ID: "dup", FilePath: "dupes/again.png",
				FileSizeBytes: int64(len(ovBytes(2)))})
		}, wantErr: `has the same bytes as fixture "overlap-image-2"`},
		{name: "extra identical bytes behind a stale declared size", n: 2, mutate: func(f *ovFix) {
			// The scan measures the file; a manifest size that lies must not
			// hide a duplicate from the length pre-filter.
			f.put("site_a", "dupes/stale.png", ovBytes(1))
			f.rows["site_a"] = append(f.rows["site_a"], ovRow{ID: "stale", FilePath: "dupes/stale.png", FileSizeBytes: 1})
		}, wantErr: `has the same bytes as fixture "overlap-image-1"`},
		{name: "extra row on the anchor's own file", n: 2, mutate: func(f *ovFix) {
			f.rows["site_a"] = append(f.rows["site_a"], ovRow{ID: "dup", FilePath: f.rows["site_a"][0].FilePath,
				FileSizeBytes: int64(len(ovBytes(1)))})
		}, wantErr: `has the same bytes as fixture "overlap-image-1"`},
		{name: "manifest is a symlink", n: 2, mutate: func(f *ovFix) {
			f.write()
			p := filepath.Join(f.roots["site_a"], "MANIFEST.json")
			real := filepath.Join(f.roots["site_a"], "real-manifest.json")
			if err := os.Rename(p, real); err != nil {
				f.t.Fatal(err)
			}
			symlink(f, real, p)
		}, wantErr: "MANIFEST.json is not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOvFix(t, tc.n)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			if tc.name != "manifest is a symlink" {
				f.write()
			}
			opts := OverlapOptions{ContractPath: f.contractPath(), Peers: f.roots}
			if tc.opts != nil {
				opts = tc.opts(f)
			}
			_, err := ValidatePeerOverlap(opts)
			if err == nil {
				t.Fatalf("validated, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "%!") {
				t.Errorf("error has a formatting fault: %v", err)
			}
		})
	}
}

func TestPeerOverlap_DetectsChangeDuringTheCheck(t *testing.T) {
	for name, change := range map[string]func(f *ovFix){
		"anchor rewritten": func(f *ovFix) { f.put("site_a", "images/pack1/anchor1.png", ovBytes(9)) },
		"manifest edited": func(f *ovFix) {
			p := filepath.Join(f.roots["site_a"], "MANIFEST.json")
			b, _ := os.ReadFile(p)
			mustWrite(f.t, p, string(b)+" ")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newOvFix(t, 2)
			overlapAfterScan = func(peer string) {
				if peer == "site_a" {
					change(f)
				}
			}
			t.Cleanup(func() { overlapAfterScan = nil })
			if _, err := f.run(); err == nil || !strings.Contains(err.Error(), "changed during the check") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// The committed contract is valid, and says exactly what the owner fixed:
// these names, peer-local ids, bytes and sources. Its site_b ids are
// recomputed from their UUIDv5 names by LoadOverlapContract itself.
func TestPeerOverlap_CommittedContract(t *testing.T) {
	c, _, err := LoadOverlapContract(filepath.Join("..", "..", "..", "seed", "contracts", "peer-overlap.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ name, a, aPath, b, sha, page, archive, member string }{
		{"overlap-image-1", "9780ee7c-ce76-6022-dfa2-46aae7d50428",
			"images/kenney-allin1/2d-assets-monster-builder-pack/spritesheet/spritesheet_default.png",
			"40aa74bc-da53-5ace-885e-521a292c1201",
			"52ac0ca27297ac18457727ed79edc19de1c1b896a465559cdd9260ec2a4740c1",
			"https://kenney.nl/assets/monster-builder-pack",
			"https://kenney.nl/media/pages/assets/monster-builder-pack/663e4ef6de-1677495438/kenney_monster-builder-pack.zip",
			"Spritesheet/spritesheet_default.png"},
		{"overlap-image-2", "337d3a4d-1b3d-80b7-b448-39283bd58319",
			"images/kenney-allin1/2d-assets-planets/parts/sphere1.png",
			"c0082a12-3ebb-5997-90bf-0d18b745bbf3",
			"b1d6e3414762406a9724549876b4286c41a5b2b60007ccbfb5305001d94c9fb7",
			"https://kenney.nl/assets/planets",
			"https://kenney.nl/media/pages/assets/planets/512b578338-1677495391/kenney_planets.zip",
			"Parts/sphere1.png"},
	}
	if len(c.Fixtures) != len(want) || strings.Join(c.Peers, ",") != "site_a,site_b" {
		t.Fatalf("contract = %+v", c)
	}
	for i, w := range want {
		g := c.Fixtures[i]
		a, b := g.Assets["site_a"], g.Assets["site_b"]
		if g.Name != w.name || g.SHA256 != w.sha || a.AssetID != w.a || a.FilePath != w.aPath || b.AssetID != w.b ||
			g.Source.AssetPage != w.page || g.Source.ArchiveURL != w.archive || g.Source.ArchiveMember != w.member ||
			g.Source.License != "CC0-1.0" || g.Source.Attribution != "Kenney (kenney.nl)" {
			t.Errorf("fixture %d = %+v", i, g)
		}
		if b.IDDerivation == nil || b.IDDerivation.Name != "artist-alley:v0.11.1:public-site-b:"+w.name {
			t.Errorf("fixture %d: site_b id derivation = %+v", i, b.IDDerivation)
		}
		if b.FilePath != "" {
			t.Errorf("fixture %d: the new site_b does not exist yet, so the contract must not fix its path", i)
		}
		if !strings.Contains(g.Rights, "not release-cleared") {
			t.Errorf("fixture %d: rights_status %q reads as a clearance", i, g.Rights)
		}
	}
}

// Every anchor row on every checked peer must carry its provenance, and
// it must be the contract's: metadata.fetched_from == source.asset_page and
// metadata.source_archive {url, member, sha256} == the contract's archive,
// member and fixture hash. Absence is a refusal, not a pass (#1514
// overlap correction round 1).
func TestPeerOverlap_AnchorProvenanceIsMandatory(t *testing.T) {
	type edit func(m map[string]any) any // returns the row's new metadata value
	without := func(key string) edit {
		return func(m map[string]any) any { delete(m, key); return m }
	}
	set := func(key string, v any) edit {
		return func(m map[string]any) any { m[key] = v; return m }
	}
	archive := func(key string, v any, drop bool) edit {
		return func(m map[string]any) any {
			sa := m["source_archive"].(map[string]any)
			if drop {
				delete(sa, key)
			} else {
				sa[key] = v
			}
			return m
		}
	}
	cases := []struct {
		name    string
		edit    edit
		wantErr string
	}{
		{"metadata absent", func(map[string]any) any { return nil }, "metadata is missing"},
		{"metadata null", func(map[string]any) any { return json.RawMessage("null") }, "metadata is missing"},
		{"metadata not an object", func(map[string]any) any { return "kenney" }, "metadata is not a JSON object"},
		{"fetched_from missing", without("fetched_from"), "fetched_from is missing"},
		{"fetched_from null", set("fetched_from", nil), "fetched_from is missing"},
		{"fetched_from empty", set("fetched_from", ""), "fetched_from is empty"},
		{"fetched_from not a string", set("fetched_from", 7), "fetched_from is not a string"},
		{"fetched_from mismatched", set("fetched_from", "https://example.invalid/assets/elsewhere"),
			"metadata.fetched_from \"https://example.invalid/assets/elsewhere\" disagrees"},
		{"source_archive missing", without("source_archive"), "metadata.source_archive is missing"},
		{"source_archive null", set("source_archive", nil), "metadata.source_archive is missing"},
		{"source_archive not an object", set("source_archive", "pack.zip"), "metadata.source_archive is not a JSON object"},
		{"source_archive url mismatched", archive("url", "https://example.invalid/other.zip", false),
			"metadata.source_archive.url"},
		{"source_archive member mismatched", archive("member", "Parts/other.png", false),
			"metadata.source_archive.member"},
		{"source_archive sha256 mismatched", archive("sha256", ovSHA("other"), false),
			"metadata.source_archive.sha256"},
		{"source_archive url missing", archive("url", nil, true), "metadata.source_archive: url is missing"},
		{"source_archive member missing", archive("member", nil, true), "metadata.source_archive: member is missing"},
		{"source_archive sha256 missing", archive("sha256", nil, true), "metadata.source_archive: sha256 is missing"},
		{"source_archive member empty", archive("member", "", false), "metadata.source_archive: member is empty"},
	}
	for _, peer := range []string{"site_a", "site_b"} {
		for _, tc := range cases {
			t.Run(peer+"/"+tc.name, func(t *testing.T) {
				f := newOvFix(t, 2)
				// Fixture 2's anchor, so a check that only looked at the first
				// row would not pass by accident.
				f.rows[peer][1].Metadata = tc.edit(ovMeta(2))
				f.write()
				_, err := f.run()
				if err == nil {
					t.Fatalf("validated, want %q", tc.wantErr)
				}
				want := fmt.Sprintf("peer %q: fixture %q", peer, "overlap-image-2")
				if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want %q and %q", err, want, tc.wantErr)
				}
				// The partial check of the damaged peer alone refuses too.
				_, err = ValidatePeerOverlap(OverlapOptions{ContractPath: f.contractPath(),
					Peers: map[string]string{peer: f.roots[peer]}, AllowPartial: true})
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("partial check of %s: err = %v", peer, err)
				}
			})
		}
	}
	// Non-anchor rows keep their old rule: no metadata requirement.
	f := newOvFix(t, 2)
	for _, peer := range []string{"site_a", "site_b"} {
		for i := 2; i < len(f.rows[peer]); i++ {
			f.rows[peer][i].Metadata = nil
		}
	}
	f.write()
	if _, err := f.run(); err != nil {
		t.Errorf("non-anchor rows without metadata were refused: %v", err)
	}
}
