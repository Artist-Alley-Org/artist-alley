// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Regressions for six defects a review of this sprint's own diff found
// (#1173, #1119, ADR 0019).
//
// They are together in one file because they have one thing in common:
// every one of them sat in a gap between two things that were each
// individually correct, and the suite that was written alongside the
// implementation did not look into any of those gaps. Four were
// reachable from the wire.
package metadata_test

import (
	"strings"
	"testing"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// An UNRECOGNISED MODE is 400 `unknown_mode`.
//
// ⚠️ Nothing else validates it. There is no spec-validation middleware
// in front of these handlers and the generated enum type is a bare
// string whose Valid() nothing calls, so `"mode": "clear"` arrives
// intact. Unchecked it was worse than cosmetic: it matched none of the
// mode-specific arms, so a REQUIRED field could be given a semantically
// empty value WITHOUT TRIPPING R1, every target fell through the
// partition switch as no_op, and the request died on the preview
// table's CHECK constraint as a 500.
func TestBatch_UnknownModeIsRefused(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("badmode")
	// REQUIRED, with an EMPTY value: the combination the unvalidated
	// mode let through.
	field := f.field("t", fieldSpec{Type: "text", Required: true})
	asset := f.asset(&owner, nil)

	res := f.preview(ctx, openapi.BatchAssetFieldMode("clear"), field, textValue(""), assetEntries(asset))
	f.wantPreviewRefusal(res, 400, openapi.BatchUnknownMode)
	if res.OK != nil {
		t.Fatal("an undefined mode must not produce a preview")
	}
	if f.rowExists(asset, field) {
		t.Fatal("and must write nothing")
	}
	// The empty string is a real mode value on the wire too.
	f.wantPreviewRefusal(
		f.preview(ctx, openapi.BatchAssetFieldMode(""), field, textValue(""), assetEntries(asset)),
		400, openapi.BatchUnknownMode)
}

// An UNRECOGNISED SELECTION KIND gets its OWN reason, not
// `empty_selection` — the selection was not empty, and a client acting
// on that code would be told a different fact from the one that refused
// it.
func TestBatch_UnknownSelectionKindIsRefused(t *testing.T) {
	f := newBatchFixture(t)
	_, ctx := f.bulkOperator("badkind")
	field := f.textField(false)
	sel := []openapi.BatchAssetFieldSelectionEntry{{
		Kind: openapi.BatchAssetFieldSelectionKind("collection"),
		Id:   assetEntries(f.asset(nil, nil))[0].Id,
	}}
	res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), sel)
	f.wantPreviewRefusal(res, 400, openapi.BatchUnknownSelectionKind)
	if res.Refusal.Reason == openapi.BatchEmptySelection {
		t.Fatal("the selection was NOT empty")
	}
}

// `remove` MATCHES; IT NEVER MINTS AND IT NEVER REFUSES.
//
// Its terms name what to take OUT, and its residual is a subset of what
// each target already held, so nothing it names is ever stored. Putting
// its terms through the mint-and-membership gate answered the wrong
// question three ways over, and the first of the three is the one that
// matters: a retired term became IMPOSSIBLE TO CLEAN OFF the records
// holding it, which is the exact freeze grandfathering exists to
// prevent, arriving through the other door.
func TestBatch_RemoveMatchesAndNeverMints(t *testing.T) {
	f := newBatchFixture(t)
	owner, _ := f.bulkOperator("rmvocab")
	f.grant(owner, capVocabExtend, nil)
	ctx := f.identity(owner)

	field := f.field("ms", fieldSpec{Type: "multi_select", OpenVocabulary: true, Options: []map[string]any{
		vocabOption("keep", "Keep", "active"),
		vocabOption("retired", "Retired", "archived"),
		vocabOptionWith("merged", "Merged", "archived", map[string]any{"replaced_by": "keep"}),
	}})

	t.Run("an ARCHIVED term with no replacement can be REMOVED", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		f.setValue(asset, field, map[string]any{"options": []string{"keep", "retired"}})

		p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("retired"), assetEntries(asset))
		if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("a held retired term must be removable; got %s", got)
		}
		res := f.apply(ctx, p.Token, "clean off a retired term", intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("apply refused: %+v", res.Refusal)
		}
		got, _ := f.storedOptions(asset, field)
		if strings.Join(got, ",") != "keep" {
			t.Fatalf("want [keep] left behind, got %v", got)
		}
	})

	t.Run("a term the field does not have is a NO_OP, not a refusal", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		f.setValue(asset, field, map[string]any{"options": []string{"keep"}})

		p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("never-existed"), assetEntries(asset))
		if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionNoOp {
			t.Fatalf("you cannot hold what does not exist; want no_op, got %s", got)
		}
		// AND NOTHING IS MINTABLE. A removal must never grow the
		// catalogue with the term it was told to take away.
		if p.MintableTerms != nil && len(*p.MintableTerms) != 0 {
			t.Fatalf("a removal mints nothing; got %+v", p.MintableTerms)
		}
	})

	t.Run("a MERGE TOMBSTONE still redirects, so the successor is removed", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		f.setValue(asset, field, map[string]any{"options": []string{"keep"}})

		// `merged` forwards to `keep`, which the target holds.
		p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("merged"), assetEntries(asset))
		if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("a tombstone must reach its successor; got %s", got)
		}
	})
}

