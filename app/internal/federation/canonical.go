// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// RFC 8785 (JSON Canonicalization Scheme) canonicalization wrapper.
//
// Why a wrapper instead of calling gowebpki/jcs directly: keeps the
// rest of the package decoupled from the canonicalization library
// (a future swap or in-tree replacement only touches this file)
// and gives us a single chokepoint to add tracing / metrics if
// federation hot paths ever need it.
//
// Spec reference: docs/spec/federation/v1.md §4.

package federation

import (
	"encoding/json"
	"fmt"

	"github.com/gowebpki/jcs"
)

// Canonicalize returns the RFC 8785 canonical byte form of the
// JSON object encoded in src.
//
// src MUST be a valid JSON object (or array; primitives are
// accepted by the underlying library but the protocol doesn't use
// them). Invalid JSON returns an error; semantically-unusual
// inputs (NaN, ±Infinity) are forbidden by the JSON grammar and
// will fail at decode.
//
// The output is the deterministic, signature-input form: object
// keys sorted by UTF-16 code unit, no whitespace, numbers in
// shortest IEEE 754 round-trip representation, strings minimally
// escaped.
func Canonicalize(src []byte) ([]byte, error) {
	out, err := jcs.Transform(src)
	if err != nil {
		return nil, fmt.Errorf("federation: canonicalize: %w", err)
	}
	return out, nil
}

// CanonicalizeValue marshals v with the standard library's
// encoding/json then canonicalizes. Convenience for callers that
// hold a Go value rather than already-encoded bytes.
//
// The intermediate marshal uses encoding/json's default behaviour —
// HTML escaping ON, no trailing newline. JCS normalizes both of
// those out anyway, so the canonical form is independent of the
// intermediate serializer's quirks.
func CanonicalizeValue(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("federation: marshal for canonicalize: %w", err)
	}
	return Canonicalize(raw)
}
