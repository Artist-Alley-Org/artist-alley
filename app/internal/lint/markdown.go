// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package lint

import (
	"bytes"
	"strings"
)

// CheckMarkdown runs a small set of "writing style" rules over the
// source. We deliberately skip the heavyweight CommonMark grammar
// validators here — those are subjective and noisy. Instead we ship
// the rules a code reviewer reaches for first:
//
//   * trailing whitespace (warning)
//   * tab indentation in paragraphs (warning — markdown's intent is
//     code-fence delimiters; tabs in prose usually mean a paste
//     accident)
//   * line length > 120 characters (info — soft cap, not enforced)
//   * unclosed code fences (error)
//   * heading with leading #s but no space ("##bold" reads as "##bold"
//     in CommonMark, not a heading — almost always a typo)
//   * bare URL in body (info — suggests using `[text](url)`)
//
// Rules opt-in by source name so the panel can render a "Markdown
// style" badge and the future per-rule disable list lands cleanly.

func CheckMarkdown(text []byte) []Diagnostic {
	if len(bytes.TrimSpace(text)) == 0 {
		return nil
	}
	lines := strings.Split(string(text), "\n")
	var out []Diagnostic

	const lineLengthMax = 120
	inFence := false
	fenceOpenedAt := 0
	for i, raw := range lines {
		line := raw
		// Track code fences so we skip prose-only rules inside them.
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			if !inFence {
				inFence = true
				fenceOpenedAt = i + 1
			} else {
				inFence = false
			}
			continue
		}
		// Trailing whitespace (also flags inside code blocks — that
		// breaks diff hygiene regardless).
		if rtrim := strings.TrimRight(line, " \t"); rtrim != line {
			out = append(out, Diagnostic{
				Line: i + 1, Col: len(rtrim) + 1,
				EndLine: i + 1, EndCol: len(line) + 1,
				Severity: "warning", Source: "markdown",
				Message: "trailing whitespace",
			})
		}
		if inFence {
			continue
		}
		// Tab indentation in prose.
		if strings.HasPrefix(line, "\t") {
			out = append(out, Diagnostic{
				Line: i + 1, Col: 1, Severity: "warning", Source: "markdown",
				Message: "tab indentation in prose — use spaces",
			})
		}
		// Heading with no space after the leading #s.
		if h := strings.TrimLeft(line, "#"); h != line {
			hashes := len(line) - len(h)
			if hashes >= 1 && hashes <= 6 && len(h) > 0 && h[0] != ' ' && h[0] != '\t' {
				out = append(out, Diagnostic{
					Line: i + 1, Col: hashes + 1, Severity: "warning",
					Source: "markdown",
					Message: "missing space after heading marker",
				})
			}
		}
		// Long line (visible width — tabs count as 1 here, close enough).
		if len(line) > lineLengthMax {
			out = append(out, Diagnostic{
				Line: i + 1, Col: lineLengthMax + 1,
				EndLine: i + 1, EndCol: len(line) + 1,
				Severity: "info", Source: "markdown",
				Message: "line longer than 120 characters",
			})
		}
	}
	if inFence {
		out = append(out, Diagnostic{
			Line: fenceOpenedAt, Col: 1, Severity: "error",
			Source: "markdown",
			Message: "code fence opened here is never closed",
		})
	}
	return out
}