// A MINTED TERM CARRIES THE OPERATOR'S SPELLING.
//
// A created option is labelled with the term it was created from, so
// handing the mint the canonical slug labelled `character-design` as
// "character-design" where the single-target writer — which passes the
// raw value straight through — labels it "Character Design". Two
// writers producing different labels for one operator input is drift,
// and the label is the half a human reads.
func TestBatch_MintedTermKeepsTheOperatorsLabel(t *testing.T) {
	f := newBatchFixture(t)
	owner, _ := f.bulkOperator("label")
	f.grant(owner, capVocabExtend, nil)
	ctx := f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field,
		optionsValue("Character Design"), assetEntries(asset))
	res := f.apply(ctx, p.Token, "mint with a real label", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	doc := string(f.optionsDoc(field))
	if !strings.Contains(doc, `"Character Design"`) {
		t.Fatalf("the minted term must keep the OPERATOR'S label, got %s", doc)
	}
	if got, _ := f.storedOptions(asset, field); strings.Join(got, ",") != "character-design" {
		t.Fatalf("and the row still stores the canonical slug, got %v", got)
	}
}

// EXPANSION IS A READ, AND IT OBEYS THE POST'S OWN READ GATE.
//
// Admission asks only whether the caller holds the bulk instrument
// ANYWHERE — a grant scoped to one team passes it — so without this a
// caller could name any post id and get its member asset ids back in
// `targets`, each politely labelled `unauthorized`, plus its
// non-emptiness in `empty_posts` and its size in `counts.expanded`.
// That contradicts the reason admission is asked BEFORE expansion in
// the first place.
func TestBatch_ExpansionObeysPostReadability(t *testing.T) {
	f := newBatchFixture(t)
	team := f.team("oracle")
	stranger := f.user("postowner")
	nosy := f.user("nosy")
	f.grant(nosy, capBulkEdit, &team) // admitted, and entitled to nothing here
	ctx := f.identity(nosy)

	secret1 := f.asset(&stranger, nil)
	secret2 := f.asset(&stranger, nil)
	private := f.post(stranger, secret1, secret2)
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE posts SET visibility = 'private' WHERE id = $1`, private); err != nil {
		t.Fatalf("make the post private: %v", err)
	}
	field := f.textField(false)

	res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), postEntries(private))
	if res.OK == nil {
		t.Fatalf("the request itself is well formed: %d %+v", res.Status, res.Refusal)
	}
	if res.OK.Counts.Expanded != 0 {
		t.Fatalf("a post the caller may not read contributes NOTHING; got %+v", res.OK.Counts)
	}
	if len(res.OK.Targets) != 0 {
		t.Fatalf("MEMBERSHIP ORACLE: %d member asset ids disclosed", len(res.OK.Targets))
	}
	// And it is not reported as an EMPTY post either: "your selection
	// reached nothing" and "you may not see that" must look the same.
	if res.OK.EmptyPosts != nil && len(*res.OK.EmptyPosts) != 0 {
		t.Fatalf("an unreadable post must not be distinguishable from one that does not exist; got %+v",
			res.OK.EmptyPosts)
	}

	// The gate is the SHIPPED rule and not a stricter invention: an
	// ORG-ONLY post is readable by any signed-in caller, and still
	// expands.
	orgOnly := f.post(stranger, secret1)
	ok := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), postEntries(orgOnly))
	if ok.Counts.Expanded != 1 {
		t.Fatalf("an org-only post is readable and must still expand; got %+v", ok.Counts)
	}
}
