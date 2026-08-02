// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Tests for the embedded shipped catalogue (#794).
//
// These need no database: the catalogue is compiled in. They pin the
// two properties the write API's fail-loud rule rests on — that the
// embedded copy is the SAME file the frontend bundles, and that the
// server flattens it the same way the client does.

package sitetext_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/sitetext"
)

// frontendCatalogue is the path the generate step copies from.
const frontendCatalogue = "../../../web/src/lib/i18n/en.json"

// TestEmbeddedCatalogueMatchesFrontend is the drift guard with teeth.
//
// CI's codegen check catches a stale catalogue.json by regenerating and
// diffing, which only works if somebody remembers to keep the CI step
// alongside scripts/generate.sh. This asserts the same invariant from
// inside the test suite, so removing the CI step does not silently
// remove the guarantee: the server must validate override keys against
// the exact key set the frontend renders.
func TestEmbeddedCatalogueMatchesFrontend(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Clean(frontendCatalogue))
	if err != nil {
		t.Skipf("frontend catalogue not reachable from here (%v); the CI drift check still covers this", err)
	}
	var nested map[string]any
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatalf("parse frontend en.json: %v", err)
	}
	want := map[string]string{}
	flattenForTest(nested, "", want)

	if got := sitetext.CatalogueSize(); got != len(want) {
		t.Fatalf("embedded catalogue has %d keys, frontend en.json has %d — run ./scripts/generate.sh", got, len(want))
	}
	for k, v := range want {
		gotV, ok := sitetext.ShippedValue(k)
		if !ok {
			t.Fatalf("key %q is in the frontend catalogue but not the embedded copy — run ./scripts/generate.sh", k)
		}
		if gotV != v {
			t.Fatalf("key %q: embedded %q, frontend %q — run ./scripts/generate.sh", k, gotV, v)
		}
	}
}

// TestKnownKey covers both directions of the check the write API makes.
func TestKnownKey(t *testing.T) {
	t.Parallel()
	// A key the navbar renders on every page. If this ever stops
	// existing the app has a bigger problem than this test.
	if !sitetext.KnownKey("nav.upload") {
		t.Errorf("nav.upload should be a known key")
	}
	if sitetext.KnownKey("nav") {
		// Intermediate objects are NOT keys — only leaves are
		// overridable, because only leaves are things `t()` resolves.
		t.Errorf("nav is an intermediate node, not an overridable key")
	}
	if sitetext.KnownKey("definitely.not.a.real.key") {
		t.Errorf("an invented key must not be known")
	}
	if sitetext.KnownKey("") {
		t.Errorf("the empty key must not be known")
	}
}

// TestCatalogueIsNonTrivial guards against the failure mode where the
// embed silently resolves to an empty or truncated file: every write
// would then 422, and "nothing can be overridden" is a subtler bug than
// a build error.
func TestCatalogueIsNonTrivial(t *testing.T) {
	t.Parallel()
	if n := sitetext.CatalogueSize(); n < 1000 {
		t.Fatalf("embedded catalogue has only %d keys; expected the full shipped set", n)
	}
}

// flattenForTest deliberately re-implements the flatten rule rather
// than calling the package's, so a change to the production flattener
// has to be made in two places to pass — the point being that the rule
// must keep matching the CLIENT's, which this mirrors.
func flattenForTest(src map[string]any, prefix string, out map[string]string) {
	for k, v := range src {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if obj, ok := v.(map[string]any); ok {
			flattenForTest(obj, key, out)
			continue
		}
		s, ok := v.(string)
		if !ok {
			// Every leaf in the shipped catalogue is a string today.
			// A non-string arriving here would make the count
			// comparison above fail loudly, which is the right
			// outcome: production's stringify() would need the same
			// treatment before it could be trusted.
			continue
		}
		out[key] = s
	}
}
