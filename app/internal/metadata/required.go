// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/richtext"
)

// R1 — what `required` MEANS on the ordinary field-value write paths
// (#1389, ADR 0012).
//
// An operator could mark a field `required` and, on every path but the
// two mirrored helpers, nothing enforced it: an empty value was
// accepted and the value could be cleared outright, on assets AND on
// collections. `SetAssetFieldValue`, `ClearAssetFieldValue`,
// `SetCollectionFieldValue` and `ClearCollectionFieldValue` each
// contained zero `Required` checks.
//
// R1: a later HUMAN Set may not write an EMPTY value into a required
// field, and a later HUMAN Clear of one is refused.
//
// # R1 is not R2, and widening either into the other is the trap
//
// R2 is collection CREATE's required-PRESENCE rule
// (collections/handler.go), which answers 422
// RequiredCollectionFieldMissing when the create body omits a required
// collection field. It is unchanged and it must not grow: asset
// creation deliberately has NO completeness gate, because dropping a
// file and pressing publish has to keep requiring nothing.
//
// R1 is the LATER-write rule, and it attaches to a value that is being
// mutated rather than to a subject that is being created.
//
// # Which writers R1 governs, and why the seed is not one of them
//
//	SetAssetFieldValue / ClearAssetFieldValue            HUMAN edit / removal
//	SetCollectionFieldValue / ClearCollectionFieldValue  HUMAN edit / removal
//	SeedCollectionFieldValueInTx                         HUMAN INITIAL, from the create body
//	ApplyAssetDefaults                                   SYSTEM
//	the extraction adapter's WriteAssetFieldValue         SYSTEM
//	mirrorFill                                           SYSTEM
//
// The seed is a HUMAN write and is still exempt, and the exemption
// comes from the R1/R2 BOUNDARY rather than from provenance: it is the
// create body, which is R2's business, and R2 already requires a value
// to be there. Its own doc says required-field validation is the
// caller's job and already happened pre-transaction.
//
// The three SYSTEM exemptions are STRUCTURAL, exactly as `read_only`'s
// are: those call sites are different Go functions with no OpenAPI
// operation and no route, which is why `AssetFieldValueWrite.set_by`
// has no `default` or `mirror` member. There is deliberately no
// caller-supplied "initial" or "system" bypass, because a flag a
// caller can send is a flag a caller can lie about.

// requiredValue describes one stored value's five columns, so the
// per-type emptiness rule is written ONCE against the shape that
// actually lands in the table rather than twice against two request
// bodies that spell the same thing differently.
type requiredValue struct {
	Text    *string
	Num     *float64
	Date    pgtype.Timestamptz
	Options []string
	Ref     pgtype.UUID
}

func assetUpsertValue(p UpsertAssetFieldValueParams) requiredValue {
	return requiredValue{Text: p.ValueText, Num: p.ValueNum, Date: p.ValueDate, Options: p.ValueOptions, Ref: p.ValueRef}
}

func collectionUpsertValue(p UpsertCollectionFieldValueParams) requiredValue {
	return requiredValue{Text: p.ValueText, Num: p.ValueNum, Date: p.ValueDate, Options: p.ValueOptions, Ref: p.ValueRef}
}

// valueIsEmpty answers "does this value carry nothing", per type.
//
// The mirrored helpers' `strings.TrimSpace(value) == ""` is text-shaped
// and is not sufficient for the other ten types — a `multi_select`
// holding `[]` and a `number` holding NULL are both empty and neither
// is a string.
//
//	text, longtext, select, tree : value_text NULL, or whitespace-only
//	rich_text                    : SEMANTIC emptiness (see below)
//	multi_select                 : value_options NULL or zero-length
//	number, boolean              : value_num NULL
//	date, datetime               : value_date NULL
//	reference                    : value_ref NULL
//
// `boolean` is worth stating out loud: FALSE is a real answer, stored
// as value_num = 0, and only a NULL value_num is empty. A rule that
// tested truthiness would delete every deliberate "no".
//
// `rich_text` does NOT use TrimSpace, and that is the case a naive
// implementation passes. Its stored value is sanitised HTML and the
// sanitiser strips no empty elements, so `<p><br></p>` survives, trims
// to itself, and would sail through a required check while rendering as
// nothing at all. richtext.IsEmpty is the server-authoritative twin of
// the display rule the frontend already ships (fieldDisplay.ts's
// htmlToPlainText, which the field count and the "is this set" test are
// built on). One rule, in one place, callable from outside this
// package.
func valueIsEmpty(fieldType string, v requiredValue) bool {
	switch fieldType {
	case richtext.FieldType:
		return v.Text == nil || richtext.IsEmpty(*v.Text)
	case "text", "longtext", "select", "tree":
		return v.Text == nil || strings.TrimSpace(*v.Text) == ""
	case "multi_select":
		return len(v.Options) == 0
	case "number", "boolean":
		return v.Num == nil
	case "date", "datetime":
		return !v.Date.Valid
	case "reference":
		return !v.Ref.Valid
	default:
		// An unknown type reaches no writer — buildUpsertParams
		// refuses it first — and guessing "empty" for one would refuse
		// a write for a reason nobody could act on.
		return false
	}
}

// requiredSetRefusal returns the sentence refusing an empty human Set
// into a required field, or "" when the write may proceed.
func requiredSetRefusal(f FieldDefinition, v requiredValue) string {
	if !f.Required || !valueIsEmpty(f.Type, v) {
		return ""
	}
	return fmt.Sprintf(
		"%s is required, so it cannot be given an empty value. Write a value, or change the field's configuration if it should be optional",
		f.Code)
}

// requiredClearRefusal returns the sentence refusing a human Clear of a
// required field, or "" when the removal may proceed.
//
// This is the direct counterweight to the optional Clear that 20a makes
// work from the edit surfaces: required + human Clear is REFUSED,
// optional + human Clear SUCCEEDS. Without both halves the human
// surface cannot show that `required` is what caused the refusal.
func requiredClearRefusal(f FieldDefinition) string {
	if !f.Required {
		return ""
	}
	return fmt.Sprintf(
		"%s is required, so its value cannot be removed. Write a different value, or change the field's configuration if it should be optional",
		f.Code)
}
