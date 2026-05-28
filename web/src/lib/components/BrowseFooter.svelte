<script lang="ts">
  // Floating footer for the browse feed: view switcher + back-to-top.
  //
  // Layout
  //   collapsed →  [⊞ view] [↑ top]
  //   expanded  →  [⊠ option]
  //                [⊠ option]
  //                [⊠ option]
  //                [−] [⊞ active] [＋]   [↑ top]
  //
  // The view switcher cycles between four modes — grid / masonry /
  // thumbnail / list — and the size +/- knobs adjust the number of
  // columns within the current mode. Picks persist to localStorage so
  // the next visit honours the user's layout. Backed by `BrowseView`
  // helpers in $lib/stores/browseView.svelte so the +page.svelte body
  // reads the resolved column count without owning the math.
  //
  // Back-to-top scrolls the closest <main> ancestor (the global
  // layout's overflow container) to the top. Page-level <html>/<body>
  // scrolling doesn't apply here — the app-shell locks the viewport
  // and only main scrolls.

  import { browseView, type ViewMode } from '$stores/browseView.svelte';
  import { t } from '$stores/lang.svelte';

  // ── View catalogue. Order chosen so the icons cluster naturally:
  //    grid (square grid), masonry (offset columns), thumbnail (dense
  //    grid), list (rows). Keeping `grid` first means the default
  //    active button visually anchors centre when expanded.
  const VIEWS: Array<{ id: ViewMode; labelKey: string; icon: string }> = [
    { id: 'grid',      labelKey: 'browse.view.grid',      icon: 'grid' },
    { id: 'masonry',   labelKey: 'browse.view.masonry',   icon: 'masonry' },
    { id: 'thumbnail', labelKey: 'browse.view.thumbnail', icon: 'thumbnail' },
    { id: 'list',      labelKey: 'browse.view.list',      icon: 'list' },
  ];

  let expanded = $state(false);

  const activeView = $derived(VIEWS.find((v) => v.id === browseView.mode) ?? VIEWS[0]);
  const otherViews = $derived(VIEWS.filter((v) => v.id !== browseView.mode));

  function pick(mode: ViewMode) {
    browseView.setMode(mode);
    expanded = false;
  }

  function dec() {
    browseView.decSize();
  }
  function inc() {
    browseView.incSize();
  }

  function toggle() {
    expanded = !expanded;
  }

  function backToTop() {
    // The app-shell's <main> is the scroll context on browse.
    const main = document.querySelector('main');
    main?.scrollTo({ top: 0, behavior: 'smooth' });
  }

  // Close the expanded cluster on Escape so keyboard users can dismiss
  // without picking. Click-outside is handled by the floating wrapper.
  function onWindowKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && expanded) {
      expanded = false;
    }
  }

  $effect(() => {
    if (!expanded) return;
    window.addEventListener('keydown', onWindowKey);
    return () => window.removeEventListener('keydown', onWindowKey);
  });
</script>

<!--
  The floating cluster sits inside the browse page so it doesn't bleed
  into other routes. `pointer-events-none` on the outer wrapper lets
  clicks pass through to the cards underneath; each button re-enables
  pointer events locally.
-->
<div
  class="pointer-events-none fixed bottom-4 left-4 z-20 flex items-end gap-3"
  aria-label={t('browse.footer.label')}
