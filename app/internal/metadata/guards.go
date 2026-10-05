// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// PER-FIELD optimistic concurrency for ordinary field values (#1119).
//
// # The token is the VALUE ROW'S OWN set_at
//
// Not the asset's `updated_at` and not the collection's. Two people
// editing two different fields of one subject are not in conflict, and
// a subject-level token would make every save on a busy record fight
// every other one. Both upserts already write `set_at = NOW()` on
// INSERT and inside ON CONFLICT DO UPDATE, and both response schemas
// already carry `set_at` as required, so the token a client needs is
// already in its hands and nothing had to be migrated to produce it.
//
// # Three states, and the third one is the one that already shipped
//
//	if_unchanged_since : guarded write against an EXISTING row
//	if_absent: true    : guarded FIRST write, against absence
//	neither            : unguarded last-write-wins, UNCHANGED
//
// The unguarded state is not a legacy accident to be tightened later.
// The upload flush writes a newly created asset's field values with no
// guard and must keep doing so, and so must any other caller that is
// not an edit surface.
//
// The two guards are MUTUALLY EXCLUSIVE and sending both is a 400: a
// write cannot be conditional on a row both existing at a known version
// and not existing.
//
// # Where the atomicity actually lives
//
// Not here. This file resolves what the caller asked for; the
// precondition is evaluated by the WHERE clause of the single statement
// that also performs the mutation (see the guarded queries in
// queries.sql). A check in this file followed by the unconditional
// upsert would be two statements at READ COMMITTED with a window
// between them, which is the failure mode the whole feature exists to
// close.

type guardKind int

const (
	guardNone guardKind = iota
	guardUnchangedSince
	guardAbsent
)

type writeGuard struct {
	kind  guardKind
	since pgtype.Timestamptz
}

func (g writeGuard) engaged() bool { return g.kind != guardNone }

// errGuardsConflict is the 400 for sending both preconditions.
var errGuardsConflict = errors.New(
	"if_unchanged_since and if_absent are mutually exclusive: a write cannot require both that a known version is still stored and that nothing is stored")

// resolveWriteGuard turns the two request members into one resolved
// precondition.
//
// `if_absent: false` is NOT a third guard. It engages nothing, and it
// does not make an accompanying `if_unchanged_since` a conflict —
// "require the row to be present" is not a precondition anybody asked
// for, and reading a false as a demand would refuse a client that
// serialises its optional booleans rather than omitting them.
func resolveWriteGuard(since *time.Time, absent *bool) (writeGuard, error) {
	wantAbsent := absent != nil && *absent
	if since != nil && wantAbsent {
		return writeGuard{}, errGuardsConflict
	}
	switch {
	case since != nil:
		return writeGuard{kind: guardUnchangedSince, since: pgtype.Timestamptz{Time: *since, Valid: true}}, nil
	case wantAbsent:
		return writeGuard{kind: guardAbsent}, nil
	default:
		return writeGuard{}, nil
	}
}

// mirroredGuardRefusal is the sentence refusing a per-field guard on a
// mirrored field.
//
// Mirrored values ARE the asset's own columns; there is no
// asset_field_value row to carry a `set_at`, and `AssetFieldValue.set_at`
// for a mirrored read is the asset's `updated_at` wearing the field
// shape. Accepting a per-field token here would mean guarding a column
// against a timestamp that belongs to a different plane, on a value the
// asset's own PATCH can change without this endpoint ever seeing it.
// The asset plane already has the guard for that: `AssetUpdate.if_unchanged_since`.
func mirroredGuardRefusal(code, col string) string {
	return fmt.Sprintf(
		"%s is a view onto assets.%s, so it takes no per-field concurrency guard: that column is written by the asset's own create and update paths too. Guard it with if_unchanged_since on PATCH /assets/{id}",
		code, col)
}

// ---------------------------------------------------------------------------
// The 409 bodies
// ---------------------------------------------------------------------------

