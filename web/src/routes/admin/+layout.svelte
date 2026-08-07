<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // /admin/* layout — sidebar nav + capability gate.
  //
  // Renders no admin surface at all to a caller who cannot open one.
  // The backend enforces every action anyway; this is the UX hide.
  //
  // The gate has three outcomes, and keeping them three is the point
  // (#956):
  //
  //   not ready          → loading
  //   caps unavailable   → "could not determine your rights" + retry
  //   ready, no caps     → "you don't have permission"
  //
  // The middle one used to fall through to the last, so a failed
  // capability lookup on the server accused a real administrator of
  // holding no rights. Both still grant exactly nothing; only the
  // explanation differs, and the explanation is the whole fix.

  import { goto } from '$app/navigation';
  import { onMount } from 'svelte';
  import { page } from '$app/state';
  import { auth } from '$stores/auth.svelte';
  import { t } from '$stores/lang.svelte';
  import AdminIcon from '$components/AdminIcon.svelte';
  import { ADMIN_SECTIONS } from '$lib/admin/sections';

  let { children } = $props();

  onMount(() => {
    if (!auth.user) void goto('/login?next=' + encodeURIComponent(page.url.pathname));
  });

  // #385 — allow the admin shell for any read-cap holder, not just
  // `system.admin`. The sidebar then lists only the sections they can
  // open (a section counts if it has a live tile whose cap they hold).
  // Every page still enforces its own cap server-side; this is the UX
  // gate that stops a read-only role seeing a bare "no permission".
  const canSeeAdmin = $derived(auth.canSeeAdmin);

  // #956 — the gate has THREE answers, not two. `canSeeAdmin` is false
  // both for an account that holds nothing and for a session whose
  // capabilities the server could not resolve, and until this branch
  // existed the layout rendered the same permission refusal for both.
  // That sentence is correct for the first and a lie for the second,
  // and the nightly that hit it burned four triage passes precisely
  // because neither an operator nor a test could tell which had
  // happened.
  //
  // It is checked BEFORE canSeeAdmin so the more specific state wins,
  // and it changes only the explanation: `auth.can()` and
  // `auth.canSeeAdmin` both refuse throughout, so this branch renders
  // no sidebar, no tiles, and no children.
  const capsUnavailable = $derived(auth.capsUnavailable);

  let retrying = $state(false);

  async function retryCapabilities(): Promise<void> {
    if (retrying) return;
    retrying = true;
    try {
      await auth.refresh();
      // A retry that comes back with no session at all is a signed-out
      // session, not a degraded one — send them to sign in rather than
      // leaving them on an admin URL that can never resolve. onMount's
      // identical redirect has already run and will not run again.
      if (!auth.user) {
        await goto('/login?next=' + encodeURIComponent(page.url.pathname));
      }
    } finally {
      retrying = false;
    }
  }
  const visibleSections = $derived(
    ADMIN_SECTIONS.filter((s) => s.tiles.some((t) => auth.canSeeTile(t))),
  );
  const overviewActive = $derived(page.url.pathname === '/admin');
</script>

<!--
  Admin shell: fills <main>'s viewport area exactly (h-full) and
  prevents <main> from scrolling — sidebar + content each own their
  own scroll. The global layout puts the navbar above main and gives
  <main> overflow-y-auto for normal pages; here we override by making
  the admin root exactly the size of main, so main has nothing to
  scroll.

  Panes:
    * aside  — overflow-y-auto so the sidebar list can scroll if it
               ever grows past viewport. Never moves with the right
               pane's scroll.
    * section — overflow-y-auto for the right-hand content. Always
                scrolls independently of the sidebar.
-->
<div class="flex h-full overflow-hidden">
  {#if !auth.ready}
    <div class="flex-1 p-6 text-fg-muted">{t('common.loading')}</div>
  {:else if capsUnavailable}
    <!--
      "We could not work out what you may do" — NOT "you may do
      nothing" (#956). Grants exactly as much as the panel below it
      does (nothing: no sidebar, no tiles, no children), and differs
      only in telling the truth about why, with the one action that can
      actually change the answer.
    -->
    <div class="flex-1 p-6">
      <div
        role="alert"
        class="space-y-3 rounded-lg border border-warning/40 bg-warning-container p-4 text-sm text-on-warning-container"
      >
        <p class="font-medium">{t('admin.caps_unavailable_title')}</p>
        <p>{t('admin.caps_unavailable_body')}</p>
        <button
          type="button"
          class="rounded-md border border-warning/50 px-3 py-1.5 font-medium hover:bg-state-hover disabled:opacity-60"
          disabled={retrying}
          onclick={retryCapabilities}
        >
          {retrying ? t('common.loading') : t('common.retry')}
        </button>
      </div>
    </div>
  {:else if !canSeeAdmin}
    <div class="flex-1 p-6">
      <div class="rounded-lg border border-danger/40 bg-danger-container p-4 text-sm text-on-danger-container">
        {t('common.no_permission')}
      </div>
    </div>
  {:else}
    <!--
      Sidebar reads at 15px (text-[15px]) — between text-sm (14) and
      text-base (16). The text-sm default was too thin to scan
      comfortably in dark mode.

      Background tier: `bg-surface-elevated` (solid, no alpha) so the
      sidebar reads as a distinct surface from the content column. The
      active-item fill uses `bg-accent-container` — a soft accent
      tint that ships with a paired `text-on-accent-container`
      foreground at definition time, so contrast is structurally
      guaranteed in both palettes.

      Inactive items use `text-fg-muted` against the elevated surface
      (≈7.5:1 in dark, ≈6:1 in light — AA body and AAA UI).
    -->
    <aside
      class="hidden h-full w-64 shrink-0 overflow-y-auto border-r border-border bg-surface-elevated px-3 py-4 md:block"
      aria-label={t('admin.title')}
    >
      <nav class="space-y-0.5 text-[15px]">
        <a
          href="/admin"
          class={`block rounded-md px-3 py-2 ${overviewActive ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
        >
          {t('admin_menu.overview')}
        </a>

        {#each visibleSections as section (section.slug)}
          {@const href = `/admin/${section.slug}`}
          {@const active = page.url.pathname === href || page.url.pathname.startsWith(href + '/')}
          <a
            {href}
            class={`flex items-center gap-2.5 rounded-md px-3 py-2 ${active ? 'bg-accent-container text-on-accent-container font-medium' : 'text-fg-muted hover:bg-state-hover hover:text-fg'}`}
          >
            <span class={active ? 'text-on-accent-container' : 'text-fg-muted'}>
              <AdminIcon name={section.iconKey} size={17} />
            </span>
            <span>{t(`admin.sections.${section.slug}.title`)}</span>
          </a>
        {/each}
      </nav>
    </aside>

    <section class="flex flex-1 min-w-0 flex-col overflow-y-auto px-6 py-6">
      {@render children?.()}
    </section>
  {/if}
</div>
