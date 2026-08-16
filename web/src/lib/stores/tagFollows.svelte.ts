// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// Which tags the signed-in reader follows (#1123).
//
// Sibling of `teamFollows`, deliberately a SEPARATE store rather than a
// second array inside it. The two answer different questions with
// different key types — a team is a row with an id, a picture and a
// name; a tag is a string — and `isFollowing` means "is this uuid in the
// set" on one and "is this exact string in the set" on the other.
// Folding them together would have produced one lookup that had to
// guess which kind it was holding.
//
// # Optimistic, for teamFollows' reason
//
// A tag follow is a bookmark: it grants nothing, nobody else can change
// it, and the only two outcomes are "followed" and "not followed". So
// this flips immediately and reverts on failure. Both endpoints are
// idempotent server-side, so a double-click cannot desynchronise them.
//
// # The tag string is the identity, exactly as stored
//
// `?tag=` matches the corpus exactly (case included), so a follow's
// string IS the filter that chip will apply. Nothing here lowercases,
// slugifies or strips a leading `#` beyond the single one the input
// affordance lets a reader type — see `normalize` below, and migration
// 00050 for why the whole product agrees on exact matching.

import { api } from '$api/client';

export interface FollowedTag {
  tag: string;
  followed_at: string;
}

/**
 * Trim, and drop ONE leading `#` if the reader typed it.
 *
 * The `#` is presentation: the rail draws a hash glyph and the heading
 * prints `#fantasy`, but the corpus stores `fantasy` and `?tag=fantasy`
 * is what matches it. A reader typing what they see is the common case,
 * so accepting it is not a special case — refusing it would be.
 *
 * ONE `#`, not a greedy strip: `##ok` is a tag whose name starts with a
 * hash, and there is no reason to believe the reader meant otherwise.
 * Whitespace is trimmed to match the server, which trims for the same
 * reason `post_tags` writes do.
 */
export function normalizeTagInput(raw: string): string {
  const trimmed = raw.trim();
  return (trimmed.startsWith('#') ? trimmed.slice(1) : trimmed).trim();
}

class TagFollows {
  /** Tags the caller follows, most recently followed first (the
   *  server's order). The rail's own `tag_order` preference is a
   *  partial override applied one layer up, in the rail store. */
  items = $state<FollowedTag[]>([]);
  /** True once a load has completed — lets the rail tell "empty" from
   *  "not asked yet" and skip rendering during boot. */
  loaded = $state(false);
  /** Tags with a follow/unfollow in flight, so a row can disable itself
   *  without a component-local flag going stale on navigation. */
  pending = $state<Set<string>>(new Set());

  /** O(1) membership. Derived rather than a `some()` scan because the
   *  manage panel asks this once per rendered row. */
  #lookup = $derived(new Set(this.items.map((t) => t.tag)));

  get tags(): string[] {
    return this.items.map((t) => t.tag);
  }

  isFollowing(tag: string): boolean {
    return this.#lookup.has(tag);
  }

  isPending(tag: string): boolean {
    return this.pending.has(tag);
  }

  /** Load the follow set. Safe to call on every mount — cheap, and it is
   *  how the rail picks up a follow made in another tab. A 401 empties
   *  the list rather than erroring: a guest has nothing to be told. */
  async load(): Promise<void> {
    const { data, error } = await api.GET('/auth/me/followed-tags', {});
    this.items = error || !data ? [] : (data as FollowedTag[]);
    this.loaded = true;
  }

  /**
   * Follow or unfollow one tag, optimistically.
   *
   * Returns the resulting state, or the unchanged state on failure.
   * A blank tag is refused locally rather than sent — the server would
   * 400 it, and a request whose only possible answer is "no" is one the
   * reader waits for needlessly.
   */
  async toggle(rawTag: string): Promise<boolean> {
    const tag = normalizeTagInput(rawTag);
    if (tag === '') return false;
    if (this.pending.has(tag)) return this.isFollowing(tag);

    const wasFollowing = this.isFollowing(tag);
    const before = this.items;

    // Reassign rather than mutate — $state tracks the assignment.
    this.pending = new Set([...this.pending, tag]);
    // A new follow goes to the FRONT, matching the server's
    // `ORDER BY created_at DESC`. Appending would put the chip the
    // reader just created at the far end of a scrolling strip, which
    // reads as the follow not having worked.
    this.items = wasFollowing
      ? this.items.filter((t) => t.tag !== tag)
      : [{ tag, followed_at: new Date().toISOString() }, ...this.items];

    try {
      const { error } = wasFollowing
        ? await api.DELETE('/tags/{tag}/follow', { params: { path: { tag } } })
        : await api.POST('/tags/{tag}/follow', { params: { path: { tag } } });
      if (error) {
        // Revert to the exact previous list, not a recomputed one.
        this.items = before;
        return wasFollowing;
      }
      return !wasFollowing;
    } catch {
      this.items = before;
      return wasFollowing;
    } finally {
      const next = new Set(this.pending);
      next.delete(tag);
      this.pending = next;
    }
  }

  /** Drop everything. Called on sign-out so the next reader does not
   *  inherit the previous one's chips. */
  reset(): void {
    this.items = [];
    this.loaded = false;
    this.pending = new Set();
  }
}

export const tagFollows = new TagFollows();
