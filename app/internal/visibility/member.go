// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

// MemberRow is one asset row as reached THROUGH a container — a post's
// `post_assets` join or a collection's `collection_resources` join. It
// carries exactly the asset columns [MemberReadable] consults, so the
// container queries select those columns and nothing else has to be
// threaded through.
//
// IsTeamMember must already fold in "the asset is team-tier AND the
// caller belongs to THIS asset's team", same contract as
// [ContentReadable]'s parameter of the same name — the container queries
// compute it with an EXISTS join in the same pass.
type MemberRow struct {
	Sensitivity      string
	Status           string
	ProcessingStatus string
	OwnerUserRef     *int64
	IsTeamMember     bool
}

// MemberReadable decides whether a caller may receive a container
// member's ASSET COLUMNS — title, description, tags, metadata, file
// hash, extension, byte size, thumbhash, dimensions (#883).
//
// # The rule it enforces
//
// Membership must never WIDEN an item. Putting someone else's asset in
// a public post or collection cannot make that asset more visible than
// it is on its own, or re-collection (#882) becomes a one-click
// permission-escalation primitive: add the asset to a public container,
// read the container, read the metadata.
//
// So this is the CONJUNCTION of the two planes an asset already lives
// under, evaluated for the same caller:
//
//   - the ROW plane — [Predicate.ToSQL] for EntityAsset, which for an
//     anonymous caller demands status='active' AND
//     processing_status='ready' AND sensitivity='public'; and
//   - the CONTENT plane — [ContentReadable], ADR 0064, which is the tier
//     rule: public admits everyone, team admits the asset's team, and
//     restricted / embargo / anything unrecognised admit only the owner
//     and the two capability holders.
//
// Conjunction, not a new rule: a member is readable iff the caller could
// have reached that asset standalone AND could have reached its bytes.
// That is what makes the direction one-way — the member view is never
// wider than the item view, and by construction cannot become wider when
// either plane is edited.
//
// # Why the CONTENT plane gates METADATA here
//
// ADR 0064 decided that sensitivity gates content, not rows: a
// restricted asset stays LISTED, with its title, so that browse can show
// it blurred with a lock (ADR 0020). That decision is unchanged and this
// does not touch the browse feed or the row predicate.
//
// A container member is a different question. The owner's rule
// (2026-08-03) is that a viewer who cannot see an item sees a
// placeholder, and *"the placeholder should never leak info. Not even
// title. Only the owner's name."* So on this path the tier that gates
// the bytes gates the columns too. The result is strictly NARROWER than
// the standalone row, which is the safe direction and is why it does not
// contradict 0064.
//
// It is worth being precise about what that does and does not buy,
// because the two planes still disagree for one caller class: an
// AUTHENTICATED non-owner can still read that same title from
// `GET /assets/{id}` and from browse, because the authenticated branch
// of the row predicate is soft-delete only (predicate.go, EntityAsset).
// The placeholder is therefore an anti-WIDENING guarantee, not a secrecy
// guarantee, until the row-level story changes (#210 / ADR 0020 Phase
// 1.28 blur-and-reveal). For an ANONYMOUS caller — who cannot reach the
// row at all — it is both.
//
// # Fails closed
//
// Every unknown sensitivity value denies, inherited from
// [ContentReadable]; a NULL owner never matches; and the anonymous
// sentinel (UserRef 0) can never match an asset owned by ref 0, both
// guards inherited from the same place.
func MemberReadable(row MemberRow, caller Caller, caps CapabilityChecker) bool {
	// SystemAdmin (wildcard) and ContentReadAll (binary plane, #474)
	// short-circuit both planes, exactly as they do in ContentReadable.
	// ContentReadAll admitting METADATA as well as bytes is deliberate:
	// its whole purpose is a demo-viewer role that renders a
	// mostly-restricted catalogue, and a catalogue of placeholders is
	// not a rendered catalogue.
	if caps != nil && (caps(SystemAdmin) || caps(ContentReadAll)) {
		return true
	}
	// The owner reaches their own asset through any container, at any
	// tier and in any workflow state — including a draft they are still
	// preparing.
	if !caller.IsAnonymous && row.OwnerUserRef != nil && *row.OwnerUserRef == caller.UserRef {
		return true
	}
	// The ROW plane's anonymous conjuncts. These mirror the anonymous
	// EntityAsset branch of Predicate.ToSQL; the third conjunct there
	// (sensitivity='public') is not repeated because ContentReadable
	// below decides the tier, and duplicating it would be a second
	// expression of one rule — the defect ADR 0063 exists to prevent.
	// Soft-delete is NOT here: a deleted member is not a placeholder,
	// it is gone, and the container queries drop those rows in SQL.
	if caller.IsAnonymous {
		if row.Status != "active" || row.ProcessingStatus != "ready" {
			return false
		}
	}
	// The CONTENT plane.
	return ContentReadable(row.Sensitivity, row.OwnerUserRef, caller, caps, row.IsTeamMember)
}
