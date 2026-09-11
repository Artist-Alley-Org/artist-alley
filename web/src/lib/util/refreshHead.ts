// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

/**
 * Merge a freshly fetched FIRST PAGE into a list the reader has already
 * paged through (#1407).
 *
 * # Why a merge and not a reload
 *
 * Every list that can go stale after a publish is cursor paged, and
 * three of them accumulate pages as the reader scrolls. The obvious
 * refresh, re-running the first page and assigning it, throws away every
 * page after it and puts the reader back at the top of a list they were
 * halfway down. That trades a stale page for a destructive one.
 *
 * The obvious alternative, pushing the new row onto the end, is worse,
 * because it puts an item wherever the client guessed rather than where
 * the server's sort says it goes, and it re-appears when a later page
 * fetch reaches it honestly.
 *
 * So: the server's first page replaces the first page, and everything
 * the reader had beyond it keeps its order behind that. Concretely,
 * given `fresh` = the page the server just returned and `existing` =
 * what is on screen:
 *
 *   result = fresh ++ (existing minus everything in fresh)
 *
 * # What that guarantees
 *
 * - **Exactly once.** An id in `fresh` is removed from the tail, so a
 *   row that was already loaded cannot appear twice, and a row pushed
 *   from page one onto page two by a new arrival is not duplicated
 *   either: it drops out of `fresh` and survives in the tail, in the
 *   position the shift actually moved it to.
 * - **Server order.** Nothing here decides where anything goes. The
 *   head is the server's answer verbatim; the tail is the server's
 *   earlier answer with the overlap taken out.
 * - **Direction agnostic.** With the feed sorted oldest-first, a new
 *   post is genuinely not on page one, `fresh` contains nothing new,
 *   and the list is left alone rather than being told a lie about
 *   where the item sits.
 * - **No cursor churn.** The caller keeps its existing `next_cursor`.
 *   Keyset cursors name a position in the sort, not an offset, so rows
 *   arriving at the head do not move it. That holds for `/search` too:
 *   its cursor is `(score, id, type)` (app/internal/search/interfaces.go),
 *   not an offset.
 *
 * # The key
 *
 * `id` by default, which is what the posts and assets lists are keyed
 * on. `/search` passes its own, because a search result's identity is
 * the PAIR `(type, id)`: the server itself tie-breaks on both, since
 * the three entity types come from three tables and nothing makes one
 * table's uuid distinguishable from another's. Keying a mixed list on
 * `id` alone would be asserting a uniqueness the data does not promise.
 */
export function mergeRefreshedHead<T>(
  existing: readonly T[],
  fresh: readonly T[],
  key: (row: T) => string = (row) => (row as { id: string }).id,
): T[] {
  const refreshed = new Set(fresh.map(key));
  return [...fresh, ...existing.filter((row) => !refreshed.has(key(row)))];
}

/**
 * Put a fetched page on the END of a list, minus anything the list
 * already holds (#1407).
 *
 * The twin of `mergeRefreshedHead`, for a surface whose append composes
 * from the list AS IT IS WHEN THE RESPONSE LANDS rather than from the
 * list the request was issued against. On `/teams/{id}` those two can
 * differ, because a post-publish head refresh can retain rows that a
 * page request already on the wire is also about to deliver, and a bare
 * concatenation then shows them twice.
 *
 * Order is the server's: the incoming page keeps its sequence, and it
 * goes after what is on screen. Only the overlap is dropped, and
 * dropping it is what keeps the row at the ONE position it already
 * occupies rather than moving it to the tail.
 */
export function appendWithoutRepeats<T>(
  existing: readonly T[],
  incoming: readonly T[],
  key: (row: T) => string = (row) => (row as { id: string }).id,
): T[] {
  const held = new Set(existing.map(key));
  return [...existing, ...incoming.filter((row) => !held.has(key(row)))];
}
