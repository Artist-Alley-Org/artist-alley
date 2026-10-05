// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// #1173 sprint 25a: needsProcessing is held to dispatch.CanPreview.
//
// needsProcessing decides an asset's insert-time processing_status by
// re-enumerating the router's twelve extension sets by hand.
// dispatch.CanPreview is the router's own answer, derived from the same
// sets plus the raster fallback rule, and `preview:missing` renders its
// SQL twin. Three readings of one question is exactly the drift ADR 0093
// decision 3 refuses, so this guard holds the hand-written one to the
// authority over every declared extension, a set of non-members, the
// spellings Normalize folds and the ones it does not, and nil.
//
// ⚠️ It was GREEN on dev at b031044f before this sprint touched
// anything; the brief required stopping if it were not.

package assets

import (
	"sort"
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
)

func TestNeedsProcessing_MatchesCanPreview(t *testing.T) {
	seen := map[string]struct{}{}
	for _, set := range []map[string]struct{}{
		dispatch.ImageExts, dispatch.GifExts, dispatch.VideoExts, dispatch.ModelExts,
		dispatch.AudioExts, dispatch.PDFExts, dispatch.FontExts, dispatch.EbookExts,
		dispatch.EPSExts, dispatch.PSDExts, dispatch.ComicExts, dispatch.TextExts,
		dispatch.ArchiveExts,
	} {
		for e := range set {
			seen[e] = struct{}{}
			seen["."+e] = struct{}{}
			seen[" "+e] = struct{}{}
			seen[strings.ToUpper(e)] = struct{}{}
			seen["."+strings.ToUpper(e)] = struct{}{}
		}
	}
	// ⚠️ A double leading dot is deliberately outside the domain: it is
	// outside Normalize's documented contract, and CanPreview strips two
	// (Has re-normalises JobTypeForExt's already-normalised input) where
	// this function strips one. Recorded as a finding in the sprint 25a
	// handoff rather than pinned either way here.
	for _, e := range []string{"md", "bin", "mtl", "npz", "docx", "rtf", "zzz", "", "PNG", ".PNG", "Jpg", "mp 4"} {
		seen[e] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, e := range keys {
		e := e
		if got, want := needsProcessing(&e), dispatch.CanPreview(&e); got != want {
			t.Errorf("%q: needsProcessing=%v, CanPreview=%v", e, got, want)
		}
	}
	if needsProcessing(nil) != dispatch.CanPreview(nil) {
		t.Error("nil: needsProcessing and CanPreview disagree")
	}
	if len(keys) < 100 {
		t.Fatalf("only %d inputs; the domain is supposed to cover every declared set", len(keys))
	}
}