// THE KEY IS ALWAYS PRESENT. `current` is required and nullable in the
// schema, and the generated Go carries it as a pointer with no
// `omitempty`, so a cleared value serialises as `"current": null`
// rather than vanishing. That distinction is the whole point: a client
// re-baselines `present: false` to `if_absent: true` and `present:
// true` to `current.set_at`, and an absent key would be
// indistinguishable from a server that forgot to send one.
//
// There is deliberately NO fabricated `set_at` for a row that does not
// exist. A timestamp invented for an absent row is a token no write
// ever produced.

// assetConflictBody reads the CURRENT committed state of one asset
// field value and renders the 409.
//
// It runs AFTER the guarded statement affected nothing and after the
// transaction has been rolled back, so what it reports is the committed
// world rather than the caller's own abandoned attempt. It may
// legitimately observe a state NEWER than the one that caused the
// refusal — a third writer can land in between — and that is the right
// answer anyway, because the value of this body is as a reconciliation
// baseline, not as a forensic record of which write won.
func (h *Handler) assetConflictBody(
	ctx context.Context,
	assetID, fieldID pgtype.UUID,
	fieldRow FieldDefinition,
) (openapi.AssetFieldValueConflict, error) {
	out := openapi.AssetFieldValueConflict{
		FieldId: openapi_types.UUID(uuid.UUID(fieldID.Bytes)),
	}
	q := New(h.Pool)
	row, err := q.GetAssetFieldValue(ctx, GetAssetFieldValueParams{AssetID: assetID, FieldID: fieldID})
	if errors.Is(err, pgx.ErrNoRows) {
		out.Present = false
		out.Current = nil
		out.Error = fmt.Sprintf(
			"%s was changed by somebody else: its value has been removed since this edit began, so nothing was written",
			fieldRow.Code)
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("metadata: read conflicting value: %w", err)
	}
	var ref resolvedRef
	if fieldRow.Type == "reference" && row.ValueRef.Valid {
		if target, refErr := q.GetReferencedAsset(ctx, row.ValueRef); refErr == nil {
			ref = resolvedRef{ID: target.ID, Title: target.Title}
		} else if !errors.Is(refErr, pgx.ErrNoRows) {
			return out, fmt.Errorf("metadata: resolve conflicting reference: %w", refErr)
		}
	}
	cur := buildAssetValue(row.FieldID, fieldRow.Code, fieldRow.Label, fieldRow.Type,
		row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef,
		row.SetBy, row.SetAt, row.SetByUserRef, fieldRow.Options, ref)
	out.Present = true
	out.Current = &cur
	out.Error = fmt.Sprintf(
		"%s was changed by somebody else since this edit began, so nothing was written",
		fieldRow.Code)
	return out, nil
}

// collectionConflictBody is the collection twin. Same contract, and
// separate only because `current` is a different value shape.
func (h *Handler) collectionConflictBody(
	ctx context.Context,
	collectionID, fieldID pgtype.UUID,
	fieldRow FieldDefinition,
) (openapi.CollectionFieldValueConflict, error) {
	out := openapi.CollectionFieldValueConflict{
		FieldId: openapi_types.UUID(uuid.UUID(fieldID.Bytes)),
	}
	q := New(h.Pool)
	row, err := q.GetCollectionFieldValue(ctx, GetCollectionFieldValueParams{CollectionID: collectionID, FieldID: fieldID})
	if errors.Is(err, pgx.ErrNoRows) {
		out.Present = false
		out.Current = nil
		out.Error = fmt.Sprintf(
			"%s was changed by somebody else: its value has been removed since this edit began, so nothing was written",
			fieldRow.Code)
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("metadata: read conflicting value: %w", err)
	}
	var ref resolvedRef
	if row.RefAssetID.Valid {
		ref = resolvedRef{ID: row.RefAssetID, Title: row.RefAssetTitle}
	}
	cur := buildCollectionValue(row.FieldID, row.Code, row.Label, row.Type,
		row.ValueText, row.ValueNum, row.ValueDate, row.ValueOptions, row.ValueRef,
		row.SetBy, row.SetAt, row.SetByUserRef, row.Options, ref)
	out.Present = true
	out.Current = &cur
	out.Error = fmt.Sprintf(
		"%s was changed by somebody else since this edit began, so nothing was written",
		fieldRow.Code)
	return out, nil
}
