// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// BATCH METADATA EDIT — vocabulary, references, and the five-gate
// authorization stack (#1173, #1119, ADR 0019, ADR 0092).
//
// # Why none of this can ride the corpus
//
// The live field definitions include ZERO with a write_capability,
// ZERO with a read_capability, ZERO read_only, and NO ROLE HOLDS
// assets.admin. Every gate this file tests is therefore constructed
// here: its own definition, its own vocabulary state, its own grant,
// its own team, its own ownership and its own reference target.
//
// # The counterweights
//
// The single-target vocabulary behaviour this file's batch layer must
// REPRODUCE rather than invent is asserted separately and passes today:
// open_vocabulary_e2e_test.go, vocabulary_curation_e2e_test.go and
// write_capability_scope_e2e_test.go.
package metadata_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/metadata"
	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

const capVocabExtend = "fields.vocabulary.extend"

// ---------------------------------------------------------------------------
// A28-A32 — grandfathering, preserved
// ---------------------------------------------------------------------------

// A28. An overwrite proposing a DEPRECATED slug THE TARGET ALREADY
// HOLDS is grandfathered: would_change, and it writes. Options are
// never hard-deleted precisely so a record carrying a since-retired
// term keeps working, and refusing this would freeze every record
// holding a deprecated term.
func TestBatch_GrandfatheredDeprecatedSlugWrites(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("gf")
	field := f.field("sel", fieldSpec{Type: "select", Options: []map[string]any{
		vocabOption("old", "Old", "deprecated"),
	}})
	holder := f.asset(&owner, nil)
	f.setValue(holder, field, map[string]any{"text": "old"})

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("old"), assetEntries(holder))
	if got, _ := partitionOf(p, holder); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("a target already holding the retired term is grandfathered; want would_change, got %s", got)
	}
	res := f.apply(ctx, p.Token, "re-save a grandfathered term", intp(1))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if got, _ := f.storedText(holder, field); got != "old" {
		t.Fatalf("want old stored, got %q", got)
	}
}

// A29. append INCLUDING a deprecated slug the target already contains
// is permitted.
func TestBatch_AppendWithHeldDeprecatedTerm(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("gfa")
	field := f.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{
		vocabOption("old", "Old", "deprecated"), vocabOption("new", "New", "active"),
	}})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"options": []string{"old"}})

	p := f.mustPreview(ctx, openapi.BatchModeAppend, field, optionsValue("old", "new"), assetEntries(asset))
	if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("want would_change, got %s", got)
	}
	res := f.apply(ctx, p.Token, "append beside a held retired term", nil)
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	got, _ := f.storedOptions(asset, field)
	if strings.Join(got, ",") != "old,new" {
		t.Fatalf("want [old new], got %v", got)
	}
}

// A30. THE SAME SLUG CHOSEN FRESH ON A SIBLING TARGET IN THE SAME
// REQUEST IS REFUSED. This is why the grandfather verdict is PER TARGET
// and cannot be settled batch-wide: one request, one slug, two answers.
func TestBatch_RetiredSlugFreshOnSiblingIsRefused(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("gfs")
	field := f.field("sel", fieldSpec{Type: "select", Options: []map[string]any{
		vocabOption("old", "Old", "deprecated"), vocabOption("other", "Other", "active"),
	}})
	holder := f.asset(&owner, nil)
	stranger := f.asset(&owner, nil)
	f.setValue(holder, field, map[string]any{"text": "old"})
	f.setValue(stranger, field, map[string]any{"text": "other"})

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("old"),
		assetEntries(holder, stranger))
	assertReconciles(t, p.Counts)

	if got, _ := partitionOf(p, holder); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("the holder is grandfathered; want would_change, got %s", got)
	}
	got, _ := partitionOf(p, stranger)
	if got != openapi.BatchPartitionRefused {
		t.Fatalf("choosing a retired term FRESH must be refused; got %s", got)
	}
	for _, tgt := range p.Targets {
		if uuid.UUID(tgt.AssetId) == stranger {
			if tgt.RefusalReason == nil || *tgt.RefusalReason != openapi.BatchRefusalVocabularyRetiredNotHeld {
				t.Fatalf("want vocabulary_retired_not_held, got %v", tgt.RefusalReason)
			}
		}
	}

	res := f.apply(ctx, p.Token, "one slug, two answers", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if v, _ := f.storedText(stranger, field); v != "other" {
		t.Fatalf("the refused sibling must keep its value, got %q", v)
	}
}

// A31. A removal SUCCEEDS while another retired term remains in the
// residual. The residual is a subset of what the target already held,
// so every term in it is grandfathered by construction — and failing
// here would make a deprecated keyword impossible to edit around.
func TestBatch_RemoveLeavesAnotherRetiredTerm(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("gfr")
	field := f.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{
		vocabOption("a", "A", "active"), vocabOption("old", "Old", "deprecated"),
	}})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"options": []string{"a", "old"}})

	p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("a"), assetEntries(asset))
	if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("want would_change, got %s", got)
	}
	res := f.apply(ctx, p.Token, "remove around a retired term", intp(1))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	got, _ := f.storedOptions(asset, field)
	if strings.Join(got, ",") != "old" {
		t.Fatalf("want [old] left behind, got %v", got)
	}
}

