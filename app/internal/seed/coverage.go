// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Coverage-profile selection for the `aa seed` DB-direct loader (#768).
//
// CI does not need catalogue SCALE, it needs catalogue COVERAGE: every
// file type, the metadata that hangs off it, and the relations between
// things. The full site_a seed is ~1,950 assets and costs ~14 minutes of
// a 20-minute UI job; the Playwright suite it feeds is under 3 minutes.
//
// The pre-existing --limit-per-extension shrink selects on the wrong
// axis. It keeps N assets per file_extension and then keeps a post only
// if EVERY one of its assets survived, so posts are collateral damage:
// against site_a, N=8 keeps 100 assets and 64 of 859 posts, N=3 keeps 44
// and 32 with only 5 of the 7 real collections holding anything. Posts
// are where the relations live (author, team, collection, tags,
// multi-asset ordering, comments), so shedding them is precisely
// shedding what needs covering. It does not even buy wall clock: the
// giant videos are the sole members of their extensions, so they survive
// every N and the byte cost barely moves (2.99 GiB at N=3).
//
// This selects on posts and closes over their assets, which inverts the
// problem: every kept post is whole by construction, and in site_a every
// asset is reachable from at least one post, so post-first selection can
// reach the entire catalogue.
//
// Three stages:
//
//  1. Greedy set-cover over posts against a universe of dimensions
//     derived from the catalogue itself (extension, asset type, states,
//     tiers, collections, teams, post kinds, typed field codes, and the
//     relation-bearing classes: has-companions, has-tags, has-review).
//     Cover is cheap — site_a's 113 dimensions fall to 18 posts.
//  2. A depth floor, which is what actually sizes the seed. Minimum
//     cover is degenerate for UI work: masonry, grid and pagination
//     specs need more than one tile per surface. So: at least K posts
//     per collection and K assets per extension, bounded by what the
//     catalogue actually holds.
//  3. Add-back for anything no post can reach, as standalone assets.
//     site_a needs none today; a future dataset might.
//
// Every stage prefers the CHEAPEST candidate that covers the same
// thing, because the seed's wall clock goes on bytes, not rows: site_a's
// twenty largest assets are 85% of its 4.88 GiB.
//
// Then it VERIFIES and returns an error on a gap, rather than warning.
// Per ADR 0068 a fixture must be able to exercise what it claims to
// cover, and a silently-degraded CI seed is the exact failure class that
// let an untextured 3D catalogue ship green twice (#750, #753). Two
// tiers of check, and the second is the one with teeth:
//
//   - the fixture-derived universe must come out fully covered;
//   - requiredDims — declared HERE, independent of any fixture — must be
//     present. That is what fails when someone regenerates the dataset
//     without fonts, or without external-texture models, instead of CI
//     going quietly green against a catalogue that cannot exercise the
//     features its specs assert on.
package seed

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mscrnt/artist-alley/app/internal/preview/format3d"
)

// Seed profiles. ProfileFull is the demo path and the default: the whole
// catalogue, nothing dropped.
const (
	ProfileFull = "full"
	ProfileCI   = "ci"

	// defaultCoverageDepth sizes the CI seed. Measured against site_a
	// (1,947 assets / 859 posts): depth 8 selects ~100 posts / 157
	// assets — 8% of the catalogue — covering all 113 catalogue
	// dimensions, every one of the 18 extensions, and all 7 collections
	// that hold any content, while still giving every collection and
	// every common extension enough tiles for the grid and pagination
	// specs.
	defaultCoverageDepth = 8
)

// dim is one coverage dimension: a class and a value within it.
type dim struct {
	Class string
	Value string
}

func (d dim) String() string { return d.Class + "=" + d.Value }

// Dimension classes. Named rather than inlined so a typo is a compile
// error and the required-set below cannot drift from the builders.
const (
	dimExt        = "ext"           // file_extension
	dimAssetType  = "asset.type"    // image / video / audio / 3d / document / font
	dimAssetSens  = "asset.sens"    // sensitivity tier
	dimAssetArch  = "asset.archive" // archive state
	dimAssetState = "asset.state"   // workflow state
	dimAssetColl  = "asset.collection"
	dimAssetTeam  = "asset.team"
	dimField      = "field"      // typed field code carrying a value
	dimCompanion  = "companions" // extension whose model declares external files
	dimRel        = "rel"        // relation-bearing classes (tags, review notes)
	dimPostState  = "post.state"
	dimPostSens   = "post.sens"
	dimPostKind   = "post.kind"
	dimPostColl   = "post.collection"
	dimPostTeam   = "post.team"
	dimPostMulti  = "post.multi" // post holds more than one asset
	dimPostMixed  = "post.mixed" // post mixes asset types
)

