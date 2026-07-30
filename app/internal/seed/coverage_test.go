// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the CI coverage profile (#768).
//
// The load-bearing ones are the FAILURE tests. A coverage report that
// only logs is worthless — the whole point of the profile is that a
// catalogue which cannot exercise a class stops the seed instead of
// producing a fixture that makes the suite green for the wrong reason
// (ADR 0068; the untextured-3D arc, #750/#753). So each of these removes
// one dimension from the fixture and asserts the seed errors and names
// what is missing.

package seed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/logging"
)

// --- fixture ----------------------------------------------------------

// covFixture builds a synthetic catalogue on disk that satisfies every
// requiredDims entry, with enough posts per collection and assets per
// extension to exercise the depth floor. Returns the catalogue; the
// caller mutates it to remove a dimension.
func covFixture(t *testing.T) *catalogues {
	t.Helper()
	dir := t.TempDir()

	// Real model files, because companion resolution reads bytes — a
	// manifest row cannot declare whether its model has external
	// textures (#750). One per container format, since each parses
	// differently and each is its own required dimension.
	mustWrite(t, filepath.Join(dir, "tex.png"), "\x89PNG\r\nfixture")
	mustWrite(t, filepath.Join(dir, "model.bin"), "binbytes")
	mustWriteGLB(t, filepath.Join(dir, "ext.glb"),
		`{"asset":{"version":"2.0"},"images":[{"uri":"tex.png"}]}`)
	mustWrite(t, filepath.Join(dir, "ext.gltf"),
		`{"asset":{"version":"2.0"},"buffers":[{"uri":"model.bin","byteLength":8}],`+
			`"images":[{"uri":"tex.png"}]}`)
	mustWrite(t, filepath.Join(dir, "ext.obj"), "mtllib ext.mtl\nv 0 0 0\n")
	mustWrite(t, filepath.Join(dir, "ext.mtl"), "newmtl m\nmap_Kd tex.png\n")
	mustWrite(t, filepath.Join(dir, "ext.fbx"), asciiFBXWithTexture)

	// Non-model bytes are never opened by selection, but write them so
	// the fixture is a real tree rather than a set of dangling paths.
	for _, name := range []string{"a.png", "b.png", "c.jpg", "clip.mp4", "sound.ogg",
		"book.epub", "face.ttf", "notes.txt"} {
		mustWrite(t, filepath.Join(dir, name), "bytes-"+name)
	}

	c := &catalogues{
		SiteRoot: dir,
		Collections: []catCollection{
			{ID: "c1", Name: "Alpha"},
			{ID: "c2", Name: "Beta"},
			{ID: "c3", Name: "Ghost"}, // deliberately holds nothing
		},
	}

	// Assets. Every extension gets several copies so a depth floor above
	// 1 has somewhere to go.
	add := func(id, typ, ext, file, coll, team string, extras func(*manifestAsset)) {
		a := manifestAsset{
			ID:              id,
			AssetType:       typ,
			Title:           id,
			FilePath:        file,
			FileExtension:   ext,
			SensitivityTier: "team",
			ArchiveState:    "active",
			WorkflowState:   "approved",
			OwnerUsername:   "u1",
			CollectionName:  coll,
			TeamName:        team,
			Tags:            []string{"t-" + ext},
			FieldValues:     map[string]any{"rating": 3},
		}
		if extras != nil {
			extras(&a)
		}
		c.Assets = append(c.Assets, a)
	}

	for i := 0; i < 6; i++ {
		add(fmt.Sprintf("img-%d", i), "image", "png", "a.png", "Alpha", "Environment", nil)
	}
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("jpg-%d", i), "image", "jpg", "c.jpg", "Beta", "Textures", nil)
	}
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("vid-%d", i), "video", "mp4", "clip.mp4", "Alpha", "Animation",
			func(a *manifestAsset) {
				a.SensitivityTier = "public"
				a.ReviewNotes = "looks good"
				a.ReviewerUsername = "u2"
			})
	}
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("aud-%d", i), "audio", "ogg", "sound.ogg", "Beta", "Audio", nil)
	}
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("doc-%d", i), "document", "epub", "book.epub", "Alpha", "Reference",
			func(a *manifestAsset) { a.ArchiveState = "draft"; a.WorkflowState = "draft" })
	}
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("fnt-%d", i), "font", "ttf", "face.ttf", "Beta", "UI", nil)
	}
	// One asset with no typed field values, to prove the profile does not
	// require every asset to carry them.
	add("txt-0", "document", "txt", "notes.txt", "Alpha", "Reference",
		func(a *manifestAsset) { a.FieldValues = nil })
	// Companion-bearing models, one per container format.
	for _, m := range []struct{ id, ext, file string }{
		{"glb", "glb", "ext.glb"},
		{"gltf", "gltf", "ext.gltf"},
		{"obj", "obj", "ext.obj"},
		{"fbx", "fbx", "ext.fbx"},
	} {
		for i := 0; i < 3; i++ {
			add(fmt.Sprintf("%s-%d", m.id, i), "3d", m.ext, m.file, "Alpha", "Props",
				func(a *manifestAsset) { a.SensitivityTier = "restricted" })
		}
	}

	// Posts. One solo post per asset plus a mixed-type multi-asset post,
	// so post-first selection can reach the whole asset set.
	for _, a := range c.Assets {
		c.Posts = append(c.Posts, manifestPost{
			ID:              "p-" + a.ID,
			Title:           "post " + a.ID,
			AssetIDs:        []string{a.ID},
			AuthorUsername:  "u1",
			CollectionName:  a.CollectionName,
			TeamName:        a.TeamName,
			WorkflowState:   "approved",
			PostKind:        "solo_showcase",
			SensitivityTier: a.SensitivityTier,
			Tags:            []string{"p"},
		})
	}
	c.Posts = append(c.Posts, manifestPost{
		ID:              "p-mixed",
		Title:           "mixed",
		AssetIDs:        []string{"img-0", "vid-0", "glb-0"},
		AuthorUsername:  "u1",
		CollectionName:  "Alpha",
		TeamName:        "Environment",
		WorkflowState:   "in_review",
		PostKind:        "multi_asset",
		SensitivityTier: "public",
		IsMixedType:     true,
	})
	return c
}