// A32. fill_empties with a retired slug: a POPULATED target is no_op
// (the value is not empty, so the retired term is never chosen), and an
// EMPTY one is refused (it would be choosing it fresh).
func TestBatch_FillEmptiesWithRetiredSlug(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("gff")
	field := f.field("sel", fieldSpec{Type: "select", Options: []map[string]any{
		vocabOption("old", "Old", "deprecated"), vocabOption("live", "Live", "active"),
	}})
	populated := f.asset(&owner, nil)
	empty := f.asset(&owner, nil)
	f.setValue(populated, field, map[string]any{"text": "live"})

	p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field, textValue("old"),
		assetEntries(populated, empty))
	if got, _ := partitionOf(p, populated); got != openapi.BatchPartitionNoOp {
		t.Fatalf("a populated target is no_op; got %s", got)
	}
	if got, _ := partitionOf(p, empty); got != openapi.BatchPartitionRefused {
		t.Fatalf("an empty target would be choosing the retired term FRESH; want refused, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// A33-A35 — non-mutating preview, lossless canonicalisation, phantoms
// ---------------------------------------------------------------------------

// A33. PREVIEW MINTS NOTHING. The options document is BYTE-IDENTICAL
// after previewing an unknown term on an OPEN vocabulary — the term is
// LISTED as mintable, and listing is not creating.
func TestBatch_PreviewMintsNothing(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("mint0")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("existing", "Existing", "active")}})
	asset := f.asset(&owner, nil)
	before := f.optionsDoc(field)

	p := f.mustPreview(ctx, openapi.BatchModeAppend, field, optionsValue("brand-new"), assetEntries(asset))
	if p.MintableTerms == nil || len(*p.MintableTerms) != 1 || (*p.MintableTerms)[0] != "brand-new" {
		t.Fatalf("the new term must be REPORTED as mintable, got %+v", p.MintableTerms)
	}
	if after := f.optionsDoc(field); !bytes.Equal(before, after) {
		t.Fatalf("A PREVIEW MUST NOT MUTATE the options document\nbefore=%s\nafter=%s", before, after)
	}
}

// A34. LOSSLESS CANONICALISATION: what the preview binds is what the
// apply stores, across every case the contract enumerates.
func TestBatch_CanonicalisationPreviewEqualsApply(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("canon")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	cases := []struct {
		name string
		send []string
		want []string
	}{
		{"whitespace-only term is DROPPED", []string{"live", "   "}, []string{"live"}},
		{"case variant reaches the EXISTING slug", []string{"LIVE"}, []string{"live"}},
		{"whitespace variant reaches the existing slug", []string{"  live  "}, []string{"live"}},
		{"alias reaches its target", []string{"uk"}, []string{"gb"}},
		{"merge tombstone reaches its successor", []string{"gone"}, []string{"live"}},
		{"two inputs collapsing to one slug DEDUPE, order preserved", []string{"LIVE", "live"}, []string{"live"}},
		{"a genuinely new term slugifies", []string{"Brand New"}, []string{"brand-new"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
				Options: []map[string]any{
					vocabOption("live", "Live", "active"),
					vocabOptionWith("gb", "United Kingdom", "active", map[string]any{"aliases": []string{"uk"}}),
					vocabOptionWith("gone", "Gone", "archived", map[string]any{"replaced_by": "live"}),
				}})
			asset := f.asset(&owner, nil)

			p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue(tc.send...), assetEntries(asset))
			if p.ResolvedValue.ValueOptions == nil {
				t.Fatal("the preview must bind a resolved option set")
			}
			gotPreview := strings.Join(*p.ResolvedValue.ValueOptions, ",")
			if gotPreview != strings.Join(tc.want, ",") {
				t.Fatalf("preview bound %q, want %q", gotPreview, strings.Join(tc.want, ","))
			}
			res := f.apply(ctx, p.Token, "canonicalisation", intp(p.Counts.WouldChange))
			if res.OK == nil {
				t.Fatalf("apply refused: %+v", res.Refusal)
			}
			stored, _ := f.storedOptions(asset, field)
			if strings.Join(stored, ",") != gotPreview {
				t.Fatalf("PREVIEW AND APPLY DISAGREE: preview %q, stored %q", gotPreview, strings.Join(stored, ","))
			}
		})
	}

	// A term with NO ALPHANUMERICS AT ALL has no addressable form and
	// never will, so it is refused rather than dropped.
	t.Run("no alphanumerics is UNKNOWN, not dropped", func(t *testing.T) {
		field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
			Options: []map[string]any{vocabOption("live", "Live", "active")}})
		asset := f.asset(&owner, nil)
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, optionsValue("!!!"), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchUnknownSlug)
	})
}

// A35. PHANTOM WOULD_CHANGE PREVENTION. A term that LOOKS new but whose
// canonical slug is ALREADY HELD counts no_op at preview and stays a
// no-op at apply. Without canonicalisation before partitioning, the
// operator would confirm a count that was never true.
func TestBatch_PhantomWouldChangePrevention(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("phantom")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("sunset", "Sunset", "active")}})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"options": []string{"sunset"}})

	// "Sunset " looks like a new term and is not one.
	p := f.mustPreview(ctx, openapi.BatchModeAppend, field, optionsValue("Sunset "), assetEntries(asset))
	if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionNoOp {
		t.Fatalf("a term canonicalising onto a HELD slug is a no_op; got %s", got)
	}
	if p.Counts.WouldChange != 0 {
		t.Fatalf("PHANTOM would_change: %+v", p.Counts)
	}
	if p.MintableTerms != nil && len(*p.MintableTerms) != 0 {
		t.Fatalf("nothing is mintable here; got %+v", p.MintableTerms)
	}

	before := f.historyCount(asset, field)
	res := f.apply(ctx, p.Token, "phantom check", nil)
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if f.historyCount(asset, field) != before {
		t.Fatal("the no-op must stay a no-op at apply")
	}
}

// ---------------------------------------------------------------------------
// A36-A41 — the coupled mint, and mint authority at apply
// ---------------------------------------------------------------------------

