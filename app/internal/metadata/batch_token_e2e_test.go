// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// BATCH METADATA EDIT — the token lifecycle, the validation precedence,
// the operator reason, the typed confirmation and the audit envelope
// (#1173, #1119, ADR 0019).
//
// # There is NO TOKEN OF ANY KIND in the repository at 80028e36
//
// So every assertion here fails there by construction. That is not the
// proof: the proof is that each one asserts a SPECIFIC ORDERING or a
// SPECIFIC DURABLE STATE that a plausible implementation gets wrong.
//
// The three that matter most:
//
//   - CONSUMPTION IS ASSERTED AGAINST THE DATABASE, not against an HTTP
//     response. An implementation that consumed on response delivery
//     would pass a status-code assertion and fail this one.
//   - THE ENUMERATION-ORACLE PROOF uses a SECOND REAL IDENTITY with its
//     own genuinely-minted tokens, not a mutated copy of the first
//     caller's. A mutated token proves the integrity check; another
//     caller's VALID token is the only thing that proves the binding.
//   - THE MODE-PROBE PROOF sends the two request shapes that
//     distinguish an overwrite token from a fill token against ANOTHER
//     CALLER'S tokens and requires them to be byte-identical, and then
//     sends the SAME two shapes against the caller's OWN equivalents and
//     requires them to differ. Neither half proves anything alone.
package metadata_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// ---------------------------------------------------------------------------
// A81-A85 — the typed confirmation count
// ---------------------------------------------------------------------------

// A81. The count confirms WOULD_CHANGE, not `eligible`. The fixture has
// REAL NO-OPS so the two numbers differ, and the ELIGIBLE number is
// asserted to be REJECTED — without that, a server confirming the wrong
// denominator would pass.
func TestBatch_ConfirmCountIsWouldChangeNotEligible(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("cc")
	field := f.field("ms", fieldSpec{Type: "multi_select", Options: []map[string]any{
		vocabOption("a", "A", "active"), vocabOption("b", "B", "active"),
	}})
	// Two targets hold "a" and will change; two hold only "b" and are
	// no-ops. would_change = 2, eligible = 4.
	willChange := []uuid.UUID{f.asset(&owner, nil), f.asset(&owner, nil)}
	noOps := []uuid.UUID{f.asset(&owner, nil), f.asset(&owner, nil)}
	for _, a := range willChange {
		f.setValue(a, field, map[string]any{"options": []string{"a", "b"}})
	}
	for _, a := range noOps {
		f.setValue(a, field, map[string]any{"options": []string{"b"}})
	}

	p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("a"),
		assetEntries(append(append([]uuid.UUID{}, willChange...), noOps...)...))
	if p.Counts.WouldChange != 2 || p.Counts.Eligible != 4 {
		t.Fatalf("the fixture must make would_change and eligible DIFFER; got %+v", p.Counts)
	}

	// The ELIGIBLE number is REJECTED.
	res := f.apply(ctx, p.Token, "the wrong denominator", intp(p.Counts.Eligible))
	f.wantRefusal(res, 400, openapi.BatchConfirmCountMismatch)
	if res.Refusal.Expected == nil || *res.Refusal.Expected != 2 {
		t.Fatalf("the refusal must NAME the expected value, got %+v", res.Refusal.Expected)
	}

	// The WOULD_CHANGE number is accepted, on the SAME token — which
	// also proves the mismatch left it unspent.
	ok := f.apply(ctx, p.Token, "the right denominator", intp(2))
	if ok.OK == nil {
		t.Fatalf("would_change must be accepted: %+v", ok.Refusal)
	}
}

