<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  /**
   * The teams directory (#684) — the front door to the studios on this
   * instance.
   *
   * Before this page the eleven seeded studios were reachable only
   * through /admin/teams or by guessing a tag: the teams API has shipped
   * for months (listTeams / getTeam / listTeamMembers / getMyTeams) with
   * no non-admin surface pointed at it at all.
   *
   * # Signed-in only, deliberately
   *
   * `teams.read` is granted to Base and anonymous holds nothing, so a
   * guest gets a 403 here. Rather than render that as a failure, the
   * page shows a sign-in prompt: nothing is broken, the surface is
   * members-only.
   *
   * Whether the DIRECTORY should be public is a real product question
   * and it is deferred, not overlooked — it is separable from whether
   * the CONTENT is public, which the visibility planes already govern
   * per item. Opening the list of studio names is a different decision
   * from opening their work, and it deserves its own.
   *
   * # Cards with a picture, not a grid of covers
   *
   * #982 gave a team a hero picture, so each card now leads with one —
   * but the card stayed a card. A full tile grid would still be mostly
   * grey squares, because a hero is optional and most teams will not set
   * one for a while; and the card carries what a tile cannot — the
   * description, the member/content counts listTeams computes, and the
   * follow button, so the directory is somewhere you can act rather than
   * only look. The picture is an avatar beside the name, which degrades
   * to the initials tile without the layout moving.
   */
  import { onMount } from 'svelte';
  import { api } from '$api/client';
  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { teamFollows } from '$stores/teamFollows.svelte';
  import { t } from '$stores/lang.svelte';
  import TeamFollowButton from '$components/TeamFollowButton.svelte';
  import TeamAvatar from '$components/TeamAvatar.svelte';

  interface Team {
    id: string;
    slug: string;
    name: string;
    description: string;
    member_count?: number;
    content_count?: number;
    /** #982 — the server's re-derived render answer; absent means the
     *  card falls back to the initials tile. */
    hero_asset_id?: string | null;
  }

  const PAGE = 100;

  let items = $state<Team[]>([]);
  let nextCursor = $state<string | null>(null);
  let loading = $state(false);
  let initialLoaded = $state(false);
  let error = $state<string | null>(null);
  /** A signed-out visitor on a members-only surface. An expected state,
   *  kept distinct from `error` for the same reason browse does it. */
  let guest = $state(false);

  async function fetchPage(cursor: string | null): Promise<void> {
    loading = true;
    error = null;
    try {
      const query: Record<string, string | number> = { limit: PAGE };
      if (cursor) query.cursor = cursor;
      const { data, error: apiErr } = await api.GET('/teams', {
        params: { query: query as never },
      });
      if (apiErr || !data) {
        if (!auth.user) {
          guest = true;
          return;
        }
        throw new Error(
          (apiErr as { error?: string } | undefined)?.error ?? t('common.failed_to_load'),
        );
      }
      const page = (data.items ?? []) as Team[];
      items = cursor ? [...items, ...page] : page;
      nextCursor = (data.next_cursor as string | null) ?? null;
    } catch (e) {
      error = e instanceof Error ? e.message : t('common.failed_to_load');
    } finally {
      loading = false;
      initialLoaded = true;
    }
  }

  onMount(() => {
    void fetchPage(null);
    // The follow buttons on these cards read their state from the
    // teamFollows store, so it has to be warm before they render — a
    // directory that shows "Follow" on a team you already follow is
    // worse than one that shows nothing.
    if (auth.user) void teamFollows.load();
  });

  const showEmpty = $derived(initialLoaded && items.length === 0 && !error && !guest);

  // Pluralisation is a ternary per stat, with all four keys written out
  // as LITERALS at the call site — see the markup below.
  //
  // The obvious tidier version is a count(key, n) helper that appends a
  // singular suffix to an interpolated key, and it is wrong here. The
  // i18n coverage sweep (i18n-coverage.test.ts) resolves every key
  // against en.json by reading the literal first argument out of the
  // SOURCE TEXT. An interpolated key reaches it uninterpolated and
  // matches nothing; the helper's other branch passes a bare variable,
  // which is not a quoted literal at all and so becomes INVISIBLE to
  // the sweep.
  //
  // That second half is the worse one: such a helper does not merely
  // fail the check, it silently exempts its keys from ever being
  // checked. Keeping the keys literal is what keeps them covered.
  //
  // The sweep reads raw text, so this note deliberately DESCRIBES the
  // broken shape instead of quoting it — the first draft of this
  // comment spelled the interpolation out and failed the test on its
  // own explanation.
  //
  // `t()` has no plural machinery and this does not invent one — a CLDR
  // plural layer is a catalogue-wide decision, not something to bolt on
  // for two strings. But "1 works" on an otherwise carefully worded card
  // is the sort of thing every reader notices and nobody files.