// A36. MINT COUPLING, the refusing half: a batch whose every
// would_change target ends in conflict commits NO new term and leaves
// the options document BYTE-IDENTICAL. "Preview predicted
// would_change > 0" is not sufficient — the operator's word never
// reached a record, so the catalogue must not have grown because of it.
func TestBatch_MintCoupled_NoCommitNoTerm(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("mintc")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"options": []string{"live"}})

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("fresh-term"), assetEntries(asset))
	before := f.optionsDoc(field)

	// Make every would_change target conflict by moving its value after
	// the preview: the guarded write now finds a set_at it did not bind.
	f.setValue(asset, field, map[string]any{"options": []string{"live", "live"}})

	res := f.apply(ctx, p.Token, "every target conflicts", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("a batch where every target conflicts is still a COMMITTED apply: %+v", res.Refusal)
	}
	if res.OK.OutcomeCounts.Changed != 0 || res.OK.OutcomeCounts.Conflict != 1 {
		t.Fatalf("want 0 changed / 1 conflict, got %+v", res.OK.OutcomeCounts)
	}
	if res.OK.CommittedTerms != nil && len(*res.OK.CommittedTerms) != 0 {
		t.Fatalf("no write stored the term, so none may be committed; got %+v", res.OK.CommittedTerms)
	}
	if after := f.optionsDoc(field); !bytes.Equal(before, after) {
		t.Fatalf("the options document must be BYTE-IDENTICAL\nbefore=%s\nafter=%s", before, after)
	}
}

// A37. MINT COUPLING, the committing half: at least one successful
// write means the term commits, ONCE.
func TestBatch_MintCoupled_CommitMintsOnce(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("mintok")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	a1 := f.asset(&owner, nil)
	a2 := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("fresh-term"),
		assetEntries(a1, a2))
	res := f.apply(ctx, p.Token, "mint on a real write", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if res.OK.CommittedTerms == nil || len(*res.OK.CommittedTerms) != 1 || (*res.OK.CommittedTerms)[0] != "fresh-term" {
		t.Fatalf("want exactly one committed term, got %+v", res.OK.CommittedTerms)
	}
	// ONCE, however many targets stored it.
	doc := string(f.optionsDoc(field))
	if n := strings.Count(doc, `"fresh-term"`); n != 1 {
		t.Fatalf("the term must appear ONCE in the document, found %d in %s", n, doc)
	}
}

// A38. MINT AUTHORITY AT APPLY: held at preview, REVOKED before apply.
// Zero writes, zero mint, options byte-identical, TOKEN NOT CONSUMED.
func TestBatch_MintAuthorityRevokedBeforeApply(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("mintrev")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("would-be-new"), assetEntries(asset))
	before := f.optionsDoc(field)

	f.revoke(owner, capVocabExtend, nil)

	res := f.apply(ctx, p.Token, "authority moved", intp(p.Counts.WouldChange))
	f.wantRefusal(res, 403, openapi.BatchVocabularyExtendRequired)
	if f.rowExists(asset, field) {
		t.Fatal("ZERO writes")
	}
	if after := f.optionsDoc(field); !bytes.Equal(before, after) {
		t.Fatal("ZERO mint: the options document must be byte-identical")
	}
	if f.tokenConsumed(p.OperationId.String()) {
		t.Fatal("a pre-write refusal MUST NOT consume the token")
	}
	if f.envelopes(p.OperationId.String()) != 0 {
		t.Fatal("a pre-write refusal commits NO audit envelope")
	}
}

// A39. `fields.vocabulary.extend` is GLOBAL-ONLY, reproduced exactly:
// a caller holding it ONLY TEAM-SCOPED cannot mint, because they cannot
// mint on the single-target path either.
func TestBatch_MintAuthorityIsGlobalOnly(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("mintteam")
	team := f.team("mint")
	f.grant(owner, capBulkEdit, nil)
	f.grant(owner, capVocabExtend, &team) // scoped ONLY
	ctx := f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, &team)

	res := f.preview(ctx, openapi.BatchModeOverwrite, field, optionsValue("new-term"), assetEntries(asset))
	f.wantPreviewRefusal(res, 403, openapi.BatchVocabularyExtendRequired)
}

// A40. system.admin alone reaches the mint, through the EXISTING
// wildcard rather than an explicit arm in the code path.
func TestBatch_MintViaSystemAdminWildcard(t *testing.T) {
	f := newBatchFixture(t)
	admin := f.user("sysadmin")
	f.grant(admin, "system.admin", nil)
	ctx := f.identity(admin)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&admin, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("admin-term"), assetEntries(asset))
	res := f.apply(ctx, p.Token, "wildcard reaches the mint", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("system.admin must reach it: %+v", res.Refusal)
	}
	if res.OK.CommittedTerms == nil || len(*res.OK.CommittedTerms) != 1 {
		t.Fatalf("want a committed term, got %+v", res.OK.CommittedTerms)
	}
}

// A41. EFFECTIVE, NOT RAW. One DIRECT grant is removed while a ROLE
// still confers the capability, and the apply proceeds. A comparison
// built on grant-set equality would refuse here, and would be asserting
// about grant rows rather than about authority.
func TestBatch_MintAuthorityIsEffectiveNotRaw(t *testing.T) {
	f := newBatchFixture(t)
	owner, _ := f.bulkOperator("eff")
	f.grant(owner, capVocabExtend, nil)
	f.grantViaRole(owner, capVocabExtend)
	ctx := f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("effective"), assetEntries(asset))

	// The DIRECT grant goes; the ROLE still confers it.
	f.revoke(owner, capVocabExtend, nil)

	res := f.apply(ctx, p.Token, "still entitled via a role", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("the caller is still ENTITLED; refusing here asserts about grant rows: %+v", res.Refusal)
	}
	if res.OK.CommittedTerms == nil || len(*res.OK.CommittedTerms) != 1 {
		t.Fatalf("want a committed term, got %+v", res.OK.CommittedTerms)
	}
}

// ---------------------------------------------------------------------------
// A42-A46 — closed vocabularies and configuration refusals
// ---------------------------------------------------------------------------

// A42. Unknown on an OPEN vocabulary WITHOUT extend is 403.
func TestBatch_UnknownOpenWithoutExtend(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("noext")
	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, nil)

	res := f.preview(ctx, openapi.BatchModeOverwrite, field, optionsValue("nope"), assetEntries(asset))
	f.wantPreviewRefusal(res, 403, openapi.BatchVocabularyExtendRequired)
}

