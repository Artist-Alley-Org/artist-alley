// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package sitetext

import (
	"embed"
	"encoding/json"
	"fmt"
)

// catalogueFS embeds the shipped English string catalogue.
//
// catalogue.json is a GENERATED artifact: a byte-for-byte copy of
// web/src/lib/i18n/en.json, produced by scripts/generate.sh and
// committed, exactly like openapi.gen.go. That is what puts it under
// CI's codegen drift check — edit en.json without regenerating and CI
// fails, rather than the server quietly validating override keys
// against a stale key set.
//
// The Go binary cannot read the frontend tree at runtime (the SPA is
// embedded as built assets, not as sources), so a copy is the only way
// the write API can know which keys exist. And it MUST know: ADR 0081
// §1 requires an override naming a nonexistent key to fail loudly, and
// the admin UI is not where that rule can live — an operator with
// `system.config.write` can call the endpoint directly.
//
//go:embed catalogue.json
var catalogueFS embed.FS

// shipped is the flattened catalogue: dotted key → English string.
// Built once at package init.
var shipped map[string]string

func init() {
	raw, err := catalogueFS.ReadFile("catalogue.json")
	if err != nil {
		// Embedded at compile time — unreachable unless the build is
		// broken, in which case failing at boot is the honest outcome.
		panic(fmt.Sprintf("sitetext: read embedded catalogue: %v", err))
	}
	var nested map[string]any
	if err := json.Unmarshal(raw, &nested); err != nil {
		panic(fmt.Sprintf("sitetext: parse embedded catalogue: %v", err))
	}
	shipped = make(map[string]string, 2500)
	flatten(nested, "", shipped)
}

// flatten walks the nested catalogue into dotted keys.
//
// Mirrors the client's flatten() in web/src/lib/stores/lang.svelte.ts
// exactly — same recursion into objects, same "everything else is a
// leaf" rule — because the two have to agree on what a key IS. If the
// server flattened differently, it would accept keys `t()` can never
// resolve and refuse keys it renders every day.
func flatten(src map[string]any, prefix string, out map[string]string) {
	for k, v := range src {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if obj, ok := v.(map[string]any); ok {
			flatten(obj, key, out)
			continue
		}
		out[key] = stringify(v)
	}
}

// stringify renders a leaf the way the client's String() does. Only
// strings appear in the catalogue today; the rest are here so a future
// numeric or boolean entry does not become the literal "%!v(...)".
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode as float64; render integers without the
		// trailing ".0" that %v would print for some values.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// KnownKey reports whether the shipped catalogue defines this key.
func KnownKey(key string) bool {
	_, ok := shipped[key]
	return ok
}

// ShippedValue returns the English string a key ships with, and whether
// the key exists at all.
func ShippedValue(key string) (string, bool) {
	v, ok := shipped[key]
	return v, ok
}

// CatalogueSize is the number of overridable keys.
func CatalogueSize() int { return len(shipped) }
