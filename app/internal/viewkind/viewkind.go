// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package viewkind is the server-side twin of the frontend's
// `kindForAsset` (#1166).
//
// # Why this exists at all
//
// The browse card's kind badge — the glyph in the tile's corner that
// says "this is a video" — is not read off a column. It is DERIVED, in
// the browser, by `kindForAsset` in
// web/src/lib/components/viewers/controller.ts, from two inputs: the
// asset's `asset_type` ref and its `file_extension`. The feed's new
// `?kind=` filter has to select exactly the posts whose badge shows the
// requested kind, so the server needs that same derivation.
//
// # Why NOT `assets.asset_type` on its own
//
// It was the obvious candidate and it is wrong, provably so against the
// seeded corpus. `asset_type` is a COARSER vocabulary than the badge's:
// ref 2 is "Document", and the badge splits that into `ebook` (an
// .epub) and `doc` (a .txt). Filtering on the ref would therefore have
// returned two different badges under one label — the exact thing the
// acceptance criterion ("every visible badge matches the filter")
// forbids. The reverse mismatch exists too: `asset_type` is not always
// set, while an extension almost always is.
//
// So the derivation is mirrored here rather than approximated. It is a
// MIRROR and not a second opinion: [TestKindSetsMatchFrontend] parses
// the TypeScript source and fails if any set, the override map, or the
// vocabulary drifts. That test is the whole reason this file is
// allowed to exist — a hand-copied extension table with no oracle is a
// drift bomb (see preview/dispatch's package comment for the last one).
//
// # Why not the preview/dispatch sets
//
// dispatch answers "which preview JOB renders this file"; this answers
// "which VIEWER opens it, and therefore which glyph the badge shows".
// They are close and deliberately different: dispatch puts .m4b in
// AudioExts (the audio worker extracts its chapters), the viewer calls
// it an `audiobook`; dispatch has no notion of `doc` at all. Pointing
// the filter at dispatch would have made the badge and the filter
// disagree on every audiobook and every text file.
package viewkind

import (
	"sort"
	"strings"
)

// Kind is one member of the frontend's `ViewKind` union. The wire
// vocabulary of `?kind=` is exactly these strings, so a value the
// browser can render is a value the server can filter by, spelled the
// same way in both places.
type Kind string

const (
	KindImage       Kind = "image"
	KindVideo       Kind = "video"
	KindPDF         Kind = "pdf"
	KindAudio       Kind = "audio"
	KindSequence    Kind = "sequence"
	KindFont        Kind = "font"
	KindSprite      Kind = "sprite"
	Kind3D          Kind = "3d"
	KindEbook       Kind = "ebook"
	KindDoc         Kind = "doc"
	KindAudiobook   Kind = "audiobook"
	KindArchive     Kind = "archive"
	KindPlaceholder Kind = "placeholder"
)

// All is the vocabulary, in the order the TypeScript union declares it.
// The parity test asserts this list against that union, so a kind added
// there and not here is a red test rather than a silently unfilterable
// kind.
func All() []Kind {
	return []Kind{
		KindImage, KindVideo, KindPDF, KindAudio, KindSequence, KindFont,
		KindSprite, Kind3D, KindEbook, KindDoc, KindAudiobook, KindArchive,
		KindPlaceholder,
	}
}

// Valid reports whether s names a kind in the vocabulary.
func Valid(s string) bool {
	for _, k := range All() {
		if string(k) == s {
			return true
		}
	}
	return false
}

// The extension sets, mirroring controller.ts one for one. Kept as
// slices rather than maps so the parity test can compare MEMBERSHIP and
// the SQL builder can hand Postgres an array; lookup goes through
// [kindForExtension], which builds its index once.
//
// Every list below is verbatim from the TypeScript, INCLUDING entries
// that look redundant. `ts` is in both videoExts and docExts, and video
// wins because it is checked first — see [KindForExtension]'s ordering
// note.
var (
	ebookExts = []string{"epub"}

	audiobookExts = []string{"m4b", "aax"}

	imageExts = []string{
		"jpg", "jpeg", "png", "gif", "webp", "bmp", "tiff", "tif",
		"avif", "heic", "heif", "svg",
		"hdr", "exr", "pic",
		"cr2", "nef", "dng", "arw", "rw2",
		"eps", "ps", "psd", "psb",
		"mobi",
		"cbz", "cbr", "cb7",
	}

	videoExts = []string{
		"mp4", "mov", "mkv", "webm", "avi", "wmv", "mpg", "mpeg", "3gp",
		"flv", "m4v", "ts", "lrv", "insv", "mts", "m2ts", "vob", "f4v",
		"mxf",
	}

	audioExts = []string{
		"mp3", "wav", "flac", "ogg", "oga", "m4a", "aac", "opus",
	}

	pdfExts = []string{"pdf"}

	fontExts = []string{"ttf", "otf", "ttc", "otc", "woff", "woff2"}

	modelExts = []string{
		"glb", "gltf", "obj", "fbx", "blend", "mview",
		"dae", "ply", "stl", "3ds", "x3d", "wrl",
		"usd", "usda", "usdc", "usdz", "abc",
		"md2", "md3", "mdl", "ms3d",
		"mb", "ma", "max",
	}

	docExts = []string{
		"txt", "log", "csv", "tsv",
		"md", "markdown", "mdx", "rst", "adoc", "org",
		"json", "jsonc", "yaml", "yml", "toml", "ini", "cfg", "conf",
		"env", "properties",
		"sh", "bash", "zsh", "fish", "ps1",
		"makefile", "mk", "dockerfile", "gitignore", "gitattributes",
		"py", "pyi", "rb", "lua", "pl", "pm",
		"js", "mjs", "cjs", "jsx", "ts", "tsx",
		"go", "rs", "java", "kt", "kts", "scala", "swift", "dart",
		"c", "h", "cpp", "cc", "cxx", "hpp", "hh", "m", "mm", "cs",
		"php", "hs", "erl", "ex", "exs", "clj", "cljs", "edn",
		"html", "htm", "css", "scss", "sass", "less",
		"vue", "svelte",
		"sql", "graphql", "gql", "xml", "plist",
		"patch", "diff",
	}

	archiveExts = []string{
		"zip", "jar", "war", "ear", "apk", "ipa",
		"7z", "rar",
		"tar", "tgz", "tbz2", "txz",
	}
)

