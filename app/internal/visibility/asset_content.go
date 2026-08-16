// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CanSeeAssetContent answers the one question every surface that speaks
// ABOUT an asset's content has to ask first: may this caller reach that
// asset AT ALL — the row AND the bytes behind it?
//
// # Who asks it
//
//   - "may I put THIS asset into a container I control" — a collection
//     (#882) or a post (#922), via collections.mayCollectAsset and
//     posts.mayAttachAsset. You may reference what you may open.
//   - "may I read or write a text annotation ON this asset" — the
//     doc-viewer review tools (#1135), via social.assetContentReadable.
//     An annotation QUOTES the content: its anchor names line and column
//     ranges of the document and its body discusses what is there. So it
//     is governed by the payload it is about, not by the principal who
//     wrote it (the #881 lesson).
//
// It was named `CanAttachAsset` while attaching was the only caller.
// #1135 found the second question and the name was the only thing that
// had to change — renamed rather than copied, because a second
// expression of a security rule is the defect epic #665 exists to remove
// (#892 and #904 each spent a sprint deleting one). See ADR 0064.
//
// # Why it lives here and not in any calling package
//
// #882 built this composition inside collections.Handler. #922 needed
// the identical question on the post surface, so the composition moved
// here, beside the two planes it composes, and every caller calls it.
//
// # The rule
//
// The conjunction [FieldsReadable] already documents (member.go, "the
// CONJUNCTION of the two planes"): a caller may reach an asset iff they
// could have reached that ROW standalone AND could have reached its
// BYTES. FieldsReadable itself is not callable here — it takes an
// already-fetched MemberRow supplied by the container queries — so this
// composes the same two planes from their existing entry points rather
// than writing a third expression of the rule:
//
//   - ROW plane — [CanSee](EntityAsset): exists and is not soft-deleted.
//     Load-bearing on its own account: ContentReadable never looks at
//     deleted_at, so without this conjunct a caller could attach a
//     deleted public asset — a member row the container's contents query
//     then drops in SQL, i.e. an invisible phantom member — and could
//     keep annotating a document that has been deleted out from under it.
//   - CONTENT plane — [CanReadContent] (ADR 0064): the tier rule. Public
//     admits everyone, team admits the asset's team, restricted /
//     embargo / anything unrecognised admit only the owner and the two
//     capability holders.
//
// # The short-circuits are inherited deliberately
//
// CanReadContent admits SystemAdmin and ContentReadAll at every tier,
// and this path keeps both. ContentReadAll's whole purpose is a role
// (the public demo's demo-viewer) that RENDERS a mostly-restricted
// catalogue; a caller who is allowed to view every asset is by this
// rule — "you may attach what you can see" — allowed to attach them.
// Narrowing it here would put the attach path out of step with
// FieldsReadable, which would then render the very members this refused
// to create.
//
// ⚠️ THE MATURE AXIS IS INHERITED TOO, AND ContentReadAll DOES NOT
// SATISFY IT (#1116). That asymmetry is deliberate and it is CanReadContent's
// — see its note. It lands here unchanged because this rule is "you may
// attach what you can see", and a viewer who has not opted in cannot
// see a mature asset. The `mature` argument is threaded rather than
// resolved here for the reason it is everywhere else: this package has
// no store to resolve it from.
//
// # Fails closed
//
// A nonexistent asset stops at the ROW plane. CanReadContent wraps
// pgx.ErrNoRows into an error (it is the "we could not load the row"
// case), so the race in which the asset is deleted between the two
// queries is folded into "not reachable" rather than surfacing as a
// 500 — which would also be an oracle, since a 500 is distinguishable
// from a 404.
//
// Callers MUST answer an unreachable asset with the SAME response they
// give a nonexistent one. Any difference turns the endpoint into a
// UUID-existence probe.
func CanSeeAssetContent(
	ctx context.Context,
	pool Pool,
	caller Caller,
	caps CapabilityChecker,
	assetID uuid.UUID,
	mature MatureViewer,
) (bool, error) {
	visible, err := CanSee(ctx, pool, EntityAsset, caller, assetID)
	if err != nil {
		return false, fmt.Errorf("row plane: %w", err)
	}
	if !visible {
		return false, nil
	}

	readable, err := CanReadContent(ctx, pool, caller, caps, assetID, mature)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("content plane: %w", err)
	}
	return readable, nil
}