// asciiFBXWithTexture is the documented FBX ASCII layout carrying one
// external texture reference (see format3d/fbx_test.go).
const asciiFBXWithTexture = `; FBX 7.3.0 project file
FBXHeaderExtension:  {
	FBXHeaderVersion: 1003
	FBXVersion: 7300
}
Objects:  {
	Video: 1, "Video::tex", "Clip" {
		Type: "Clip"
		Filename: "tex.png"
		RelativeFilename: "tex.png"
	}
}
`

func covRun(t *testing.T, c *catalogues, depth int) (*coverageReport, error) {
	t.Helper()
	return c.applyCoverageProfile(depth, logging.Setup("error", "text"))
}

// --- happy path -------------------------------------------------------

func TestCoverageProfile_CoversUniverseAndHoldsDepthFloor(t *testing.T) {
	c := covFixture(t)
	assetsBefore, postsBefore := len(c.Assets), len(c.Posts)

	rep, err := covRun(t, c, 3)
	if err != nil {
		t.Fatalf("coverage profile: %v\n%s", err, rep.Summary())
	}
	if len(rep.Uncovered) != 0 {
		t.Fatalf("uncovered dimensions: %s", dimList(rep.Uncovered))
	}
	if rep.Covered != rep.Universe {
		t.Fatalf("covered %d of %d", rep.Covered, rep.Universe)
	}
	// It has to actually shrink something, or the profile is a very
	// elaborate no-op.
	if rep.Assets >= assetsBefore || rep.Posts >= postsBefore {
		t.Errorf("no shrink at depth 3: %d/%d assets, %d/%d posts",
			rep.Assets, assetsBefore, rep.Posts, postsBefore)
	}
	// The catalogue is actually mutated, not just reported on.
	if len(c.Assets) != rep.Assets || len(c.Posts) != rep.Posts {
		t.Fatalf("catalogue not narrowed: %d assets / %d posts vs report %d / %d",
			len(c.Assets), len(c.Posts), rep.Assets, rep.Posts)
	}

	// Every collection with catalogue content keeps at least `depth`
	// posts — the masonry / pagination floor.
	for _, name := range []string{"Alpha", "Beta"} {
		if rep.PerCollection[name] < 3 {
			t.Errorf("collection %s kept %d posts, want >= 3", name, rep.PerCollection[name])
		}
	}
	// Ghost holds nothing in the FULL catalogue either — reported, not
	// treated as damage.
	if !contains(rep.EmptyCollections, "Ghost") {
		t.Errorf("Ghost should be reported as empty in the catalogue, got %v",
			rep.EmptyCollections)
	}

	// Every extension present in the catalogue survives, at the floor or
	// at whatever the catalogue holds if that is less.
	for _, ext := range []string{"png", "jpg", "mp4", "ogg", "epub", "ttf", "txt",
		"glb", "gltf", "obj", "fbx"} {
		if rep.PerExtension[ext] == 0 {
			t.Errorf("extension %s absent from the selection", ext)
		}
	}
	if rep.PerExtension["png"] < 3 {
		t.Errorf("png kept %d assets, want >= 3 (depth floor)", rep.PerExtension["png"])
	}
	if rep.PerExtension["txt"] != 1 {
		t.Errorf("txt kept %d, want 1 (only one exists — the floor is bounded by supply)",
			rep.PerExtension["txt"])
	}
	// Companion-bearing models survive as such.
	if rep.Companions < 4 {
		t.Errorf("companion-bearing assets = %d, want >= 4 (one per container format)",
			rep.Companions)
	}
}

