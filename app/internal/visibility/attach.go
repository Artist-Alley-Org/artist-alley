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

// CanAttachAsset answers "may this caller put THIS asset into a
// container they control" — a collection (#882) or a post (#922).
//
// # Why it lives here and not in either container package
//
// #882 built this composition inside collections.Handler. #922 needed
// the identical question on the post surface, and a second copy of a
// security rule is the defect epic #665 exists to remove (#892 and #904
// each spent a sprint deleting one). So the composition moved here,
// beside the two planes it composes, and both container packages call
// it.
//
// # The rule
//
// The conjunction [FieldsReadable] already documents (member.go, "the
// CONJUNCTION of the two planes"): a caller may attach an asset iff they
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
//     then drops in SQL, i.e. an invisible phantom member.
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
// # Fails closed
//
// A nonexistent asset stops at the ROW plane. CanReadContent wraps
// pgx.ErrNoRows into an error (it is the "we could not load the row"
// case), so the race in which the asset is deleted between the two
// queries is folded into "not attachable" rather than surfacing as a
// 500 — which would also be an oracle, since a 500 is distinguishable
// from a 404.
//
// Callers MUST answer an unattachable asset with the SAME response they
// give a nonexistent one. Any difference turns the endpoint into a
// UUID-existence probe.
func CanAttachAsset(
	ctx context.Context,
	pool Pool,
	caller Caller,
	caps CapabilityChecker,
	assetID uuid.UUID,
) (bool, error) {
	visible, err := CanSee(ctx, pool, EntityAsset, caller, assetID)
	if err != nil {
		return false, fmt.Errorf("row plane: %w", err)
	}
	if !visible {
		return false, nil
	}

	readable, err := CanReadContent(ctx, pool, caller, caps, assetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("content plane: %w", err)
	}
	return readable, nil
}
