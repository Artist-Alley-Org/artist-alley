// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CheckJSON runs the standard-library decoder against the source
// bytes and emits a single error Diagnostic on the first syntax
// failure. encoding/json's SyntaxError carries a byte offset; we
// translate that to (line, col) so the gutter dot lands on the
// right line.
//
// Limitation: encoding/json bails on the first error, so deeply
// broken files only show one diagnostic at a time. That matches
// what `python -m json.tool` and most editors do; iterating to find
// "all" errors would need a hand-rolled parser.
//
// jsonc support: encoding/json doesn't strip // or /* */ comments,
// so .jsonc files run through the same checker for now and may
// false-positive on the comment lines. A future commit can preprocess
// jsonc bodies by stripping single-line + block comments before the
// Decode call.
func CheckJSON(text []byte) []Diagnostic {
	if len(bytes.TrimSpace(text)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return []Diagnostic{jsonErrToDiag(text, err)}
	}
	// Catch trailing-garbage cases ("{}<EXTRA>") that the first
	// Decode call doesn't surface.
	if dec.More() {
		off := dec.InputOffset()
		line, col := byteOffsetToLineCol(text, int(off))
		return []Diagnostic{{
			Line: line, Col: col, Severity: "error",
			Source: "json",
			Message: "unexpected content after top-level value",
		}}
	}
	return nil
}

func jsonErrToDiag(text []byte, err error) Diagnostic {
	if se, ok := err.(*json.SyntaxError); ok {
		line, col := byteOffsetToLineCol(text, int(se.Offset))
		return Diagnostic{
			Line: line, Col: col, Severity: "error", Source: "json",
			Message: se.Error(),
		}
	}
	// Type / unmarshal errors carry no offset — pin to line 1.
	return Diagnostic{
		Line: 1, Col: 1, Severity: "error", Source: "json",
		Message: fmt.Sprintf("parse failed: %s", err.Error()),
	}
}

// byteOffsetToLineCol returns 1-based line + column for a byte
// offset into a UTF-8 source. Treats every byte as advancing the
// column counter, which is close-enough for the ASCII-dominant
// formats this package targets; the small drift on multi-byte
// characters is tolerable for a lint indicator.
func byteOffsetToLineCol(text []byte, off int) (int, int) {
	if off < 0 {
		off = 0
	}
	if off > len(text) {
		off = len(text)
	}
	line := 1
	col := 1
	for i := 0; i < off; i++ {
		if text[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}