// Selection must not depend on Go map iteration order: two runs over the
// same catalogue produce the same fixture, or a CI failure is not
// reproducible from the same inputs.
func TestCoverageProfile_Deterministic(t *testing.T) {
	var ids [2]string
	for i := range ids {
		c := covFixture(t)
		if _, err := covRun(t, c, 4); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		var b strings.Builder
		for _, p := range c.Posts {
			b.WriteString(p.ID + ",")
		}
		b.WriteString("|")
		for _, a := range c.Assets {
			b.WriteString(a.ID + ",")
		}
		ids[i] = b.String()
	}
	if ids[0] != ids[1] {
		t.Fatalf("selection is not deterministic:\n  %s\n  %s", ids[0], ids[1])
	}
}

// An asset no post references is unreachable by post-first selection, so
// it is pulled in standalone rather than losing its coverage. site_a has
// none today; a regenerated dataset easily could.
func TestCoverageProfile_AddsBackPostlessAssets(t *testing.T) {
	c := covFixture(t)
	// A lone .hdr in no post at all — a whole extension the post graph
	// cannot reach.
	mustWrite(t, filepath.Join(c.SiteRoot, "sky.hdr"), "hdr-bytes")
	c.Assets = append(c.Assets, manifestAsset{
		ID: "hdr-orphan", AssetType: "image", Title: "sky", FilePath: "sky.hdr",
		FileExtension: "hdr", SensitivityTier: "team", ArchiveState: "active",
		WorkflowState: "approved", OwnerUsername: "u1", CollectionName: "Alpha",
		TeamName: "Environment", Tags: []string{"sky"},
		FieldValues: map[string]any{"rating": 1},
	})

	rep, err := covRun(t, c, 3)
	if err != nil {
		t.Fatalf("coverage profile: %v\n%s", err, rep.Summary())
	}
	if rep.PerExtension["hdr"] != 1 {
		t.Errorf("hdr kept %d, want 1 (added back despite no post)", rep.PerExtension["hdr"])
	}
	if rep.AddedOrphans == 0 {
		t.Error("report should count the post-less add-back")
	}
	found := false
	for _, a := range c.Assets {
		if a.ID == "hdr-orphan" {
			found = true
		}
	}
	if !found {
		t.Error("the post-less asset is missing from the narrowed catalogue")
	}
}

// --- failure paths: the acceptance criterion --------------------------

// Remove a whole media class from the fixture. The seed must ERROR and
// name it, because a suite that asserts on font rendering would
// otherwise pass against a catalogue holding no fonts.
func TestCoverageProfile_ErrorsWhenMediaClassMissing(t *testing.T) {
	c := covFixture(t)
	dropAssets(c, func(a manifestAsset) bool { return a.AssetType == "font" })
	assetsBefore, postsBefore := len(c.Assets), len(c.Posts)

	rep, err := covRun(t, c, 3)
	if err == nil {
		t.Fatalf("expected an error for a catalogue with no fonts; got none\n%s",
			rep.Summary())
	}
	if !strings.Contains(err.Error(), "asset.type=font") {
		t.Fatalf("error should name the missing dimension, got: %v", err)
	}
	if !containsDim(rep.MissingReq, dim{dimAssetType, "font"}) {
		t.Fatalf("report should list asset.type=font as missing, got %s",
			dimList(rep.MissingReq))
	}
	// A failed profile leaves the catalogue alone: the caller aborts, and
	// a half-narrowed catalogue would be worse than either outcome.
	if len(c.Assets) != assetsBefore || len(c.Posts) != postsBefore {
		t.Fatalf("catalogue mutated despite the failure: %d/%d assets, %d/%d posts",
			len(c.Assets), assetsBefore, len(c.Posts), postsBefore)
	}
}

// The #750 class, made unfailable-in-silence: a catalogue whose GLBs are
// all self-contained can never produce a companion row, so the companion
// specs would assert against rows that were never meant to exist. Same
// bytes, same manifest — only the model's declared references change.
func TestCoverageProfile_ErrorsWhenCompanionClassMissing(t *testing.T) {
	c := covFixture(t)
	// Rewrite the GLB so it declares nothing external. Everything else,
	// including the manifest row and the extension, is untouched.
	mustWriteGLB(t, filepath.Join(c.SiteRoot, "ext.glb"), `{"asset":{"version":"2.0"}}`)

	rep, err := covRun(t, c, 3)
	if err == nil {
		t.Fatalf("expected an error for a catalogue with no external-texture GLB\n%s",
			rep.Summary())
	}
	if !strings.Contains(err.Error(), "companions=glb") {
		t.Fatalf("error should name companions=glb, got: %v", err)
	}
	// The other container formats are unaffected — the check is
	// per-format, not "some model somewhere has a companion".
	if containsDim(rep.MissingReq, dim{dimCompanion, "obj"}) {
		t.Errorf("obj companions should still be covered: %s", dimList(rep.MissingReq))
	}
}