// requiredDims is the declared floor: coverage classes the UI suite
// asserts on, stated independently of whatever catalogue is mounted. A
// catalogue that cannot supply one of these fails the seed.
//
// Deliberately classes, not counts, and deliberately not every dimension
// the current dataset happens to hold — a dataset refresh that drops a
// long tail (say the single .mkv) should not break CI, but one that
// drops all fonts, or all external-texture models, must.
var requiredDims = []dim{
	// Every media pipeline the viewer has a renderer for.
	{dimAssetType, "image"},
	{dimAssetType, "video"},
	{dimAssetType, "audio"},
	{dimAssetType, "3d"},
	{dimAssetType, "document"},
	{dimAssetType, "font"},

	// The #750/#753 arc: models whose textures/buffers are SEPARATE
	// files. A selection that is not companion-aware guts these
	// silently, and the companion tests still pass against a catalogue
	// with no companions in it — they assert on rows that were never
	// meant to exist. Each container format parses differently, so each
	// is its own requirement.
	{dimCompanion, "glb"},
	{dimCompanion, "gltf"},
	{dimCompanion, "obj"},
	{dimCompanion, "fbx"},

	// Relations. Without these the seed is a pile of files.
	{dimRel, "tags"},
	{dimRel, "review"},       // review_notes + reviewer => seeded comments
	{dimRel, "field_values"}, // typed metadata on the asset
	{dimPostMulti, "true"},   // multi-asset post => ordering, playlist
	{dimPostMixed, "true"},   // mixed-type post => cross-renderer playlist
}

// coverageReport is what a profile run tallies, for the log line and for
// tests to assert against.
type coverageReport struct {
	Universe      int
	Covered       int
	Uncovered     []dim
	MissingReq    []dim
	GreedyPosts   int
	Posts         int
	Assets        int
	PostsBefore   int
	AssetsBefore  int
	Depth         int
	PerCollection map[string]int
	PerExtension  map[string]int
	Companions    int // selected assets that declare external files
	AddedOrphans  int
	// Bytes is what the seeder will actually read + hash + re-write.
	// Reported because it, not the row count, is what the seed step
	// spends its wall clock on.
	Bytes       int64
	BytesBefore int64
	// EmptyCollections are catalogue collections with no content in the
	// FULL catalogue either. They are empty in the demo seed too; listed
	// so a reader does not read them as damage this profile did.
	EmptyCollections []string
}