// A43. Unknown on a CLOSED vocabulary is 422 — a different answer,
// because the fix is a correction rather than a grant. Asserted on
// `select` and `tree` too, which ALWAYS take the closed branch however
// the open_vocabulary flag is set.
func TestBatch_UnknownClosedVocabulary(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("closed")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)
	asset := f.asset(&owner, nil)

	t.Run("multi_select closed", func(t *testing.T) {
		field := f.field("kw", fieldSpec{Type: "multi_select",
			Options: []map[string]any{vocabOption("live", "Live", "active")}})
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, optionsValue("nope"), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchUnknownSlug)
	})
	// ⚠️ open_vocabulary is honoured for multi_select ONLY. `select`
	// and `tree` take the closed branch EVEN WITH THE FLAG SET and
	// EVEN FOR A CALLER HOLDING extend.
	for _, typ := range []string{"select", "tree"} {
		t.Run(typ+" ignores open_vocabulary", func(t *testing.T) {
			field := f.field("v", fieldSpec{Type: typ, OpenVocabulary: true,
				Options: []map[string]any{vocabOption("live", "Live", "active")}})
			res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("nope"), assetEntries(asset))
			f.wantPreviewRefusal(res, 422, openapi.BatchUnknownSlug)
		})
	}
}

// A44-A46. read_only, mirrors_column and regexp_filter.
func TestBatch_ConfigurationRefusals(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("cfg")
	asset := f.asset(&owner, nil)

	t.Run("read_only", func(t *testing.T) {
		field := f.field("ro", fieldSpec{Type: "text", ReadOnly: true})
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchFieldReadOnly)
	})
	t.Run("mirrors_column", func(t *testing.T) {
		field := f.field("mir", fieldSpec{Type: "text", MirrorsColumn: "title"})
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchFieldMirrored)
	})
	t.Run("regexp_filter", func(t *testing.T) {
		field := f.field("pat", fieldSpec{Type: "text", RegexpFilter: `[0-9]+`})
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("letters"), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchPatternMismatch)
		if ok := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("1234"), assetEntries(asset)); ok.OK == nil {
			t.Fatalf("a matching value must be accepted, got %d %+v", ok.Status, ok.Refusal)
		}
	})
	t.Run("archived field", func(t *testing.T) {
		field := f.field("arch", fieldSpec{Type: "text", Status: "archived"})
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchFieldArchived)
	})
}

// ---------------------------------------------------------------------------
// A47-A50 — reference, four states
// ---------------------------------------------------------------------------

func TestBatch_ReferenceFourStates(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("ref")
	field := f.field("ref", fieldSpec{Type: "reference"})

	// A47. Absent or soft-deleted AT PREVIEW: 422 dangling_reference,
	// ZERO targets.
	t.Run("absent at preview", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, refValue(uuid.New()), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchDanglingReference)
	})
	t.Run("soft-deleted at preview", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		target := f.asset(&owner, nil)
		f.softDeleteAsset(target)
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, refValue(target), assetEntries(asset))
		f.wantPreviewRefusal(res, 422, openapi.BatchDanglingReference)
	})

	// A48. Live at preview, SOFT-DELETED before apply: 409
	// reference_invalidated, ZERO field writes. THE STATUS AND REASON
	// DIFFER FROM A47's — the first says it never resolved, this says
	// it stopped, and they call for different corrections.
	t.Run("soft-deleted between preview and apply", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		target := f.asset(&owner, nil)
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, refValue(target), assetEntries(asset))
		f.softDeleteAsset(target)

		res := f.apply(ctx, p.Token, "target vanished", intp(p.Counts.WouldChange))
		f.wantRefusal(res, 409, openapi.BatchReferenceInvalidated)
		if res.Status == 422 || (res.Refusal != nil && res.Refusal.Reason == openapi.BatchDanglingReference) {
			t.Fatal("reference_invalidated and dangling_reference must NOT be collapsed")
		}
		if f.rowExists(asset, field) {
			t.Fatal("ZERO field writes")
		}
		if f.tokenConsumed(p.OperationId.String()) {
			t.Fatal("a pre-write refusal leaves the token usable")
		}
	})

	// A49. HARD ABSENT before apply: the same 409.
	t.Run("hard absent between preview and apply", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		target := f.asset(&owner, nil)
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, refValue(target), assetEntries(asset))
		if _, err := f.pool.Exec(f.ctx, `DELETE FROM assets WHERE id = $1`, target); err != nil {
			t.Fatalf("hard delete: %v", err)
		}
		res := f.apply(ctx, p.Token, "target hard-deleted", intp(p.Counts.WouldChange))
		f.wantRefusal(res, 409, openapi.BatchReferenceInvalidated)
		if f.rowExists(asset, field) {
			t.Fatal("ZERO field writes")
		}
	})

	// A50. ARCHIVED BUT NOT DELETED IS VALID. Archive is not deletion:
	// the gate filters deleted_at and NEVER status, on the way in and
	// on the way out.
	t.Run("archived target is VALID", func(t *testing.T) {
		asset := f.asset(&owner, nil)
		target := f.asset(&owner, nil)
		f.archiveAsset(target)
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, refValue(target), assetEntries(asset))
		res := f.apply(ctx, p.Token, "archived is not deleted", intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("an ARCHIVED reference target is valid: %+v", res.Refusal)
		}
		if res.OK.OutcomeCounts.Changed != 1 {
			t.Fatalf("the batch must write it, got %+v", res.OK.OutcomeCounts)
		}
	})
}

// ---------------------------------------------------------------------------
// A51-A53 — G4, the field's own write_capability
// ---------------------------------------------------------------------------

