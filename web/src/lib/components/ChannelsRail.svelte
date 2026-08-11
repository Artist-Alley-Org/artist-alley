<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The channels rail (#577) — the teams you follow, above the browse
   * feed.
   *
   * # Why it is a rail and not a sidebar
   *
   * Browse already has one horizontal strip over the feed
   * (FeaturedRail), the page is full-viewport-width by design, and a
   * new left sidebar would be a second navigation system competing
   * with the navbar for the same job. So this is the pattern that is
   * already here: a heading, a horizontally-scrolling row of chips, no
   * new chrome.
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
  import { channels } from '$stores/channels.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';

  onMount(() => {
    if (auth.user) void channels.load();
  });

  // Re-load when the session changes — signing in mid-visit must fill
  // the rail, and signing out must empty it rather than leave the
  // previous user's studios on screen.
  $effect(() => {
    if (auth.user) {
      void channels.load();
    } else {
      channels.reset();
    }
  });

  /** Two initials, the same fallback the profile header uses for a
   *  user with no avatar. Teams carry no image on the wire yet
   *  (out of scope this sprint), so every chip uses it. */
  function initials(name: string): string {
    const parts = name.trim().split(/\s+/).slice(0, 2);
    return parts.map((p) => p.slice(0, 1).toUpperCase()).join('') || '?';
  }
</script>

{#if auth.user && channels.loaded}
  <section aria-labelledby="channels-rail-heading" data-testid="channels-rail">
    <div class="mb-2 flex items-baseline justify-between gap-3">
      <h2 id="channels-rail-heading" class="text-sm font-semibold text-fg">
        {t('channels.rail_title')}
      </h2>
      <a href="/teams" class="shrink-0 text-xs font-medium text-accent hover:underline">
        {t('channels.browse_all')}
      </a>
    </div>

    {#if channels.items.length === 0}
      <!-- Actionable, not decorative: the whole point of the empty
           state is the link to the directory. -->
      <p
        class="rounded-lg border border-dashed border-border px-4 py-3 text-sm text-fg-muted"
        data-testid="channels-rail-empty"
      >
        {t('channels.empty')}
        <a href="/teams" class="font-medium text-accent hover:underline">{t('channels.empty_cta')}</a>
      </p>
    {:else}
      <!-- Horizontal scroll rather than wrap: the rail must stay one
           row tall whether the user follows two studios or forty, or it
           starts pushing the feed off the fold. -->
      <ul class="flex gap-2 overflow-x-auto pb-1">
        {#each channels.items as team (team.id)}
          <li class="shrink-0">
            <a
              href={`/teams/${team.id}`}
              class="flex items-center gap-2 rounded-full border border-border bg-surface-elevated py-1 pl-1 pr-3
                     text-sm text-fg transition-colors hover:border-border-strong hover:bg-state-hover"
              title={team.description || team.name}
            >
              <span
                class="flex h-7 w-7 items-center justify-center rounded-full bg-state-hover text-[0.65rem]
                       font-semibold text-fg-muted"
                aria-hidden="true">{initials(team.name)}</span
              >
              <span class="max-w-[12rem] truncate font-medium">{team.name}</span>
            </a>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}
