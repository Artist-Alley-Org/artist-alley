// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FieldsRow is one asset row reduced to exactly the columns
// [FieldsReadable] consults. Every surface that emits asset COLUMNS
// selects these five and nothing else has to be threaded through.
//
// IsTeamMember must already fold in "the asset is team-tier AND the
// caller belongs to THIS asset's team", same contract as
// [ContentReadable]'s parameter of the same name — the queries compute
// it with an EXISTS join in the same pass.
type FieldsRow struct {
	Sensitivity      string
	Status           string
	ProcessingStatus string
	OwnerUserRef     *int64
	IsTeamMember     bool
}

// FieldsReadable decides whether a caller may receive an asset's
// COLUMNS — title, description, tags, metadata, file hash, extension,
// byte size, thumbhash, dimensions (#883, #899).
//
// # Which surfaces
//
// All of them. This started (#883) as the container-member rule and was
// named MemberReadable for it; #899 found the same leak on the two
// surfaces that reach an asset directly, so the rule is now
// surface-neutral and every path that serialises asset columns routes
// through it:
//
//   - a post member and a collection resource (#883);
//   - `GET /assets/{id}` and the asset browse list (#899);
//   - a search hit, a suggest completion and the asset facets (#899);
//   - IIIF presentation manifests.
//
// # The rule it enforces
//
// An asset you cannot OPEN must not hand you its metadata, and reaching
// it through a container must never WIDEN it. So this is the
// CONJUNCTION of the two planes an asset already lives under, evaluated
// for the same caller:
//
//   - the ROW plane — [Predicate.ToSQL] for EntityAsset, which for an
//     anonymous caller demands status='active' AND
//     processing_status='ready' AND sensitivity='public'; and
//   - the CONTENT plane — [ContentReadable], ADR 0064, which is the tier
//     rule: public admits everyone, team admits the asset's team, and
//     restricted / embargo / anything unrecognised admit only the owner
//     and the two capability holders.
//
// Conjunction, not a new rule: the columns are readable iff the caller
// could have reached that asset's row AND could have reached its bytes.
// That is what makes the direction one-way — no serialisation of an
// asset can be wider than the narrowest plane it sits under, and by
// construction cannot become wider when either plane is edited.
//
// # Why the CONTENT plane gates METADATA
//
// ADR 0064 decided that sensitivity gates content, not rows: a
// restricted asset stays LISTED so browse can show it as a placeholder
// with its owner's name, which is what makes "request access" (#881)
// mean anything. That decision is unchanged — this does not remove a
// single row from a single feed.
//
// What it changes is the PAYLOAD on the row that stays. The owner's
// rule (2026-08-03) is that a viewer who cannot see an item sees a
// placeholder, and *"the placeholder should never leak info. Not even
// title. Only the owner's name."* So the tier that gates the bytes
// gates the columns too. The result is strictly NARROWER than the row
// the predicate returned, which is the safe direction and is why it
// does not contradict 0064. The ADR 0020 amendment (2026-08-04) records
// the split explicitly: 0020 governs the IMAGE, this governs the
// FIELDS.
//
// # What is deliberately NOT closed
//
// EXISTENCE is still disclosed, by design — decision 1 of #899. So is
// the fact that a restricted asset's indexed text MATCHES a given
// search query, because the row is still returned by search. A
// determined caller can probe that oracle word by word. Closing it
// means dropping restricted rows from results, which contradicts the
// placeholder decision; it is a product call, not a bug to fix here.
//
// # Fails closed
//
// Every unknown sensitivity value denies, inherited from
// [ContentReadable]; a NULL owner never matches; and the anonymous
// sentinel (UserRef 0) can never match an asset owned by ref 0, both
// guards inherited from the same place.
func FieldsReadable(row FieldsRow, caller Caller, caps CapabilityChecker) bool {
	// SystemAdmin (wildcard) and ContentReadAll (binary plane, #474)
	// short-circuit both planes, exactly as they do in ContentReadable.
	// ContentReadAll admitting METADATA as well as bytes is deliberate:
	// its whole purpose is a demo-viewer role that renders a
	// mostly-restricted catalogue, and a catalogue of placeholders is
	// not a rendered catalogue.
	if caps != nil && (caps(SystemAdmin) || caps(ContentReadAll)) {
		return true
	}
	// The owner reaches their own asset on every surface, at any tier
	// and in any workflow state — including a draft they are still
	// preparing.
	if !caller.IsAnonymous && row.OwnerUserRef != nil && *row.OwnerUserRef == caller.UserRef {
		return true
	}
	// The ROW plane's anonymous conjuncts. These mirror the anonymous
	// EntityAsset branch of Predicate.ToSQL; the third conjunct there
	// (sensitivity='public') is not repeated because ContentReadable
	// below decides the tier, and duplicating it would be a second
	// expression of one rule — the defect ADR 0063 exists to prevent.
	// Soft-delete is NOT here: a deleted asset is not a placeholder, it
	// is gone, and every caller's query drops those rows in SQL.
	if caller.IsAnonymous {
		if row.Status != "active" || row.ProcessingStatus != "ready" {
			return false
		}
	}
	// The CONTENT plane.
	return ContentReadable(row.Sensitivity, row.OwnerUserRef, caller, caps, row.IsTeamMember)
}

