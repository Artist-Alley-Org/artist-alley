// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package assets

import (
	"errors"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// errUploadReceiptInvalid is the one answer for an AssetCreate
// upload_receipt that cannot be used (#1595): malformed, sent without a
// file_hash, issued to another user, for another hash, expired, or
// already used. One message for all of them, so the endpoint never says
// whether someone else's receipt exists.
var errUploadReceiptInvalid = errors.New("upload_receipt is not valid for this upload")

// originalBasenameToAPI projects the write-once ingest provenance
// (#1595) onto an Asset response. An empty source is a row type that
// does not carry the columns (the list projections), which says nothing,
// so both fields stay absent rather than claiming "unknown".
func originalBasenameToAPI(basename *string, source string) (*string, *openapi.AssetOriginalBasenameSource) {
	if source == "" {
		return nil, nil
	}
	s := openapi.AssetOriginalBasenameSource(source)
	return basename, &s
}