</script>

<svelte:head>
  <title>{t('teams.directory_title')} — {site.name}</title>
</svelte:head>

<div class="w-full px-4 py-8 sm:px-6">
  <header class="mb-6">
    <h1 class="text-2xl font-bold text-fg">{t('teams.directory_title')}</h1>
    <p class="mt-1 max-w-2xl text-sm text-fg-muted">{t('teams.directory_blurb')}</p>
  </header>

  {#if guest}
    <div class="rounded-xl border border-dashed border-border p-12 text-center" data-testid="teams-guest">
      <p class="text-base font-medium text-fg">{t('teams.guest_title')}</p>
      <p class="mx-auto mt-1 max-w-md text-sm text-fg-muted">{t('teams.guest_hint')}</p>
      <a
        href="/login"
        class="mt-4 inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-on-accent"
      >
        {t('user_menu.sign_in')}
      </a>
    </div>
  {:else if error}
    <div role="alert" class="rounded-md border border-danger/40 bg-danger-container px-4 py-3 text-sm text-danger">
      {error}
    </div>
  {:else if showEmpty}
    <div class="rounded-xl border border-dashed border-border p-12 text-center text-fg-muted">
      <p class="font-medium text-fg">{t('teams.directory_empty')}</p>
      <p class="mt-1 text-sm">{t('teams.directory_empty_hint')}</p>
    </div>
  {:else}
    <ul
      class="grid gap-3 [grid-template-columns:repeat(auto-fill,minmax(20rem,1fr))]"
      data-testid="teams-directory"
    >
      {#each items as team (team.id)}
        <li
          class="flex flex-col gap-3 rounded-xl border border-border bg-surface-elevated p-4
                 transition-colors hover:border-border-strong"
        >
          <div class="flex items-start gap-3">
            <a href={`/teams/${team.id}`} class="flex min-w-0 flex-1 items-start gap-3">
              <TeamAvatar {team} class="h-10 w-10 rounded-lg" textClass="text-xs" />
              <span class="min-w-0 flex-1">
                <h2 class="truncate text-base font-semibold text-fg hover:underline">{team.name}</h2>
                <span class="block truncate text-xs text-fg-muted">@{team.slug}</span>
              </span>
            </a>
            <!-- Follow from the directory: the rail is the destination,
                 and making the user open each team first to subscribe
                 would be friction with no purpose. -->
            <TeamFollowButton {team} compact />
          </div>

          {#if team.description}
            <p class="line-clamp-2 text-sm text-fg-muted">{team.description}</p>
          {/if}

          <!-- The recent-content hint. Both numbers come from listTeams
               in one batched query; absent means the server did not send
               them rather than zero, so they are branched on separately. -->
          <p class="mt-auto flex flex-wrap gap-x-3 text-xs text-fg-muted">
            {#if team.member_count != null}
              <span>
                {team.member_count === 1
                  ? t('teams.member_count_one')
                  : t('teams.member_count', { count: team.member_count })}
              </span>
            {/if}
            {#if team.content_count != null}
              <span>
                {team.content_count === 1
                  ? t('teams.content_count_one')
                  : t('teams.content_count', { count: team.content_count })}
              </span>
            {/if}
          </p>
        </li>
      {/each}
    </ul>

    {#if nextCursor}
      <div class="mt-6 text-center">
        <button
          type="button"
          class="rounded-md border border-border px-4 py-2 text-sm font-medium text-fg hover:border-border-strong disabled:opacity-60"
          onclick={() => void fetchPage(nextCursor)}
          disabled={loading}
        >
          {loading ? t('common.loading') : t('teams.load_more')}
        </button>
      </div>
    {/if}
  {/if}
</div>