// FieldsColumnsSQL is the SELECT-list fragment carrying exactly the
// columns [FieldsRow] needs, plus the owner's display name for the
// placeholder. `alias` is the assets table alias ("" for none), and
// `callerArg` is the placeholder holding the caller's user_ref for the
// team-membership EXISTS.
//
// One fragment rather than five hand-copied column lists: a surface
// that selects four of the five silently decides the fifth, and the
// fifth is usually sensitivity.
//
// Column order, which callers scan positionally:
//
//	sensitivity, status, processing_status, owner_user_ref,
//	is_team_member, owner_display_name
func FieldsColumnsSQL(alias, callerArg string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	return p + `sensitivity, ` + p + `status, ` + p + `processing_status, ` + p + `owner_user_ref,
	       (` + p + `team_id IS NOT NULL AND EXISTS (
	            SELECT 1 FROM team_memberships tm
	             WHERE tm.team_id = ` + p + `team_id AND tm.user_ref = ` + callerArg + `::BIGINT)) AS is_team_member,
	       COALESCE((SELECT COALESCE(NULLIF(up.display_name, ''), u.username)
	                   FROM "user" u
	                   LEFT JOIN user_profiles up ON up.user_ref = u.ref
	                  WHERE u.ref = ` + p + `owner_user_ref), '') AS owner_display_name`
}

// FieldsPool is the subset of pgxpool.Pool LoadFieldsRow uses.
type FieldsPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadFieldsRow reads one asset's [FieldsRow] plus its owner's display
// name, resolving team membership for the caller in the same round
// trip. It is the pool-loading sibling of the query-free
// [FieldsReadable], exactly as [CanReadContent] is to
// [ContentReadable]: surfaces that already have the columns (the browse
// list, the search projections) call FieldsReadable directly; surfaces
// that reach a single asset call this.
//
// Fails CLOSED — a missing row returns the zero FieldsRow and an error,
// and the zero FieldsRow denies on every tier, because an asset we
// cannot load is an asset whose columns we do not hand out.
func LoadFieldsRow(
	ctx context.Context,
	pool FieldsPool,
	caller Caller,
	assetID uuid.UUID,
) (FieldsRow, string, error) {
	var (
		row       FieldsRow
		ownerName string
	)
	err := pool.QueryRow(ctx,
		`SELECT `+FieldsColumnsSQL("assets", "$2")+` FROM assets WHERE id = $1`,
		assetID, caller.UserRef,
	).Scan(&row.Sensitivity, &row.Status, &row.ProcessingStatus,
		&row.OwnerUserRef, &row.IsTeamMember, &ownerName)
	if err != nil {
		return FieldsRow{}, "", fmt.Errorf("visibility.LoadFieldsRow: %w", err)
	}
	return row, ownerName, nil
}
