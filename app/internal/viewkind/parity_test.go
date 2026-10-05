// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The drift oracle for the mirrored kind resolver (#1166).
//
// package viewkind restates a derivation that lives in TypeScript, and
// a hand-copied extension table with no oracle is exactly the drift
// bomb preview/dispatch's package comment was written about. So the
// mirror is held to its source the same way sitetext's embedded
// catalogue is: by reading the frontend file and comparing.
//
// If one of these goes red, the fix is not to edit the expectation. It
// is to bring this package back in line with controller.ts — or, if the
// frontend genuinely gained a kind, to add it here AND to the `?kind=`
// vocabulary in openapi.yaml, which is the whole point of failing here
// rather than shipping a filter that silently cannot select the new
// kind.

package viewkind

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// controllerTS is the frontend file this package mirrors.
const controllerTS = "../../../web/src/lib/components/viewers/controller.ts"

func readController(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(controllerTS))
	if err != nil {
		// Same escape hatch sitetext's catalogue guard uses: the Go
		// module is built from trees that do not always carry web/.
		// CI builds from the repo root, where it does.
		t.Skipf("frontend controller not reachable from here (%v)", err)
	}
	return string(raw)
}

// stripComments removes // line comments so the set literals — which
// are heavily commented — parse cleanly. Block comments do not appear
// inside the literals; the doc comments above them are outside the
// slices we cut.
func stripComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

var quoted = regexp.MustCompile(`'([^']*)'`)

// tsSet extracts `const NAME = new Set([ 'a', 'b' ]);` from the source.
func tsSet(t *testing.T, src, name string) []string {
	t.Helper()
	head := "const " + name + " = new Set(["
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("controller.ts: could not find %s — the mirror cannot be verified, so it must not be trusted", name)
	}
	rest := src[i+len(head):]
	j := strings.Index(rest, "])")
	if j < 0 {
		t.Fatalf("controller.ts: %s literal is not terminated", name)
	}
	var out []string
	for _, m := range quoted.FindAllStringSubmatch(rest[:j], -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		t.Fatalf("controller.ts: %s parsed as empty", name)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func assertSameSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	g, w := sortedCopy(got), sortedCopy(want)
	if strings.Join(g, ",") == strings.Join(w, ",") {
		return
	}
	inGo := map[string]bool{}
	for _, e := range g {
		inGo[e] = true
	}
	inTS := map[string]bool{}
	for _, e := range w {
		inTS[e] = true
	}
	var onlyGo, onlyTS []string
	for _, e := range g {
		if !inTS[e] {
			onlyGo = append(onlyGo, e)
		}
	}
	for _, e := range w {
		if !inGo[e] {
			onlyTS = append(onlyTS, e)
		}
	}
	t.Errorf("%s has DRIFTED from controller.ts.\n  only in Go: %v\n  only in TS: %v\n"+
		"The badge and the ?kind= filter now disagree for those extensions.",
		name, onlyGo, onlyTS)
}

func TestKindSetsMatchFrontend(t *testing.T) {
	t.Parallel()
	src := stripComments(readController(t))

	for _, c := range []struct {
		tsName string
		got    []string
	}{
		{"EBOOK_EXTS", ebookExts},
		{"AUDIOBOOK_EXTS", audiobookExts},
		{"IMAGE_EXTS", imageExts},
		{"VIDEO_EXTS", videoExts},
		{"AUDIO_EXTS", audioExts},
		{"PDF_EXTS", pdfExts},
		{"FONT_EXTS", fontExts},
		{"MODEL_EXTS", modelExts},
		{"DOC_EXTS", docExts},
		{"ARCHIVE_EXTS", archiveExts},
	} {
		assertSameSet(t, c.tsName, c.got, tsSet(t, src, c.tsName))
	}
}

// TestVocabularyMatchesFrontend pins the ViewKind union itself. A kind
// added there without a home here would be unfilterable, and the UI —
// which renders its checkbox list from the same union's icon map —
// would offer a box that returns an empty feed.
func TestVocabularyMatchesFrontend(t *testing.T) {
	t.Parallel()
	src := readController(t)

	i := strings.Index(src, "export type ViewKind =")
	if i < 0 {
		t.Fatal("controller.ts: ViewKind union not found")
	}
	rest := src[i:]
	j := strings.Index(rest, ";")
	if j < 0 {
		t.Fatal("controller.ts: ViewKind union is not terminated")
	}
	var want []string
	for _, m := range quoted.FindAllStringSubmatch(rest[:j], -1) {
		want = append(want, m[1])
	}

	got := make([]string, 0, len(All()))
	for _, k := range All() {
		got = append(got, string(k))
	}
	assertSameSet(t, "ViewKind", got, want)

	// Order too: All() claims to be the union's declaration order, and
	// the UI's checkbox order is derived from the same list.
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("All() order %v does not match the ViewKind union's declaration order %v", got, want)
	}
}

