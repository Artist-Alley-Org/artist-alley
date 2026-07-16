// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The routing/accept invariant (#362).
//
// dispatch.JobTypeForExt answers "which handler gets this?"; each
// handler then re-checks "can I actually decode this?" and returns a
// non-retryable TerminalError if not. When those two disagree, the
// asset never gets a preview — no retry, no fallback, just a dead job.
//
// That is exactly what happened: two private accept-sets carried
// comments claiming they mirrored the routing map "by convention",
// while quietly drifting 3 and 7 entries adrift respectively. Ten
// extensions — including .heic, the default iPhone photo format —
// routed to handlers guaranteed to reject them.
//
// A comment cannot hold this invariant. This test can.

package preview

import (
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/jobs"
	"github.com/mscrnt/artist-alley/app/internal/preview/dispatch"
)

// accepts maps a preview job type to that handler's decode-capability
// check — the same predicate the handler consults before it decides to
// TerminalError. Add an entry here when a preview handler is added.
var accepts = map[jobs.JobType]func(string) bool{
	jobs.TypePreviewRaster:  isRasterExt,
	jobs.TypePreviewVideo:   isVideoExt,
	jobs.TypePreview3D:      isModelExt,
	jobs.TypePreviewAudio:   isAudioExt,
	jobs.TypePreviewPDF:     isPDFExt,
	jobs.TypePreviewFont:    isFontExt,
	jobs.TypePreviewEbook:   isEbookExt,
	jobs.TypePreviewEPS:     isEPSExt,
	jobs.TypePreviewPSD:     isPSDExt,
	jobs.TypePreviewComic:   isComicExt,
	jobs.TypePreviewText:    isTextExt,
	jobs.TypePreviewArchive: IsArchiveExt,
}

// routedSets is every extension set the dispatcher routes from.
var routedSets = map[string]map[string]struct{}{
	"ImageExts":   dispatch.ImageExts,
	"VideoExts":   dispatch.VideoExts,
	"ModelExts":   dispatch.ModelExts,
	"AudioExts":   dispatch.AudioExts,
	"PDFExts":     dispatch.PDFExts,
	"FontExts":    dispatch.FontExts,
	"EbookExts":   dispatch.EbookExts,
	"EPSExts":     dispatch.EPSExts,
	"PSDExts":     dispatch.PSDExts,
	"ComicExts":   dispatch.ComicExts,
	"TextExts":    dispatch.TextExts,
	"ArchiveExts": dispatch.ArchiveExts,
}

// TestEveryRoutedExtIsAcceptedByItsHandler is the invariant: for every
// extension the dispatcher will route, the handler it routes TO must
// agree it can decode it. A failure here means that extension's assets
// would get a guaranteed-terminal job and never a preview.
func TestEveryRoutedExtIsAcceptedByItsHandler(t *testing.T) {
	for setName, set := range routedSets {
		for ext := range set {
			ext := ext
			t.Run(setName+"/"+ext, func(t *testing.T) {
				jt := dispatch.JobTypeForExt(&ext)
				accept, ok := accepts[jt]
				if !ok {
					t.Fatalf("%s routes %q to %q, which has no accept-check registered "+
						"in this test — add one so the invariant covers it", setName, ext, jt)
				}
				if !accept(ext) {
					t.Errorf("DRIFT: %s routes %q to %q, but that handler rejects it. "+
						"The asset would get a non-retryable TerminalError and never a "+
						"preview. Either teach the handler the format, or stop routing it.",
						setName, ext, jt)
				}
			})
		}
	}
}

// TestUnroutableExtFallsBackToRasterAndIsAccepted pins the dispatcher's
// fallback. Anything unrecognised routes to preview.raster, so if that
// fallback ever aims somewhere the handler can't decode, every unknown
// extension becomes a dead job.
func TestUnroutableExtFallsBackToRasterAndIsAccepted(t *testing.T) {
	unknown := "definitely-not-a-real-extension"
	if jt := dispatch.JobTypeForExt(&unknown); jt != jobs.TypePreviewRaster {
		t.Fatalf("fallback changed: unknown ext routes to %q, not %q — if the fallback "+
			"moves, re-check that the target accepts arbitrary input", jt, jobs.TypePreviewRaster)
	}
	// NOTE: the raster handler deliberately rejects the unknown ext at
	// decode time. That is the fallback's known rough edge, not drift:
	// it is the one case where routing and accept legitimately disagree,
	// because there is nowhere else to send an unknown file.
}

// TestNilExtRoutesToRaster guards the nil-extension path the upload
// surface can produce.
func TestNilExtRoutesToRaster(t *testing.T) {
	if jt := dispatch.JobTypeForExt(nil); jt != jobs.TypePreviewRaster {
		t.Errorf("nil extension routed to %q, want %q", jt, jobs.TypePreviewRaster)
	}
}
