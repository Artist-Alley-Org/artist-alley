// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// app/api/openapi.yaml must not hold a mapping key that is really the
// tail of a split value (#1598).
//
// In a YAML flow mapping, `{ type: string, description: Foo, bar. }`
// ends the description at the comma: it parses as description "Foo"
// plus a key "bar." with a null value. Nothing downstream notices.
// oapi-codegen ignores the unknown keyword, the Go types do not change,
// and every test stays green, while the published spec carries a
// truncated description and a junk schema key. That is how two
// descriptions on PreservationEvent shipped split until a contract
// comparison against the site's reviewed copy found them.
//
// The tell is a key that contains whitespace and maps to null. Real
// OpenAPI keys never contain whitespace (paths, schema names, keywords
// and media types are all single tokens), and a null value under one is
// the residue of a split scalar.

package openapi

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// splitValueKeys returns "line N: /path/to/key" for every mapping key
// that contains whitespace and whose value is null.
func splitValueKeys(doc []byte) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	var out []string
	var walk func(n *yaml.Node, path string)
	walk = func(n *yaml.Node, path string) {
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				p := path + "/" + k.Value
				if v.Kind == yaml.ScalarNode && v.Tag == "!!null" && strings.ContainsAny(k.Value, " \t") {
					out = append(out, fmt.Sprintf("line %d: %s", k.Line, p))
				}
				walk(v, p)
			}
		case yaml.SequenceNode:
			for i, c := range n.Content {
				walk(c, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(&root, "")
	return out, nil
}

func TestOpenAPISpec_NoValueSplitIntoAKey(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	found, err := splitValueKeys(raw)
	if err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	for _, f := range found {
		t.Errorf("openapi.yaml %s: a key with a space and no value is the tail of a split scalar; quote the value it came from (an unquoted comma inside { ... } ends it)", f)
	}
}

func TestSplitValueKeys_CatchesAnUnquotedFlowMapComma(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{
			name: "unquoted comma in a flow mapping splits the value",
			doc:  "schema:\n  field: { type: string, description: The digest, if it had one. }\n",
			want: []string{"line 2: /schema/field/if it had one."},
		},
		{
			name: "the same value quoted stays whole",
			doc:  "schema:\n  field: { type: string, description: \"The digest, if it had one.\" }\n",
		},
		{
			name: "a comma in a block-style value is fine",
			doc:  "schema:\n  field:\n    type: string\n    description: The digest, if it had one.\n",
		},
		{
			name: "a genuinely null single-token key is not flagged",
			doc:  "schema:\n  field: { type: string, example: }\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := splitValueKeys([]byte(c.doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
