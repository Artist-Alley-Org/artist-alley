<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The teams rail (#577) — now a FEED FILTER over the browse page
   * (#1113), not a navigation strip.
   *
   * # The reversal, and what it changes
   *
   * #1097 shipped these chips as links to team pages. A chip now
   * FILTERS THE FEED IN PLACE: click one and the wall below narrows to
   * that team's posts, on the same page, with `?team=` in the URL so a
   * reload and a shared link reproduce it. Click it again, or the
   * "All teams" chip, and the filter clears. Unfiltered is the default.
   *
   * That makes the chips toggle BUTTONS with `aria-pressed`, not links,
   * and it is why the rail no longer navigates anywhere: a strip where
   * some chips filter and others leave the page would be two controls
   * wearing one shape. Team pages stay reachable through the manage
   * panel's Explore entry and the /teams directory.
   *
   * The filtering itself costs no backend at all — `GET /posts` has
   * taken `team_id` since the team page shipped, and the browse page
   * passes the same parameter. It composes with the Latest/Following
   * pill and the sort direction server-side because all three are
   * `/posts` parameters.
   *
   * # The rail lists EVERY VISIBLE TEAM
   *
   * It used to be the follow set, which made a filter strip that could
   * only offer what you had already subscribed to. Follows survive as
   * the SORT — followed teams lead, then the rest by name — and the
   * featured team keeps first position among teams (#1084). See
   * `teamRail.railTeams` for the ordering and why the featured slot is
   * deduped rather than merged.
   *
   * Curation (hide a chip, reorder the followed group) lives in the ⋯
   * panel and is CLIENT-APPLIED: hiding a team removes its chip and
   * leaves its posts in the feed. The rail is furniture; the feed is
   * content.
   *
   * # Sticky
   *
   * The strip pins below the site chrome while the feed scrolls under
   * it (the featured slider above scrolls away normally). `top-0` is
   * the whole implementation, and it is correct only because #1122
   * moved <main>'s scrollport to start at the chrome's bottom edge —
   * before that, `top: 0` pinned to y=0, i.e. underneath the navbar.
   *
   * ## 390px: it UNPINS
   *
   * Stated as a decision, with the measurement behind it. On a 390x844
   * phone the chrome is 99px and this strip is ~64px: pinning it spends
   * 19% of the viewport permanently on chrome, on the width where the
   * feed is a single column and the reader is scrolling one card at a
   * time. Collapsing the strip's height was the alternative and buys
   * back ~24px for a chip row too short to read a team name in. So
   * below `sm` the rail scrolls away with the rest of the page and
   * comes back the way the featured strip does — by scrolling up. This
   * is the "mobile is a REDUCED app" call, not a shrink.
   *
   * # No visible heading (#1030), and the one that IS visible
   *
   * The strip still has no `<h2>` of its own — a row of team chips is
   * self-evidently a row of teams — and carries its name as the
   * section's `aria-label`. The heading that appears BELOW the rail
   * ("All Teams", or the filtered team's name with a Follow button) is
   * the browse page's, not this component's: it describes the FEED, and
   * it scrolls with the feed rather than pinning with the strip.
   *
   * # Signed-in only
   *
   * A guest holds no `teams.read`, so there is nothing to fetch and
   * nothing to filter with — the rail renders nothing at all rather
   * than an empty state advertising a members-only surface. Unchanged
   * by this issue.
   */
  import { teamFollows } from '$stores/teamFollows.svelte';
  import { teamRail } from '$stores/teamRail.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import TeamAvatar from '$components/TeamAvatar.svelte';
  import TeamRailManageMenu from '$components/TeamRailManageMenu.svelte';
  import {
    createRailScroll,
    RAIL_ARROW_CLASS,
    RAIL_ARROW_LIVE_CLASS,
    RAIL_ARROW_DISABLED_CLASS,
  } from '$lib/util/railScroll.svelte';

  interface Props {
    /** The team the feed is currently filtered to, or null for the
     *  unfiltered default. Owned by the browse page's URL, not by this
     *  component — a rail that held its own selection would disagree
     *  with the address bar the first time someone used the back
     *  button. */
    activeTeamId?: string | null;
    /** Called with the team to filter to, or null to clear. */
    onselect?: (id: string | null) => void;
  }
  let { activeTeamId = null, onselect }: Props = $props();

  // One effect, no onMount beside it. This component used to run both,
  // which fired every load twice on first paint — an `$effect` already
  // runs after mount, so the onMount arm was a duplicate of the mount
  // case rather than a different one. The effect covers both: it fills
  // the rail on mount and on a mid-visit sign-in, and empties it on
  // sign-out rather than leaving the previous user's studios — and
  // their curation — on screen.
  $effect(() => {
    if (auth.user) {
      void teamFollows.load();
      teamRail.init();
    } else {
      teamFollows.reset();
      teamRail.reset();
    }
  });

  const teams = $derived(teamRail.railTeams);
  const featuredIds = $derived(new Set(teamFollows.featured.map((c) => c.id)));

  /** Is there anything for this reader to filter BY?
   *
   *  Deliberately the unfiltered source lists, not `teams` — a reader
   *  who hid every chip must keep the strip, because the ⋯ is the only
   *  way to unhide one and a rail that deletes its own manage button is
   *  a trap.
   *
   *  When both are empty there is nothing at all: the instance has no
   *  teams, or this caller holds no `teams.read` and `/teams` answered
   *  403 (an impersonated Base account, measured — the seeded
   *  non-admins do not hold it). Both render NOTHING rather than a
   *  strip of two controls over an empty strip. That is the same
   *  judgement the guest arm has always made, now that the rail's
   *  source is the whole directory rather than the caller's follows:
   *  the old "you aren't following any teams yet — find some" empty
   *  state pointed at a page these readers cannot open either. */
  const hasTeams = $derived(teamRail.teams.length > 0 || teamFollows.featured.length > 0);

  let scroller = $state<HTMLDivElement | null>(null);
  const rail = createRailScroll(() => scroller, {
    itemSelector: '[data-testid="teams-rail-chip"]',
    // 8px — `gap-2` in the markup below. One number in two places, and
    // the step reads the RENDERED chip's width so a mismatch costs a
    // few px of overshoot rather than a broken control.
    gap: 8,
    fallbackWidth: 160,
  });

  // Re-measure when the strip's contents or box change. Reading the
  // length is what re-runs this once the teams land.
  $effect(() => {
    void teams.length;
    return rail.attach();
  });

  /** Toggle semantics: clicking the active chip clears the filter.
   *  Single-select, so there is never more than one to clear. */
  function pick(id: string) {
    onselect?.(activeTeamId === id ? null : id);
  }
</script>

{#if auth.user && teamRail.loaded && hasTeams}
  <!-- `aria-label` rather than `aria-labelledby`: the heading it used
       to point at is gone (#1030). The string is the same one, so the
       region's name in the accessibility tree is unchanged.

       STICKY from `sm` up only — see the component note for the 390px
       measurement. `bg-surface` is not decoration: a transparent sticky
       strip has the feed scrolling visibly through the chips. The
       negative inline margin + matching padding let that background
       reach the page gutters while the chips stay aligned with the grid
       beneath them. -->
  <section
    aria-label={t('teams.rail_title')}
    data-testid="teams-rail"
    class="-mx-4 px-4 py-1 sm:sticky sm:top-0 sm:z-20 sm:-mx-6 sm:bg-surface sm:px-6"
  >
    <!-- ONE row: the two fixed controls and the scroller are siblings,
         not stacked. `min-w-0` on the scroller is load-bearing — a flex
         child defaults to min-width:auto and would refuse to shrink
         below its content, pushing the fixed controls off screen for
         anyone who can see more than a handful of teams. -->
    <div class="flex items-center gap-2">
      <div class="shrink-0">
        <TeamRailManageMenu />
      </div>

      <!-- "All teams" — the CLEAR-FILTER control, at the head of the
           strip (#1097 put it there; #1113 changed what it does).
           Pinned beside the scroller rather than inside it so it never
           scrolls away, and a toggle button rather than a link because
           it is one of the filter's states, not a destination. The
           directory moved into the manage panel's Explore entry.

           Below `sm` the LABEL collapses and the glyph stands alone:
           measured at 390px with the label showing, the ⋯ and this chip
           took 197 of 390px and left the scroller 146 — barely one team
           chip. `sr-only`, not `hidden`, so the accessible name is
           unchanged at every width. -->
      <button
        type="button"
        onclick={() => onselect?.(null)}
        aria-pressed={activeTeamId === null}
        class="flex min-h-12 shrink-0 items-center gap-2.5 rounded-full border py-1 pl-1 pr-1
               text-sm font-medium transition-colors sm:pr-4 {activeTeamId === null
          ? 'border-accent bg-accent-container text-on-accent-container'
          : 'border-border bg-surface-elevated text-fg hover:border-border-strong hover:bg-state-hover'}"
        data-testid="teams-rail-browse-all"
      >
        <span
          class="flex h-10 w-10 items-center justify-center rounded-full bg-state-hover text-fg-muted"
          aria-hidden="true"
        >
          <svg
            xmlns="http://www.w3.org/2000/svg"
            width="18"
            height="18"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            stroke-width="2"
            stroke-linecap="round"
            stroke-linejoin="round"
          >
            <rect x="3" y="3" width="7" height="7" rx="1" />
            <rect x="14" y="3" width="7" height="7" rx="1" />
            <rect x="3" y="14" width="7" height="7" rx="1" />
            <rect x="14" y="14" width="7" height="7" rx="1" />
          </svg>
        </span>
        <span class="sr-only whitespace-nowrap sm:not-sr-only">{t('teams.rail_clear_filter')}</span>
      </button>

      <!-- `group/rail` scopes the chevrons' hover reveal to the strip;
           `relative` is what they position against. -->
      <div class="group/rail relative min-w-0 flex-1">
        <!-- Horizontal scroll rather than wrap: the rail must stay one
             row tall whether the instance has two teams or forty, or it
             starts pushing the feed off the fold.

             `rail-scroller` hides the scrollbar (global, app.css) and
             the pointer handlers are the drag pan that replaces it —
             both halves come from the shared helper, which is also
             where the featured strip's identical behaviour lives.

             `role="group"` because a div carrying pointer handlers is
             otherwise a control with no role. It takes the LABEL here
             (unlike the featured strip, which leaves it off): these
             chips are a filter control, and "Filter the feed by team"
             is what a screen reader needs before walking a row of
             studio names. -->
        <div
          bind:this={scroller}
          {...rail.handlers}
          role="group"
          aria-label={t('teams.rail_filter_label')}
          class="rail-scroller flex gap-2 overflow-x-auto py-1 {rail.dragging
            ? 'cursor-grabbing'
            : 'cursor-grab'}"
          data-testid="teams-rail-scroller"
        >
          {#each teams as team (team.id)}
            {@const active = activeTeamId === team.id}
            {@const featured = featuredIds.has(team.id)}
            <!-- Chip size (#1097): the avatar is 40px and the chip
                 clears 48px tall, so the whole thing is past the 44px
                 tap-target minimum on its own rather than relying on the
                 row's padding. `min-h-12` rather than a fixed height so
                 a language with taller glyphs is not clipped. -->
            <button
              type="button"
              onclick={() => pick(team.id)}
              aria-pressed={active}
              title={team.description || team.name}
              data-testid="teams-rail-chip"
              data-team-id={team.id}
              class="flex min-h-12 shrink-0 items-center gap-2.5 rounded-full border py-1 pl-1 pr-4
                     text-sm transition-colors {active
                ? 'border-accent bg-accent-container font-medium text-on-accent-container'
                : featured
                  ? 'border-accent bg-surface-elevated text-fg hover:bg-state-hover'
                  : 'border-border bg-surface-elevated text-fg hover:border-border-strong hover:bg-state-hover'}"
            >
              <TeamAvatar {team} class="h-10 w-10 rounded-full" textClass="text-sm" />
              <span class="max-w-[12rem] truncate font-medium">{team.name}</span>
              {#if featured}
                <!-- The accent border is the visual cue; this is the
                     same cue for a screen reader, which cannot see a
                     border. Not a visible badge: the strip is dense and
                     a repeated word next to one chip is noise. -->
                <span class="sr-only">({t('teams.featured')})</span>
              {/if}
            </button>
          {/each}
        </div>

        <!-- Edge chevrons (#1113's addition — #1097 confirmed the rail
             never had any). Absolutely positioned OVER the scroller's
             ends so they cost the strip no width on a wide viewport.
             `aria-disabled` + `tabindex=-1` rather than `disabled`: a
             button that leaves the tab order as you scroll is a control
             that MOVES under a keyboard reader, which is worse than one
             that is present and inert.

             GONE BELOW `sm`, and this is where the teams rail parts
             company with the featured strip rather than copying it.
             Overlaying a 40px control on a 425px card costs a tenth of
             it; overlaying two on a 280px strip of ~150px chips buries
             most of both visible chips — measured at 390px, where the
             left chevron sat on the featured team's avatar and the
             right one over half the next chip's name. Nothing is lost
             with them gone: touch swipe crosses the strip natively, the
             drag pan is mouse-only anyway, and the keyboard path is a
             desktop path. -->
        <button
          type="button"
          onclick={rail.prev}
          aria-disabled={rail.atStart}
          tabindex={rail.atStart ? -1 : 0}
          aria-label={t('teams.rail_prev')}
          title={t('teams.rail_prev')}
          data-testid="teams-rail-prev"
          class="{RAIL_ARROW_CLASS} left-0 max-sm:hidden {rail.atStart
            ? RAIL_ARROW_DISABLED_CLASS
            : RAIL_ARROW_LIVE_CLASS}"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m15 18-6-6 6-6" /></svg>
        </button>
        <button
          type="button"
          onclick={rail.next}
          aria-disabled={rail.atEnd}
          tabindex={rail.atEnd ? -1 : 0}
          aria-label={t('teams.rail_next')}
          title={t('teams.rail_next')}
          data-testid="teams-rail-next"
          class="{RAIL_ARROW_CLASS} right-0 max-sm:hidden {rail.atEnd
            ? RAIL_ARROW_DISABLED_CLASS
            : RAIL_ARROW_LIVE_CLASS}"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 18 6-6-6-6" /></svg>
        </button>
      </div>
    </div>
  </section>
{/if}
