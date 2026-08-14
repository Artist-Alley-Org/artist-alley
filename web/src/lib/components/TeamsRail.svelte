<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The teams rail (#577) — the teams you follow, above the browse
   * feed.
   *
   * # Why it is a rail and not a sidebar
   *
   * Browse already has one horizontal strip over the feed
   * (FeaturedRail), the page is full-viewport-width by design, and a
   * new left sidebar would be a second navigation system competing
   * with the navbar for the same job. So this is the pattern that is
   * already here: a horizontally-scrolling row of chips, no new chrome.
   *
   * # The featured slot and the "All teams" chip (#1084)
   *
   * Two pieces of curated furniture, both of which #1030 deliberately
   * left for this change rather than guessing at early.
   *
   * The FEATURED TEAM runs first. It is an operator's placement in
   * `featured_items`, not a follow, so it is kept in its own array —
   * merging it into the follow set would make every follow button in
   * the product claim the caller follows it (see teamFollows.featured).
   * A featured team the reader already follows is drawn once, from the
   * featured slot, because it is the same team and two chips for one
   * studio reads as a bug.
   *
   * The "ALL TEAMS" affordance moved out of its own row and into the
   * strip. It is PINNED beside the scroller rather than being the last
   * `<li>` inside it: as a list item it would sit past forty followed
   * teams, off the right edge, reachable only by scrolling — which is a
   * worse affordance than the row it replaced, not a better one. So the
   * strip is a flex row of [scrolling list][chip], the chip never
   * scrolls away, and the featured team keeps first position.
   *
   * # No visible heading (#1030)
   *
   * A row of team chips is self-evidently a row of teams, and browse
   * stacked three labelled sections before the reader reached a single
   * piece of work. The heading is gone VISUALLY only: the `<section>`
   * carries the same string as `aria-label`, so it is still a named
   * region a screen reader can jump to. Deleting the `<h2>` without
   * that would have left `aria-labelledby` pointing at a dead id and
   * turned a named landmark into an anonymous `<div>` — an
   * accessibility regression dressed as a cleanup.
   *
   * # Signed-in only, and quiet when there is nothing to say
   *
   * A guest holds no `teams.read`, so there is nothing to fetch and
   * nothing to offer — the rail renders nothing at all rather than an
   * empty state advertising a members-only surface.
   *
   * A signed-in user with no follows DOES get an empty state, because
   * for them it is actionable: it is the pointer to the /teams
   * directory. That is the difference between "you cannot have this"
   * and "you have not picked any yet".
   *
   * Nothing renders before the first load resolves. A rail that
   * flashes its empty state and then fills in reads as a bug on every
   * single page load for anyone who follows anything.
   */
  import { onMount } from 'svelte';
  import { teamFollows } from '$stores/teamFollows.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import TeamAvatar from '$components/TeamAvatar.svelte';

  onMount(() => {
    if (auth.user) void teamFollows.load();
  });

  // The rail's render order: the curated slot, then the caller's own
  // follows in the server's name order. A featured team the caller also
  // follows is dropped from the second half rather than the first —
  // the placement is why it is on screen at all, and demoting it to
  // alphabetical position would silently undo the curation for exactly
  // the readers most likely to notice.
  const featuredIds = $derived(new Set(teamFollows.featured.map((c) => c.id)));
  const followed = $derived(teamFollows.items.filter((c) => !featuredIds.has(c.id)));
  const hasTeams = $derived(teamFollows.featured.length > 0 || followed.length > 0);

  // Re-load when the session changes — signing in mid-visit must fill
  // the rail, and signing out must empty it rather than leave the
  // previous user's studios on screen.
  $effect(() => {
    if (auth.user) {
      void teamFollows.load();
    } else {
      teamFollows.reset();
    }
  });

</script>

{#if auth.user && teamFollows.loaded}
  <!-- `aria-label` rather than `aria-labelledby`: the heading it used to
       point at is gone (#1030). The string is the same one, so the
       region's name in the accessibility tree is unchanged — only its
       visual rendering went away. -->
  <section aria-label={t('teams.rail_title')} data-testid="teams-rail">
    <!-- ONE row: the scroller and the directory chip are siblings, not
         stacked. `min-w-0` on the scroller is load-bearing — a flex
         child defaults to min-width:auto and would refuse to shrink
         below its content, pushing the chip off screen for anyone who
         follows more than a handful of teams. -->
    <div class="flex items-center gap-2">
      <div class="min-w-0 flex-1">
        {#if !hasTeams}
          <!-- Actionable, not decorative: the whole point of the empty
               state is the link to the directory. Shown only when there
               is genuinely nothing in the strip — a featured team with
               no follows is still a rail with something in it. -->
          <p
            class="rounded-lg border border-dashed border-border px-4 py-3 text-sm text-fg-muted"
            data-testid="teams-rail-empty"
          >
            {t('teams.empty')}
            <a href="/teams" class="font-medium text-accent hover:underline"
              >{t('teams.empty_cta')}</a
            >
          </p>
        {:else}
          <!-- Horizontal scroll rather than wrap: the rail must stay one
               row tall whether the user follows two studios or forty, or
               it starts pushing the feed off the fold. -->
          <ul class="flex gap-2 overflow-x-auto pb-1">
            {#each teamFollows.featured as team (team.id)}
              <li class="shrink-0">
                <a
                  href={`/teams/${team.id}`}
                  class="flex items-center gap-2 rounded-full border border-accent bg-surface-elevated py-1 pl-1 pr-3
                         text-sm text-fg transition-colors hover:bg-state-hover"
                  title={team.description || team.name}
                  data-testid="teams-rail-featured"
                >
                  <TeamAvatar {team} />
                  <span class="max-w-[12rem] truncate font-medium">{team.name}</span>
                  <!-- The accent border is the visual cue; this is the
                       same cue for a screen reader, which cannot see a
                       border. Not a visible badge: the strip is dense
                       and a repeated word next to one chip is noise. -->
                  <span class="sr-only">({t('teams.featured')})</span>
                </a>
              </li>
            {/each}
            {#each followed as team (team.id)}
              <li class="shrink-0">
                <a
                  href={`/teams/${team.id}`}
                  class="flex items-center gap-2 rounded-full border border-border bg-surface-elevated py-1 pl-1 pr-3
                         text-sm text-fg transition-colors hover:border-border-strong hover:bg-state-hover"
                  title={team.description || team.name}
                >
                  <TeamAvatar {team} />
                  <span class="max-w-[12rem] truncate font-medium">{team.name}</span>
                </a>
              </li>
            {/each}
          </ul>
        {/if}
      </div>

      <!-- The directory chip, pinned at the end of the strip (#1084).
           `shrink-0` keeps it at full width at 390px, where the
           scroller gets whatever is left. -->
      <a
        href="/teams"
        class="shrink-0 self-center rounded-full border border-border px-3 py-1.5 text-xs font-medium
               text-accent transition-colors hover:border-border-strong hover:bg-state-hover"
        data-testid="teams-rail-browse-all"
      >
        {t('teams.browse_all')}
      </a>
    </div>
  </section>
{/if}
