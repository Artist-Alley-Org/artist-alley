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
   * # The featured slot and the "All teams" chip (#1084, #1097)
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
   * worse affordance than the row it replaced, not a better one.
   *
   * #1097 moves it from the tail to the HEAD. Pinning already solved
   * "never scrolls away"; what it did not solve is that the exit from a
   * strip you are reading left-to-right sat at the end of it, so the
   * reader met forty studios before the one control that says "there
   * are more of these". At the head it is the first thing in the row
   * and the destination is legible before the browsing starts. The
   * featured team still keeps first position AMONG TEAMS — the
   * furniture leads, the curation leads the content.
   *
   * So the strip is a flex row of [⋯][All teams][scrolling list]: two
   * fixed controls that never scroll, then everything that does.
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
   *
   * # The manage menu (#1097)
   *
   * A `⋯` at the far left, opening the rail's own options. It is NOT
   * capability-gated: everything behind it is about the CALLER'S
   * follows, which is their own list and needs no permission on the
   * teams themselves. A menu that appears only for admins would be
   * telling ordinary readers that managing what they follow is an
   * administrative act.
   *
   * It is the shared `Menu` primitive — the same one ColumnPicker uses
   * one surface over — rather than a hand-rolled panel, and that choice
   * is load-bearing twice over.
   *
   * First, dismissal. `Menu` light-dismisses on CLICK, not on
   * pointerdown. ViewControls' #1096 panel uses pointerdown and #1105
   * is the bill for it: the panel's collapse re-flows its own footer
   * cluster between pointerdown and pointerup, the sibling button slides
   * out from under the cursor, and the click the reader aimed never
   * lands. This rail has exactly that shape — the menu's neighbours are
   * layout siblings in the same flex row — so the dismissal has to
   * survive the reflow it causes. On `click` it does: the aimed control
   * performs its action and THEN the menu closes.
   *
   * Second, keyboard. Escape closes and returns focus to the trigger,
   * arrows walk the items, and that behaviour lives in one component
   * instead of being re-derived per menu.
   *
   * The items NAVIGATE; none of them mutates anything from here. A
   * reorder/hide surface does not exist yet and inventing one inside a
   * navigation strip is how a strip becomes an application. Both
   * entries land on /teams today because the directory is where follows
   * are managed; they are separate items because they are separate
   * intents, and the day a dedicated follow-management surface exists,
   * one of them repoints without the menu changing shape.
   */
  import { onMount } from 'svelte';
  import { teamFollows } from '$stores/teamFollows.svelte';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import Menu from '$components/Menu.svelte';
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
    <!-- ONE row: the two fixed controls and the scroller are siblings,
         not stacked. `min-w-0` on the scroller is load-bearing — a flex
         child defaults to min-width:auto and would refuse to shrink
         below its content, pushing the fixed controls off screen for
         anyone who follows more than a handful of teams. -->
    <div class="flex items-center gap-2">
      <!-- The manage menu, far left (#1097). Sized to the chips beside
           it so the row reads as one strip rather than a button with a
           list next to it; `h-12 w-12` is also comfortably past the
           44px tap target the chips are held to. -->
      <div class="shrink-0">
        <!-- `triggerClass` is not decoration: Menu's default wraps the
             trigger in a `display: contents` button, which generates no
             box, cannot take focus and is skipped by Tab — measured, and
             true of every other menu in the app (see Menu's own prop
             docs). `inline-flex` gives the button a box back, which is
             the whole of what makes this menu keyboard-reachable. -->
        <Menu
          align="left"
          triggerClass="inline-flex rounded-full"
          triggerTestId="teams-rail-manage"
          panelTestId="teams-rail-manage-menu"
        >
          {#snippet trigger({ open })}
            <!-- The name is TEXT INSIDE the trigger, not an `aria-label`
                 on this span (#1108 review).

                 Menu wraps the trigger snippet in the button, so the
                 button's accessible name is computed FROM ITS CONTENT —
                 and the only content here is an `aria-hidden` glyph.
                 Labelling the span looks like it fixes that and does not:
                 a bare `<span>` maps to role `generic`, which ARIA 1.2
                 forbids naming, so a conforming engine ignores the
                 attribute and the button is left nameless. Chromium is
                 lenient and does report "Team rail options", which is why
                 this survived review — it is a name that exists in one
                 engine.

                 Visually-hidden text has no such caveat: it is content,
                 name-from-content is what a button does, and it is the
                 shape AdminMenu's identical ⋯ trigger already uses. -->
            <span
              class="flex h-12 w-12 items-center justify-center rounded-full border border-border
                     bg-surface-elevated text-fg-muted transition-colors hover:border-border-strong
                     hover:bg-state-hover hover:text-fg"
              class:border-border-strong={open}
              class:text-fg={open}
            >
              <svg
                xmlns="http://www.w3.org/2000/svg"
                width="20"
                height="20"
                viewBox="0 0 24 24"
                fill="currentColor"
                aria-hidden="true"
              >
                <circle cx="5" cy="12" r="2" />
                <circle cx="12" cy="12" r="2" />
                <circle cx="19" cy="12" r="2" />
              </svg>
              <span class="sr-only">{t('teams.rail_manage')}</span>
            </span>
          {/snippet}

          <a
            href="/teams"
            role="menuitem"
            class="block px-3 py-2 text-sm text-fg hover:bg-state-hover"
            data-testid="teams-rail-manage-all"
          >
            {t('teams.browse_all')}
          </a>
          <a
            href="/teams"
            role="menuitem"
            class="block px-3 py-2 text-sm text-fg hover:bg-state-hover"
            data-testid="teams-rail-manage-follows"
          >
            {t('teams.rail_manage_follows')}
          </a>
        </Menu>
      </div>

      <!-- The directory chip, now at the HEAD of the strip (#1097).
           `shrink-0` so it keeps its size at 390px, where the scroller
           gets whatever is left. The grid glyph is what makes it read
           as "everything" beside a row of individual studios — without
           it, a text-only chip in chip position looks like one more
           team called "All teams".

           Below `sm` the LABEL collapses and the glyph stands alone.
           Measured at 390px with the label showing: the ⋯ and this chip
           took 197 of 390px and left the scroller 146 — barely one team
           chip, so the strip stopped looking like a strip on exactly
           the width where it has the least room to explain itself. The
           label goes `sr-only`, not `hidden`: the accessible name is
           unchanged at every width, only the pixels go away. -->
      <a
        href="/teams"
        class="flex min-h-12 shrink-0 items-center gap-2.5 rounded-full border border-border
               bg-surface-elevated py-1 pl-1 pr-1 text-sm font-medium text-fg transition-colors
               hover:border-border-strong hover:bg-state-hover sm:pr-4"
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
        <span class="sr-only whitespace-nowrap sm:not-sr-only">{t('teams.browse_all')}</span>
      </a>

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
          <!-- Chip size (#1097). The avatar is 40px and the chip clears
               48px tall, so the whole thing is past the 44px minimum tap
               target rather than relying on the row's padding to get
               there — the old 28px avatar in a 34px chip was a
               touch-target failure as much as a legibility one.

               `min-h-12` rather than a fixed height: the name is the
               chip's content and a fixed height would clip a language
               whose glyphs are taller than English's. -->
          <ul class="flex gap-2 overflow-x-auto pb-1">
            {#each teamFollows.featured as team (team.id)}
              <li class="shrink-0">
                <a
                  href={`/teams/${team.id}`}
                  class="flex min-h-12 items-center gap-2.5 rounded-full border border-accent bg-surface-elevated
                         py-1 pl-1 pr-4 text-sm text-fg transition-colors hover:bg-state-hover"
                  title={team.description || team.name}
                  data-testid="teams-rail-featured"
                >
                  <TeamAvatar {team} class="h-10 w-10 rounded-full" textClass="text-sm" />
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
                  class="flex min-h-12 items-center gap-2.5 rounded-full border border-border bg-surface-elevated
                         py-1 pl-1 pr-4 text-sm text-fg transition-colors hover:border-border-strong
                         hover:bg-state-hover"
                  title={team.description || team.name}
                >
                  <TeamAvatar {team} class="h-10 w-10 rounded-full" textClass="text-sm" />
                  <span class="max-w-[12rem] truncate font-medium">{team.name}</span>
                </a>
              </li>
            {/each}
          </ul>
        {/if}
      </div>
    </div>
  </section>
{/if}