// A51-A53. A caller who holds the bulk instrument, owns every target
// and can read the field but LACKS its write_capability is refused
// BATCH-WIDE — with ZERO TARGETS INSPECTED and zero writes. Granted the
// capability GLOBALLY they proceed; held only TEAM-SCOPED they are
// STILL REFUSED, because the shipped rule is global-only.
func TestBatch_FieldWriteCapability(t *testing.T) {
	f := newBatchFixture(t)
	team := f.team("wc")

	t.Run("lacking it refuses batch-wide, zero inspection", func(t *testing.T) {
		owner, ctx := f.bulkOperator("wc1")
		field := f.field("wc", fieldSpec{Type: "text", WriteCapability: "fields.admin"})
		asset := f.asset(&owner, nil)
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset))
		f.wantPreviewRefusal(res, 403, openapi.BatchFieldWriteCapabilityRequired)
		if res.OK != nil {
			t.Fatal("ZERO TARGETS INSPECTED: a refusal on a field-level gate carries no partition")
		}
		if f.rowExists(asset, field) {
			t.Fatal("zero writes")
		}
	})

	t.Run("granted GLOBALLY it proceeds", func(t *testing.T) {
		owner, _ := f.bulkOperator("wc2")
		f.grant(owner, "fields.admin", nil)
		ctx := f.identity(owner)
		field := f.field("wc", fieldSpec{Type: "text", WriteCapability: "fields.admin"})
		asset := f.asset(&owner, nil)
		if p := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset)); p.OK == nil {
			t.Fatalf("a global holding must pass, got %d %+v", p.Status, p.Refusal)
		}
	})

	// ⚠️ THE SHIPPED ASYMMETRY, REPRODUCED AND NOT FIXED. The field's
	// write_capability is global-only on the single-target writer while
	// its READ gate is team-scope aware. The batch must answer the same
	// way the ordinary endpoint does; widening it here would quietly
	// grant scoped holders a write they do not have.
	t.Run("held only TEAM-SCOPED is STILL REFUSED", func(t *testing.T) {
		owner := f.user("wc3")
		f.grant(owner, capBulkEdit, nil)
		f.grant(owner, "fields.admin", &team)
		ctx := f.identity(owner)
		field := f.field("wc", fieldSpec{Type: "text", WriteCapability: "fields.admin"})
		asset := f.asset(&owner, &team)
		res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset))
		f.wantPreviewRefusal(res, 403, openapi.BatchFieldWriteCapabilityRequired)
	})
}

// ---------------------------------------------------------------------------
// A54-A60 — G5, and the anti-oracle boundary
// ---------------------------------------------------------------------------

// A54. A caller who may WRITE but may not READ the field on a subject
// gets `unreadable`, and the target is never written.
func TestBatch_CanWriteCannotRead(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("nr")
	field := f.field("rc", fieldSpec{Type: "text", ReadCapability: "fields.admin"})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(asset))
	if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionUnreadable {
		t.Fatalf("want unreadable, got %s", got)
	}
	if p.Counts.WouldChange != 0 {
		t.Fatalf("an unreadable target is never a would_change: %+v", p.Counts)
	}
	res := f.apply(ctx, p.Token, "unreadable target", intp(0))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if f.rowExists(asset, field) {
		t.Fatal("an unreadable target must never be written")
	}
}

// A55. THE ORACLE PROOF. Two targets in the SAME TEAM holding
// DIFFERENT stored values produce BYTE-IDENTICAL preview output —
// same partition, no set_at, nothing that could distinguish them —
// because neither is readable. An implementation that inspected the
// value before the read gate would report different partitions here and
// pass every other assertion in this file.
func TestBatch_UnreadableTargetsAreIndistinguishable(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("oracle")
	field := f.field("rc", fieldSpec{Type: "text", ReadCapability: "fields.admin"})

	holdsValue := f.asset(&owner, nil)
	holdsNothing := f.asset(&owner, nil)
	f.setValue(holdsValue, field, map[string]any{"text": "a protected secret"})

	p := f.mustPreview(ctx, openapi.BatchModeFillEmpties, field, textValue("x"),
		assetEntries(holdsValue, holdsNothing))

	var seen []openapi.BatchAssetFieldPreviewTarget
	for _, tgt := range p.Targets {
		if uuid.UUID(tgt.AssetId) == holdsValue || uuid.UUID(tgt.AssetId) == holdsNothing {
			seen = append(seen, tgt)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("want both targets reported, got %d", len(seen))
	}
	if seen[0].Partition != seen[1].Partition {
		t.Fatalf("the two targets differ ONLY in a value the caller may not read; partitions %s vs %s",
			seen[0].Partition, seen[1].Partition)
	}
	for _, tgt := range seen {
		if tgt.Partition != openapi.BatchPartitionUnreadable {
			t.Fatalf("want unreadable, got %s", tgt.Partition)
		}
		if tgt.IfUnchangedSince != nil {
			t.Fatal("set_at discloses that a value exists and when it was written")
		}
		if tgt.RefusalReason != nil {
			t.Fatal("a refusal reason could describe the held value")
		}
	}
	if p.Counts.Unreadable != 2 || p.Counts.NoOp != 0 || p.Counts.WouldChange != 0 {
		t.Fatalf("no emptiness may be observable: %+v", p.Counts)
	}
}

// A56-A59. The same non-disclosure across ALL FOUR MODES — emptiness,
// membership, equality and set_at are unobservable in each.
func TestBatch_UnreadableAcrossAllFourModes(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("oracle4")
	field := f.field("rc", fieldSpec{Type: "multi_select", ReadCapability: "fields.admin",
		Options: []map[string]any{vocabOption("a", "A", "active"), vocabOption("b", "B", "active")}})

	holdsA := f.asset(&owner, nil)
	holdsAB := f.asset(&owner, nil)
	holdsNothing := f.asset(&owner, nil)
	f.setValue(holdsA, field, map[string]any{"options": []string{"a"}})
	f.setValue(holdsAB, field, map[string]any{"options": []string{"a", "b"}})

	selection := assetEntries(holdsA, holdsAB, holdsNothing)
	for _, mode := range []openapi.BatchAssetFieldMode{
		openapi.BatchModeOverwrite, openapi.BatchModeFillEmpties,
		openapi.BatchModeAppend, openapi.BatchModeRemove,
	} {
		t.Run(string(mode), func(t *testing.T) {
			p := f.mustPreview(ctx, mode, field, optionsValue("a"), selection)
			if p.Counts.Unreadable != 3 {
				t.Fatalf("%s: all three must be unreadable, got %+v", mode, p.Counts)
			}
			if p.Counts.WouldChange != 0 || p.Counts.NoOp != 0 || p.Counts.Refused != 0 {
				t.Fatalf("%s: nothing about the value may be observable, got %+v", mode, p.Counts)
			}
			for _, tgt := range p.Targets {
				if tgt.IfUnchangedSince != nil || tgt.RefusalReason != nil {
					t.Fatalf("%s: target %s leaked value state", mode, tgt.AssetId)
				}
			}
		})
	}
}

// A60. The AUDIT ENVELOPE for an unreadable target carries its id and
// its partition label ONLY. The audit log is read under
// system.audit.read by a DIFFERENT POPULATION from the field's readers,
// so putting the value there would be a side channel around the field's
// own gate — a thousand records at a time.
func TestBatch_AuditEnvelopeDisclosesNoValues(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("audit0")
	field := f.field("rc", fieldSpec{Type: "text", ReadCapability: "fields.admin"})
	unreadable := f.asset(&owner, nil)
	f.setValue(unreadable, field, map[string]any{"text": "a protected secret"})

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("new value"), assetEntries(unreadable))
	res := f.apply(ctx, p.Token, "audit disclosure check", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}

	env := f.envelope(p.OperationId.String())
	raw := strings.ToLower(mustJSON(t, env))
	for _, forbidden := range []string{"a protected secret", "new value"} {
		if strings.Contains(raw, strings.ToLower(forbidden)) {
			t.Fatalf("the envelope must record NO FIELD VALUE, found %q in %s", forbidden, raw)
		}
	}
	targets, ok := env["target_ids"].(map[string]any)
	if !ok {
		t.Fatalf("the envelope must account for its targets, got %v", env["target_ids"])
	}
	ids, ok := targets["unreadable"].([]any)
	if !ok || len(ids) != 1 || ids[0].(string) != unreadable.String() {
		t.Fatalf("the unreadable target contributes its ID AND ITS LABEL ONLY, got %v", targets)
	}
}

