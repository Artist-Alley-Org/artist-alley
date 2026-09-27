// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Asset-title verification (#1464). The verifier compared post titles
// exactly (postdrift.go compare) and never read an asset's title, so a
// database whose asset titles were stale, edited, or still punctuated
// reported VERIFIED. These tests drive the same fixture as
// verify_test.go and assert on the failure text only, so they compile
// and run against the verifier before the change as well: the edited
// and punctuated cases FAIL there, which is the point.

package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/storage"
	storagefs "github.com/mscrnt/artist-alley/app/internal/storage/fs"
)

const (
	titleEqualityMark    = ": title differs from the manifest"
	titlePunctuationMark = "contains a forbidden character"
)

// titleVerdicts counts, per asset id, the title-equality and the
// title-punctuation failures in rep, plus every title failure of either
// kind across the whole report (so a stray verdict on another asset is
// visible, not just a missing one on the asset under test).
func titleVerdicts(rep *VerifyReport, id string) (equality, punctuation, total int) {
	prefix := "asset " + id + ": title"
	for _, msg := range rep.Failures {
		isEq := strings.Contains(msg, titleEqualityMark)
		isPunct := strings.Contains(msg, titlePunctuationMark)
		if isEq || isPunct {
			total++
		}
		if !strings.HasPrefix(msg, prefix) {
			continue
		}
		if isEq {
			equality++
		}
		if isPunct {
			punctuation++
		}
	}
	return
}

// setTitle rewrites one asset's title in BOTH the manifest and the
// database, the way a seed over that manifest would have left it.
func (f *verifyFixture) setTitle(i int, title string) {
	f.t.Helper()
	f.assets[i].Title = title
	f.setDBTitle(i, title)
	f.writeSite()
}

