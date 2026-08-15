// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Kenneth Blossom

// The browse teams rail's contents and the reader's edit of them
// (#1113).
//
// # What this store owns, and what it deliberately does not
//
// It owns the ALL-TEAMS list (`listTeams`) and the CURATION — which
// chips the reader hid, and the order they dragged the followed ones
// into. It does NOT own follows: `teamFollows` already does, three
// surfaces read from it, and a second copy of the follow set is how a
// follow made on a team page stops moving the rail (that store's own
// note explains the bill). So the rail composes the two.
//
// # The rail lists every visible team, not the follow set
//
// This is the #1113 reversal. Until now the rail WAS the follow set,
// which made a reader's first visit an empty strip and a pointer at the
// directory. It is now a filter control over the feed, and a filter
// with only the things you already subscribed to cannot introduce you
// to anything. Follows survive as the SORT — followed teams lead — and
// as the "Following" feed tab's input, which is untouched.
//
// # Curation is applied HERE, on the client
//
// `user_preferences.team_rail` is never read by the server. Hiding a
// team from your rail must not hide its posts from your feed, and the
// cheapest guarantee of that is that no server-side query has the list
// available to consult. See the openapi schema for the full argument.

import { api } from '$api/client';
import { auth } from '$stores/auth.svelte';
import { teamFollows, type TeamSummary } from '$stores/teamFollows.svelte';

/** One page is plenty: the directory itself pages at 100 and an
 *  instance with more teams than this has a rail nobody scrolls
 *  anyway. Deliberately not paged — a filter strip that grows as you
 *  drag it would re-order under the cursor. */
const TEAM_LIMIT = 200;

class TeamRailState {
  /** Every team the caller can see, in the server's name order. */
  teams = $state<TeamSummary[]>([]);
  /** True once the teams list has resolved. The rail renders nothing
   *  before then — a strip that flashes and then fills reads as a bug
   *  on every page load. */
  loaded = $state(false);

  /** Ids the reader removed from their rail. */
  hidden = $state<string[]>([]);
  /** The reader's explicit ordering, applied to the FOLLOWED group.
   *  Partial lists are normal: what is named here leads, in this order,
   *  and everything else keeps its previous relative position. */
  order = $state<string[]>([]);

  /** True while a curation write is in flight or queued. Lets the panel
   *  avoid claiming a save landed before it did. */
  saving = $state(false);

  #seeded = false;
  #loading = false;
  #timer: ReturnType<typeof setTimeout> | null = null;

  /**
   * Seed the curation from the session and fetch the teams.
   *
   * The seed is SYNCHRONOUS and comes off `/auth/me`, which the root
   * layout awaits before any page renders — the #706 pattern. That is
   * what stops the rail painting the uncurated list and then
   * rearranging itself in front of the reader.
   *
   * Safe to call on every mount. The seed runs once per session so a
   * pending unsaved reorder is never clobbered by a remount; the fetch
   * re-runs, which is how the rail picks up a team created elsewhere.
   */
  init(): void {
    if (!auth.user) {
      this.reset();
      return;
    }
    if (!this.#seeded) {
      this.#seeded = true;
      const rail = auth.user.teamRail;
      // Absent means THE DEFAULT RAIL — every visible team, in the
      // server's order — not an empty one. Reading it as "hide
      // everything" would blank the rail for every account that has
      // never opened the manage panel, which is all of them.
      this.hidden = [...(rail?.hidden_team_ids ?? [])];
      this.order = [...(rail?.team_order ?? [])];
    }
    void this.load();
  }

