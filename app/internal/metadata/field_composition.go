// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

// EFFECTIVE FIELD READABILITY, and the two endpoints that report it
// (#1173, #1119, ADR 0099 §5).
//
// # Why this file exists at all
//
// Sprint 20b needed conditional visibility, and conditional visibility
// evaluated over field values the caller may not read is an ORACLE over
// protected metadata: the dependent's visibility is observable and the
// condition is stored, so watching whether a control appears reads out
// the controller's value. Building it on the read path as it stood would
// have been a security regression wearing a feature's clothes.
//
// The read path as it stood, measured rather than assumed:
//
//   - GetAssetFields contained ZERO per-field capability checks. Its only
//     gate was a 401 on an anonymous caller, so any authenticated caller
//     received the values of every gated field on the asset.
//   - GetCollectionFields did check, but against GLOBALLY held codes
//     only, and it filtered by DROPPING THE ROW — so "withheld" and
//     "never set" arrived as the same nothing.
//
// Both halves are closed here, and they are two DIFFERENT requirements
// that a single change cannot satisfy:
//
//  1. NON-DISCLOSURE: the protected value must not cross the wire. That
//     is [fieldReadableOnSubject] applied as a filter on the two value
//     reads.
//  2. TRUSTWORTHY EFFECTIVE READABILITY: a client must be able to tell
//     "withheld" from "unset", because those two have OPPOSITE
//     consequences for a condition. That is the field-composition read,
//     whose response shape carries no values at all.
//
// ⛔ FOUR DESIGNS THAT LOOK EQUIVALENT AND ARE NOT. Sending the value
// with a `readable: false` flag; sending it and dropping it in the
// client; inferring authority from the caller's capability list; and
// treating the presence of a value as proof that the caller may read it.
// The first two disclose. The third is wrong because
// `Identity.Capabilities` holds GLOBAL codes only, so a team-scoped grant
// is invisible in the browser and the inference answers "no" for exactly
// the operator the field was configured for. The fourth collapses
// "withheld" and "unset" back into one state, which is the defect this
// file was written to fix.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mscrnt/artist-alley/app/internal/openapi"
)

// fieldReadableOnSubject answers the ONE readability question, for one
// caller, one field's read gate, and one subject's team scope.
//
// ⛔ EVERY READ PATH IN THIS PACKAGE GOES THROUGH THIS FUNCTION. Two
// copies of a security rule drift, and the drift is the bug; that is the
// same argument `canMutateAsset` makes about `canSetAssetStatus`, and the
// reason `canReadField` now delegates here rather than keeping its own
// `id.Can` call.
//
// The three arms, in order:
//
//   - No gate. An empty or absent `read_capability` means the field is
//     readable by anyone who reached the endpoint. Most fields.
//   - The SCOPED arm, preferred when the subject actually HAS a team.
//     `id.Can(code, auth.InTeam(...))` is not a shortcut, it is the only
//     correct route: `Identity.scopedCaps` is assembled by the resolver
//     from direct grants, from a recursive walk of role parents carrying
//     `user_roles.team_id`, minus revokes, then expanded through
//     `team_closure`. A team-scoped ROLE assignment produces ZERO rows in
//     `user_capability_grants`, so any hand-rolled derivation would miss
//     precisely the path that matters.
//   - The GLOBAL arm. A global holding works in any scope.
//
// ⚠️ THE NULLABLE TRAP, the same one `hasAssetCapability` documents: a
// team-less subject SKIPS the scoped disjunct rather than treating "no
// scope required" as "anyone passes". `assets.team_id` is nullable, and
// `collections` has no team column at all, so on a collection a
// team-scoped grant confers nothing and the global holding is the whole
// answer. That asymmetry is pre-existing and deliberate; giving
// collections a team is a separate decision.
func fieldReadableOnSubject(id *auth.Identity, readCapability *string, teamID pgtype.UUID) bool {
	if id == nil {
		return false
	}
	if readCapability == nil || *readCapability == "" {
		return true
	}
	if teamID.Valid && id.Can(*readCapability, auth.InTeam(uuid.UUID(teamID.Bytes))) {
		return true
	}
	return id.Can(*readCapability)
}

