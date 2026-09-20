// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

package search

import (
	"bytes"
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mscrnt/artist-alley/app/internal/search/facet"
	"github.com/mscrnt/artist-alley/app/internal/visibility"
)

// The RECENT ORDER (#1173, sprint 25b): how `last:N` runs through the
// engine.
//
// # One ordering fact, four consumers
//
// The order is `recency DESC, id DESC, type ASC`. It is spelled ONCE, as
// [recentKey.before], and everything else is derived from it:
//
//   - the WINDOW (which rows are inside `last:N`) is a facet predicate
//     rendered by facet.recentWindowSQL over the same clocks and the
//     same integer rank ([facet.RecentRank], which descends in type
//     string order so `type ASC` is `rank DESC` and the whole tuple
//     compares one way);
//   - the SQL KEYSET that positions each arm after the cursor
//     ([recentKeysetFragment]) is `ROW(clock, id, rank) < ROW($ts, $id,
//     $rank)`, one row comparison, no `<`/`<=` switch, because the rank
//     is IN the tuple rather than implied by which arm is rendering;
//   - the Go MERGE across arms sorts on [recentKey.before];
//   - the Go CURSOR CUT keeps a hit when the cursor's key is before it.
//
// [keysetFragment] is NOT reused: its `<`/`<=` rule encodes the
// relevance order's `type DESC` tie-break and would be wrong here.
//
// # The effective recency key is private
//
// [Hit.recency] is `created_at` for assets and collections and
// `posted_at` for posts, set by every arm on every hit; [Hit.CreatedAt]
// stays the public creation timestamp on every entity (`posts.created_at`
// for posts) and is neither overwritten nor read by this order.
//
// # Count
//
// Inside a window the count is EXACT and never capped: the window holds
// at most N rows, N is at most [dsl.MaxLastWindow], and every other term
// narrows inside it, so the per-arm count statements (LIMIT
// TotalCountCap+1) can never truncate. [assembleTotal] is the pure rule.

// recentWindow is the recent order's execution inputs: the entity types
// the window ranks over. nil means the relevance order.
type recentWindow struct {
	types []HitType
}

// entityOf maps a hit type to its visibility entity.
func entityOf(t HitType) visibility.EntityType {
	switch t {
	case HitTypeCollection:
		return visibility.EntityCollection
	case HitTypePost:
		return visibility.EntityPost
	}
	return visibility.EntityAsset
}

// recentRank is [facet.RecentRank] on a hit type.
func recentRank(t HitType) int { return facet.RecentRank(entityOf(t)) }

// recentArms renders the window's arms for the requested types from the
// facet package's baselines, the SAME fragments the suggestion
// aggregators count with, binding from offset+1. Nil arms and no args
// in the relevance order, so every relevance statement is byte-for-byte
// what it was.
func recentArms(ctx context.Context, q Query, win *recentWindow, offset int) ([]facet.RecentArm, []any, error) {
	if win == nil {
		return nil, nil, nil
	}
	entities := make([]visibility.EntityType, 0, len(win.types))
	for _, t := range win.types {
		entities = append(entities, entityOf(t))
	}
	return facet.RecentArms(ctx, facet.Request{
		Caller:       visibility.NewCaller(q.CallerUserRef),
		Caps:         q.Caps,
		PostCaps:     q.PostCaps,
		MutationCaps: q.MutationCaps,
		Mature:       q.Mature,
	}, entities, offset)
}

// recentContextOf is [renderContextOf] plus the window's arms.
func recentContextOf(q Query, callerArg string, arms []facet.RecentArm) facet.RenderContext {
	rc := renderContextOf(q, callerArg)
	rc.RecentArms = arms
	return rc
}

// recentKey is one hit's position in the recent order.
type recentKey struct {
	ts time.Time
	id uuid.UUID
	t  HitType
}

func recentKeyOf(h Hit) recentKey { return recentKey{ts: h.recency, id: h.ID, t: h.Type} }

func recentKeyOfCursor(c Cursor) recentKey {
	return recentKey{ts: c.LastRecency, id: c.LastID, t: c.LastType}
}

// before reports whether k sorts EARLIER than o in the recent order:
// newer first, then the greater id, then the smaller type string
// (asset, collection, post), which is the greater rank.
//
// Times compare with Equal/After so a key read from the wire (unix
// microseconds, UTC) and one scanned from Postgres (microseconds, some
// zone) compare as instants and not as struct bytes. Ids compare as
// bytes, which is what Postgres's uuid comparison does and what the
// hex string comparison the relevance order uses amounts to.
func (k recentKey) before(o recentKey) bool {
	if !k.ts.Equal(o.ts) {
		return k.ts.After(o.ts)
	}
	if c := bytes.Compare(k.id[:], o.id[:]); c != 0 {
		return c > 0
	}
	return recentRank(k.t) > recentRank(o.t)
}

// recentKeysetFragment positions ONE arm after the cursor in the recent
// order: `ROW(clock, id, rank) < ROW($ts, $id, $rank)`, where rank is
// the arm's constant [facet.RecentRank] and the cursor's rank is bound
// beside its timestamp and id. Empty for the first page. `base` is the
// last placeholder index already in use.
func recentKeysetFragment(t HitType, cur *Cursor, clockCol, idCol string, base int) (string, []any) {
	if cur == nil {
		return "", nil
	}
	return "\n\t\t   AND ROW(" + clockCol + ", " + idCol + ", " + strconv.Itoa(recentRank(t)) + ") < " +
			"ROW($" + strconv.Itoa(base+1) + "::TIMESTAMPTZ, $" + strconv.Itoa(base+2) + "::UUID, $" +
			strconv.Itoa(base+3) + "::INT)",
		[]any{cur.LastRecency, cur.LastID, recentRank(cur.LastType)}
}

// assembleTotal is the count rule, pure so the boundary can be proven
// without a corpus.
//
// Relevance: any arm at or over [TotalCountCap] flags the cap, and a
// sum at or over it is flagged and clamped, the "10,000+" contract ADR
// 0056 §1 fixes. Recent: the sum, exact, never flagged and never
// clamped; a window of `last:10000` holding exactly 10,000 rows reports
// 10,000 and `total_count_capped: false`, because the number is the
// size of the window and the window is bounded by construction.
func assembleTotal(perTypeCount map[HitType]int, recent bool) (total int, capped bool) {
	for _, c := range perTypeCount {
		if !recent && c >= TotalCountCap {
			capped = true
		}
		total += c
	}
	if !recent && total >= TotalCountCap {
		capped = true
		total = TotalCountCap
	}
	return total, capped
}