// Summary renders the coverage report for stdout, where whoever ran the
// seed (or whoever is reading a CI log) is looking. The structured log
// line carries the same facts, but a JSON handler at INFO is not where
// an operator finds out what their fixture can and cannot exercise.
func (r *coverageReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n=== seed coverage profile (depth %d) ===\n", r.Depth)
	fmt.Fprintf(&b, "  posts    %d / %d\n", r.Posts, r.PostsBefore)
	fmt.Fprintf(&b, "  assets   %d / %d\n", r.Assets, r.AssetsBefore)
	fmt.Fprintf(&b, "  bytes    %s / %s\n", humanBytes(r.Bytes), humanBytes(r.BytesBefore))
	fmt.Fprintf(&b, "  coverage %d / %d dimensions (%d from set-cover, rest from depth floor)\n",
		r.Covered, r.Universe, r.GreedyPosts)
	fmt.Fprintf(&b, "  companion-bearing assets  %d\n", r.Companions)
	fmt.Fprintf(&b, "  assets per extension   %s\n", countLine(r.PerExtension))
	fmt.Fprintf(&b, "  posts per collection   %s\n", countLine(r.PerCollection))
	if r.AddedOrphans > 0 {
		fmt.Fprintf(&b, "  added %d asset(s) no post reaches\n", r.AddedOrphans)
	}
	if len(r.EmptyCollections) > 0 {
		fmt.Fprintf(&b, "  NOTE: %d collection(s) hold no catalogue content at all and are "+
			"empty in the FULL seed too, not because of this profile: %s\n",
			len(r.EmptyCollections), strings.Join(r.EmptyCollections, ", "))
	}
	if len(r.MissingReq) > 0 {
		fmt.Fprintf(&b, "  FAIL: required dimensions absent from the catalogue: %s\n",
			dimList(r.MissingReq))
	}
	if len(r.Uncovered) > 0 {
		fmt.Fprintf(&b, "  FAIL: uncovered catalogue dimensions: %s\n", dimList(r.Uncovered))
	}
	return b.String()
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func countLine(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

// assetDims returns the dimensions one asset contributes. companion is
// whether the file declares external companions on disk.
func assetDims(a manifestAsset, companion bool) []dim {
	out := []dim{
		{dimExt, a.FileExtension},
		{dimAssetType, a.AssetType},
		{dimAssetSens, a.SensitivityTier},
		{dimAssetArch, a.ArchiveState},
		{dimAssetState, a.WorkflowState},
		{dimAssetTeam, a.TeamName},
	}
	if a.CollectionName != "" {
		out = append(out, dim{dimAssetColl, a.CollectionName})
	}
	codes := make([]string, 0, len(a.FieldValues))
	for k := range a.FieldValues {
		codes = append(codes, k)
	}
	sort.Strings(codes) // map order is not deterministic; selection must be
	for _, k := range codes {
		out = append(out, dim{dimField, k})
	}
	if len(a.FieldValues) > 0 {
		out = append(out, dim{dimRel, "field_values"})
	}
	if len(a.Tags) > 0 {
		out = append(out, dim{dimRel, "tags"})
	}
	if strings.TrimSpace(a.ReviewNotes) != "" && a.ReviewerUsername != "" {
		out = append(out, dim{dimRel, "review"})
	}
	if companion {
		out = append(out, dim{dimCompanion, a.FileExtension})
	}
	return out
}

// postDims returns the dimensions one post contributes on its own,
// before its assets are folded in.
func postDims(p manifestPost) []dim {
	out := []dim{
		{dimPostState, p.WorkflowState},
		{dimPostSens, p.SensitivityTier},
		{dimPostKind, p.PostKind},
		{dimPostTeam, p.TeamName},
		{dimPostMulti, boolValue(len(p.AssetIDs) > 1)},
		{dimPostMixed, boolValue(p.IsMixedType)},
	}
	if p.CollectionName != "" {
		out = append(out, dim{dimPostColl, p.CollectionName})
	}
	return out
}

func boolValue(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// resolveCompanionAssets reports which manifest assets declare external
// companion files that exist on disk. Only model containers can, so only
// those are opened; over the full site_a catalogue this is ~2s of header
// reads, which is noise against a seed measured in minutes.
//
// A resolve error is treated as "no companions" and left to the seed's
// own soft-fail logging — selection must not die on one bad model.
func resolveCompanionAssets(assets []manifestAsset, siteRoot string) map[string]bool {
	out := make(map[string]bool, len(assets))
	for _, a := range assets {
		switch strings.ToLower(a.FileExtension) {
		case "gltf", "glb", "obj", "fbx":
		default:
			continue
		}
		found, _, err := format3d.ResolveCompanions(filepath.Join(siteRoot, a.FilePath))
		if err == nil && len(found) > 0 {
			out[a.ID] = true
		}
	}
	return out
}

// applyCoverageProfile shrinks the catalogue to a coverage-complete
// subset: greedy set-cover over posts, then a depth floor of `depth`
// posts per collection and `depth` assets per extension, then add-back
// for anything no post reaches. Byte cost is the tiebreak throughout.
//
// It returns an error — leaving the catalogue UNCHANGED — if the
// selection leaves a universe dimension uncovered, or if the catalogue
// cannot supply a requiredDims entry. The report comes back either way,
// so the caller can print what was and was not covered before it dies.
func (c *catalogues) applyCoverageProfile(depth int, log *slog.Logger) (*coverageReport, error) {
	if depth < 1 {
		depth = 1
	}
	rep := &coverageReport{
		Depth:         depth,
		AssetsBefore:  len(c.Assets),
		PostsBefore:   len(c.Posts),
		PerCollection: map[string]int{},
		PerExtension:  map[string]int{},
	}
	for _, a := range c.Assets {
		rep.BytesBefore += a.FileSizeBytes
	}

	companions := resolveCompanionAssets(c.Assets, c.SiteRoot)

	byID := make(map[string]int, len(c.Assets))
	for i, a := range c.Assets {
		byID[a.ID] = i
	}
	assetDimCache := make([][]dim, len(c.Assets))
	for i, a := range c.Assets {
		assetDimCache[i] = assetDims(a, companions[a.ID])
	}

	// Universe + per-post dimension sets. A post's set is its own
	// dimensions unioned with those of every asset it holds, because
	// selecting the post selects the assets.
	universe := map[dim]bool{}
	for _, d := range assetDimCache {
		for _, x := range d {
			universe[x] = true
		}
	}
	postSets := make([]map[dim]bool, len(c.Posts))
	for i, p := range c.Posts {
		set := map[dim]bool{}
		for _, x := range postDims(p) {
			set[x] = true
			universe[x] = true
		}
		for _, aid := range p.AssetIDs {
			ai, ok := byID[aid]
			if !ok {
				continue // post references an asset the manifest lost
			}
			for _, x := range assetDimCache[ai] {
				set[x] = true
			}
		}
		postSets[i] = set
	}
	rep.Universe = len(universe)

	// Byte cost per post. Selection is cheap in rows and expensive in
	// BYTES: the seeder reads, hashes and re-writes every selected file,
	// and site_a's byte weight is wildly skewed — the twenty largest
	// assets are 85% of 4.88 GiB, one .mkv alone is 1.1 GiB. A
	// selection that ignores size picks 11% of the assets and still
	// drags in 82% of the bytes, which on a CI runner reading the
	// dataset off a network mount is most of the wall clock. So size is
	// a tiebreak everywhere below: among candidates that cover the same
	// thing, take the cheapest. Coverage does not care how big a file
	// is; the clock does.
	postBytes := make([]int64, len(c.Posts))
	for i, p := range c.Posts {
		for _, aid := range p.AssetIDs {
			if ai, ok := byID[aid]; ok {
				postBytes[i] += c.Assets[ai].FileSizeBytes
			}
		}
	}
	// cheapestFirst orders post indices by byte cost, then by catalogue
	// position — a total order, so the depth floor's fills are
	// deterministic without depending on map iteration.
	cheapestFirst := make([]int, len(c.Posts))
	for i := range cheapestFirst {
		cheapestFirst[i] = i
	}
	sort.SliceStable(cheapestFirst, func(x, y int) bool {
		return postBytes[cheapestFirst[x]] < postBytes[cheapestFirst[y]]
	})
	// Same ordering over assets, for the add-back stages, which pick
	// individual assets rather than posts.
	cheapestAssets := make([]int, len(c.Assets))
	for i := range cheapestAssets {
		cheapestAssets[i] = i
	}
	sort.SliceStable(cheapestAssets, func(x, y int) bool {
		return c.Assets[cheapestAssets[x]].FileSizeBytes <
			c.Assets[cheapestAssets[y]].FileSizeBytes
	})

	// --- stage 1: greedy set-cover over posts ---------------------------
	selected := make([]bool, len(c.Posts))
	covered := map[dim]bool{}
	for {
		best, bestGain, bestCost := -1, 0, int64(0)
		for _, i := range cheapestFirst {
			if selected[i] {
				continue
			}
			gain := 0
			for d := range postSets[i] {
				if !covered[d] {
					gain++
				}
			}
			if gain == 0 {
				continue
			}
			// Highest gain wins; ties go to the cheapest post, then to
			// the earlier one in cheapestFirst order.
			if gain > bestGain || (gain == bestGain && postBytes[i] < bestCost) {
				best, bestGain, bestCost = i, gain, postBytes[i]
			}
		}
		if best < 0 {
			break
		}
		selected[best] = true
		for d := range postSets[best] {
			covered[d] = true
		}
		rep.GreedyPosts++
	}

	// --- stage 2: depth floor -------------------------------------------
	// Tallies are recomputed from the selection rather than tracked
	// incrementally, so they cannot drift from what is actually kept.
	collCount, extCount := c.tally(selected, byID)

	// K posts per collection, for every collection that HAS posts in the
	// catalogue. A collection with no catalogue content is empty in the
	// full seed too and is reported, not filled.
	collOrder := []string{}
	collSeen := map[string]bool{}
	for _, p := range c.Posts {
		if p.CollectionName != "" && !collSeen[p.CollectionName] {
			collSeen[p.CollectionName] = true
			collOrder = append(collOrder, p.CollectionName)
		}
	}
	for _, name := range collOrder {
		for _, i := range cheapestFirst {
			if collCount[name] >= depth {
				break
			}
			if selected[i] || c.Posts[i].CollectionName != name {
				continue
			}
			selected[i] = true
			collCount, extCount = c.tally(selected, byID)
		}
	}

	// K assets per extension, bounded by what the catalogue holds — the
	// long tail (.md, .gif, .mkv, .mov) has exactly one asset each, and
	// demanding K of them would be an unsatisfiable floor rather than a
	// coverage guarantee.
	extAvail := map[string]int{}
	extOrder := []string{}
	for _, a := range c.Assets {
		if extAvail[a.FileExtension] == 0 {
			extOrder = append(extOrder, a.FileExtension)
		}
		extAvail[a.FileExtension]++
	}
	for _, ext := range extOrder {
		want := depth
		if extAvail[ext] < want {
			want = extAvail[ext]
		}
		for _, i := range cheapestFirst {
			if extCount[ext] >= want {
				break
			}
			if selected[i] {
				continue
			}
			holds := false
			for _, aid := range c.Posts[i].AssetIDs {
				if ai, ok := byID[aid]; ok && c.Assets[ai].FileExtension == ext {
					holds = true
					break
				}
			}
			if !holds {
				continue
			}
			selected[i] = true
			collCount, extCount = c.tally(selected, byID)
		}
	}

	// --- materialise ----------------------------------------------------
	keptAssetIDs := map[string]bool{}
	var keptPosts []manifestPost
	for i, p := range c.Posts {
		if !selected[i] {
			continue
		}
		keptPosts = append(keptPosts, p)
		for _, aid := range p.AssetIDs {
			if _, ok := byID[aid]; ok {
				keptAssetIDs[aid] = true
			}
		}
	}

	// --- stage 3: add back extensions no post can reach ------------------
	// site_a has none (every asset sits in at least one post), but a
	// catalogue where some extension exists only outside the post graph
	// would otherwise lose that extension entirely.
	for _, ext := range extOrder {
		if extCount[ext] > 0 {
			continue
		}
		want := depth
		if extAvail[ext] < want {
			want = extAvail[ext]
		}
		added := 0
		for _, ai := range cheapestAssets {
			if added >= want {
				break
			}
			a := c.Assets[ai]
			if a.FileExtension != ext || keptAssetIDs[a.ID] {
				continue
			}
			keptAssetIDs[a.ID] = true
			added++
			rep.AddedOrphans++
		}
		extCount[ext] += added
	}

	// Anything STILL uncovered can only live on an asset outside the
	// selected post graph — a catalogue where, say, one team's work is
	// entirely post-less. Pull one such asset in per gap rather than
	// failing: the seeder inserts assets independently of posts, so an
	// asset with no post is a valid (if thin) fixture row, and a
	// catalogue that genuinely holds the thing should not be rejected
	// for holding it in an awkward place.
	have := map[dim]bool{}
	for i, a := range c.Assets {
		if keptAssetIDs[a.ID] {
			for _, d := range assetDimCache[i] {
				have[d] = true
			}
		}
	}
	for i, p := range c.Posts {
		if selected[i] {
			for _, d := range postDims(p) {
				have[d] = true
			}
		}
	}
	for _, d := range sortedDims(universe) {
		if have[d] {
			continue
		}
		for _, i := range cheapestAssets {
			a := c.Assets[i]
			if keptAssetIDs[a.ID] || !hasDim(assetDimCache[i], d) {
				continue
			}
			keptAssetIDs[a.ID] = true
			rep.AddedOrphans++
			extCount[a.FileExtension]++
			for _, x := range assetDimCache[i] {
				have[x] = true
			}
			break
		}
	}

	var keptAssets []manifestAsset
	for _, a := range c.Assets {
		if keptAssetIDs[a.ID] {
			keptAssets = append(keptAssets, a)
		}
	}

	// --- verify ----------------------------------------------------------
	final := map[dim]bool{}
	for _, a := range keptAssets {
		for _, d := range assetDims(a, companions[a.ID]) {
			final[d] = true
		}
		if companions[a.ID] {
			rep.Companions++
		}
		rep.Bytes += a.FileSizeBytes
	}
	for _, p := range keptPosts {
		for _, d := range postDims(p) {
			final[d] = true
		}
	}
	for d := range universe {
		if !final[d] {
			rep.Uncovered = append(rep.Uncovered, d)
		}
	}
	for _, d := range requiredDims {
		if !final[d] {
			rep.MissingReq = append(rep.MissingReq, d)
		}
	}
	sortDims(rep.Uncovered)
	sortDims(rep.MissingReq)
	rep.Covered = len(universe) - len(rep.Uncovered)

	for _, name := range collectionNames(c.Collections) {
		if !collSeen[name] && !assetCollSeen(keptAssets, name) {
			rep.EmptyCollections = append(rep.EmptyCollections, name)
		}
	}
	rep.PerCollection = collCount
	rep.PerExtension = extCount
	rep.Posts = len(keptPosts)
	rep.Assets = len(keptAssets)

	if log != nil {
		log.Info("seed.coverage_profile",
			"depth", depth,
			"universe", rep.Universe, "covered", rep.Covered,
			"greedy_posts", rep.GreedyPosts,
			"posts", fmt.Sprintf("%d/%d", rep.Posts, rep.PostsBefore),
			"assets", fmt.Sprintf("%d/%d", rep.Assets, rep.AssetsBefore),
			"companion_assets", rep.Companions,
			"extensions", len(rep.PerExtension),
			"collections_with_posts", len(rep.PerCollection),
			"collections_empty_in_catalogue", rep.EmptyCollections,
			"added_unreachable", rep.AddedOrphans)
	}

	// Fail, do not warn. A CI seed that quietly dropped a coverage class
	// produces a green suite that proves nothing about that class.
	if len(rep.MissingReq) > 0 {
		return rep, fmt.Errorf(
			"seed coverage profile: catalogue cannot supply %d required dimension(s): %s "+
				"(the dataset at %s does not contain them — a suite run against this seed "+
				"would pass without exercising them)",
			len(rep.MissingReq), dimList(rep.MissingReq), c.SiteRoot)
	}
	if len(rep.Uncovered) > 0 {
		return rep, fmt.Errorf(
			"seed coverage profile: selection left %d catalogue dimension(s) uncovered: %s",
			len(rep.Uncovered), dimList(rep.Uncovered))
	}

	c.Assets = keptAssets
	c.Posts = keptPosts
	return rep, nil
}

// tally recomputes posts-per-collection and assets-per-extension for the
// current selection.
func (c *catalogues) tally(selected []bool, byID map[string]int) (map[string]int, map[string]int) {
	coll := map[string]int{}
	ext := map[string]int{}
	seen := map[string]bool{}
	for i, p := range c.Posts {
		if !selected[i] {
			continue
		}
		if p.CollectionName != "" {
			coll[p.CollectionName]++
		}
		for _, aid := range p.AssetIDs {
			ai, ok := byID[aid]
			if !ok || seen[aid] {
				continue
			}
			seen[aid] = true
			ext[c.Assets[ai].FileExtension]++
		}
	}
	return coll, ext
}

func collectionNames(cs []catCollection) []string {
	out := make([]string, 0, len(cs))
	for _, x := range cs {
		out = append(out, x.Name)
	}
	return out
}

func assetCollSeen(assets []manifestAsset, name string) bool {
	for _, a := range assets {
		if a.CollectionName == name {
			return true
		}
	}
	return false
}

// sortedDims flattens a dimension set into a deterministic order, so a
// selection cannot vary with Go's map iteration.
func sortedDims(set map[dim]bool) []dim {
	out := make([]dim, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sortDims(out)
	return out
}

func hasDim(ds []dim, want dim) bool {
	for _, d := range ds {
		if d == want {
			return true
		}
	}
	return false
}

func sortDims(ds []dim) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Class != ds[j].Class {
			return ds[i].Class < ds[j].Class
		}
		return ds[i].Value < ds[j].Value
	})
}

func dimList(ds []dim) string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.String())
	}
	return strings.Join(out, ", ")
}
