// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package preview

import "github.com/mscrnt/artist-alley/app/internal/preview/dispatch"

// ---------------------------------------------------------------------------
// One payload, thirteen names.
//
// Every preview.* job carries the same three fields — which asset, which
// content hash, which extension — and until #760 each handler declared
// its own struct for them while the three enqueue sites (upload,
// "Recreate previews", `aa seed`) each hand-rolled a map[string]string.
// Sixteen independent descriptions of one wire format, none of which the
// compiler could compare.
//
// That is what made the force flag dangerous to add: a `map[string]string`
// can only carry `"force": "true"`, which a `Force bool` rejects at
// unmarshal time, and a handler whose struct was missed would keep
// skipping while reporting success — the exact defect (#760) with a new
// face.
//
// These are ALIASES (`=`), not definitions: they are literally
// dispatch.Payload, so a producer and a handler cannot disagree, adding
// a field is one edit, and every existing `AudioPayload{AssetID: …}`
// literal in the tree still compiles. The distinct names are kept
// because they document which job a given Handle reads.
// ---------------------------------------------------------------------------

type (
	// RasterPayload is the body of a preview.raster job.
	RasterPayload = dispatch.Payload
	// AudioPayload is the body of a preview.audio job.
	AudioPayload = dispatch.Payload
	// VideoPayload is the body of a preview.video job.
	VideoPayload = dispatch.Payload
	// ModelPayload is the body of a preview.3d job.
	ModelPayload = dispatch.Payload
	// PDFPayload is the body of a preview.pdf job.
	PDFPayload = dispatch.Payload
	// FontPayload is the body of a preview.font job.
	FontPayload = dispatch.Payload
	// EPUBPayload is the body of a preview.ebook job.
	EPUBPayload = dispatch.Payload
	// EPSPayload is the body of a preview.eps job.
	EPSPayload = dispatch.Payload
	// PSDPayload is the body of a preview.psd job.
	PSDPayload = dispatch.Payload
	// ComicPayload is the body of a preview.comic job.
	ComicPayload = dispatch.Payload
	// TextPayload is the body of a preview.text job.
	TextPayload = dispatch.Payload
	// ArchivePayload is the body of a preview.archive job.
	ArchivePayload = dispatch.Payload
)