// ---------------------------------------------------------------------------
// A61-A69 — G1, G2 and effective permission at apply
// ---------------------------------------------------------------------------

// A61. NO BULK HOLDING ANYWHERE is 403 BEFORE EXPANSION.
func TestBatch_NoBulkHoldingRefusedBeforeExpansion(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("nobulk")
	ctx := f.identity(owner)
	field := f.textField(false)
	asset := f.asset(&owner, nil)
	post := f.post(owner, asset)

	res := f.preview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), postEntries(post))
	f.wantPreviewRefusal(res, 403, openapi.BatchBulkCapabilityRequired)
	if res.OK != nil {
		t.Fatal("no expansion may have happened")
	}
}

// A62-A64. A grant SCOPED TO TEAM A over a selection spanning A and B;
// a scoped grant and a TEAM-LESS target; a global grant reaching one.
//
// ⚠️ THE TEAM-LESS TRAP. `assets.team_id` is nullable and a team-less
// asset has no scope for a scoped grant to match. It must never be read
// as "no scope required, therefore anyone passes".
func TestBatch_BulkScopeIsPerTarget(t *testing.T) {
	f := newBatchFixture(t)
	teamA := f.team("A")
	teamB := f.team("B")
	field := f.textField(false)

	t.Run("scoped to A, selection spans A and B", func(t *testing.T) {
		owner := f.user("scoped")
		f.grant(owner, capBulkEdit, &teamA)
		f.grant(owner, "assets.admin", &teamA)
		f.grant(owner, "assets.admin", &teamB)
		ctx := f.identity(owner)
		inA := f.asset(nil, &teamA)
		inB := f.asset(nil, &teamB)

		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(inA, inB))
		assertReconciles(t, p.Counts)
		if got, _ := partitionOf(p, inA); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("team A is in scope; want would_change, got %s", got)
		}
		if got, _ := partitionOf(p, inB); got != openapi.BatchPartitionUnauthorized {
			t.Fatalf("team B is NOT in scope; want unauthorized, got %s", got)
		}
	})

	t.Run("a scoped grant does NOT reach a team-less asset", func(t *testing.T) {
		owner := f.user("scopedless")
		f.grant(owner, capBulkEdit, &teamA)
		ctx := f.identity(owner)
		teamless := f.asset(&owner, nil) // owned, so G2 passes
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(teamless))
		if got, _ := partitionOf(p, teamless); got != openapi.BatchPartitionUnauthorized {
			t.Fatalf("a team-less asset needs a GLOBAL holding; want unauthorized, got %s", got)
		}
	})

	t.Run("a GLOBAL grant reaches a team-less asset", func(t *testing.T) {
		owner, ctx := f.bulkOperator("globalless")
		teamless := f.asset(&owner, nil)
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(teamless))
		if got, _ := partitionOf(p, teamless); got != openapi.BatchPartitionWouldChange {
			t.Fatalf("a global holding reaches it; want would_change, got %s", got)
		}
	})
}

// A65. system.admin alone reaches EVERY target, through the existing
// wildcard, with NO EXPLICIT WILDCARD ARM in the code path.
func TestBatch_SystemAdminReachesEveryTarget(t *testing.T) {
	f := newBatchFixture(t)
	admin := f.user("sa")
	f.grant(admin, "system.admin", nil)
	ctx := f.identity(admin)
	team := f.team("sa")
	stranger := f.user("stranger")
	field := f.textField(false)

	teamAsset := f.asset(&stranger, &team)
	teamless := f.asset(&stranger, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"),
		assetEntries(teamAsset, teamless))
	if p.Counts.WouldChange != 2 || p.Counts.Unauthorized != 0 {
		t.Fatalf("system.admin reaches everything; got %+v", p.Counts)
	}
}

// A66. Holding the INSTRUMENT is not authority: a caller with a global
// bulk grant who fails the ORDINARY subject rule gets `unauthorized`.
// The bulk capability composes with the shipped rule; it never replaces
// it.
func TestBatch_InstrumentDoesNotReplaceSubjectAuthority(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("instr")
	_ = owner
	stranger := f.user("notmine")
	field := f.textField(false)
	notMine := f.asset(&stranger, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(notMine))
	if got, _ := partitionOf(p, notMine); got != openapi.BatchPartitionUnauthorized {
		t.Fatalf("holding the instrument is not authority over somebody else's asset; got %s", got)
	}
}

