// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// THE OLD BEHAVIOUR, exercised (#1173, #1119, ADR 0019).
//
// # What this file is for
//
// The batch endpoints do not exist at dev@80028e36, so no Class A test
// can be compiled there — and "the endpoint was absent" is not a proof
// of anything, because it is equally true of a feature that was never
// specified.
//
// So this file states what the system ACTUALLY DID at that commit, for
// each semantic family the batch contract governs, using only the
// SHIPPED single-target endpoints. It COMPILES AND PASSES at
// 80028e36. Every probe here asserts an old behaviour that the
// corresponding Class A row contradicts, which is what makes those rows
// red there: not the absence of a route, but a demonstrated behaviour
// the new contract forbids.
//
// It also passes at HEAD, and must — this sprint does not change the
// single-target plane. These are not Class A rows and are not counted
// as any. They are the evidence underneath them.
//
// # The families it cannot speak for
//
// Some of the contract governs capabilities with NO baseline
// counterpart of any kind — a token that can be spent, a partition that
// can be reconciled, a confirmation that can be typed. For those the
// old behaviour is genuine absence, and this file says so by NOT
// claiming a probe rather than by manufacturing one. The mapping in the
// PR names those rows individually.
package metadata_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// OLD BEHAVIOUR — THE SUBJECT GATE IS ABSENT.
//
// The ordinary field-value writer asks nothing about who owns the
// asset. A caller with no ownership, no team and no `assets.admin`
// writes it and gets a 200.
//
// Class A rows 61-69 assert the opposite for the batch: that same
// caller's targets partition `unauthorized` and are never written. On a
// system that behaves as this probe demonstrates, every one of those
// assertions fails.
func TestOldBehaviour_FieldValueWriterHasNoSubjectGate(t *testing.T) {
	f := newBatchFixture(t)
	writer := f.user("ob-writer")
	stranger := f.user("ob-owner")
	ctx := f.identity(writer)

	field := f.field("ob", fieldSpec{Type: "text"})
	notTheirs := f.asset(&stranger, nil)

	status, msg := f.setSingle(ctx, notTheirs, field, writeText("written by a stranger"))
	if status != 200 {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: the field-value writer is expected to accept a "+
			"write to an asset the caller does not own; got %d %s", status, msg)
	}
	if got, _ := f.storedText(notTheirs, field); got != "written by a stranger" {
		t.Fatalf("and to store it; got %q", got)
	}
	t.Log("OLD BEHAVIOUR: a caller with no ownership, no team and no assets.admin " +
		"writes another user's asset field and gets 200. Rows 61-69 forbid this.")
}

// OLD BEHAVIOUR — A WRITE NEVER CONSULTS READABILITY.
//
// A field carrying a `read_capability` the caller does not hold is
// still WRITABLE by them through the single-target endpoint: the write
// path has no read gate at all.
//
// Class A rows 54-60 assert that the batch partitions such a target
// `unreadable` and never writes it, and that nothing about its stored
// value is observable. On a system that behaves as this probe
// demonstrates, those rows fail.
func TestOldBehaviour_WritePathIgnoresReadCapability(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-nr")
	ctx := f.identity(owner)
	field := f.field("obrc", fieldSpec{Type: "text", ReadCapability: "fields.admin"})
	asset := f.asset(&owner, nil)

	status, msg := f.setSingle(ctx, asset, field, writeText("written without read rights"))
	if status != 200 {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: a caller lacking the field's read_capability is "+
			"expected to be able to WRITE it; got %d %s", status, msg)
	}
	t.Log("OLD BEHAVIOUR: the write path consults no read gate, so a caller who cannot read " +
		"a field can still set it. Rows 54-60 make readability a WRITE-path gate.")
}

