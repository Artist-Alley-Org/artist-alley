// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package collections

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mscrnt/artist-alley/app/internal/auth"
)

// ---------------------------------------------------------------------------
// The container half of a membership write, for callers outside this package
// ---------------------------------------------------------------------------
//
// #882. A collection has TWO kinds of member — `collection_resources`
// (assets) and `collection_posts` (posts) — and adding either asks the
// same two questions: may this caller mutate THIS collection, and may
// this caller reach the THING being put in it.
//
// The asset endpoints answer both in this package, because both halves
// live here. The post endpoints cannot: the payload they return is a
// `Post` with its membership hydrated and its #883 placeholders applied,
// and the gate they need is the post read rule — both of which live in
// `posts`. Rebuilding either here would be a second expression of a rule
// (epic #665), and the deleted ListCollectionPostsPage in
// posts/queries.sql is the note left by the last person to look at this:
// "a future collection-posts listing must go through the post read rule
// (posts.readRuleSQL) the way ListPostsByAssetGated does."
//
// So the post endpoints live in `posts` and obtain the CONTAINER half
// from here rather than restating it. This file is that seam, and it is
// deliberately one function: a caller cannot fetch the row and forget to
// ask about it, or ask about a row it fetched with a different query
// (GetCollectionIncludingDeleted, say, which would let a member be
// pinned into a collection sitting in the trash).

// ErrCollectionUnreachable is returned by [ResolveMemberWrite] when the
// collection does not exist, is soft-deleted, or the caller may not
// mutate it.
//
// ONE error for all three on purpose. The two conditions are separately
// knowable here and must not be separately reportable: a caller who can
// tell "not yours" from "no such thing" can enumerate collection UUIDs,
// which is the same oracle the ADD gate on the member side exists to
// close. Callers answer 404 and say nothing more.
//
// This is a deliberate NARROWING of what the asset endpoints do — they
// answer 404 for an absent collection and 403 "not the owner of this
// collection" for one the caller cannot mutate, which does distinguish
// the two. That difference is grandfathered, not endorsed: the asset
// routes have shipped with it since before #882 and changing them is a
// visible API contract change for every existing client. New routes do
// not inherit it.
var ErrCollectionUnreachable = errors.New("collections: collection not found or not mutable")

// MemberWriteTarget is a collection resolved for a membership write:
// confirmed to exist, confirmed live, confirmed mutable by the caller.
//
// It carries the Name because every membership write emits an Add /
// Remove activity whose payload records the target's last-known name
// (see emit.AddToCollection). Fetching the row twice to get it — once
// to authorise, once to label — is how the two come to disagree.
type MemberWriteTarget struct {
	ID   uuid.UUID
	Name string
	// OwnerUserRef is the collection's owner. Exposed for callers that
	// need to attribute the container (audit, notifications); the
	// authorisation question is already answered by the fact that you
	// hold this struct at all.
	OwnerUserRef int64
}

// ResolveMemberWrite answers the container half of a membership write:
// "may this caller add to / remove from THIS collection".
//
// It is [canMutateCollection] — owner, `collections.admin`, or
// `system.admin` — applied to the row GetCollection returns, which
// excludes soft-deleted collections. Both halves matter and neither is
// restated at the call site: this returns the SAME answer the asset
// endpoints in this package compute inline, so the two member surfaces
// cannot drift into disagreeing about who owns a collection.
//
// Returns [ErrCollectionUnreachable] for absent, deleted, and
// not-yours alike; any other error is a real failure to reach the
// database and callers must surface it as a 500 rather than a refusal.
func ResolveMemberWrite(
	ctx context.Context,
	db DBTX,
	caller *auth.Identity,
	collectionID uuid.UUID,
) (MemberWriteTarget, error) {
	row, err := New(db).GetCollection(ctx, pgtype.UUID{Bytes: collectionID, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return MemberWriteTarget{}, ErrCollectionUnreachable
		}
		return MemberWriteTarget{}, fmt.Errorf("collections: resolve member write: %w", err)
	}
	if !canMutateCollection(caller, row) {
		return MemberWriteTarget{}, ErrCollectionUnreachable
	}
	return MemberWriteTarget{
		ID:           uuid.UUID(row.ID.Bytes),
		Name:         row.Name,
		OwnerUserRef: row.OwnerUserRef,
	}, nil
}
