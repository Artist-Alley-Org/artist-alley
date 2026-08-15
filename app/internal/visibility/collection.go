// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package visibility

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// CanReadCollection answers "may this caller read THIS collection" —
// the whole rule, in one place.
//
// # Why it exists
//
// [Filter] for [EntityCollection] has no admin disjunct: the row plane
// describes who a collection is *shared with*, and an instance admin is
// not on anybody's share list. So the rule "an instance admin may read
// any collection" has always been applied OUTSIDE the predicate, by
// each caller that wanted it. That was one site until #910 added a
// second (a collection-scoped facet search), and the second exists only
// because its author noticed the first and copied it.
//
// The symptom of two copies is not a leak, it is a feature that looks
// broken: let the copies drift and an admin opens a foreign private
// collection's page perfectly well, then gets an empty result from the
// "Search in this collection" button on that same page. One rule, two
// verdicts. #1059 is that defect; epic #665 is the general form.
//
// # Why not just widen Filter(EntityCollection)
//
// Because the admin-blind rule is a real question somebody still needs
// to ask — an audit view, a "what would an ordinary user see from here"
// preview. Folding the disjunct into the primitive answers the widened
// question for EVERY caller and destroys the ability to ask the narrow
// one. A NAME adds a capability without taking one away.
//
// # What this composite ASSUMES — read before copying it
//
// The admin disjunct is safe AT A CALL SITE THAT HAS ALREADY
// ESTABLISHED THE ADMIN PLANE for this collection. Both current callers
// have: the endpoint the caller is standing on already hands a
// system.admin the collection itself, so also letting them scope a
// search to it discloses nothing they could not obtain by listing its
// contents.
//
// That reasoning is PER-CALLER, not general. Dropping these two lines
// onto a path where a system.admin is not already able to open the
// collection would be widening a read with no such argument — and
// because it is a named helper, it would look like precedent rather
// than like a new decision. If you are reaching for this on a surface
// where the admin cannot already open the collection, you want
// [CanSee](EntityCollection) and a fresh argument, not this.
//
// This is a READ rule and only a read rule. It is deliberately NOT the
// answer to three neighbouring system.admin checks that look identical:
// "an admin also sees soft-deleted rows" (so the Restore button has
// something to render) is a different question about a different set of
// rows, and collections' canMutateCollection is a WRITE gate that
// admits collections.admin as well. Absorbing any of them into this
// would be a widening dressed as a cleanup.
//
// # Order, and fails closed
//
// The capability is checked FIRST, so an admin costs zero queries — and
// so the answer for an admin cannot depend on a database error. For
// everyone else the row plane decides. [CanSee] already collapses "row
// does not exist" and "predicate rejects" into (false, nil) and folds
// pgx.ErrNoRows itself, so a nonexistent id is simply unreadable and
// there is no second query that could surface one; an error out of here
// is a real infrastructure failure, and callers must treat it as a
// refusal rather than letting it change the response shape.
//
// A nil caps means "no capabilities" — anonymous callers get the row
// plane and nothing else.
func CanReadCollection(
	ctx context.Context,
	pool Pool,
	caller Caller,
	caps CapabilityChecker,
	collectionID uuid.UUID,
) (bool, error) {
	if caps != nil && caps(SystemAdmin) {
		return true, nil
	}

	visible, err := CanSee(ctx, pool, EntityCollection, caller, collectionID)
	if err != nil {
		return false, fmt.Errorf("row plane: %w", err)
	}
	return visible, nil
}

// CollectionReadableSQL is [CanReadCollection] as a WHERE-clause
// fragment: the same rule, for a surface that must decide it over a SET
// of collections instead of one id (#1078).
//
// It follows [Predicate.ToSQL]'s contract — the fragment starts with
// " AND ", placeholders number from argOffset+1, and the caller appends
// the returned args in order — with ONE addition: a system.admin gets
// the EMPTY fragment and no args, because "no restriction" is what the
// admin arm means and an admin must not pay for a predicate that would
// only narrow them. An empty string concatenates into a WHERE clause as
// a no-op, so the call site needs no branch.
//
// # Why this exists rather than a third open-coded disjunct
//
// #1059 gave the admin disjunct one home after two call sites had
// hand-copied it. #1078 is the third surface that needs it — the
// collections autocomplete source, where a system.admin got no
// completions for private collection names they can open perfectly well
// from the collection page. That surface composes a predicate over a
// set, so it could not call [CanReadCollection] and would have
// open-coded the disjunct a third time. This is the same decision in
// the shape that surface can consume.
//
// Everything [CanReadCollection]'s doc says about what the admin arm
// ASSUMES applies here unchanged, and applies harder: a fragment is
// easier to paste than a function call. Do not reach for this on a
// surface where a system.admin cannot already open the collections in
// question.
//
// # ⚠️ Soft-delete is NOT decided here, and that is deliberate
//
// On the row-plane arm the predicate carries `deleted_at IS NULL`, as
// it does everywhere. On the ADMIN arm nothing does — the fragment is
// empty. That is faithful to [CanReadCollection], whose doc explicitly
// disclaims the tombstone question ("an admin also sees soft-deleted
// rows … is a different question about a different set of rows"), and
// GetCollection depends on exactly that behaviour to reach its Restore
// branch.
//
// So a CALLER that must not surface tombstoned rows to an admin — a
// corpus, a listing, an autocomplete — has to say so itself. That is a
// corpus constraint, not a second copy of the read rule, and it is the
// ONLY expression of it on the admin arm rather than a restatement of
// the predicate's (the #449 defect). State it inline and say why.
func CollectionReadableSQL(
	ctx context.Context,
	alias string,
	caller Caller,
	caps CapabilityChecker,
	argOffset int,
) (string, []any, error) {
	// Capability FIRST, mirroring CanReadCollection: an admin costs
	// zero query planning, and their answer cannot depend on the
	// predicate builder failing.
	if caps != nil && caps(SystemAdmin) {
		return "", nil, nil
	}

	pred, err := Filter(ctx, EntityCollection, caller)
	if err != nil {
		return "", nil, fmt.Errorf("collection row plane: %w", err)
	}
	frag, args := pred.ToSQL(alias, argOffset)
	return frag, args, nil
}
