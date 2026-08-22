// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package viewkind

import (
	"sort"
	"strconv"
	"strings"
)

// KindSQL is the SQL twin of [ForAsset]: an expression that resolves ONE
// asset row down to the badge kind its card draws, reading the same two
// columns the Go resolver reads (#1251).
//
// `alias` is the assets table alias the expression should qualify its
// columns with ("" for an un-aliased FROM); it is suffixed with a dot
// here so call sites pass the alias and nothing else.
//
// # Why an expression, and not the set-membership arms it replaces
//
// #1166 shipped the filter as the INVERSE of this: for the kinds the
// caller selected, compile the asset_type refs and the extensions that
// resolve to them, then ask whether the row is in those sets. That
// worked, and it had two costs this shape does not.
//
//   - It was a SECOND expression of [ForAsset] with no oracle over it.
//     The compile step (`Compile`) and the two SQL arms together
//     restated the resolver's precedence rules, and the only test that
//     could see a disagreement was one that happened to plant a fixture
//     on the disputed extension. Resolving the kind and COMPARING it is
//     the same function transcribed, so posts.TestKindSQLMatchesForAsset
//     can drive the whole vocabulary through Postgres and diff it
//     against Go — the exhaustive oracle the arms could not have.
//   - The selected sets had to be BOUND ARRAYS, so one filter term
//     needed three placeholders. The shared filter grammar gives each
//     term exactly one, and that contract is what lets a dimension be
//     added without changing the arity of every other one (see
//     facet.dimensionSQL). Here the placeholder holds the kind NAME and
//     the comparison is `<this expression> = $n`, which is one term, one
//     placeholder, and reads like what it means.
//
// It also deletes a special case rather than carrying it: `sequence` is
// a real kind that no single asset can resolve to, which the arms had to
// spell as "an empty selection is a never-satisfied conjunct, not an
// absent one". Here it is simply a value this expression never returns.
//
// # The order of the branches IS the resolver's precedence
//
// The overriding `asset_type` refs come first because [ForAsset] checks
// them first — a sprite atlas is a PNG and only the ref separates it
// from a texture. The extension lookup is the SIMPLE-CASE form so the
// normalisation runs ONCE per row rather than once per candidate kind,
// and its branches are emitted in [extensionOrder], skipping any
// extension an earlier group already claimed. That is [extIndex]'s
// construction rule, written out: `ts` belongs to video, so it must not
// appear again under doc.
//
// A row that matches no branch is [KindPlaceholder] — the resolver's own
// "I could not tell" answer. NULL flows to it correctly in both halves:
// a NULL `asset_type` makes every `=` above NULL rather than true, and a
// NULL `file_extension` makes the inner CASE return NULL, which the
// COALESCE turns into the placeholder exactly as `ext == nil` does in
// Go.
//
// # Nothing in it comes from caller text
//
// Every literal below is spliced from this package's own constants, the
// same sanctioned pattern visibility.FieldsReadableSQL uses for its
// team UUIDs. The caller's bytes stay in the placeholder they are
// compared against.
func KindSQL(alias string) string {
	p := strings.TrimSpace(alias)
	if p != "" {
		p += "."
	}

	var b strings.Builder
	b.WriteString("CASE\n")

	// The overriding refs, ascending so the rendered text is stable for
	// a given vocabulary (Go map iteration is not).
	refs := make([]int64, 0, len(assetTypeKind))
	for ref := range assetTypeKind {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i] < refs[j] })
	for _, ref := range refs {
		b.WriteString("            WHEN " + p + "asset_type = " + strconv.FormatInt(ref, 10) +
			" THEN " + quote(string(assetTypeKind[ref])) + "\n")
	}

	// The extension table. `btrim` before `lower` before stripping ONE
	// leading dot is [Normalize] in the same order — TrimSpace, ToLower,
	// TrimPrefix(".") — so an extension stored as " .PNG " resolves the
	// way the Go resolver resolves it.
	b.WriteString("            ELSE COALESCE(CASE regexp_replace(lower(btrim(" +
		p + "file_extension)), '^\\.', '')\n")
	for _, group := range extensionOrder {
		for _, e := range group.exts {
			if extIndex[e] != group.kind {
				continue
			}
			b.WriteString("              WHEN " + quote(e) + " THEN " + quote(string(group.kind)) + "\n")
		}
	}
	b.WriteString("            END, " + quote(string(KindPlaceholder)) + ")\n")
	b.WriteString("          END")
	return b.String()
}

// quote renders a Go string as a SQL literal.
//
// Every value it is handed is a constant declared in this package, so
// the doubling below is belt and braces rather than the guard it would
// have to be for caller text — but it is here so that adding a kind or an
// extension whose name contains an apostrophe cannot turn a vocabulary
// edit into a syntax error at runtime.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
