// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package provenance holds the one rule for an asset's ingest base name
// (#1595): what an upload request may carry as X-Original-Filename, how
// it becomes a safe base name, and when it may travel on as archival-1
// originalFilename.
//
// The value is the file name AS RECEIVED in the upload request. The
// server cannot see the uploader's disk, so it is never an attestation
// of a name a file had anywhere: archival-1 calls it "untrusted, and
// never a path" (spec v1 §17.2.2), and so does this package. What it
// does guarantee is narrower and checkable: the name came from the
// upload request itself (not from later, editable metadata), it was
// reduced to a base name, and it is safe to show and to export.
//
// The rule (Basename) is the one migration 00081 mirrors in SQL for the
// legacy backfill and the assets / upload_receipts checks:
//
//   - valid UTF-8;
//   - no control character (C0, DEL, C1) and no bidi formatting mark,
//     anywhere in the supplied value;
//   - path components of either slash style are stripped, and the base
//     name left must not be empty, "." or "..", and is at most 255
//     bytes.
//
// It is at least as strict as federation.checkBasename, the archival-1
// receiver rule, so every base name accepted here is a valid
// originalFilename (pinned by a contract test in package federation).
package provenance

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxBasenameBytes is the archival-1 limit on originalFilename.
const MaxBasenameBytes = 255

// Source says where an asset's original base name came from. The three
// values are the assets.original_basename_source vocabulary.
type Source string

const (
	// SourceUploadRequest: carried by the upload request itself
	// (X-Original-Filename on POST /storage/objects), bound to the asset
	// through that upload's receipt.
	SourceUploadRequest Source = "upload_request"
	// SourceClientReported: backfilled from metadata.original_filename,
	// a value any client could send and an editor could change. Never
	// upgraded to SourceUploadRequest.
	SourceClientReported Source = "client_reported"
	// SourceUnknown: nothing trustworthy was supplied. The base name is
	// absent exactly when the source is unknown.
	SourceUnknown Source = "unknown"
)

// ErrInvalidBasename is returned for a supplied name that is not a safe
// base name. It never carries the value: a rejected name may be a path,
// and paths are not logged or echoed.
var ErrInvalidBasename = errors.New("provenance: not a safe file base name")

// FromUploadHeader turns an X-Original-Filename header value into a safe
// base name. Browsers cannot send UTF-8 in a header (fetch and XHR refuse
// anything outside ISO-8859-1), so a client percent-encodes the name as
// UTF-8 (encodeURIComponent); a plain ASCII name needs no encoding. A
// literal '%' in a name must therefore be sent as %25. Malformed
// encoding, invalid UTF-8, a control or bidi mark anywhere, or nothing
// usable left after stripping the path is ErrInvalidBasename.
func FromUploadHeader(raw string) (string, error) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", ErrInvalidBasename
	}
	return Basename(decoded)
}

// Basename reduces a supplied file name to a safe base name, or returns
// ErrInvalidBasename. Characters are checked in the WHOLE value before
// the path is stripped, so a control character hidden in a directory
// component still rejects the name.
func Basename(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", ErrInvalidBasename
	}
	for _, r := range s {
		if forbiddenRune(r) {
			return "", ErrInvalidBasename
		}
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	if !bareOK(s) {
		return "", ErrInvalidBasename
	}
	return s, nil
}

// IsBareBasename reports whether s is already a safe base name with no
// path component at all: the legacy backfill's rule, which never derives
// a name from a path.
func IsBareBasename(s string) bool {
	if !utf8.ValidString(s) || strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, r := range s {
		if forbiddenRune(r) {
			return false
		}
	}
	return bareOK(s)
}

func bareOK(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= MaxBasenameBytes
}

// forbiddenRune: C0 and C1 controls and DEL (unicode.IsControl), and the
// bidi formatting marks that make a displayed name lie about its
// extension (U+061C, U+200E, U+200F, U+202A..U+202E, U+2066..U+2069).
func forbiddenRune(r rune) bool {
	if unicode.IsControl(r) {
		return true
	}
	switch {
	case r == 0x061C, r == 0x200E, r == 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// ArchivalOriginalFilename is the archival-1 originalFilename for an
// asset's original file: its base name when that came from the upload
// request, and "" (omit the member) otherwise. A client_reported name is
// an editable claim, not the name the file was received under, so it is
// never exported as one. The value is a base name by construction (the
// assets checks enforce it), never a path.
func ArchivalOriginalFilename(basename *string, source Source) string {
	if source != SourceUploadRequest || basename == nil || !IsBareBasename(*basename) {
		return ""
	}
	return *basename
}