// setDBTitle edits only the database row: the drift a reseed without
// --reset, or an edit after the seed, leaves behind.
func (f *verifyFixture) setDBTitle(i int, title string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE assets SET title = $1 WHERE id = $2`, title, f.assetID(i)); err != nil {
		f.t.Fatal(err)
	}
}

func assertTitleVerdicts(t *testing.T, rep *VerifyReport, id string, wantEq, wantPunct int) {
	t.Helper()
	eq, punct, _ := titleVerdicts(rep, id)
	if eq != wantEq || punct != wantPunct {
		t.Fatalf("asset %s: %d equality and %d punctuation title failure(s), want %d and %d; failures were:\n  %s",
			id, eq, punct, wantEq, wantPunct, strings.Join(rep.Failures, "\n  "))
	}
}

func assertTitleTotal(t *testing.T, rep *VerifyReport, want int) {
	t.Helper()
	if _, _, total := titleVerdicts(rep, ""); total != want {
		t.Fatalf("%d title failure(s) across the report, want %d; failures were:\n  %s",
			total, want, strings.Join(rep.Failures, "\n  "))
	}
}

// N=0: a manifest with no assets produces no title verdict at all.
func TestVerifyAssetTitle_NoManifestAssets(t *testing.T) {
	f := newVerifyFixture(t)
	f.assets = nil
	f.posts = nil
	f.writeSite()
	assertTitleTotal(t, f.verify(VerifyOptions{}), 0)
}

// singleAssetManifest narrows the verifier's input to exactly ONE
// present asset: the fixture's first asset, alone in MANIFEST.json, and
// no catalogue posts (the fixture's post spans both assets). The
// fixture's other rows stay in the database; the verifier only judges
// what the manifest names. It asserts the boundary it creates, from the
// file on disk and from the report, so an N=1 test cannot silently run
// at N=2.
func (f *verifyFixture) singleAssetManifest() {
	f.t.Helper()
	f.assets = f.assets[:1]
	f.posts = nil
	f.writeSite()
	b, err := os.ReadFile(filepath.Join(f.siteRoot, "MANIFEST.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var onDisk []manifestAsset
	if err := json.Unmarshal(b, &onDisk); err != nil {
		f.t.Fatal(err)
	}
	if len(onDisk) != 1 || onDisk[0].ID != f.assets[0].ID {
		f.t.Fatalf("MANIFEST.json holds %d asset(s), want exactly 1 (%s)", len(onDisk), f.assets[0].ID)
	}
}

func assertSinglePresentAsset(t *testing.T, rep *VerifyReport) {
	t.Helper()
	if rep.Assets != 1 || rep.AssetsPresent != 1 {
		t.Fatalf("the verifier read %d manifest asset(s), %d present; want exactly 1 and 1", rep.Assets, rep.AssetsPresent)
	}
}

// N=1: the manifest holds exactly one present asset whose database
// title equals the seeder-bound manifest title. No title verdict.
func TestVerifyAssetTitle_EqualUnpunctuatedTitlesPass(t *testing.T) {
	f := newVerifyFixture(t)
	f.singleAssetManifest()
	rep := f.verify(VerifyOptions{})
	assertSinglePresentAsset(t, rep)
	assertTitleVerdicts(t, rep, f.assets[0].ID, 0, 0)
	assertTitleTotal(t, rep, 0)
}

// N=1: the manifest holds exactly one present asset, and its database
// title was edited after the seed. Exactly one equality failure, naming
// the asset, the expected and the held value; no punctuation verdict
// and no other title verdict. FAILS on the verifier before #1464, which
// reported this VERIFIED.
func TestVerifyAssetTitle_OneEditedTitleFails(t *testing.T) {
	f := newVerifyFixture(t)
	f.singleAssetManifest()
	f.setDBTitle(0, "edited after the seed")
	rep := f.verify(VerifyOptions{})
	assertSinglePresentAsset(t, rep)
	assertTitleVerdicts(t, rep, f.assets[0].ID, 1, 0)
	assertTitleTotal(t, rep, 1)
	mustContain(t, rep.Failures, f.assets[0].ID, titleEqualityMark,
		`"`+f.assets[0].Title+`"`, `"edited after the seed"`)
}

// N>=2: five assets, two edited. Exactly those two fail, each naming
// its own id; the three untouched ids are never named. A check that
// counted "some title failure" would pass on the wrong assets too.
// FAILS on the verifier before #1464.
func TestVerifyAssetTitle_TwoOfFiveEditedFailExactly(t *testing.T) {
	f := newVerifyFixture(t)
	for i := 0; i < 3; i++ {
		a := f.newAsset("", 20)
		// Outside any collection: an uncovered collection asset would
		// legitimately expect a backfill post, which is not this test.
		a.CollectionName = ""
		f.assets = append(f.assets, a)
	}
	f.writeSite()
	edited := map[int]bool{1: true, 3: true}
	for i := range edited {
		f.setDBTitle(i, "edited "+f.assets[i].ID[:8])
	}
	rep := f.verify(VerifyOptions{})
	for i, a := range f.assets {
		if edited[i] {
			assertTitleVerdicts(t, rep, a.ID, 1, 0)
		} else {
			assertTitleVerdicts(t, rep, a.ID, 0, 0)
		}
	}
	assertTitleTotal(t, rep, 2)
}

// The punctuation invariant is independent of equality: a comma the
// manifest and the database AGREE on still fails, once, and equality
// does not. FAILS on the verifier before #1464.
func TestVerifyAssetTitle_EqualCommaTitleFailsPunctuationOnly(t *testing.T) {
	f := newVerifyFixture(t)
	f.setTitle(0, "a, b")
	rep := f.verify(VerifyOptions{})
	assertTitleVerdicts(t, rep, f.assets[0].ID, 0, 1)
	assertTitleTotal(t, rep, 1)
	mustContain(t, rep.Failures, f.assets[0].ID, titlePunctuationMark, "comma U+002C")
}

func TestVerifyAssetTitle_EqualEmDashTitleFailsPunctuationOnly(t *testing.T) {
	f := newVerifyFixture(t)
	f.setTitle(0, "a — b")
	rep := f.verify(VerifyOptions{})
	assertTitleVerdicts(t, rep, f.assets[0].ID, 0, 1)
	assertTitleTotal(t, rep, 1)
	mustContain(t, rep.Failures, f.assets[0].ID, titlePunctuationMark, "em dash U+2014")
	mustNotContain(t, rep.Failures, "comma U+002C")
}

// Both characters in one title are ONE punctuation failure for that
// asset, naming both.
func TestVerifyAssetTitle_CommaAndEmDashAreOneFailure(t *testing.T) {
	f := newVerifyFixture(t)
	f.setTitle(0, "a, b — c")
	rep := f.verify(VerifyOptions{})
	assertTitleVerdicts(t, rep, f.assets[0].ID, 0, 1)
	assertTitleTotal(t, rep, 1)
	mustContain(t, rep.Failures, f.assets[0].ID, titlePunctuationMark, "comma U+002C and em dash U+2014")
}

// Unequal AND punctuated: the database holds "x, y" where the manifest
// says "x - y". Two failures for that asset, one of each kind.
func TestVerifyAssetTitle_UnequalAndPunctuatedFailsBoth(t *testing.T) {
	f := newVerifyFixture(t)
	f.setTitle(0, "x - y")
	f.setDBTitle(0, "x, y")
	rep := f.verify(VerifyOptions{})
	assertTitleVerdicts(t, rep, f.assets[0].ID, 1, 1)
	assertTitleTotal(t, rep, 2)
	mustContain(t, rep.Failures, f.assets[0].ID, titleEqualityMark, `"x - y"`, `"x, y"`)
}

// An empty (or blank) manifest title is seeded as "Untitled"; that is
// the seeder's binding, so the verifier must accept it rather than
// compare against the raw manifest string.
func TestVerifyAssetTitle_EmptyManifestTitleIsUntitled(t *testing.T) {
	for _, manifestTitle := range []string{"", "   "} {
		f := newVerifyFixture(t)
		f.assets[0].Title = manifestTitle
		f.writeSite()
		f.setDBTitle(0, "Untitled")
		rep := f.verify(VerifyOptions{})
		assertTitleVerdicts(t, rep, f.assets[0].ID, 0, 0)
		assertTitleTotal(t, rep, 0)
	}
}

// An absent asset (never written, or soft-deleted) keeps its existing
// absence failure and gets no title verdict, even with a punctuated
// manifest title.
func TestVerifyAssetTitle_AbsentAssetGetsNoTitleVerdict(t *testing.T) {
	f := newVerifyFixture(t)
	missing := manifestAsset{
		ID: uuid.New().String(), AssetType: "image", Title: "gone, away",
		FilePath: "images/gone.png", FileExtension: "png",
		FileSizeBytes: 5, SensitivityTier: "public", ArchiveState: "active",
		OwnerUsername: f.username, CreatedAt: "2025-05-05T12:00:00Z", UpdatedAt: "2025-05-05T12:00:00Z",
	}
	f.assets = append(f.assets, missing)
	f.writeSite()
	writeSiteFile(t, f.siteRoot, missing.FilePath, []byte("other"))

	// And a soft-deleted row, whose manifest title disagrees with it.
	f.setDBTitle(1, "stale, deleted")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE assets SET deleted_at = now() WHERE id = $1`, f.assetID(1)); err != nil {
		t.Fatal(err)
	}

	rep := f.verify(VerifyOptions{})
	mustContain(t, rep.Failures, missing.ID, "absent from the database")
	mustContain(t, rep.Failures, f.assets[1].ID, "absent from the database")
	assertTitleVerdicts(t, rep, missing.ID, 0, 0)
	assertTitleVerdicts(t, rep, f.assets[1].ID, 0, 0)
	assertTitleTotal(t, rep, 0)
}