// assetTeamForFields loads the team scope of one asset.
//
// A missing asset returns an INVALID team rather than an error: these
// callers already answer for assets that may not exist, and a team-less
// answer is the conservative one (the scoped disjunct is skipped, so the
// caller falls back to their global holding).
func (h *Handler) assetTeamForFields(ctx context.Context, assetID pgtype.UUID) (pgtype.UUID, error) {
	team, err := New(h.Pool).GetAssetTeamForFieldComposition(ctx, assetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, nil
		}
		return pgtype.UUID{}, fmt.Errorf("metadata: asset team: %w", err)
	}
	return team, nil
}

// ---------------------------------------------------------------------------
// GetAssetFieldComposition
// ---------------------------------------------------------------------------

// GetAssetFieldComposition reports EFFECTIVE per-field readability for
// one asset, and carries no values.
//
// One entry per non-archived `asset` definition. Archived definitions are
// excluded to match the status semantics every composition surface uses
// (#528): they never appear on a form, so reporting readability for one
// would describe a control that is not there. That exclusion is also what
// makes ADR 0099's later-archive drift work: a controller archived after
// a valid configuration resolves to nothing here, the term becomes
// unevaluable, and the dependent fails open without anything having
// rewritten the stored condition.
func (h *Handler) GetAssetFieldComposition(
	ctx context.Context,
	req openapi.GetAssetFieldCompositionRequestObject,
) (openapi.GetAssetFieldCompositionResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetAssetFieldComposition401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	pgAsset := pgtype.UUID{Bytes: uuid.UUID(req.Id), Valid: true}
	team, err := h.assetTeamForFields(ctx, pgAsset)
	if err != nil {
		return nil, err
	}
	rows, err := New(h.Pool).ListFieldDefinitionsForComposition(ctx, string(SubjectAsset))
	if err != nil {
		return nil, fmt.Errorf("metadata: composition definitions: %w", err)
	}
	out := make([]openapi.FieldCompositionState, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.FieldCompositionState{
			FieldId:   openapi_types.UUID(r.ID.Bytes),
			FieldCode: r.Code,
			Readable:  fieldReadableOnSubject(id, r.ReadCapability, team),
		})
	}
	return openapi.GetAssetFieldComposition200JSONResponse(out), nil
}

// ---------------------------------------------------------------------------
// GetCollectionFieldComposition
// ---------------------------------------------------------------------------

// GetCollectionFieldComposition is the collection twin.
//
// No team lookup, because `collections` carries no `team_id` column: a
// team-scoped grant confers nothing here and the caller's global holding
// is the whole answer. The zero-value `pgtype.UUID` passed below is that
// statement, not an oversight, and it makes the asymmetry visible at the
// call site rather than hiding it inside the helper.
//
// This endpoint matters MORE on collections than on assets, which is the
// opposite of what the pre-existing filtering suggests.
// GetCollectionFields already dropped unreadable values, so a withheld
// value and a value that was never set have always arrived as the same
// nothing. Those two states have OPPOSITE consequences for a condition
// (unevaluable SHOWS the dependent, absent HIDES it), so without this
// read a collection form cannot tell them apart at all.
func (h *Handler) GetCollectionFieldComposition(
	ctx context.Context,
	req openapi.GetCollectionFieldCompositionRequestObject,
) (openapi.GetCollectionFieldCompositionResponseObject, error) {
	id := auth.IdentityFromContext(ctx)
	if id == nil {
		return openapi.GetCollectionFieldComposition401JSONResponse{
			UnauthorizedJSONResponse: openapi.UnauthorizedJSONResponse{Error: "authentication required"},
		}, nil
	}
	rows, err := New(h.Pool).ListFieldDefinitionsForComposition(ctx, string(SubjectCollection))
	if err != nil {
		return nil, fmt.Errorf("metadata: composition definitions: %w", err)
	}
	out := make([]openapi.FieldCompositionState, 0, len(rows))
	for _, r := range rows {
		out = append(out, openapi.FieldCompositionState{
			FieldId:   openapi_types.UUID(r.ID.Bytes),
			FieldCode: r.Code,
			// No team plane on a collection. See the doc comment.
			Readable: fieldReadableOnSubject(id, r.ReadCapability, pgtype.UUID{}),
		})
	}
	return openapi.GetCollectionFieldComposition200JSONResponse(out), nil
}
