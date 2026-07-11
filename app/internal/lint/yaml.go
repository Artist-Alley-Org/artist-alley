// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package lint

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

// CheckYAML decodes the source through gopkg.in/yaml.v3 and emits an
// error Diagnostic on the first parse failure. yaml.v3 errors are
// formatted as "yaml: line N: <msg>"; we tease the line out so the
// gutter dot lands correctly. Column always 1 — yaml.v3 doesn't
// expose a finer offset on parse errors.
//
// Supports multi-document YAML — we iterate Decode until io.EOF so
// errors in any document fragment surface.
func CheckYAML(text []byte) []Diagnostic {
	if len(bytes.TrimSpace(text)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(text))
	var out []Diagnostic
	for {
		var probe any
		err := dec.Decode(&probe)
		if err == nil {
			continue
		}
		if err.Error() == "EOF" {
			break
		}
		// yaml.v3 wraps multiple errors in a TypeError with .Errors;
		// surface every entry.
		if te, ok := err.(*yaml.TypeError); ok {
			for _, e := range te.Errors {
				out = append(out, yamlMsgToDiag(e))
			}
			continue
		}
		out = append(out, yamlMsgToDiag(err.Error()))
		break
	}
	return out
}

func yamlMsgToDiag(msg string) Diagnostic {
	// Format: "yaml: line N: <rest>" or "line N: <rest>" or just <rest>.
	line := 1
	rest := msg
	if i := strings.Index(rest, "line "); i >= 0 {
		j := strings.Index(rest[i+5:], ":")
		if j > 0 {
			ln := 0
			for _, c := range rest[i+5 : i+5+j] {
				if c < '0' || c > '9' {
					ln = 0
					break
				}
				ln = ln*10 + int(c-'0')
			}
			if ln > 0 {
				line = ln
				rest = strings.TrimSpace(rest[i+5+j+1:])
			}
		}
	}
	rest = strings.TrimPrefix(rest, "yaml: ")
	return Diagnostic{
		Line: line, Col: 1, Severity: "error", Source: "yaml",
		Message: rest,
	}
}
