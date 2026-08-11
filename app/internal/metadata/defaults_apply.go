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
)

// ---------------------------------------------------------------------------
// Applying upload defaults at asset creation (#793)
// ---------------------------------------------------------------------------
//
// This runs INSIDE the asset-creation transaction, before commit. Two
// reasons, and the second is the one that matters:
//
//   * an asset that exists without its defaults, even for a moment, is
//     an asset another reader can observe half-formed;
//   * defaults have to be applied at creation rather than after
//     extraction, because extraction is an async job enqueued only for
//     six image extensions. A default that waited for extraction would
//     never arrive on a .glb, a .pdf or a .wav — which is most of the
//     catalogue — and the upload modal could not honestly tell an
//     artist what their asset is about to carry.
//
// Applying first and letting extraction improve on it is what
// SetByDefault buys: the value is there immediately, and the applier
// knows it is a placeholder rather than a decision.

// ApplyDefaultsParams is everything the defaults pass needs about the
// asset being created.
type ApplyDefaultsParams struct {
	AssetID   uuid.UUID
	AssetType int64
	UserRef   int64
	// Now is the creation instant, supplied by the caller so every
	// default on one asset agrees and so a test can pin it.
	Now time.Time
}

// AppliedDefault records one default that actually landed. Returned so
// the caller can log or audit; nothing depends on the order.
type AppliedDefault struct {
	FieldID   uuid.UUID
	FieldCode string
	// TeamID names the team whose override supplied the value, or is
	// invalid when the field's own default did.
	TeamID pgtype.UUID
}