// OLD BEHAVIOUR — DISCOVERING WHAT WOULD BE REFUSED REQUIRES MUTATING
// WHAT WOULD NOT.
//
// This is the single most load-bearing probe in the file, because it is
// the old behaviour behind the ENTIRE preview-and-partition contract.
//
// At baseline the only way to apply one change across several assets is
// several writes. They commit as they go. So an operator who discovers
// on the third asset that it refuses has ALREADY COMMITTED the first
// two — there is no way to learn the shape of the operation without
// performing part of it.
//
// Class A rows 1-27 and 106 assert that a preview reports every
// target's partition WITH NOTHING WRITTEN, and that the counts
// reconcile before anything is applied. On a system that behaves as
// this probe demonstrates, they fail.
func TestOldBehaviour_DiscoveryRequiresPartialMutation(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-partial")
	ctx := f.identity(owner)

	// A required field: the empty write is refused, the others are not.
	field := f.field("obreq", fieldSpec{Type: "text", Required: true})
	first := f.asset(&owner, nil)
	second := f.asset(&owner, nil)
	refuses := f.asset(&owner, nil)

	if s, _ := f.setSingle(ctx, first, field, writeText("value")); s != 200 {
		t.Fatalf("the first write is expected to succeed, got %d", s)
	}
	if s, _ := f.setSingle(ctx, second, field, writeText("value")); s != 200 {
		t.Fatalf("the second write is expected to succeed, got %d", s)
	}
	// The operator learns about the refusal only by reaching it.
	if s, _ := f.setSingle(ctx, refuses, field, writeText("")); s != 422 {
		t.Fatalf("the third is expected to refuse, got %d", s)
	}

	// AND THE FIRST TWO ARE ALREADY COMMITTED.
	for _, a := range []uuid.UUID{first, second} {
		if !f.rowExists(a, field) {
			t.Fatal("OLD BEHAVIOUR CLAIM FAILED: the earlier writes are expected to have committed")
		}
	}
	t.Log("OLD BEHAVIOUR: there is no way to learn which targets would refuse without " +
		"having already written the ones that would not. Rows 1-27 and 106 require a " +
		"complete partition with ZERO writes.")
}

// OLD BEHAVIOUR — REPEATING AN OPERATION REPEATS ITS EFFECTS.
//
// There is no token, no single-use guard and no idempotency of any
// kind: an operator who submits the same change twice writes twice, and
// the record carries two history rows for one intent.
//
// Class A rows 102-105 assert that a spent token answers 409 with ZERO
// additional writes, mints or envelopes. On a system that behaves as
// this probe demonstrates, they fail.
func TestOldBehaviour_ResubmissionWritesTwice(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-replay")
	ctx := f.identity(owner)
	field := f.field("obrep", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)

	for i := 0; i < 2; i++ {
		if s, _ := f.setSingle(ctx, asset, field, writeText("same value")); s != 200 {
			t.Fatalf("submission %d is expected to succeed, got %d", i+1, s)
		}
	}
	if n := f.historyCount(asset, field); n != 2 {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: two submissions are expected to write twice; "+
			"got %d history rows", n)
	}
	t.Log("OLD BEHAVIOUR: nothing makes an operation single-use, so a resubmission repeats " +
		"its effects. Rows 102-105 make the token spendable exactly once.")
}

// OLD BEHAVIOUR — A WRITE CARRIES NO OPERATOR REASON, AND NOTHING
// RECORDS ONE.
//
// The single-target write body has no `reason` member at all, and no
// audit event is emitted for a field-value change: the only trace is
// `asset_field_value_history`, which records WHAT changed and never
// WHY.
//
// Class A rows 86-94 assert a REQUIRED reason with two distinct bounds,
// recorded verbatim in an audit envelope. On a system that behaves as
// this probe demonstrates, they fail.
func TestOldBehaviour_NoOperatorReasonIsCarriedOrRecorded(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-reason")
	ctx := f.identity(owner)
	field := f.field("obrsn", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)

	before := f.auditEventCount()
	if s, _ := f.setSingle(ctx, asset, field, writeText("changed for reasons unrecorded")); s != 200 {
		t.Fatalf("the write is expected to succeed, got %d", s)
	}
	if after := f.auditEventCount(); after != before {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: a field-value write is expected to emit NO audit "+
			"event; %d appeared", after-before)
	}
	// And the history row it does write carries no reason column.
	var cols int
	if err := f.pool.QueryRow(f.ctx, `
		SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'asset_field_value_history' AND column_name = 'reason'`).Scan(&cols); err != nil {
		t.Fatalf("inspect history columns: %v", err)
	}
	if cols != 0 {
		t.Fatal("OLD BEHAVIOUR CLAIM FAILED: the history table is expected to have no reason column")
	}
	t.Log("OLD BEHAVIOUR: a field-value change records what changed and never why, and emits " +
		"no audit event. Rows 86-94 and 107 require a mandatory reason in one envelope.")
}