// A catalogue with no multi-asset post cannot exercise playlist ordering.
func TestCoverageProfile_ErrorsWhenRelationClassMissing(t *testing.T) {
	c := covFixture(t)
	var kept []manifestPost
	for _, p := range c.Posts {
		if len(p.AssetIDs) > 1 {
			continue
		}
		kept = append(kept, p)
	}
	c.Posts = kept

	_, err := covRun(t, c, 3)
	if err == nil {
		t.Fatal("expected an error for a catalogue with no multi-asset post")
	}
	for _, want := range []string{"post.multi=true", "post.mixed=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %s, got: %v", want, err)
		}
	}
}

// An unknown profile name is a typo, not a request for the full seed.
func TestCoverageProfile_RejectsUnknownName(t *testing.T) {
	r := NewRunner(nil, nil, Options{Profile: "cli", Logger: logging.Setup("error", "text")})
	_, err := r.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), `unknown seed profile "cli"`) {
		t.Fatalf("want unknown-profile error, got %v", err)
	}
}

// --limit-per-extension runs AFTER the profile and cascade-drops posts
// whose assets it cut, which would re-open the exact hole the profile
// closes — and the coverage report, printed before it runs, would say
// otherwise. Refuse the combination.
func TestCoverageProfile_RejectsExtensionLimitCombination(t *testing.T) {
	r := NewRunner(nil, nil, Options{
		Profile:     ProfileCI,
		LimitPerExt: 8,
		Logger:      logging.Setup("error", "text"),
	})
	_, err := r.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %v", err)
	}
}

// --- against the real dataset -----------------------------------------

// Runs the profile over the actual site_a catalogue when it is mounted,
// which is the only place the numbers in the doc comment can be checked.
// Skips (does not fail) elsewhere — the dataset lives on a NAS share and
// CI runners mount it at a different path.
func TestCoverageProfile_RealCatalogue(t *testing.T) {
	root := os.Getenv("AA_SEED_SITE_ROOT")
	if root == "" {
		t.Skip("AA_SEED_SITE_ROOT not set; real-catalogue coverage test skipped")
	}
	if _, err := os.Stat(filepath.Join(root, "MANIFEST.json")); err != nil {
		t.Skipf("no MANIFEST.json under %s; skipping", root)
	}
	c := &catalogues{SiteRoot: root}
	loadOrSkip(t, filepath.Join(root, "MANIFEST.json"), &c.Assets)
	loadOrSkip(t, filepath.Join(root, "posts.json"), &c.Posts)
	loadOrSkip(t, "../../../seed/profiles/dataset.collections.json", &c.Collections)

	rep, err := c.applyCoverageProfile(defaultCoverageDepth, logging.Setup("error", "text"))
	if rep != nil {
		t.Log(rep.Summary())
	}
	if err != nil {
		t.Fatalf("real catalogue failed coverage: %v", err)
	}
	if rep.Assets > rep.AssetsBefore/4 {
		t.Errorf("selection kept %d of %d assets — too large to be a CI seed",
			rep.Assets, rep.AssetsBefore)
	}
	if rep.Companions == 0 {
		t.Error("no companion-bearing assets survived")
	}
}

// --- helpers ----------------------------------------------------------

func loadOrSkip(t *testing.T, path string, dst any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// dropAssets removes matching assets and every post that referenced one,
// mirroring what a regenerated dataset missing a media class looks like.
func dropAssets(c *catalogues, match func(manifestAsset) bool) {
	gone := map[string]bool{}
	var keptAssets []manifestAsset
	for _, a := range c.Assets {
		if match(a) {
			gone[a.ID] = true
			continue
		}
		keptAssets = append(keptAssets, a)
	}
	var keptPosts []manifestPost
	for _, p := range c.Posts {
		var ids []string
		for _, aid := range p.AssetIDs {
			if !gone[aid] {
				ids = append(ids, aid)
			}
		}
		if len(ids) == 0 {
			continue
		}
		p.AssetIDs = ids
		keptPosts = append(keptPosts, p)
	}
	c.Assets, c.Posts = keptAssets, keptPosts
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func containsDim(ds []dim, want dim) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}