  async load(): Promise<void> {
    if (this.#loading) return;
    this.#loading = true;
    try {
      const { data, error } = await api.GET('/teams', {
        params: { query: { limit: TEAM_LIMIT } as never },
      });
      // A 401/403 empties the list rather than erroring: a guest holds
      // no `teams.read` and there is nothing to tell them here. The
      // rail's own guard is `auth.user`, so this path is only reached
      // by a session that lost its rights mid-visit.
      this.teams = error || !data ? [] : ((data.items ?? []) as TeamSummary[]);
    } finally {
      this.#loading = false;
      this.loaded = true;
    }
  }

  /** Drop everything. Called on sign-out so the next user does not
   *  inherit the previous one's rail — including their curation, which
   *  is the half a stale `#seeded` flag would otherwise keep. */
  reset(): void {
    this.teams = [];
    this.hidden = [];
    this.order = [];
    this.loaded = false;
    this.#seeded = false;
  }

  isHidden(id: string): boolean {
    return this.hidden.includes(id);
  }

  /**
   * The rail's render order.
   *
   *   1. the operator's featured team, first among teams (#1084),
   *   2. teams the caller follows, in the reader's own order and then
   *      by the server's name order,
   *   3. everything else the caller can see, by name,
   *   minus anything hidden.
   *
   * The featured team is drawn from `teamFollows.featured` and deduped
   * out of the rest: it is on screen because an operator placed it
   * there, and demoting it to alphabetical position for the readers who
   * happen to follow it would silently undo the curation for exactly
   * the people most likely to notice.
   *
   * Hiding applies to the featured slot too. It is the reader's rail;
   * an un-hideable chip would be the one piece of it they cannot edit.
   */
  get railTeams(): TeamSummary[] {
    const hidden = new Set(this.hidden);
    const featured = teamFollows.featured.filter((c) => !hidden.has(c.id));
    const featuredIds = new Set(featured.map((c) => c.id));
    const followedIds = new Set(teamFollows.items.map((c) => c.id));

    const rest = this.teams.filter((c) => !hidden.has(c.id) && !featuredIds.has(c.id));
    const followed = rest.filter((c) => followedIds.has(c.id));
    const unfollowed = rest.filter((c) => !followedIds.has(c.id));
    return [...featured, ...this.sortByUserOrder(followed), ...unfollowed];
  }

  /**
   * Apply the reader's `team_order` to a list.
   *
   * Named ids lead in the order given; everything else keeps its
   * incoming relative position behind them. Written as a partition
   * rather than a comparator over "index or Infinity" because that
   * comparator is not a total order when two teams are both unnamed —
   * `localeCompare` would have to break the tie and the incoming order
   * (the server's) is the better tiebreak, being the one the reader
   * already saw.
   */
  sortByUserOrder(list: TeamSummary[]): TeamSummary[] {
    if (this.order.length === 0) return list;
    const byId = new Map(list.map((c) => [c.id, c]));
    const lead: TeamSummary[] = [];
    for (const id of this.order) {
      const team = byId.get(id);
      if (team) {
        lead.push(team);
        byId.delete(id);
      }
    }
    return [...lead, ...byId.values()];
  }

  /** Hide / un-hide a team's chip. Never touches the feed: the browse
   *  page's query is driven by the ACTIVE chip, not by this list. */
  toggleHidden(id: string): void {
    this.hidden = this.hidden.includes(id)
      ? this.hidden.filter((x) => x !== id)
      : [...this.hidden, id];
    this.persist();
  }

  /**
   * Move a followed team one place up (-1) or down (+1) in the rail.
   *
   * The stored `order` is rewritten from the CURRENT rendered sequence
   * of followed teams rather than patched in place, because a partial
   * stored list plus a move is ambiguous — moving an unnamed team up
   * past another unnamed team has no expression in a list that mentions
   * neither. Materialising the visible sequence at the moment of the
   * move makes the result exactly what the reader just saw, and it is
   * still a partial list of the whole instance: it names the teams the
   * reader follows, not every team that exists.
   */
  move(id: string, delta: -1 | 1): void {
    const seq = this.followedInRailOrder().map((c) => c.id);
    const from = seq.indexOf(id);
    if (from < 0) return;
    const to = from + delta;
    if (to < 0 || to >= seq.length) return;
    seq.splice(to, 0, ...seq.splice(from, 1));
    this.order = seq;
    this.persist();
  }

  /** The followed teams in the sequence the rail is drawing them —
   *  including any hidden ones, so a hidden team keeps its place in the
   *  order rather than being re-sorted to the end when it comes back. */
  followedInRailOrder(): TeamSummary[] {
    const known = new Map(this.teams.map((c) => [c.id, c]));
    const followed = teamFollows.items.map((c) => known.get(c.id) ?? c);
    return this.sortByUserOrder(followed);
  }

  /**
   * Persist the curation, debounced.
   *
   * ⚠️ PATCH /account/preferences is FULL-OBJECT REPLACEMENT — the
   * endpoint says so and the handler means it. Sending `{team_rail}`
   * alone would clear this reader's notification channels, email
   * cadence, default views and feed filters in one keystroke. So every
   * write reads the current object first and sends it back with the
   * rail swapped in. That is a round trip per save, and it is not
   * negotiable at this contract; the debounce is what keeps a drag from
   * spending one per frame.
   */
  persist(): void {
    if (!auth.user) return;
    this.saving = true;
    if (this.#timer) clearTimeout(this.#timer);
    this.#timer = setTimeout(() => {
      this.#timer = null;
      void this.#write();
    }, 400);
  }

  /** Flush a pending debounce immediately. The manage panel calls this
   *  when it closes, so a reorder followed straight by a reload cannot
   *  lose the last move. */
  async flush(): Promise<void> {
    if (this.#timer) {
      clearTimeout(this.#timer);
      this.#timer = null;
      await this.#write();
    }
  }

  async #write(): Promise<void> {
    try {
      const current = await api.GET('/account/preferences', {});
      if (current.error || !current.data) return;
      const p = current.data as {
        notification_channels?: Record<string, string[]>;
        email_cadence?: Record<string, string>;
        default_views?: Record<string, string>;
        feed_filters?: { show_restricted?: boolean };
      };
      await api.PATCH('/account/preferences', {
        body: {
          notification_channels: p.notification_channels ?? {},
          email_cadence: p.email_cadence ?? {},
          default_views: p.default_views ?? {},
          feed_filters: p.feed_filters ?? {},
          team_rail: { hidden_team_ids: this.hidden, team_order: this.order },
        } as never,
      });
    } finally {
      this.saving = false;
    }
  }
}

export const teamRail = new TeamRailState();