>
  <!-- View switcher -->
  <div class="pointer-events-auto flex flex-col items-center gap-1.5">
    {#if expanded}
      <!-- Top row: inactive view options laid out horizontally so the
           active button below acts as a centred anchor. -->
      <div class="flex items-center gap-1.5">
      {#each otherViews as v (v.id)}
        <button
          type="button"
          onclick={() => pick(v.id)}
          title={t(v.labelKey)}
          aria-label={t(v.labelKey)}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-state-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {#if v.icon === 'grid'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="7" height="7" rx="1" />
              <rect x="14" y="3" width="7" height="7" rx="1" />
              <rect x="14" y="14" width="7" height="7" rx="1" />
              <rect x="3" y="14" width="7" height="7" rx="1" />
            </svg>
          {:else if v.icon === 'masonry'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3" y="3" width="7" height="10" rx="1" />
              <rect x="14" y="3" width="7" height="6" rx="1" />
              <rect x="3" y="16" width="7" height="5" rx="1" />
              <rect x="14" y="12" width="7" height="9" rx="1" />
            </svg>
          {:else if v.icon === 'thumbnail'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <rect x="3"  y="3"  width="4" height="4" rx="0.5" />
              <rect x="10" y="3"  width="4" height="4" rx="0.5" />
              <rect x="17" y="3"  width="4" height="4" rx="0.5" />
              <rect x="3"  y="10" width="4" height="4" rx="0.5" />
              <rect x="10" y="10" width="4" height="4" rx="0.5" />
              <rect x="17" y="10" width="4" height="4" rx="0.5" />
              <rect x="3"  y="17" width="4" height="4" rx="0.5" />
              <rect x="10" y="17" width="4" height="4" rx="0.5" />
              <rect x="17" y="17" width="4" height="4" rx="0.5" />
            </svg>
          {:else if v.icon === 'list'}
            <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
              <line x1="8" y1="6"  x2="21" y2="6" />
              <line x1="8" y1="12" x2="21" y2="12" />
              <line x1="8" y1="18" x2="21" y2="18" />
              <line x1="3" y1="6"  x2="3.01" y2="6" />
              <line x1="3" y1="12" x2="3.01" y2="12" />
              <line x1="3" y1="18" x2="3.01" y2="18" />
            </svg>
          {/if}
        </button>
      {/each}
      </div>
    {/if}

    <!-- Active-view row: [-] [active] [+] when expanded, just [active] collapsed -->
    <div class="flex items-center gap-1.5">
      {#if expanded}
        <button
          type="button"
          onclick={dec}
          disabled={!browseView.canDec}
          title={t('browse.footer.dec_size')}
          aria-label={t('browse.footer.dec_size')}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-state-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-40"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
        </button>
      {/if}

      <button
        type="button"
        onclick={toggle}
        title={t('browse.footer.toggle')}
        aria-label={t('browse.footer.toggle')}
        aria-expanded={expanded}
        class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-accent text-on-accent shadow-lg transition-colors hover:bg-accent/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {#if activeView.icon === 'grid'}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3" y="3" width="7" height="7" rx="1" />
            <rect x="14" y="3" width="7" height="7" rx="1" />
            <rect x="14" y="14" width="7" height="7" rx="1" />
            <rect x="3" y="14" width="7" height="7" rx="1" />
          </svg>
        {:else if activeView.icon === 'masonry'}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3" y="3" width="7" height="10" rx="1" />
            <rect x="14" y="3" width="7" height="6" rx="1" />
            <rect x="3" y="16" width="7" height="5" rx="1" />
            <rect x="14" y="12" width="7" height="9" rx="1" />
          </svg>
        {:else if activeView.icon === 'thumbnail'}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <rect x="3"  y="3"  width="4" height="4" rx="0.5" />
            <rect x="10" y="3"  width="4" height="4" rx="0.5" />
            <rect x="17" y="3"  width="4" height="4" rx="0.5" />
            <rect x="3"  y="10" width="4" height="4" rx="0.5" />
            <rect x="10" y="10" width="4" height="4" rx="0.5" />
            <rect x="17" y="10" width="4" height="4" rx="0.5" />
            <rect x="3"  y="17" width="4" height="4" rx="0.5" />
            <rect x="10" y="17" width="4" height="4" rx="0.5" />
            <rect x="17" y="17" width="4" height="4" rx="0.5" />
          </svg>
        {:else}
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <line x1="8" y1="6"  x2="21" y2="6" />
            <line x1="8" y1="12" x2="21" y2="12" />
            <line x1="8" y1="18" x2="21" y2="18" />
            <line x1="3" y1="6"  x2="3.01" y2="6" />
            <line x1="3" y1="12" x2="3.01" y2="12" />
            <line x1="3" y1="18" x2="3.01" y2="18" />
          </svg>
        {/if}
      </button>

      {#if expanded}
        <button
          type="button"
          onclick={inc}
          disabled={!browseView.canInc}
          title={t('browse.footer.inc_size')}
          aria-label={t('browse.footer.inc_size')}
          class="inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-state-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-40"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
            <line x1="12" y1="5" x2="12" y2="19" />
            <line x1="5" y1="12" x2="19" y2="12" />
          </svg>
        </button>
      {/if}
    </div>
  </div>

  <!-- Back-to-top -->
  <button
    type="button"
    onclick={backToTop}
    title={t('browse.footer.back_to_top')}
    aria-label={t('browse.footer.back_to_top')}
    class="pointer-events-auto inline-flex h-11 w-11 items-center justify-center rounded-full border border-border bg-surface-elevated text-fg shadow-lg transition-colors hover:bg-state-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
  >
    <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <line x1="12" y1="19" x2="12" y2="5" />
      <polyline points="5 12 12 5 19 12" />
    </svg>
  </button>
</div>
