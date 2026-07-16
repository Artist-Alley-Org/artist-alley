// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package openapi

// RawSpecJSON returns the embedded OpenAPI specification as raw JSON
// bytes. oapi-codegen already flate-compresses + base64-encodes the
// spec into openapi.gen.go (embedded-spec: true) and decodes it once,
// lazily, into `rawSpec`; this is just the exported door onto that
// blob so the HTTP layer can serve it to the in-app API explorer
// (Scalar) without a second copy of the spec on disk.
//
// Hand-written and deliberately kept OUT of openapi.gen.go so it
// survives regeneration — generate.sh only rewrites the .gen.go file.
func RawSpecJSON() ([]byte, error) {
	return rawSpec()
}
