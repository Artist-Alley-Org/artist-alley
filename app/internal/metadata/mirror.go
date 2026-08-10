// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package metadata

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// ---------------------------------------------------------------------------
// MIRRORED FIELDS — a field definition that is a VIEW onto an assets column
// ---------------------------------------------------------------------------
//
// #822. `assets.title` and `assets.description` are real columns with 93 Go
// references between them, and `title` / `description` were ALSO shipped field
// definitions. Nothing said they were the same thing, so a value written
// through the field plane became a second, independent copy of the column that
// nothing kept in agreement. `field_definition.mirrors_column` (migration
// 00044) is the declaration that closes it: the column stays the storage, the
// definition becomes a view onto it.
//
// # This file is the ONE home
//
// Every Go path that would otherwise touch mirrored storage comes through
// here: the value read (GetAssetFields), the value write and clear
// (SetAssetFieldValue / ClearAssetFieldValue), the upload-defaults pass, and
// the extraction writer adapter in internal/http. None of them restates the
// rule; each OBTAINS it from the definition row it already holds. Nothing in
// Go — here or anywhere else — names `title` or `description`: the CHECK
// constraint on `mirrors_column` is the only enumeration of what is
// mirrorable, so widening the set is a migration and not a sweep.
//
// # Why the database refuses the copy as well
//
// "The Go paths route mirrored writes to the column" is a rule expressed in
// Go, which is the exact defect #822 is about. So migration 00044 also refuses
// a mirrored field any row in `asset_field_value` / `asset_field_value_history`
// outright — on every path, including the seed loader, psql, a future import,
// and any Go path nobody has taught. The functions below are how a path stays
// on the right side of that constraint; the constraint is what makes being on
// the wrong side impossible rather than merely discouraged.

// MirrorColumnOf returns the `assets` column this field declares itself a view
// onto, and whether it declares one at all. The single predicate; no caller
// tests `MirrorsColumn` directly.
func MirrorColumnOf(f FieldDefinition) (string, bool) {
	return mirrorColumn(f.MirrorsColumn)
}

func mirrorColumn(col *string) (string, bool) {
	if col == nil || *col == "" {
		return "", false
	}
	return *col, true
}

// ErrMirroredAssetGone reports that the asset a mirrored write named is absent
// or soft-deleted. Distinct from "the write did nothing", which is a legal
// answer on the fill path.
var ErrMirroredAssetGone = errors.New("metadata: mirrored asset not found")

// mirrorWrite persists a mirrored value INTO THE COLUMN and returns what the
// column now holds, with the asset's new updated_at.
//
// Raw pgx rather than an sqlc query, and the reason is worth recording so the
// next reader does not "tidy" it back: sqlc collapses `SELECT *` over a
// set-returning function to one `interface{}` column and cannot name the
// OUT parameters of `asset_mirror_write` at all. The alternative was to
// degrade the function to a scalar and have Go reconstruct the persisted value
// from what it sent — an echo, which is precisely the thing a mirrored write
// must not return.
func mirrorWrite(ctx context.Context, db DBTX, assetID pgtype.UUID, col string, value string) (string, pgtype.Timestamptz, error) {
	var (
		stored string
		at     pgtype.Timestamptz
	)
	err := db.QueryRow(ctx,
		`SELECT mirrored_value, mirrored_at FROM public.asset_mirror_write($1, $2, $3)`,
		assetID, col, value,
	).Scan(&stored, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", at, ErrMirroredAssetGone
	}
	return stored, at, err
}

