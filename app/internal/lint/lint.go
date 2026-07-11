// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Package lint runs language-aware syntax checks against an asset's
// source text and returns a unified Diagnostic shape the doc viewer
// can paint into CodeMirror's gutter + the side-panel list.
//
// Design — first cut ships three Go-native checkers (JSON / YAML /
// Markdown) so the framework runs against zero external deps. Adding
// a new language is one file:
//
//   1. Implement func Check<Lang>(text []byte) []Diagnostic.
//   2. Wire the extension in linterFor below.
//
// Future: subprocess-based linters (py_compile, node --check,
// luac -p, shellcheck) slot in behind a dispatcher that probes the
// container for the binary and falls back to "linter not available"
// when missing. The Diagnostic shape stays stable; only the
// producer changes.

package lint

import (
	"strings"
)

// Diagnostic mirrors CodeMirror 6's Diagnostic shape so the frontend
// can hand the list straight to the lint extension. Line + col are
// 1-based; severity is one of "info" / "warning" / "error".
type Diagnostic struct {
	Line     int    `json:"line"`
	Col      int    `json:"col"`
	EndLine  int    `json:"end_line,omitempty"`
	EndCol   int    `json:"end_col,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	// Source is the linter that produced this diagnostic — useful
	// for the panel header and for future "rule docs" deep-links.
	Source string `json:"source"`
}

// Result is what the handler hands back to the client. Empty
// diagnostics + Skipped=true means we recognised the format but had
// no linter configured (so the panel can render "lint not available
// for this language" rather than "0 issues found").
type Result struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	Linter      string       `json:"linter"`
	Skipped     bool         `json:"skipped"`
}

// Run dispatches by extension and returns a Result. ext is the
// lower-case extension WITHOUT the leading dot.
func Run(ext string, text []byte) Result {
	checker := linterFor(ext)
	if checker.fn == nil {
		return Result{Linter: "none", Skipped: true}
	}
	return Result{
		Diagnostics: checker.fn(text),
		Linter:      checker.name,
	}
}

type linterEntry struct {
	name string
	fn   func([]byte) []Diagnostic
}

func linterFor(ext string) linterEntry {
	switch strings.ToLower(ext) {
	case "json", "jsonc":
		return linterEntry{"json", CheckJSON}
	case "yaml", "yml":
		return linterEntry{"yaml", CheckYAML}
	case "md", "markdown", "mdx":
		return linterEntry{"markdown", CheckMarkdown}
	}
	return linterEntry{}
}

// SupportedExtensions lists the file extensions Run knows a linter
// for. The handler exposes this so the panel can preemptively gate
// the "Run lint" button (and the frontend can deep-link a docs page
// for each linter later).
func SupportedExtensions() []string {
	return []string{"json", "jsonc", "yaml", "yml", "md", "markdown", "mdx"}
}
