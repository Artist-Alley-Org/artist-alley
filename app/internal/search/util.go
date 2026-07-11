// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import "encoding/base64"

// encodeB64 returns the standard-base64 encoding of b, or "" for
// empty input. Used for byte-column values (thumbhash, etc.) that
// the API surfaces as JSON strings.
func encodeB64(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}