// A82-A85. The four confirmation refusals.
func TestBatch_ConfirmCountRefusals(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("ccr")
	field := f.field("ms", fieldSpec{Type: "multi_select",
		Options: []map[string]any{vocabOption("a", "A", "active"), vocabOption("b", "B", "active")}})

	newToken := func(mode openapi.BatchAssetFieldMode) *openapi.BatchAssetFieldPreview {
		asset := f.asset(&owner, nil)
		f.setValue(asset, field, map[string]any{"options": []string{"a"}})
		return f.mustPreview(ctx, mode, field, optionsValue("b"), assetEntries(asset))
	}

	// A82. ABSENT on a mode that requires it.
	for _, mode := range []openapi.BatchAssetFieldMode{openapi.BatchModeOverwrite, openapi.BatchModeRemove} {
		t.Run(string(mode)+"/absent", func(t *testing.T) {
			p := newToken(mode)
			res := f.apply(ctx, p.Token, "no count", nil)
			f.wantRefusal(res, 400, openapi.BatchConfirmCountRequired)
		})
	}

	// A83. NON-INTEGER, NEGATIVE and ABOVE-CEILING — three cases. The
	// non-integer case cannot be built through the typed request object
	// (the generated member is an int), so the two the wire can carry
	// are asserted here and the third is a property of the schema.
	t.Run("negative", func(t *testing.T) {
		p := newToken(openapi.BatchModeOverwrite)
		res := f.apply(ctx, p.Token, "negative", intp(-1))
		f.wantRefusal(res, 400, openapi.BatchConfirmCountInvalid)
	})
	t.Run("above the ceiling", func(t *testing.T) {
		p := newToken(openapi.BatchModeOverwrite)
		res := f.apply(ctx, p.Token, "too big", intp(1001))
		f.wantRefusal(res, 400, openapi.BatchConfirmCountInvalid)
	})

	// A84. MISMATCH names the expected value.
	t.Run("mismatch names the expected value", func(t *testing.T) {
		p := newToken(openapi.BatchModeOverwrite)
		res := f.apply(ctx, p.Token, "wrong number", intp(p.Counts.WouldChange+1))
		f.wantRefusal(res, 400, openapi.BatchConfirmCountMismatch)
		if res.Refusal.Expected == nil || *res.Refusal.Expected != p.Counts.WouldChange {
			t.Fatalf("want expected=%d, got %+v", p.Counts.WouldChange, res.Refusal.Expected)
		}
	})

	// A85. SUPPLIED where it does not apply is REFUSED, NOT SILENTLY
	// IGNORED. A count on a mode that has none means the client and the
	// server disagree about what the operation is.
	for _, mode := range []openapi.BatchAssetFieldMode{openapi.BatchModeFillEmpties, openapi.BatchModeAppend} {
		t.Run(string(mode)+"/supplied", func(t *testing.T) {
			p := newToken(mode)
			res := f.apply(ctx, p.Token, "count where none applies", intp(p.Counts.WouldChange))
			f.wantRefusal(res, 400, openapi.BatchConfirmCountNotApplicable)
			// And ABSENT is accepted on the same modes.
			ok := f.apply(ctx, p.Token, "no count needed", nil)
			if ok.OK == nil {
				t.Fatalf("%s takes no count: %+v", mode, ok.Refusal)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A86-A94 — the operator reason
// ---------------------------------------------------------------------------

func TestBatch_OperatorReason(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("reason")
	field := f.field("t", fieldSpec{Type: "text"})

	fresh := func() *openapi.BatchAssetFieldPreview {
		asset := f.asset(&owner, nil)
		return f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("v"), assetEntries(asset))
	}

	// A86. Absent, empty and whitespace-only are all reason_required.
	for _, name := range []struct{ label, value string }{
		{"absent", ""}, {"empty", ""}, {"whitespace only", "   \t\n  "},
	} {
		t.Run("required/"+name.label, func(t *testing.T) {
			p := fresh()
			res := f.apply(ctx, p.Token, name.value, intp(p.Counts.WouldChange))
			f.wantRefusal(res, 400, openapi.BatchReasonRequired)
		})
	}

	// A87. EXACTLY 500 NON-WHITESPACE code points is ACCEPTED.
	t.Run("exactly 500 is accepted", func(t *testing.T) {
		p := fresh()
		res := f.apply(ctx, p.Token, strings.Repeat("a", 500), intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("500 code points is within the limit: %+v", res.Refusal)
		}
	})

	// A88. WHITESPACE AROUND A 500-CODE-POINT BODY — 504 raw — is
	// ACCEPTED. THE RECONCILIATION PROOF: the raw ceiling is checked
	// before trimming and the semantic limit after, and a server that
	// applied one bound in the other's place would refuse this.
	t.Run("504 raw around a 500 body is accepted", func(t *testing.T) {
		p := fresh()
		raw := "  " + strings.Repeat("a", 500) + "  "
		if len([]rune(raw)) != 504 {
			t.Fatalf("fixture must be 504 raw, is %d", len([]rune(raw)))
		}
		res := f.apply(ctx, p.Token, raw, intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("whitespace around a 500-code-point body must be accepted: %+v", res.Refusal)
		}
		// AUDITED VERBATIM AFTER TRIMMING, byte-identical on read-back.
		env := f.envelope(p.OperationId.String())
		got, _ := env["reason"].(string)
		if got != strings.Repeat("a", 500) {
			t.Fatalf("the audit must store the TRIMMED value byte-identically; got %d code points", len([]rune(got)))
		}
	})

	// A89. 501 AFTER TRIM is reason_too_long.
	t.Run("501 after trim is too long", func(t *testing.T) {
		p := fresh()
		res := f.apply(ctx, p.Token, strings.Repeat("a", 501), intp(p.Counts.WouldChange))
		f.wantRefusal(res, 400, openapi.BatchReasonTooLong)
	})

	// A90. 500 MULTI-BYTE CODE POINTS is ACCEPTED — CODE POINTS, NOT
	// BYTES. Roughly 1,500 bytes, which a byte-counting implementation
	// would refuse.
	t.Run("500 multi-byte code points is accepted", func(t *testing.T) {
		p := fresh()
		reason := strings.Repeat("日", 500)
		if len([]rune(reason)) != 500 || len(reason) < 1400 {
			t.Fatalf("fixture must be 500 code points and well over 500 bytes; %d/%d",
				len([]rune(reason)), len(reason))
		}
		res := f.apply(ctx, p.Token, reason, intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("the limit is CODE POINTS, not bytes: %+v", res.Refusal)
		}
	})

	// A91. RAW OVER 2,000 is reason_payload_too_large, asserted DISTINCT
	// from reason_too_long. Two bounds, two refusals, two corrections.
	t.Run("raw over 2000 is a DISTINCT refusal", func(t *testing.T) {
		p := fresh()
		res := f.apply(ctx, p.Token, strings.Repeat("a", 2001), intp(p.Counts.WouldChange))
		f.wantRefusal(res, 400, openapi.BatchReasonPayloadTooLarge)
		if res.Refusal.Reason == openapi.BatchReasonTooLong {
			t.Fatal("the raw ceiling and the semantic limit must not collapse into one refusal")
		}
	})

	// A92. A payload that is mostly whitespace still trips the RAW
	// ceiling — which is the whole reason it is checked before trimming.
	t.Run("a whitespace payload still trips the raw ceiling", func(t *testing.T) {
		p := fresh()
		res := f.apply(ctx, p.Token, strings.Repeat(" ", 2001)+"why", intp(p.Counts.WouldChange))
		f.wantRefusal(res, 400, openapi.BatchReasonPayloadTooLarge)
	})

	// A93. Every reason refusal LEAVES THE TOKEN UNSPENT.
	t.Run("a reason refusal does not consume the token", func(t *testing.T) {
		p := fresh()
		f.apply(ctx, p.Token, "", intp(p.Counts.WouldChange))
		if f.tokenConsumed(p.OperationId.String()) {
			t.Fatal("a Phase 0 refusal must not consume the token")
		}
		ok := f.apply(ctx, p.Token, "corrected", intp(p.Counts.WouldChange))
		if ok.OK == nil {
			t.Fatalf("the same token must still work: %+v", ok.Refusal)
		}
	})

	// A94. AUDIT ROUND-TRIP: an ordinary reason comes back byte-identical.
	t.Run("audit round-trip is byte-identical", func(t *testing.T) {
		p := fresh()
		reason := "Rewriting the credit line after the 2026 rights review — see ticket 4471."
		res := f.apply(ctx, p.Token, reason, intp(p.Counts.WouldChange))
		if res.OK == nil {
			t.Fatalf("apply refused: %+v", res.Refusal)
		}
		env := f.envelope(p.OperationId.String())
		if got, _ := env["reason"].(string); got != reason {
			t.Fatalf("want %q byte-identical, got %q", reason, got)
		}
	})
}

// ---------------------------------------------------------------------------
// A95-A101 — the validation precedence and the enumeration oracle
// ---------------------------------------------------------------------------

// A95. This caller's OWN, unconsumed, EXPIRED token is 409
// preview_expired.
func TestBatch_OwnExpiredToken(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("exp")
	field := f.field("t", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("v"), assetEntries(asset))
	f.expireToken(p.OperationId.String())

	res := f.apply(ctx, p.Token, "expired", intp(p.Counts.WouldChange))
	f.wantRefusal(res, 409, openapi.BatchPreviewExpired)
	if f.rowExists(asset, field) {
		t.Fatal("zero writes")
	}
}

// A96. An absent token member is 400 token_required — a Phase 0 check,
// because it is a fact about the request shape.
func TestBatch_TokenRequired(t *testing.T) {
	f := newBatchFixture(t)
	_, ctx := f.bulkOperator("tokreq")
	res := f.apply(ctx, "", "a reason", nil)
	f.wantRefusal(res, 400, openapi.BatchTokenRequired)
}

// A97-A100. THE ENUMERATION-ORACLE PROOF.
//
// A SECOND REAL IDENTITY with its own genuinely minted tokens — not a
// mutated copy of the first caller's, which would only ever prove the
// integrity check. Malformed, unknown/tampered and ANOTHER CALLER'S
// VALID token must be BYTE-IDENTICAL in status, body and message, and
// must STAY identical when that other caller's token is expired,
// consumed, or of a particular mode.
func TestBatch_TokenInvalidCollapse(t *testing.T) {
	f := newBatchFixture(t)
	mine, myCtx := f.bulkOperator("me")
	theirs, theirCtx := f.bulkOperator("them")

	field := f.field("ms", fieldSpec{Type: "multi_select",
		Options: []map[string]any{vocabOption("a", "A", "active"), vocabOption("b", "B", "active")}})

	// The other caller's tokens are REAL: minted by them, on their own
	// assets, through the ordinary path.
	theirAsset := func() uuid.UUID { return f.asset(&theirs, nil) }
	theirOverwrite := f.mustPreview(theirCtx, openapi.BatchModeOverwrite, field,
		optionsValue("a"), assetEntries(theirAsset()))
	theirFill := f.mustPreview(theirCtx, openapi.BatchModeFillEmpties, field,
		optionsValue("a"), assetEntries(theirAsset()))
	theirExpired := f.mustPreview(theirCtx, openapi.BatchModeOverwrite, field,
		optionsValue("a"), assetEntries(theirAsset()))
	f.expireToken(theirExpired.OperationId.String())
	theirConsumed := f.mustPreview(theirCtx, openapi.BatchModeOverwrite, field,
		optionsValue("a"), assetEntries(theirAsset()))
	// GENUINELY CONSUMED, by actually applying it.
	if r := f.apply(theirCtx, theirConsumed.Token, "spend it for real",
		intp(theirConsumed.Counts.WouldChange)); r.OK == nil {
		t.Fatalf("the other caller's own apply must succeed: %+v", r.Refusal)
	}
	if !f.tokenConsumed(theirConsumed.OperationId.String()) {
		t.Fatal("the fixture needs a GENUINELY consumed token")
	}

	// A tampered token: valid shape, real length, no row.
	tampered := strings.Repeat("A", len(theirOverwrite.Token))

	// The canonical refusal, taken from the malformed case.
	canonical := f.apply(myCtx, "not-a-token", "probing", nil)
	f.wantRefusal(canonical, 403, openapi.BatchPreviewTokenInvalid)

	same := func(label string, got applyResult) {
		t.Helper()
		if got.Status != canonical.Status {
			t.Fatalf("%s: status %d differs from the canonical %d", label, got.Status, canonical.Status)
		}
		if got.Refusal == nil {
			t.Fatalf("%s: no refusal body", label)
		}
		if got.Refusal.Reason != canonical.Refusal.Reason || got.Refusal.Error != canonical.Refusal.Error {
			t.Fatalf("%s: body %+v differs from the canonical %+v", label, got.Refusal, canonical.Refusal)
		}
		if mustJSON(t, *got.Refusal) != mustJSON(t, *canonical.Refusal) {
			t.Fatalf("%s: the bodies must be BYTE-IDENTICAL", label)
		}
	}

	// A97 — malformed, tampered/unknown, and another caller's VALID token.
	same("unknown/tampered", f.apply(myCtx, tampered, "probing", nil))
	same("another caller's valid token", f.apply(myCtx, theirOverwrite.Token, "probing", nil))

	// A98 — another caller's EXPIRED token is 403, NOT 409: steps 1-2
	// run before step 4.
	same("another caller's EXPIRED token", f.apply(myCtx, theirExpired.Token, "probing", nil))

	// A99 — another caller's CONSUMED token is 403, NOT 409: steps 1-2
	// run before step 3.
	same("another caller's CONSUMED token", f.apply(myCtx, theirConsumed.Token, "probing", nil))

	// A100 — THE MODE-PROBE PROOF, both halves.
	//
	// Against ANOTHER CALLER'S tokens, an overwrite token with the count
	// OMITTED and a fill token with the count SUPPLIED must be
	// byte-identical to each other AND to the canonical refusal. If
	// mode-specific validation ran before binding, these two would
	// answer confirm_count_required and confirm_count_not_applicable and
	// would spell out the mode of somebody else's preview.
	same("their overwrite token, count omitted", f.apply(myCtx, theirOverwrite.Token, "probing", nil))
	same("their fill token, count supplied", f.apply(myCtx, theirFill.Token, "probing", intp(1)))

	// And THE SAME TWO REQUEST SHAPES against MY OWN equivalent tokens
	// must DIFFER — which is what proves the mode-specific validation
	// exists at all and runs only after binding.
	myAsset := func() uuid.UUID { return f.asset(&mine, nil) }
	myOverwrite := f.mustPreview(myCtx, openapi.BatchModeOverwrite, field,
		optionsValue("a"), assetEntries(myAsset()))
	myFill := f.mustPreview(myCtx, openapi.BatchModeFillEmpties, field,
		optionsValue("a"), assetEntries(myAsset()))

	f.wantRefusal(f.apply(myCtx, myOverwrite.Token, "probing", nil),
		400, openapi.BatchConfirmCountRequired)
	f.wantRefusal(f.apply(myCtx, myFill.Token, "probing", intp(1)),
		400, openapi.BatchConfirmCountNotApplicable)
}

// A101. THE STEP-3-BEFORE-4 PRECEDENCE PROOF. This caller's own token,
// BOTH consumed AND expired, answers 409 preview_consumed and NOT
// preview_expired — because consumed tells the operator their operation
// ALREADY RAN, where expired would invite them to run it again.
func TestBatch_ConsumedWinsOverExpired(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("cwe")
	field := f.field("t", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("v"), assetEntries(asset))
	if r := f.apply(ctx, p.Token, "spend it", intp(p.Counts.WouldChange)); r.OK == nil {
		t.Fatalf("apply refused: %+v", r.Refusal)
	}
	f.expireToken(p.OperationId.String()) // now BOTH consumed and expired

	res := f.apply(ctx, p.Token, "replay a stale spent token", intp(p.Counts.WouldChange))
	f.wantRefusal(res, 409, openapi.BatchPreviewConsumed)
	if res.Refusal.Reason == openapi.BatchPreviewExpired {
		t.Fatal("CONSUMED WINS: reporting expiry would invite a duplicate run")
	}
}

// ---------------------------------------------------------------------------
// A102-A105 — consumption, replay, and the zero-change apply
// ---------------------------------------------------------------------------

// A102-A103. CONSUMPTION IS DURABLE AND ATOMIC, asserted DIRECTLY
// AGAINST THE DATABASE rather than against the HTTP response: after one
// apply, the field rows changed, EXACTLY ONE envelope exists, and the
// token is marked consumed — ALL THREE PRESENT TOGETHER. Then the same
// token is retried and observed to add nothing.
//
// An implementation that treated a delivered 200 as the consumption
// boundary passes a status assertion and fails this one.
func TestBatch_ConsumptionIsDurableAndAtomic(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("durable")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	a1 := f.asset(&owner, nil)
	a2 := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("minted-here"),
		assetEntries(a1, a2))
	res := f.apply(ctx, p.Token, "durable outcome", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	op := p.OperationId.String()

	// ALL THREE, from the DATABASE.
	for _, a := range []uuid.UUID{a1, a2} {
		if opts, ok := f.storedOptions(a, field); !ok || strings.Join(opts, ",") != "minted-here" {
			t.Fatalf("asset %s: want the value written, got %v", a, opts)
		}
	}
	if n := f.envelopes(op); n != 1 {
		t.Fatalf("want EXACTLY ONE audit envelope, found %d", n)
	}
	if !f.tokenConsumed(op) {
		t.Fatal("the token must be marked consumed IN THE SAME durable outcome")
	}
	beforeHistory := f.historyCount(a1, field) + f.historyCount(a2, field)
	beforeDoc := string(f.optionsDoc(field))

	// A102 — the replay. This is also the LOST-RESPONSE case: the
	// server committed, and the client (having lost the result or not)
	// sends the same token again.
	replay := f.apply(ctx, p.Token, "replay", intp(p.Counts.WouldChange))
	f.wantRefusal(replay, 409, openapi.BatchPreviewConsumed)

	if got := f.historyCount(a1, field) + f.historyCount(a2, field); got != beforeHistory {
		t.Fatalf("ZERO additional field writes; history went %d -> %d", beforeHistory, got)
	}
	if got := string(f.optionsDoc(field)); got != beforeDoc {
		t.Fatal("ZERO additional vocabulary mutations")
	}
	if n := f.envelopes(op); n != 1 {
		t.Fatalf("EXACTLY ONE ENVELOPE PER TOKEN, EVER; found %d after the replay", n)
	}
}

// A104. A PRE-WRITE REFUSAL DOES NOT CONSUME THE TOKEN. Refused twice,
// on two different grounds, then retried with a corrected payload — and
// it SUCCEEDS. Asserted with NO ENVELOPE written by the refused
// attempts.
func TestBatch_PreWriteRefusalLeavesTokenSpendable(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("prewrite")
	field := f.field("t", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("v"), assetEntries(asset))
	op := p.OperationId.String()

	f.wantRefusal(f.apply(ctx, p.Token, "a good reason", intp(p.Counts.WouldChange+5)),
		400, openapi.BatchConfirmCountMismatch)
	f.wantRefusal(f.apply(ctx, p.Token, "", intp(p.Counts.WouldChange)),
		400, openapi.BatchReasonRequired)

	if f.tokenConsumed(op) {
		t.Fatal("a pre-write refusal must leave the token usable")
	}
	if n := f.envelopes(op); n != 0 {
		t.Fatalf("the refused attempts must write NO envelope; found %d", n)
	}
	if f.rowExists(asset, field) {
		t.Fatal("the refused attempts must write nothing")
	}

	ok := f.apply(ctx, p.Token, "corrected", intp(p.Counts.WouldChange))
	if ok.OK == nil {
		t.Fatalf("the corrected retry must succeed on the SAME token: %+v", ok.Refusal)
	}
	if n := f.envelopes(op); n != 1 {
		t.Fatalf("exactly one envelope after the successful apply, found %d", n)
	}
}

// A105. A would_change == 0 APPLY IS A REAL OPERATION. It completes,
// the TOKEN IS CONSUMED, EXACTLY ONE envelope records the zero-change
// operation with its reason and counts, and ZERO field rows change. It
// is NOT a no-op that leaves the token spendable.
func TestBatch_ZeroChangeApplyIsARealOperation(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("zero")
	field := f.field("ms", fieldSpec{Type: "multi_select",
		Options: []map[string]any{vocabOption("a", "A", "active"), vocabOption("b", "B", "active")}})
	asset := f.asset(&owner, nil)
	f.setValue(asset, field, map[string]any{"options": []string{"b"}})

	// Removing a term the target does not hold: eligible, zero changes.
	p := f.mustPreview(ctx, openapi.BatchModeRemove, field, optionsValue("a"), assetEntries(asset))
	if p.Counts.WouldChange != 0 || p.Counts.NoOp != 1 {
		t.Fatalf("want 0 would_change / 1 no_op, got %+v", p.Counts)
	}
	beforeHistory := f.historyCount(asset, field)

	res := f.apply(ctx, p.Token, "nothing to do, and that is the answer", intp(0))
	if res.OK == nil {
		t.Fatalf("a zero-change apply is a REAL operation: %+v", res.Refusal)
	}
	op := p.OperationId.String()
	if !f.tokenConsumed(op) {
		t.Fatal("a zero-change apply CONSUMES its token")
	}
	if n := f.envelopes(op); n != 1 {
		t.Fatalf("want exactly one envelope for the zero-change operation, found %d", n)
	}
	env := f.envelope(op)
	if got, _ := env["reason"].(string); got != "nothing to do, and that is the answer" {
		t.Fatalf("the envelope must carry the reason, got %q", got)
	}
	if f.historyCount(asset, field) != beforeHistory {
		t.Fatal("ZERO field rows change")
	}

	// A second attempt is a replay.
	f.wantRefusal(f.apply(ctx, p.Token, "again", intp(0)), 409, openapi.BatchPreviewConsumed)
}

// ---------------------------------------------------------------------------
// A106-A107 — the apply equation and the envelope's contents
// ---------------------------------------------------------------------------

// A106. would_change = changed + conflict + gone + unauthorized_at_apply
// + error, on a batch that populates FOUR of the five.
func TestBatch_ApplyEquationHolds(t *testing.T) {
	f := newBatchFixture(t)
	teamA := f.team("eqA")
	teamB := f.team("eqB")
	owner := f.user("apeq")
	f.grant(owner, capBulkEdit, nil)
	f.grant(owner, capBulkEdit, &teamA)
	f.grant(owner, "assets.admin", nil)
	ctx := f.identity(owner)

	field := f.field("t", fieldSpec{Type: "text"})
	willChange := f.asset(nil, &teamA)
	willConflict := f.asset(nil, &teamA)
	willVanish := f.asset(nil, &teamA)
	willLoseScope := f.asset(nil, &teamB)
	f.setValue(willConflict, field, map[string]any{"text": "original"})

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, textValue("batch"),
		assetEntries(willChange, willConflict, willVanish, willLoseScope))
	if p.Counts.WouldChange != 4 {
		t.Fatalf("want 4 would_change, got %+v", p.Counts)
	}

	f.setValue(willConflict, field, map[string]any{"text": "moved"})
	f.softDeleteAsset(willVanish)
	f.revoke(owner, capBulkEdit, nil)      // global goes
	f.revoke(owner, "assets.admin", nil)   // and with it team B's reach
	f.grant(owner, "assets.admin", &teamA) // team A stays reachable

	res := f.apply(ctx, p.Token, "four outcomes at once", intp(4))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}
	assertApplyReconciles(t, res.OK)
	o := res.OK.OutcomeCounts
	if o.Changed != 1 || o.Conflict != 1 || o.Gone != 1 || o.UnauthorizedAtApply != 1 {
		t.Fatalf("want one of each of four outcomes, got %+v", o)
	}
}

// A107. THE ENVELOPE'S CONTENTS: actor, reason, mode, confirm_count,
// ALL SIX partition counts, committed terms and the target ids.
func TestBatch_AuditEnvelopeContents(t *testing.T) {
	f := newBatchFixture(t)
	owner, ctx := f.bulkOperator("env")
	f.grant(owner, capVocabExtend, nil)
	ctx = f.identity(owner)
	stranger := f.user("envstranger")

	field := f.field("kw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	mine := f.asset(&owner, nil)
	notMine := f.asset(&stranger, nil)

	p := f.mustPreview(ctx, openapi.BatchModeOverwrite, field, optionsValue("envelope-term"),
		assetEntries(mine, notMine))
	res := f.apply(ctx, p.Token, "recording the whole operation", intp(p.Counts.WouldChange))
	if res.OK == nil {
		t.Fatalf("apply refused: %+v", res.Refusal)
	}

	env := f.envelope(p.OperationId.String())
	for _, key := range []string{
		"operation_id", "mode", "field_id", "field_code", "reason", "confirm_count",
		"expanded", "eligible", "would_change", "no_op", "refused",
		"inapplicable", "unreadable", "unauthorized",
		"changed", "conflict", "gone", "unauthorized_at_apply", "error",
		"selection_entry_count", "committed_terms", "target_ids",
	} {
		if _, ok := env[key]; !ok {
			t.Fatalf("the envelope is missing %q: %v", key, env)
		}
	}
	if env["mode"] != "overwrite" {
		t.Fatalf("want mode overwrite, got %v", env["mode"])
	}
	if got := env["confirm_count"]; got == nil {
		t.Fatal("the accepted confirmation count must be recorded")
	}
	terms, _ := env["committed_terms"].([]any)
	if len(terms) != 1 || terms[0] != "envelope-term" {
		t.Fatalf("want the committed term recorded, got %v", env["committed_terms"])
	}
	targets, _ := env["target_ids"].(map[string]any)
	if _, ok := targets["changed"]; !ok {
		t.Fatalf("the written target must be accounted for, got %v", targets)
	}
	if _, ok := targets["unauthorized"]; !ok {
		t.Fatalf("the unauthorized target must be accounted for too, got %v", targets)
	}

	// The ACTOR is the initiating human, on the row rather than in the
	// payload.
	var actor *int64
	if err := f.pool.QueryRow(f.ctx,
		`SELECT actor_user_ref FROM audit_events WHERE metadata->>'operation_id' = $1`,
		p.OperationId.String()).Scan(&actor); err != nil {
		t.Fatalf("read actor: %v", err)
	}
	if actor == nil || *actor != owner {
		t.Fatalf("want the initiating human %d as actor, got %v", owner, actor)
	}
}