// extensionOrder is the PRECEDENCE the frontend's `kindForExtension`
// evaluates, and the order is load-bearing: the sets overlap (`ts` is a
// video container and a TypeScript source; `m4b` is an audio container
// and an audiobook), so the first match decides. Stating the order once,
// as data, is what lets [KindForExtension] and [ExtensionsFor] agree
// without either restating it.
var extensionOrder = []struct {
	kind Kind
	exts []string
}{
	{KindEbook, ebookExts},
	{KindAudiobook, audiobookExts},
	{KindImage, imageExts},
	{KindVideo, videoExts},
	{KindAudio, audioExts},
	{KindPDF, pdfExts},
	{KindFont, fontExts},
	{Kind3D, modelExts},
	{KindDoc, docExts},
	{KindArchive, archiveExts},
}

// assetTypeKind is the frontend's ASSET_TYPE_KIND: the handful of
// `asset_types` refs that OVERRIDE the extension, because the extension
// cannot tell them apart from something else. A sprite atlas is a PNG
// and a texture is a PNG; only the ref separates them.
var assetTypeKind = map[int64]Kind{
	6:  KindArchive,
	11: KindAudiobook,
	13: KindSprite,
}

// extIndex is the flattened lookup, built once. Precedence is applied
// at build time — the first set to claim an extension keeps it — so a
// map lookup gives the same answer the ordered scan would.
var extIndex = func() map[string]Kind {
	m := make(map[string]Kind, 256)
	for _, group := range extensionOrder {
		for _, e := range group.exts {
			if _, taken := m[e]; !taken {
				m[e] = group.kind
			}
		}
	}
	return m
}()

// Normalize lowercases an extension and strips a leading dot, matching
// the frontend's `ext.toLowerCase().replace(/^\./, ”)`.
func Normalize(ext string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
}

// KindForExtension mirrors the frontend's `kindForExtension`. An empty
// or unrecognised extension is [KindPlaceholder] — the resolver's own
// "I could not tell" answer, not an error.
func KindForExtension(ext string) Kind {
	e := Normalize(ext)
	if e == "" {
		return KindPlaceholder
	}
	if k, ok := extIndex[e]; ok {
		return k
	}
	return KindPlaceholder
}

// ForAsset mirrors the frontend's `kindForAsset`: the asset_type ref
// wins when it is one of the three overriding refs, otherwise the
// extension decides.
func ForAsset(assetType *int64, ext *string) Kind {
	if assetType != nil {
		if k, ok := assetTypeKind[*assetType]; ok {
			return k
		}
	}
	if ext == nil {
		return KindPlaceholder
	}
	return KindForExtension(*ext)
}

// ParseList reads the `?kind=` parameter: a comma-joined list, matching
// how `/search?types=` already spells a multi-value narrowing filter on
// this codebase's wire.
//
// ⚠️ AN UNRECOGNISED NAME IS DROPPED, NOT IGNORED, and the difference
// decides whether the filter can widen. `?kind=nonsense` parses to an
// EMPTY-but-PRESENT selection, which selects no posts, rather than to
// "no filter", which would have served the whole feed under a label
// promising one kind. `ok` reports "the caller asked for a kind filter"
// so the caller can tell that case from an absent parameter; the
// returned slice is what to filter BY.
//
// An empty or whitespace-only value is not a filter at all (ok=false) —
// that is what the UI sends when every box is ticked, and "all types"
// means no conjunct.
func ParseList(s string) (kinds []Kind, ok bool) {
	if strings.TrimSpace(s) == "" {
		return nil, false
	}
	seen := make(map[Kind]struct{}, 8)
	out := make([]Kind, 0, 8)
	for _, part := range strings.Split(s, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		k := Kind(name)
		if !Valid(name) {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out, true
}

// KnownExtensions is every extension the resolver recognises, sorted.
//
// Nothing in the query path needs it since #1251 moved the filter onto
// [KindSQL], which resolves a row's kind rather than testing it against
// compiled sets. It survives as the VOCABULARY, which is what the parity
// tests enumerate: TestKindSQLMirrorsResolver walks it to prove the
// rendered branch table claims each extension exactly once.
func KnownExtensions() []string {
	out := make([]string, 0, len(extIndex))
	for e := range extIndex {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