// ApplyAssetDefaults writes the resolved default for every asset field
// that has one and that this asset does not already carry a value for.
//
// Precedence, top to bottom (the 2026-07-31 amendment to ADR 0081 §3):
//
//	a value already on the row   — never touched; the writer's
//	                               ON CONFLICT DO NOTHING guarantees it
//	the uploader's team override — when exactly one applies
//	the field's own default
//	nothing
//
// Extraction sits ABOVE all of this, and gets there by running later
// and by recognising SetByDefault — not by being consulted here.
//
// Errors that concern ONE field (an unparseable document, a context
// that will not resolve) are skipped rather than propagated: a default
// is a convenience, and failing an upload because an operator left a
// bad default on an unrelated field would trade a small annoyance for a
// total one. Errors that concern the whole pass (a query failing) are
// returned, because they mean the transaction is already in trouble.
func ApplyAssetDefaults(ctx context.Context, db DBTX, p ApplyDefaultsParams) ([]AppliedDefault, error) {
	q := New(db)

	teams, err := q.ListDefaultTeamsForUser(ctx, p.UserRef)
	if err != nil {
		return nil, fmt.Errorf("metadata: defaults: read teams: %w", err)
	}
	teamIDs := make([]pgtype.UUID, 0, len(teams))
	for _, t := range teams {
		teamIDs = append(teamIDs, t.ID)
	}

	rows, err := q.ListAssetDefaultCandidates(ctx, ListAssetDefaultCandidatesParams{
		TeamIds: teamIDs,
		Rt:      p.AssetType,
	})
	if err != nil {
		return nil, fmt.Errorf("metadata: defaults: read candidates: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	rc := DefaultResolveContext{Now: p.Now}
	// Only pay for the two context lookups when a default actually
	// wants them. Most instances have neither, and an upload should not
	// buy two queries to discover that.
	if candidatesNeedContext(rows) {
		if display, err := q.GetDefaultUserDisplay(ctx, p.UserRef); err == nil {
			rc.UserDisplay = display
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("metadata: defaults: read user display: %w", err)
		}
		// A team name is only unambiguous when the uploader is in
		// exactly one team. In any other case `uploading_team` stays
		// unresolved and its default is simply not applied — see
		// chooseOverride for why guessing is worse than a blank.
		if len(teams) == 1 {
			rc.TeamName = teams[0].Name
		}
	}

	assetID := pgtype.UUID{Bytes: p.AssetID, Valid: true}
	var applied []AppliedDefault

	for _, group := range groupCandidates(rows) {
		raw, fromTeam := chooseDefault(group)
		if len(raw) == 0 {
			continue
		}
		def, ok, err := ParseFieldDefault(raw)
		if err != nil || !ok {
			// Stored documents are validated on write, so this is a
			// row someone edited around the API. Skip it.
			continue
		}
		write, ok := ResolveFieldDefault(group.Type, def, rc)
		if !ok {
			continue
		}
		params, err := buildUpsertParams(assetID, group.ID, group.Type, write, nil)
		if err != nil {
			continue
		}

		// A default on a MIRRORED field (#822) fills the COLUMN, and only
		// when the column is still empty — the same "a value already on the
		// row is never touched" rule, one plane over. Falling through to the
		// writer below would hit migration 00044's guard trigger and fail
		// the whole upload transaction over a convenience.
		//
		// No history row for the same reason the API path writes none: a
		// per-field trail that exists only for changes made through the
		// field plane is a trail that lies by omission.
		if col, ok := mirrorColumn(group.MirrorsColumn); ok {
			if params.ValueText == nil {
				continue
			}
			wrote, mErr := mirrorFill(ctx, db, assetID, col, *params.ValueText)
			if mErr != nil {
				return applied, fmt.Errorf("metadata: defaults: mirror %s: %w", group.Code, mErr)
			}
			if wrote {
				applied = append(applied, AppliedDefault{
					FieldID:   uuid.UUID(group.ID.Bytes),
					FieldCode: group.Code,
					TeamID:    fromTeam,
				})
			}
			continue
		}

		n, err := q.InsertAssetFieldValueIfAbsent(ctx, InsertAssetFieldValueIfAbsentParams{
			AssetID:      assetID,
			FieldID:      group.ID,
			ValueText:    params.ValueText,
			ValueNum:     params.ValueNum,
			ValueDate:    params.ValueDate,
			ValueOptions: params.ValueOptions,
			ValueRef:     params.ValueRef,
		})
		if err != nil {
			return applied, fmt.Errorf("metadata: defaults: write %s: %w", group.Code, err)
		}
		if n == 0 {
			// Something was already there. That is the rule working,
			// not a failure.
			continue
		}

		// A value with no history entry is a value with no story. The
		// set_by column says "a default", this says when and which one.
		if newJSON, jErr := valueRowToJSON(
			params.ValueText, params.ValueNum, params.ValueDate,
			params.ValueOptions, params.ValueRef, group.Type,
		); jErr == nil {
			_ = q.AppendAssetFieldValueHistory(ctx, AppendAssetFieldValueHistoryParams{
				AssetID:  assetID,
				FieldID:  group.ID,
				OldValue: nil,
				NewValue: newJSON,
				SetBy:    SetByDefault,
				// Deliberately nil: nobody chose this. Attributing it
				// to the uploader would be the same lie as recording it
				// as 'manual'.
				ChangedByUserRef: nil,
			})
		}

		applied = append(applied, AppliedDefault{
			FieldID:   uuid.UUID(group.ID.Bytes),
			FieldCode: group.Code,
			TeamID:    fromTeam,
		})
	}

	return applied, nil
}

// candidateGroup is one field plus every override row that came back
// for it.
type candidateGroup struct {
	ID      pgtype.UUID
	Code    string
	Type    string
	Options []byte
	Default []byte
	// MirrorsColumn is the assets column this field declares itself a view
	// onto (#822), or nil for ordinary field-owned storage.
	MirrorsColumn *string
	Overrides     []candidateOverride
}

type candidateOverride struct {
	TeamID pgtype.UUID
	Value  []byte
}

// groupCandidates folds the LEFT JOIN's one-row-per-override shape back
// into one entry per field, preserving the query's ORDER BY.
func groupCandidates(rows []ListAssetDefaultCandidatesRow) []candidateGroup {
	var out []candidateGroup
	index := map[uuid.UUID]int{}
	for _, r := range rows {
		key := uuid.UUID(r.ID.Bytes)
		i, seen := index[key]
		if !seen {
			out = append(out, candidateGroup{
				ID:            r.ID,
				Code:          r.Code,
				Type:          r.Type,
				Options:       r.Options,
				Default:       r.DefaultValue,
				MirrorsColumn: r.MirrorsColumn,
			})
			i = len(out) - 1
			index[key] = i
		}
		if r.TeamID.Valid && len(r.OverrideValue) > 0 {
			out[i].Overrides = append(out[i].Overrides, candidateOverride{
				TeamID: r.TeamID,
				Value:  r.OverrideValue,
			})
		}
	}
	return out
}

// chooseDefault picks the document to apply, and says where it came
// from.
//
// The interesting case is an uploader in two teams that BOTH override
// the same field. There is no correct answer — neither team is "the"
// team of the upload — so both overrides are discarded and the field's
// own default applies. That is deliberately not resolved by an ORDER BY
// on team name or creation date: a rule like that produces a confident
// answer that is right half the time and is impossible for an operator
// to predict, which is worse than falling back to the value everyone
// agreed on.
func chooseDefault(g candidateGroup) ([]byte, pgtype.UUID) {
	if len(g.Overrides) == 1 {
		return g.Overrides[0].Value, g.Overrides[0].TeamID
	}
	return g.Default, pgtype.UUID{}
}

// candidatesNeedContext reports whether any document in play is a
// context default, i.e. whether the two context lookups are worth
// running.
func candidatesNeedContext(rows []ListAssetDefaultCandidatesRow) bool {
	for _, r := range rows {
		for _, raw := range [][]byte{r.DefaultValue, r.OverrideValue} {
			if d, ok, err := ParseFieldDefault(raw); err == nil && ok && d.Kind == DefaultKindContext {
				return true
			}
		}
	}
	return false
}
