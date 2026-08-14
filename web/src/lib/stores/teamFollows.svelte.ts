// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The teams rail's state: which teams the signed-in user follows
// (#577).
//
// Named for what it holds — the caller's team FOLLOWS — not for the
// competitor's word this shipped under (#1029). `teams` alone would
// have been the wrong rename: this is not every team on the instance,
// it is the subset one reader bookmarked, and the directory page
// fetches the full list separately.
//
// # Why a shared store and not per-component fetches
//
// Three surfaces need this one list at once: the rail on browse, the
// follow button on a team page, and the follow button on a directory
// card. If each owned its own copy, following a team from the team page
// would leave the rail showing yesterday's answer until a reload — the
// single most visible thing this sprint ships, broken by a caching
// choice.
//
// So the list lives here, the mutations live here, and every surface
// reads the same $state. Following anywhere updates the rail
// everywhere, with no cross-component plumbing.
//
// # Optimistic, unlike FollowButton
//
// The user-follow button deliberately re-fetches its relationship after
// every click rather than mutating local state, because a user
// relationship has states the client cannot predict — the target may
// have blocked you between render and click.
//
// A team follow has no such states. It is a bookmark: it grants
// nothing, nobody else can change it, and the only two outcomes are
// "followed" and "not followed". So this flips immediately and reverts
// on failure. The rail is chrome the user is pointing at; a round trip
// before it moves reads as lag.
//
// # No counts, no unread
//
// Deliberately absent. A followed team is not a notification feed
// (#520 owns that arc), and there is no follower count on the wire to
// cache.

import { api } from '$api/client';

export interface TeamSummary {
  id: string;
  slug: string;
  name: string;
  description: string;
}

class TeamFollows {
  /** Teams the caller follows, ordered by name (the server's order). */
  items = $state<TeamSummary[]>([]);
  /** True once a load has completed — lets the rail tell "empty" from
   *  "not asked yet" and skip rendering an empty state during boot. */
  loaded = $state(false);
  /** Teams with a follow/unfollow in flight, so a button can disable
   *  itself without a component-local flag going stale on navigation. */
  pending = $state<Set<string>>(new Set());

  isFollowing(id: string): boolean {
    return this.items.some((c) => c.id === id);
  }

  isPending(id: string): boolean {
    return this.pending.has(id);
  }

  /** Load the rail. Safe to call on every mount — cheap, and it is how
   *  the rail picks up a follow made in another tab. A 401/403 (signed
   *  out, or no teams.read) empties the rail rather than erroring:
   *  there is nothing for a guest to be told here. */
  async load(): Promise<void> {
    const { data, error } = await api.GET('/auth/me/followed-teams', {});
    if (error || !data) {
      this.items = [];
      this.loaded = true;
      return;
    }
    this.items = data as TeamSummary[];
    this.loaded = true;
  }

  /**
   * Follow or unfollow, optimistically.
   *
   * Both endpoints are idempotent server-side, so a double-click cannot
   * desynchronise the two: the second request is a no-op success and
   * the local state was already where it needed to be. The `pending`
   * guard is about not firing the request twice, not about correctness.
   *
   * Returns the resulting state, or the unchanged state on failure.
   */
  async toggle(team: TeamSummary): Promise<boolean> {
    if (this.pending.has(team.id)) return this.isFollowing(team.id);
    const wasFollowing = this.isFollowing(team.id);
    const before = this.items;

    // Reassign rather than mutate — $state tracks the assignment.
    this.pending = new Set([...this.pending, team.id]);
    this.items = wasFollowing
      ? this.items.filter((c) => c.id !== team.id)
      : [...this.items, team].sort((a, b) => a.name.localeCompare(b.name));

    try {
      const { error } = wasFollowing
        ? await api.DELETE('/teams/{id}/follow', { params: { path: { id: team.id } } })
        : await api.POST('/teams/{id}/follow', { params: { path: { id: team.id } } });
      if (error) {
        // Revert to the exact previous list, not a recomputed one — the
        // server may have been mid-change and a re-sort would invent an
        // order nothing asked for.
        this.items = before;
        return wasFollowing;
      }
      return !wasFollowing;
    } catch {
      this.items = before;
      return wasFollowing;
    } finally {
      const next = new Set(this.pending);
      next.delete(team.id);
      this.pending = next;
    }
  }

  /** Drop everything. Called on sign-out so the next user does not
   *  inherit the previous one's rail. */
  reset(): void {
    this.items = [];
    this.loaded = false;
    this.pending = new Set();
  }
}

export const teamFollows = new TeamFollows();
