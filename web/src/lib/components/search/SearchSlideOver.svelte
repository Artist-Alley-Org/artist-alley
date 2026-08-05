<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The one overlay shell /search's two secondary surfaces share: the
  // facet browser and the advanced query builder (#850).
  //
  // ONE implementation at every width, deliberately. #901 happened
  // because the facet rail was designed for a desktop layout and then
  // had to survive a phone; a panel that is already an overlay has
  // nothing to retrofit. It is full-width under `sm` and a 24rem drawer
  // above it, which is the same component doing the same thing rather
  // than two behaviours behind a breakpoint.
  //
  // Answering #903's question for this surface while it is being built,
  // per the brief: NOTHING disappears on a phone. The query input, the
  // kind chips and the results are the page at every width; the facet
  // counts and the builder are one tap away at every width.

  import type { Snippet } from 'svelte';
  import { t } from '$stores/lang.svelte';

  interface Props {
    open: boolean;
    title: string;
    onclose: () => void;
    children: Snippet;
    /** Optional footer actions (the facet panel's "clear all"). */
    footer?: Snippet;
  }
  let { open, title, onclose, children, footer }: Props = $props();

  function onkeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') onclose();
  }
</script>

<svelte:window onkeydown={open ? onkeydown : undefined} />

{#if open}
  <!-- Scrim. Its click closes; the panel stops propagation by being a
       sibling target check, the same pattern the save dialogs use. -->
  <div
    class="fixed inset-0 z-40 bg-black/40"
    role="presentation"
    onclick={onclose}
  ></div>

  <!-- A <div>, not an <aside>: `role="dialog"` is interactive and svelte's
       a11y pass rejects it on a landmark element. The dialog role is the
       load-bearing part — it is what makes the panel a modal context for
       assistive tech — so the element gives way, not the role. -->
  <div
    class="fixed inset-y-0 right-0 z-50 flex w-full max-w-full flex-col border-l border-border
           bg-surface shadow-xl sm:w-[24rem]"
    role="dialog"
    aria-modal="true"
    aria-label={title}
    tabindex="-1"
    data-testid="search-slideover"
  >
    <header class="flex items-center justify-between border-b border-border px-4 py-3">
      <h2 class="text-sm font-semibold uppercase tracking-wide text-fg-muted">{title}</h2>
      <button
        type="button"
        onclick={onclose}
        class="inline-flex h-11 w-11 items-center justify-center rounded-md text-fg-muted hover:bg-state-hover hover:text-fg"
        aria-label={t('common.close')}
        data-testid="search-slideover-close"
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <line x1="18" y1="6" x2="6" y2="18" /><line x1="6" y1="6" x2="18" y2="18" />
        </svg>
      </button>
    </header>

    <div class="min-w-0 flex-1 overflow-y-auto px-4 py-4">
      {@render children()}
    </div>

    {#if footer}
      <footer class="border-t border-border px-4 py-3">{@render footer()}</footer>
    {/if}
  </div>
{/if}