// A collapsed asset keeps its existing collapse failure and gets no
// title verdict; the sibling that holds the bytes is still checked.
func TestVerifyAssetTitle_CollapsedAssetGetsNoTitleVerdict(t *testing.T) {
	f := newVerifyFixture(t)
	ctx := context.Background()
	backend, err := storagefs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := storage.NewService(backend, f.pool)
	payload := []byte("aa1464 identical bytes " + f.salt)
	sibling := f.newAsset("", int64(len(payload)))
	sibling.CollectionName = ""
	up, err := svc.UploadOriginal(ctx, bytes.NewReader(payload), "image/png",
		storage.PinRef{SubjectType: "asset", SubjectID: sibling.ID})
	if err != nil {
		t.Fatalf("UploadOriginal: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = f.pool.Exec(c, `UPDATE assets SET file_hash = NULL WHERE file_hash = $1`, up.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_pins WHERE object_hash = $1`, up.Hash)
		_, _ = f.pool.Exec(c, `DELETE FROM storage_objects WHERE hash = $1`, up.Hash)
	})
	if _, err := f.pool.Exec(ctx, `UPDATE assets SET file_hash = $1 WHERE id = $2`, up.Hash, parseUUID(sibling.ID)); err != nil {
		t.Fatal(err)
	}
	collapsed := manifestAsset{
		ID: uuid.New().String(), AssetType: "image", Title: "collapsed, — twice",
		FilePath: "images/collapsed.png", FileExtension: "png",
		FileSizeBytes: int64(len(payload)), SensitivityTier: "public", ArchiveState: "active",
		OwnerUsername: f.username, CreatedAt: "2025-05-05T12:00:00Z", UpdatedAt: "2025-05-05T12:00:00Z",
	}
	f.assets = append(f.assets, sibling, collapsed)
	f.writeSite()
	writeSiteFile(t, f.siteRoot, sibling.FilePath, payload)
	writeSiteFile(t, f.siteRoot, collapsed.FilePath, payload)

	rep := f.verify(VerifyOptions{})
	if rep.AssetsCollapsed != 1 {
		t.Fatalf("collapsed assets %d, want 1", rep.AssetsCollapsed)
	}
	mustContain(t, rep.Failures, collapsed.ID, "collapsed it by content address", sibling.ID)
	assertTitleVerdicts(t, rep, collapsed.ID, 0, 0)
	assertTitleVerdicts(t, rep, sibling.ID, 0, 0)
	assertTitleTotal(t, rep, 0)
}

func writeSiteFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