// OLD BEHAVIOUR — LEARNING WHETHER A TERM IS NEW REQUIRES CREATING IT.
//
// Minting lives inside the write transaction. There is no way to ask
// "would this term be created" without performing the write that
// creates it, and no way to make the creation conditional on the write
// having reached a record.
//
// Class A rows 33 and 36-37 assert that a preview mints NOTHING and
// that a term commits only if a successful write stored it. On a system
// that behaves as this probe demonstrates, they fail.
func TestOldBehaviour_MintingRequiresWriting(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-mint")
	f.grant(owner, capVocabExtend, nil)
	ctx := f.identity(owner)

	field := f.field("obkw", fieldSpec{Type: "multi_select", OpenVocabulary: true,
		Options: []map[string]any{vocabOption("live", "Live", "active")}})
	asset := f.asset(&owner, nil)
	before := string(f.optionsDoc(field))

	opts := []string{"brand-new-term"}
	status, msg := f.setSingle(ctx, asset, field, openapi.AssetFieldValueWrite{ValueOptions: &opts})
	if status != 200 {
		t.Fatalf("the write is expected to succeed, got %d %s", status, msg)
	}
	if after := string(f.optionsDoc(field)); after == before {
		t.Fatal("OLD BEHAVIOUR CLAIM FAILED: the write is expected to have CREATED the term")
	}
	t.Log("OLD BEHAVIOUR: the only way to find out a term is new is to create it, as part of " +
		"the write. Rows 33 and 36-37 require a preview that mints nothing and a mint " +
		"coupled to a committed write.")
}

// OLD BEHAVIOUR — A REFERENCE IS CHECKED ONCE, PER WRITE, AND ARCHIVE
// IS NOT DELETION.
//
// The write gate filters `deleted_at` and never `status`, so an
// ARCHIVED asset is a valid target; and the check is scoped to the one
// write that performs it, so nothing holds the target still across a
// series of writes.
//
// Class A rows 47-50 assert 422 at preview versus 409 at apply as
// DISTINCT answers, with the target held for the whole batch. Row 50's
// archived case is the half the old behaviour already gets right, and
// it is probed here so the batch's reproduction of it is provably a
// reproduction.
func TestOldBehaviour_ReferenceGateFiltersDeletedNotArchived(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-ref")
	ctx := f.identity(owner)
	field := f.field("obref", fieldSpec{Type: "reference"})
	subject := f.asset(&owner, nil)

	archived := f.asset(&owner, nil)
	f.archiveAsset(archived)
	ref := openapi_types.UUID(archived)
	if s, msg := f.setSingle(ctx, subject, field,
		openapi.AssetFieldValueWrite{ValueRef: &ref}); s != 200 {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: an ARCHIVED reference target is expected to be "+
			"valid; got %d %s", s, msg)
	}

	deleted := f.asset(&owner, nil)
	f.softDeleteAsset(deleted)
	ref2 := openapi_types.UUID(deleted)
	if s, _ := f.setSingle(ctx, subject, field,
		openapi.AssetFieldValueWrite{ValueRef: &ref2}); s != 422 {
		t.Fatalf("a SOFT-DELETED target is expected to be refused; got %d", s)
	}
	t.Log("OLD BEHAVIOUR: only deleted_at invalidates a reference target; archive does not. " +
		"Row 50 reproduces this; rows 47-49 add the preview/apply distinction it has no " +
		"counterpart for.")
}

