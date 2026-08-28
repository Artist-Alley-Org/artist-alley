// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

// The anonymous asset read rule, held as DATA (#1209).
//
// # Why this is a table and not two pieces of code
//
// Two things need the same answer, in two languages. [Predicate.ToSQL]
// renders it as a WHERE fragment so a query returns only the rows a
// stranger may list; [AnonymouslyVisible] evaluates it in Go so the
// cover picker can be told, about a row it already holds, whether a
// stranger would see the picture.
//
// Writing the second one out by hand would be a second copy of a
// SECURITY rule, and a second copy is a rule that will eventually
// disagree with itself. That is #1023's finding and ADR 0070's
// amendment, and it is the whole reason the conjuncts live here as a
// list that both consumers walk. Adding, dropping or retitling a
// condition moves the SQL and the boolean in one edit, because there is
// only one edit to make.
//
// # What the conjuncts are, and why each is required
//
// Named in ADR 0063 and spelled out on [Predicate.ToSQL]: a draft or
// archived asset is not published, a non-public sensitivity tier is not
// for strangers, and an asset still processing has no derivatives to
// serve. Soft-delete is the fourth and is handled separately at both
// consumers, because it is the one conjunct [IncludeSoftDeleted] can
// waive and these three must never be reachable by it.
//
// # Scope
//
// The ANONYMOUS branch only. The authenticated asset branch is
// soft-delete alone, and closing its sensitivity gap needs a product
// decision the operator has not made (see [Predicate.ToSQL]); nothing
// here touches it.
type anonymousAssetCondition struct {
	// Column is the asset column, unqualified. The SQL renderer
	// prefixes the caller's alias.
	Column string
	// Want is the single value that column must hold. A literal rather
	// than a bound parameter because the anonymous branches bind no
	// arguments at all, which is what lets a zero-arg fragment compose
	// at any placeholder offset.
	Want string
	// Get reads the same column off a scanned row, so the Go evaluator
	// and the SQL cannot be pointed at different columns.
	Get func(FieldsRow) string
}

var anonymousAssetConditions = []anonymousAssetCondition{
	{Column: "status", Want: "active", Get: func(r FieldsRow) string { return r.Status }},
	{Column: "sensitivity", Want: "public", Get: func(r FieldsRow) string { return r.Sensitivity }},
	{Column: "processing_status", Want: "ready", Get: func(r FieldsRow) string { return r.ProcessingStatus }},
}

// anonymousAssetSQL renders the three conjuncts above against a
// prepared alias prefix (either "" or "alias."). Bound to no arguments.
func anonymousAssetSQL(aliasPrefix string) []string {
	out := make([]string, 0, len(anonymousAssetConditions))
	for _, c := range anonymousAssetConditions {
		out = append(out, aliasPrefix+c.Column+" = '"+c.Want+"'")
	}
	return out
}

// AnonymouslyVisible answers, for an asset row the CALLER already
// holds, whether an ANONYMOUS visitor's read rule would return it
// (#1209).
//
// # What it is for
//
// A curator picking a cover for a public collection or post is choosing
// a picture that strangers will be shown. If the picked asset fails the
// anonymous rule, every anonymous surface falls back to something else
// and the curator's choice silently does nothing. The picker used to be
// able to check only `status`, because the Asset payload carries
// `status` and deliberately carries no `sensitivity`, so a team-tier
// pick fell back unwarned. It said so in its own doc comment rather
// than guessing, because a warning that guesses in the reassuring
// direction is worse than no warning.
//
// # It is a statement about the ROW, not about this caller
//
// The answer does not depend on who is asking. That is the point: the
// question is "what does a stranger see", and it has one answer per
// row. It is therefore NOT a readability decision and must never be
// used as one. [FieldsReadable] and [PreviewReadable] decide what this
// caller receives; a surface that reached for this instead would be
// asking the wrong question and would be wrong for every signed-in
// caller.
//
// # Disclosure
//
// This is derived from the GATE rather than from content, and it
// discloses nothing new. The fact it states is precisely the fact any
// visitor can establish for themselves by opening the same URL with no
// session, so a caller who could not otherwise learn it can learn it
// from the public internet. It rides only on payloads that already
// passed [FieldsReadable]: the #899 placeholder is a complete literal
// with three permitted keys and does not carry this one, so a withheld
// row states nothing about its own tier.
//
// `softDeleted` is the fourth conjunct, passed separately because it is
// the only one [IncludeSoftDeleted] may waive on the SQL side and it
// must not be waivable here at all.
func AnonymouslyVisible(row FieldsRow, softDeleted bool) bool {
	if softDeleted {
		return false
	}
	for _, c := range anonymousAssetConditions {
		if c.Get(row) != c.Want {
			return false
		}
	}
	return true
}