// mirrorFill is the if-absent counterpart, for the upload-defaults pass. The
// bool is false when the column already held something — the same answer
// InsertAssetFieldValueIfAbsent's zero-rows gives, and the same rule: a value
// already on the row is never touched.
func mirrorFill(ctx context.Context, db DBTX, assetID pgtype.UUID, col string, value string) (bool, error) {
	var (
		stored string
		at     pgtype.Timestamptz
	)
	err := db.QueryRow(ctx,
		`SELECT mirrored_value, mirrored_at FROM public.asset_mirror_fill($1, $2, $3)`,
		assetID, col, value,
	).Scan(&stored, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MirrorColumnForField resolves the declaration from a bare field id, for a
// caller that holds no definition row. Only the extraction writer adapter
// needs it — the applier hands it a field id and a value and nothing else.
func MirrorColumnForField(ctx context.Context, db DBTX, fieldID uuid.UUID) (string, bool, error) {
	col, err := New(db).GetFieldMirrorColumn(ctx, pgtype.UUID{Bytes: fieldID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	c, ok := mirrorColumn(col)
	return c, ok, nil
}

// ReadMirroredValue projects a mirrored field's current value. Empty means
// unset — and an absent or soft-deleted asset reads empty too, because both
// answers are "there is nothing here", which is the only question the callers
// (the extraction skip_if_set probe) ask.
func ReadMirroredValue(ctx context.Context, db DBTX, assetID uuid.UUID, col string) (string, error) {
	return New(db).ReadAssetMirroredValue(ctx, ReadAssetMirroredValueParams{
		AssetID:       pgtype.UUID{Bytes: assetID, Valid: true},
		MirrorsColumn: col,
	})
}

// WriteMirroredValue is the extraction pipeline's door into the same write the
// API path takes. Exported because the writer adapter lives in internal/http;
// unexported everywhere else, so no third implementation can appear.
func WriteMirroredValue(ctx context.Context, db DBTX, assetID uuid.UUID, col string, value string) error {
	_, _, err := mirrorWrite(ctx, db, pgtype.UUID{Bytes: assetID, Valid: true}, col, value)
	return err
}

// ---------------------------------------------------------------------------
// The permission answer
// ---------------------------------------------------------------------------

// mirroredWriteRefusal reports why a caller may not write this mirrored field,
// or "" when they may.
//
// # The question #822 forced, and the answer
//
// The field plane and the column plane have different gates, and until now
// they never met. Writing `assets.title` through `PATCH /assets/{id}` requires
// owner, a team-scoped `assets.admin`, or the global grant. Writing a field
// value through `PUT /assets/{id}/fields/{field_id}` requires an authenticated
// session and, optionally, the field's own `write_capability` — which
// `title` does not carry.
//
// A mirrored write is a write to the COLUMN. If it kept the field plane's
// gate, declaring `title` a mirror would have handed every authenticated user
// the power to rewrite the title of every asset on the instance: not a
// divergence bug, an authorisation regression, shipped as a side effect of a
// data-model tidy-up.
//
// So the gate is scoped to the PAYLOAD, not to the endpoint that carries it:
// **a mirrored write must satisfy the underlying column's rule**, and the
// field's own `write_capability` still applies on top when it has one. The
// two gates compose; neither replaces the other. That is the same reading
// #881 arrived at for its own escalation, and it is why the rule now lives in
// visibility.AssetMutationCaps.MayMutateOwned where both packages obtain it
// rather than each stating it.
//
// A refused caller changes nothing: this runs before the write, like
// UpdateAsset's own gate, because a gate that answers 403 after writing is not
// a gate.
func mirroredWriteRefusal(ctx context.Context, db DBTX, id *auth.Identity, assetID pgtype.UUID) (string, error) {
	if id == nil || id.IsAnonymous() {
		return "you may not edit this asset", nil
	}
	subject, err := New(db).GetAssetMirrorSubject(ctx, assetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrMirroredAssetGone
		}
		return "", err
	}
	var team *uuid.UUID
	if subject.TeamID.Valid {
		t := uuid.UUID(subject.TeamID.Bytes)
		team = &t
	}
	caps := visibility.ResolveAssetMutationCaps(
		func(code string) bool { return id.Can(code) },
		id.ScopedTeams(visibility.AssetsAdmin),
	)
	if caps.MayMutateOwned(id.UserRef, subject.OwnerUserRef, team) {
		return "", nil
	}
	return "you may not edit this asset", nil
}

// ---------------------------------------------------------------------------
// Reading mirrored values back into the field list
// ---------------------------------------------------------------------------

// SetByMirror is the provenance a projected column carries. `set_by_user_ref`
// is always nil beside it: the column records no author, and inventing one
// would be a claim about who last edited it that nothing supports. `set_at` is
// the asset's own updated_at, the honest nearest thing.
//
// Not a member of the write enum — a caller cannot claim it, for the same
// reason it cannot claim `default`.
const SetByMirror = "mirror"

// mergeFieldValues interleaves the stored and mirrored halves of an asset's
// field values into the single order the API promises — (display_group,
// display_order, code), the ORDER BY both queries already use.
//
// Two lists rather than one query because a mirrored field HAS no row to join
// to; the guard trigger guarantees it never will. Sorting here is what keeps
// that structural fact invisible to a client, which is the point of a view.
func mergeFieldValues(stored []ListAssetFieldValuesRow, mirrored []ListAssetMirroredValuesRow) []fieldValueEntry {
	out := make([]fieldValueEntry, 0, len(stored)+len(mirrored))
	for i := range stored {
		out = append(out, fieldValueEntry{
			group: stored[i].DisplayGroup, order: stored[i].DisplayOrder, code: stored[i].Code,
			stored: &stored[i],
		})
	}
	for i := range mirrored {
		out = append(out, fieldValueEntry{
			group: mirrored[i].DisplayGroup, order: mirrored[i].DisplayOrder, code: mirrored[i].Code,
			mirrored: &mirrored[i],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if c := strings.Compare(a.group, b.group); c != 0 {
			return c < 0
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.code < b.code
	})
	return out
}

// fieldValueEntry is one row of the merged list. Exactly one of the two
// pointers is set: a field either stores its own value or is a view onto a
// column, and migration 00044's guard trigger is what makes "both" unsayable.
type fieldValueEntry struct {
	group    string
	order    int32
	code     string
	stored   *ListAssetFieldValuesRow
	mirrored *ListAssetMirroredValuesRow
}