// OLD BEHAVIOUR — A HIDDEN FIELD IS WRITABLE.
//
// `display_condition` is a form hint and never authorization, so a
// field hidden on a subject still takes writes.
//
// Class A row 114 asserts the batch preserves this. It is probed here
// so the batch's answer is provably the SAME answer rather than a
// coincidence.
func TestOldBehaviour_HiddenFieldIsWritable(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-hidden")
	ctx := f.identity(owner)

	controller := f.field("obctl", fieldSpec{Type: "select",
		Options: []map[string]any{vocabOption("yes", "Yes", "active"), vocabOption("no", "No", "active")}})
	dependent := f.field("obdep", fieldSpec{Type: "text"})
	var code string
	if err := f.pool.QueryRow(f.ctx, `SELECT code FROM field_definition WHERE id = $1`,
		controller).Scan(&code); err != nil {
		t.Fatalf("read controller code: %v", err)
	}
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE field_definition SET display_condition = $1 WHERE id = $2`,
		[]string{code + "=yes"}, dependent); err != nil {
		t.Fatalf("configure display_condition: %v", err)
	}

	asset := f.asset(&owner, nil)
	f.setValue(asset, controller, map[string]any{"text": "no"}) // the dependent is hidden

	if s, msg := f.setSingle(ctx, asset, dependent, writeText("written while hidden")); s != 200 {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: a hidden field is expected to remain writable; "+
			"got %d %s", s, msg)
	}
	t.Log("OLD BEHAVIOUR: display_condition hides a control and gates nothing. Row 114 " +
		"preserves this on the batch plane.")
}

// OLD BEHAVIOUR — NOTHING BOUNDS HOW MANY ASSETS ONE INTENT MAY REACH.
//
// There is no ceiling anywhere on the single-target plane: an operator
// scripting the same change across any number of assets is refused at
// no N. The count is bounded only by their patience.
//
// Class A rows 108-110 assert two hard ceilings that refuse rather than
// trim. On a system that behaves as this probe demonstrates, they fail.
func TestOldBehaviour_NoCeilingBoundsAnOperation(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-ceiling")
	ctx := f.identity(owner)
	field := f.field("obceil", fieldSpec{Type: "text"})

	// A modest N stands for any N: the point is that no refusal exists
	// at any size, and the writer cannot tell one write from the
	// thousandth.
	for i := 0; i < 25; i++ {
		asset := f.asset(&owner, nil)
		if s, _ := f.setSingle(ctx, asset, field, writeText("unbounded")); s != 200 {
			t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: write %d is expected to succeed; got %d", i, s)
		}
	}
	t.Log("OLD BEHAVIOUR: no ceiling of any kind bounds how many assets one intent reaches. " +
		"Rows 108-110 add two, and refuse rather than trim.")
}

// OLD BEHAVIOUR — THERE IS NO AUTHORITY READ TO MAKE ATOMIC.
//
// The batch's per-target invariant is that the subject's owner and team,
// the verdict drawn from them, and the write that verdict authorises are
// ONE atomic operation. At baseline there is no such pair to serialise:
// the write path READS NEITHER, so ownership can change to a stranger
// and the very next write still lands.
//
// ⚠️ A near-miss worth naming, because it looks like the guarantee and
// is not. A field-value write DOES touch the `assets` row — the
// `asset_field_value_search_text` trigger fires AFTER the write and
// issues `UPDATE assets SET search_text = …`, which takes FOR NO KEY
// UPDATE and will therefore wait on a concurrent uncommitted transfer.
// But that serialises the TAIL of the write, long after the value has
// been decided, and there was no authority read at the head for it to
// protect. The batch's FOR SHARE is taken BEFORE the read, which is the
// property the invariant is about and the one the trigger cannot supply.
//
// Class A rows 70-76 assert seven serialisation seams. On a system that
// behaves as this probe demonstrates, they fail.
func TestOldBehaviour_NoAuthorityReadExistsToSerialise(t *testing.T) {
	f := newBatchFixture(t)
	writer := f.user("ob-race")
	stranger := f.user("ob-race-new-owner")
	ctx := f.identity(writer)
	field := f.field("obrace", fieldSpec{Type: "text"})
	asset := f.asset(&writer, nil)

	// The transfer COMMITS, so there is no race to lose — the write
	// that follows sees the new owner and proceeds anyway.
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE assets SET owner_user_ref = $1 WHERE id = $2`, stranger, asset); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if s, msg := f.setSingle(ctx, asset, field, writeText("written after the transfer")); s != 200 {
		t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: the write is expected to succeed against an asset "+
			"transferred away; got %d %s", s, msg)
	}

	// And a team move changes nothing either.
	team := f.team("ob-race-team")
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE assets SET team_id = $1 WHERE id = $2`, team, asset); err != nil {
		t.Fatalf("team move: %v", err)
	}
	if s, _ := f.setSingle(ctx, asset, field, writeText("written after the team move")); s != 200 {
		t.Fatalf("the write is expected to succeed after a team move; got %d", s)
	}
	t.Log("OLD BEHAVIOUR: the write path reads neither owner nor team, so there is no " +
		"authority-and-write pair to serialise and no state a transfer could race. " +
		"Rows 70-76 introduce seven seams that only exist once the read does.")
}

// OLD BEHAVIOUR — THE GRANDFATHER VERDICT IS PER WRITE, AND THE TWO
// ANSWERS CANNOT COEXIST IN ONE OPERATION.
//
// A deprecated term may be kept and not chosen, and the test consults
// what the record already holds. At baseline that verdict is reached
// once per write, so the same slug being permitted on one asset and
// refused on another is TWO OPERATIONS, and the permitted one has
// already committed by the time the refusal is discovered.
//
// Class A rows 28-32 assert both verdicts in ONE request, reported
// together with nothing written. On a system that behaves as this probe
// demonstrates, they fail.
func TestOldBehaviour_GrandfatherVerdictIsPerWrite(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-gf")
	ctx := f.identity(owner)
	field := f.field("obgf", fieldSpec{Type: "select", Options: []map[string]any{
		vocabOption("old", "Old", "deprecated"), vocabOption("other", "Other", "active"),
	}})
	holder := f.asset(&owner, nil)
	stranger := f.asset(&owner, nil)
	f.setValue(holder, field, map[string]any{"text": "old"})
	f.setValue(stranger, field, map[string]any{"text": "other"})

	if s, msg := f.setSingle(ctx, holder, field, writeText("old")); s != 200 {
		t.Fatalf("re-saving a HELD retired term is expected to succeed; got %d %s", s, msg)
	}
	if s, _ := f.setSingle(ctx, stranger, field, writeText("old")); s != 422 {
		t.Fatalf("choosing it FRESH is expected to be refused; got %d", s)
	}
	if got, _ := f.storedText(holder, field); got != "old" {
		t.Fatal("OLD BEHAVIOUR CLAIM FAILED: the permitted write is expected to have committed " +
			"before the refusal was discovered")
	}
	t.Log("OLD BEHAVIOUR: one slug, two verdicts, two separate writes — and the permitted one " +
		"commits before the refused one is even attempted. Rows 28-32 require both verdicts " +
		"in ONE request with nothing written.")
}

// OLD BEHAVIOUR — `fields.vocabulary.extend` IS GLOBAL-ONLY, AND IT IS
// EFFECTIVE RATHER THAN RAW.
//
// Class A rows 39-41 assert the batch reproduces exactly this. Probed
// here so the reproduction is provably a reproduction and not a
// coincidence — and so that if a later sprint makes the shipped gate
// team-scope aware, this probe is what fails first.
func TestOldBehaviour_VocabularyExtendIsGlobalOnlyAndEffective(t *testing.T) {
	f := newBatchFixture(t)
	team := f.team("ob-ext")

	newField := func() uuid.UUID {
		return f.field("obext", fieldSpec{Type: "multi_select", OpenVocabulary: true,
			Options: []map[string]any{vocabOption("live", "Live", "active")}})
	}
	mint := func(ctx context.Context, field, asset uuid.UUID, term string) int {
		opts := []string{term}
		s, _ := f.setSingle(ctx, asset, field, openapi.AssetFieldValueWrite{ValueOptions: &opts})
		return s
	}

	t.Run("a TEAM-SCOPED holding does not mint", func(t *testing.T) {
		u := f.user("ob-ext-scoped")
		f.grant(u, capVocabExtend, &team)
		ctx := f.identity(u)
		field, asset := newField(), f.asset(&u, &team)
		if s := mint(ctx, field, asset, "scoped-attempt"); s != 422 {
			t.Fatalf("OLD BEHAVIOUR CLAIM FAILED: extend is expected to be GLOBAL-ONLY; got %d", s)
		}
	})

	t.Run("system.admin mints through the wildcard", func(t *testing.T) {
		u := f.user("ob-ext-admin")
		f.grant(u, "system.admin", nil)
		ctx := f.identity(u)
		field, asset := newField(), f.asset(&u, nil)
		if s := mint(ctx, field, asset, "admin-term"); s != 200 {
			t.Fatalf("system.admin is expected to reach the mint; got %d", s)
		}
	})

	t.Run("EFFECTIVE, not raw: a role still confers it", func(t *testing.T) {
		u := f.user("ob-ext-role")
		f.grantViaRole(u, capVocabExtend)
		ctx := f.identity(u)
		field, asset := newField(), f.asset(&u, nil)
		if s := mint(ctx, field, asset, "role-term"); s != 200 {
			t.Fatalf("a capability conferred through a ROLE is expected to work; got %d", s)
		}
	})
	t.Log("OLD BEHAVIOUR: extend is global-only, wildcarded by system.admin, and resolved " +
		"effectively. Rows 39-41 reproduce all three on the batch plane.")
}

// OLD BEHAVIOUR — CONFIGURATION REFUSALS ARE REACHED ONE WRITE AT A
// TIME.
//
// read_only, a mirrored column and a `regexp_filter` each refuse on the
// single-target path. They are properties of the FIELD, so at baseline
// an operator changing a hundred assets discovers them on the first
// write — after which they have written nothing, but they had no way to
// know that before trying.
//
// Class A rows 42-46 assert the batch refuses them BATCH-WIDE with ZERO
// TARGETS INSPECTED. Probed here so the batch's refusals are provably
// the shipped ones.
func TestOldBehaviour_ConfigurationRefusalsArePerWrite(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-cfg")
	ctx := f.identity(owner)
	asset := f.asset(&owner, nil)

	for _, tc := range []struct {
		name string
		spec fieldSpec
		send string
		want int
	}{
		{"read_only", fieldSpec{Type: "text", ReadOnly: true}, "x", 422},
		{"mirrors_column", fieldSpec{Type: "text", MirrorsColumn: "title"}, "x", 200},
		{"regexp_filter", fieldSpec{Type: "text", RegexpFilter: `[0-9]+`}, "letters", 422},
		{"unknown slug, closed vocabulary", fieldSpec{Type: "select",
			Options: []map[string]any{vocabOption("a", "A", "active")}}, "nope", 422},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field := f.field("obcfg", tc.spec)
			if s, msg := f.setSingle(ctx, asset, field, writeText(tc.send)); s != tc.want {
				t.Fatalf("OLD BEHAVIOUR CLAIM FAILED for %s: want %d, got %d %s",
					tc.name, tc.want, s, msg)
			}
		})
	}
	t.Log("OLD BEHAVIOUR: these refusals exist per write. A MIRRORED field is WRITABLE here " +
		"(the writer redirects to the asset column); rows 44-46 refuse it batch-wide instead, " +
		"because the batch has no business writing a second plane's values.")
}

// OLD BEHAVIOUR — A WRITE COMMITS IMMEDIATELY, WITH NO CONFIRMATION AND
// NO NO-OP DETECTION.
//
// Nothing counts what is about to change, nothing asks the operator to
// confirm a number, and a write carrying the value already stored still
// advances `set_at` and appends a history row.
//
// Class A row 80 asserts the batch reports `no_op == 0` for exactly
// this reason, and rows 81-85 add the typed confirmation ADR 0019
// specifies and nothing implements. On a system that behaves as this
// probe demonstrates, they fail.
func TestOldBehaviour_WritesCommitWithNoConfirmationStep(t *testing.T) {
	f := newBatchFixture(t)
	owner := f.user("ob-confirm")
	ctx := f.identity(owner)
	field := f.field("obcnf", fieldSpec{Type: "text"})
	asset := f.asset(&owner, nil)

	f.setValue(asset, field, map[string]any{"text": "same"})
	before := f.setAt(asset, field)
	beforeHistory := f.historyCount(asset, field)

	// The identical value, written again. No confirmation is asked for
	// and none can be supplied: the write body has no such member.
	if s, _ := f.setSingle(ctx, asset, field, writeText("same")); s != 200 {
		t.Fatalf("the write is expected to succeed, got %d", s)
	}
	if after := f.setAt(asset, field); !after.After(before) {
		t.Fatal("OLD BEHAVIOUR CLAIM FAILED: an identical write is expected to advance set_at")
	}
	if f.historyCount(asset, field) != beforeHistory+1 {
		t.Fatal("OLD BEHAVIOUR CLAIM FAILED: it is expected to append a history row")
	}
	t.Log("OLD BEHAVIOUR: a write commits on submission, with nothing counted and nothing " +
		"confirmed, and an identical value is still a change to the record. Row 80 keeps that " +
		"last fact; rows 81-85 add the confirmation that has no counterpart here.")
}