// TestAssetTypeOverridesMatchFrontend pins ASSET_TYPE_KIND. This is the
// half that decides a sprite atlas is a sprite and not a PNG, and it is
// the half a migration adding an asset type is most likely to change.
func TestAssetTypeOverridesMatchFrontend(t *testing.T) {
	t.Parallel()
	src := stripComments(readController(t))

	head := "const ASSET_TYPE_KIND: Record<number, ViewKind> = {"
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatal("controller.ts: ASSET_TYPE_KIND not found")
	}
	rest := src[i+len(head):]
	j := strings.Index(rest, "}")
	if j < 0 {
		t.Fatal("controller.ts: ASSET_TYPE_KIND is not terminated")
	}
	entry := regexp.MustCompile(`(\d+)\s*:\s*'([^']+)'`)
	want := map[string]string{}
	for _, m := range entry.FindAllStringSubmatch(rest[:j], -1) {
		want[m[1]] = m[2]
	}
	if len(want) == 0 {
		t.Fatal("controller.ts: ASSET_TYPE_KIND parsed as empty")
	}
	got := map[string]string{}
	for ref, k := range assetTypeKind {
		got[itoa(ref)] = string(k)
	}
	if len(got) != len(want) {
		t.Fatalf("ASSET_TYPE_KIND size drifted: go=%v ts=%v", got, want)
	}
	for ref, k := range want {
		if got[ref] != k {
			t.Errorf("ASSET_TYPE_KIND[%s]: go=%q ts=%q", ref, got[ref], k)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestPrecedenceOverlaps pins the ORDER of the checks, which the set
// comparisons above cannot see. Each of these extensions is in two sets
// and the frontend's evaluation order decides which wins; get the order
// wrong and `?kind=doc` starts returning video files.
func TestPrecedenceOverlaps(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		ext  string
		want Kind
	}{
		{"ts", KindVideo},      // video container vs TypeScript source
		{"m4b", KindAudiobook}, // audiobook checked before audio
		{"aax", KindAudiobook},
		{"epub", KindEbook}, // ebook checked before image (mobi is image)
		{"mobi", KindImage},
		{"cbz", KindImage},
		{"eps", KindImage}, // image before doc
		{"ps", KindImage},
		{"m", KindDoc}, // objective-c source, not a container
		{"txt", KindDoc},
		{"png", KindImage},
		{"", KindPlaceholder},
		{"nonesuch", KindPlaceholder},
	} {
		if got := KindForExtension(c.ext); got != c.want {
			t.Errorf("KindForExtension(%q) = %q, want %q", c.ext, got, c.want)
		}
	}
}

// TestForAssetOverrideWins is the asset_type half of the resolver: a
// PNG uploaded as a sprite atlas is a sprite.
func TestForAssetOverrideWins(t *testing.T) {
	t.Parallel()
	png := "png"
	ref := func(v int64) *int64 { return &v }

	if got := ForAsset(ref(13), &png); got != KindSprite {
		t.Errorf("asset_type 13 + png = %q, want sprite", got)
	}
	if got := ForAsset(ref(1), &png); got != KindImage {
		t.Errorf("asset_type 1 + png = %q, want image", got)
	}
	if got := ForAsset(nil, &png); got != KindImage {
		t.Errorf("no asset_type + png = %q, want image", got)
	}
	if got := ForAsset(nil, nil); got != KindPlaceholder {
		t.Errorf("no asset_type + no extension = %q, want placeholder", got)
	}
	// The corpus case that ruled out filtering on asset_type: ref 2
	// ("Document") carries both of these and the badge splits them.
	epub, txt := "epub", "txt"
	if ForAsset(ref(2), &epub) == ForAsset(ref(2), &txt) {
		t.Error("epub and txt resolved to the same kind — the asset_type-ref " +
			"shortcut this package exists to avoid has crept back in")
	}
}

// TestParseListNarrows pins the fail-closed reading of the parameter.
func TestParseListNarrows(t *testing.T) {
	t.Parallel()

	if _, ok := ParseList(""); ok {
		t.Error(`ParseList("") reported a filter; an absent selection must be no conjunct`)
	}
	if _, ok := ParseList("   "); ok {
		t.Error("ParseList(whitespace) reported a filter")
	}

	kinds, ok := ParseList("nonsense")
	if !ok {
		t.Fatal("ParseList(nonsense) must report a PRESENT filter, or an unknown kind widens to the whole feed")
	}
	if len(kinds) != 0 {
		t.Errorf("ParseList(nonsense) = %v, want an empty selection", kinds)
	}
	// What an all-unknown selection then MEANS is decided one layer up,
	// because it is a property of the request and not of the vocabulary:
	// "the caller asked for a kind filter and named nothing renderable"
	// must select no posts rather than every post. See
	// ListPostsPageParams.KindsRequested and
	// TestKindFilter_UnknownKindReturnsNothing.

	kinds, _ = ParseList(" Image , video ,image, ,3D ")
	if len(kinds) != 3 || kinds[0] != KindImage || kinds[1] != KindVideo || kinds[2] != Kind3D {
		t.Errorf("ParseList = %v, want [image video 3d] (case-folded, trimmed, de-duplicated)", kinds)
	}
}

// TestKindSQLMirrorsResolver is the pure-Go half of the oracle over
// [KindSQL]: it reads the branch table back out of the rendered SQL and
// checks it against the Go resolver, extension by extension.
//
// The DB half — TestKindSQLMatchesForAsset in package posts — runs the
// same expression through Postgres. This one needs no database, so a
// vocabulary edit that breaks the correspondence goes red on a laptop
// with no stack up, which is where a table like this actually gets
// edited.
//
// The properties it pins are the ones the arms this replaced had to
// state separately:
//
//   - Every recognised extension appears EXACTLY ONCE. A duplicate would
//     be an unreachable second branch and a silent precedence change.
//   - Each maps to the kind [KindForExtension] gives it, so `ts` reads
//     video and never doc.
//   - The overriding asset_type refs are all present, ahead of the
//     extension table.
//   - `sequence` appears nowhere: no asset resolves to it, which is why
//     the filter needs no "empty selection" special case any more.
func TestKindSQLMirrorsResolver(t *testing.T) {
	t.Parallel()

	rendered := KindSQL("ca")

	extBranch := regexp.MustCompile(`WHEN '([^']+)' THEN '([^']+)'`)
	seen := map[string]int{}
	for _, m := range extBranch.FindAllStringSubmatch(rendered, -1) {
		ext, kind := m[1], m[2]
		seen[ext]++
		if got := KindForExtension(ext); string(got) != kind {
			t.Errorf("KindSQL maps %q to %q; KindForExtension says %q", ext, kind, got)
		}
	}
	for _, e := range KnownExtensions() {
		if seen[e] != 1 {
			t.Errorf("extension %q appears in %d branches of KindSQL, want exactly 1", e, seen[e])
		}
	}
	if len(seen) != len(KnownExtensions()) {
		t.Errorf("KindSQL renders %d extension branches, the resolver knows %d",
			len(seen), len(KnownExtensions()))
	}

	refBranch := regexp.MustCompile(`WHEN ca\.asset_type = (\d+) THEN '([^']+)'`)
	refs := refBranch.FindAllStringSubmatch(rendered, -1)
	if len(refs) != len(assetTypeKind) {
		t.Errorf("KindSQL renders %d asset_type branches, the override map has %d",
			len(refs), len(assetTypeKind))
	}
	firstExt := extBranch.FindStringIndex(rendered)
	for _, m := range refs {
		ref, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatalf("unparseable ref %q", m[1])
		}
		if want := assetTypeKind[ref]; string(want) != m[2] {
			t.Errorf("KindSQL maps asset_type %d to %q, the override map says %q", ref, m[2], want)
		}
		at := strings.Index(rendered, m[0])
		if firstExt != nil && at > firstExt[0] {
			t.Errorf("asset_type branch %q is rendered AFTER the extension table; "+
				"ForAsset checks the overriding ref FIRST and a sprite atlas would read image", m[0])
		}
	}

	// `sequence` is in the vocabulary and no single asset can resolve to
	// it, so the expression must never return it.
	if strings.Contains(rendered, "'"+string(KindSequence)+"'") {
		t.Error("KindSQL can return `sequence`; no single asset resolves to it")
	}
}