// A67. THE EFFECTIVE-PERMISSION CASE. The GLOBAL bulk grant is revoked
// between preview and apply while a SCOPED grant for team A remains,
// and the selection spans A and B. The batch is NOT failed wholesale:
// A proceeds, B becomes unauthorized_at_apply. Refusing the whole batch
// would deny work the caller is still entitled to do.
func TestBatch_PartialScopeLossIsNotWholeBatchFailure(t *testing.T) {
	f := newBatchFixture(t)
	teamA := f.team("effA")
	teamB := f.team("effB")
	owner := f.user("partial")
	f.grant(owner, capBulkEdit, nil)    // global, to be revoked
	f.grant(owner, capBulkEdit, &teamA) // scoped, survives
	f.grant(owner, "assets.admin", &teamA)
	f.grant(owner, "assets.admin", &teamB)
	ctx := f.identity(owner)

	field := f.textField(false)
	inA := f.asset(nil, &teamA)
	inB := f.asset(nil, &teamB)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(inA, inB))
	if p.Counts.WouldChange != 2 {
		t.Fatalf("both targets are in scope at preview; got %+v", p.Counts)
	}

	f.revoke(owner, capBulkEdit, nil)

	res := f.apply(ctx, p.Token, "scope narrowed mid-flight", intp(2))
	if res.OK == nil {
		t.Fatalf("admission still HOLDS via the scoped grant; a whole-batch refusal is wrong: %+v", res.Refusal)
	}
	assertApplyReconciles(t, res.OK)
	if got, _ := outcomeOf(res.OK, inA); got != openapi.BatchOutcomeChanged {
		t.Fatalf("team A is still in scope; want changed, got %s", got)
	}
	got, _ := outcomeOf(res.OK, inB)
	if got != openapi.BatchOutcomeUnauthorizedAtApply {
		t.Fatalf("team B is no longer in scope; want unauthorized_at_apply, got %s", got)
	}
	for _, tgt := range res.OK.Targets {
		if uuid.UUID(tgt.AssetId) == inB {
			if tgt.UnauthorizedReason == nil || *tgt.UnauthorizedReason != openapi.BatchUnauthorizedBulkScope {
				t.Fatalf("want the bulk_scope sub-reason, got %v", tgt.UnauthorizedReason)
			}
		}
	}
	if got, _ := f.storedText(inA, field); got != "x" {
		t.Fatalf("team A must be written, got %q", got)
	}
	if f.rowExists(inB, field) {
		t.Fatal("team B must not be written")
	}
}

// A68. The field's write_capability revoked between preview and apply
// is a BATCH-WIDE 403 with zero writes — a field-level gate is a
// property of the field, not of any target.
func TestBatch_WriteCapabilityRevokedBeforeApply(t *testing.T) {
	f := newBatchFixture(t)
	owner, _ := f.bulkOperator("wcrev")
	f.grant(owner, "fields.admin", nil)
	ctx := f.identity(owner)
	field := f.field("wc", fieldSpec{Type: "text", WriteCapability: "fields.admin"})
	a1 := f.asset(&owner, nil)
	a2 := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(a1, a2))
	f.revoke(owner, "fields.admin", nil)

	res := f.apply(ctx, p.Token, "field gate moved", intp(p.Counts.WouldChange))
	f.wantRefusal(res, 403, openapi.BatchFieldWriteCapabilityRequired)
	for _, a := range []uuid.UUID{a1, a2} {
		if f.rowExists(a, field) {
			t.Fatal("ZERO writes")
		}
	}
	if f.tokenConsumed(p.OperationId.String()) {
		t.Fatal("a pre-write refusal leaves the token usable")
	}
}

// A69. read_capability revoked for ONE TEAM makes that target
// unauthorized_at_apply with the `unreadable` sub-reason, and the rest
// of the batch proceeds.
func TestBatch_ReadCapabilityRevokedForOneTeam(t *testing.T) {
	f := newBatchFixture(t)
	teamA := f.team("rcA")
	teamB := f.team("rcB")
	owner := f.user("rcrev")
	f.grant(owner, capBulkEdit, nil)
	f.grant(owner, "assets.admin", nil)
	f.grant(owner, "fields.admin", &teamA)
	f.grant(owner, "fields.admin", &teamB)
	ctx := f.identity(owner)

	field := f.field("rc", fieldSpec{Type: "text", ReadCapability: "fields.admin"})
	inA := f.asset(nil, &teamA)
	inB := f.asset(nil, &teamB)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("x"), assetEntries(inA, inB))
	if p.Counts.WouldChange != 2 {
		t.Fatalf("both readable at preview; got %+v", p.Counts)
	}

	f.revoke(owner, "fields.admin", &teamB)

	res := f.apply(ctx, p.Token, "read gate narrowed", intp(2))
	if res.OK == nil {
		t.Fatalf("apply refused wholesale: %+v", res.Refusal)
	}
	if got, _ := outcomeOf(res.OK, inA); got != openapi.BatchOutcomeChanged {
		t.Fatalf("team A must still be written, got %s", got)
	}
	for _, tgt := range res.OK.Targets {
		if uuid.UUID(tgt.AssetId) == inB {
			if tgt.Outcome != openapi.BatchOutcomeUnauthorizedAtApply {
				t.Fatalf("want unauthorized_at_apply, got %s", tgt.Outcome)
			}
			if tgt.UnauthorizedReason == nil || *tgt.UnauthorizedReason != openapi.BatchUnauthorizedUnreadable {
				t.Fatalf("want the unreadable sub-reason, got %v", tgt.UnauthorizedReason)
			}
		}
	}
	if f.rowExists(inB, field) {
		t.Fatal("the newly-unreadable target must not be written")
	}
}

// ---------------------------------------------------------------------------
// A77-A79 — per-target staleness at apply
// ---------------------------------------------------------------------------

func TestBatch_PerTargetStalenessAtApply(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("stale")
	field := f.field("t", fieldSpec{Type: "text"})

	// A77. One target's value changed → conflict, the rest apply.
	t.Run("value moved is conflict, rest proceed", func(t *testing.T) {
		moved := f.asset(&owner, nil)
		steady := f.asset(&owner, nil)
		f.setValue(moved, field, map[string]any{"text": "original"})
		f.setValue(steady, field, map[string]any{"text": "original"})

		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
			assetEntries(moved, steady))
		f.setValue(moved, field, map[string]any{"text": "somebody else"})

		res := f.apply(ctx, p.Token, "one target moved", intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("a partial apply is still a COMMITTED apply: %+v", res.Refusal)
		}
		assertApplyReconciles(t, res.OK)
		if got, _ := outcomeOf(res.OK, moved); got != openapi.BatchOutcomeConflict {
			t.Fatalf("want conflict, got %s", got)
		}
		if got, _ := outcomeOf(res.OK, steady); got != openapi.BatchOutcomeChanged {
			t.Fatalf("want changed, got %s", got)
		}
		if v, _ := f.storedText(moved, field); v != "somebody else" {
			t.Fatalf("a stale write must not erase a newer value it never saw, got %q", v)
		}
	})

	// A78. SOFT-DELETED after preview → gone.
	t.Run("soft-deleted is gone", func(t *testing.T) {
		doomed := f.asset(&owner, nil)
		survivor := f.asset(&owner, nil)
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
			assetEntries(doomed, survivor))
		f.softDeleteAsset(doomed)

		res := f.apply(ctx, p.Token, "one target deleted", intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("apply refused: %+v", res.Refusal)
		}
		assertApplyReconciles(t, res.OK)
		if got, _ := outcomeOf(res.OK, doomed); got != openapi.BatchOutcomeGone {
			t.Fatalf("want gone, got %s", got)
		}
		if got, _ := outcomeOf(res.OK, survivor); got != openapi.BatchOutcomeChanged {
			t.Fatalf("want changed, got %s", got)
		}
	})

	// A79. ARCHIVED after preview is NOT a state change and IS STILL
	// WRITTEN. Only deleted_at removes a subject from the probe.
	t.Run("archived is still written", func(t *testing.T) {
		archived := f.asset(&owner, nil)
		p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
			assetEntries(archived))
		f.archiveAsset(archived)

		res := f.apply(ctx, p.Token, "archive is not deletion", intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("apply refused: %+v", res.Refusal)
		}
		if got, _ := outcomeOf(res.OK, archived); got != openapi.BatchOutcomeChanged {
			t.Fatalf("ARCHIVE IS NOT DELETION; want changed, got %s", got)
		}
		if v, _ := f.storedText(archived, field); v != "batch" {
			t.Fatalf("want the value written, got %q", v)
		}
	})
}

// A114. A field HIDDEN by its display_condition is STILL BATCH-WRITABLE.
// display_condition is a FORM HINT and never authorization: it decides
// whether a control is drawn and nothing about access or write validity
// (ADR 0099 §1).
func TestBatch_HiddenFieldIsStillWritable(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("hidden")

	controller := f.field("ctl", fieldSpec{Type: "select",
		Options: []map[string]any{vocabOption("yes", "Yes", "active"), vocabOption("no", "No", "active")}})
	dependent := f.field("dep", fieldSpec{Type: "text"})

	// Configured through the DATABASE and then read back and EVALUATED,
	// rather than assumed. A conditional skip here would be worse than
	// no test at all: if the shape ever stopped being accepted, the row
	// would carry no condition, the dependent would be trivially visible,
	// and the test would go green while asserting nothing.
	var controllerCode string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT code FROM field_definition WHERE id = $1`, controller).Scan(&controllerCode); err != nil {
		t.Fatalf("read the controller's code: %v", err)
	}
	// The term is `<code><op><value>` with NO field: prefix — the bare
	// grammar ADR 0099 specifies. Writing "=yes" would name an EMPTY
	// controller code, which resolves to nothing, FAILS THE CONDITION
	// OPEN, and leaves the dependent visible: the test would then have
	// asserted that a VISIBLE field is writable, which proves nothing.
	condition := []string{controllerCode + "=yes"}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE field_definition SET display_condition = $1 WHERE id = $2`,
		condition, dependent); err != nil {
		t.Fatalf("configure display_condition: %v", err)
	}
	var stored []string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT display_condition FROM field_definition WHERE id = $1`, dependent).Scan(&stored); err != nil {
		t.Fatalf("read display_condition back: %v", err)
	}
	if len(stored) != 1 || stored[0] != controllerCode+"=yes" {
		t.Fatalf("the condition must actually be stored, got %v", stored)
	}

	asset := f.asset(&owner, nil)
	// The controller says "no", so the dependent is HIDDEN — asserted
	// through the SHIPPED evaluator, not inferred from the fixture.
	f.setValue(asset, controller, map[string]any{"text": "no"})
	shown := metadata.EvaluateDisplayCondition(stored, func(code string) (metadata.ControllerState, bool) {
		if code != controllerCode {
			return metadata.ControllerState{}, false
		}
		return metadata.ControllerState{
			Type: "select", Readable: true, Text: "no", HasText: true,
		}, true
	})
	if shown {
		t.Fatal("the fixture must make the dependent HIDDEN; otherwise this test asserts nothing")
	}

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, dependent, textValue("written anyway"),
		assetEntries(asset))
	if got, _ := partitionOf(p, asset); got != openapi.BatchPartitionWouldChange {
		t.Fatalf("a hidden field is still writable; want would_change, got %s", got)
	}
	res := f.apply(ctx, p.Token, "hidden but writable", intp(1))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	if v, _ := f.storedText(asset, dependent); v != "written anyway" {
		t.Fatalf("want the value written, got %q", v)
	}
}
