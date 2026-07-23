<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Per-card multi-select checkbox (#515 slice 3). Renders in CardThumb's
  // children slot, top-left (the tool row owns top-right). The selected
  // set lives in the shared `selection` singleton — this is only the
  // affordance; toggling here persists across pagination + navigation.
  //
  // Same click-escape as the tool row (stopPropagation + preventDefault)
  // so ticking a card never fires the stretched-link navigation.
  //
  // Visible on hover / focus (fine pointers), and pinned visible on EVERY
  // card once a selection is active or this card is selected — the
  // standard multi-select "selection mode" affordance — plus always on
  // touch (`@media (hover: none)`).
  //
  // Gated like the tool row's write actions (auth.user && !site.demoMode):
  // every consumer of selection today (#39 bulk ops) is write/ownership
  // gated, and the demo blocks writes at the nginx edge, so a live
  // checkbox there would only lead to a dead end. (If #39 later adds a
  // READ-ONLY bulk action — CSV export / contact sheet — this gate is the
  // thing to revisit; flagged in the PR rather than pre-relaxed.)

  import { auth } from '$stores/auth.svelte';
  import { site } from '$stores/site.svelte';
  import { selection } from '$stores/selection.svelte';
  import { t } from '$stores/lang.svelte';

  interface Props {
    /** The id this card contributes to the selection (post id on browse,
     *  asset id in collections / profile assets). */
    id: string;
  }

  let { id }: Props = $props();

  const canSelect = $derived(!!auth.user && !site.demoMode);
  const selected = $derived(selection.has(id));
  // Pinned visible while selecting; otherwise hover/focus/touch-revealed.
  const pinned = $derived(selected || selection.active);

  function toggle(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    selection.toggle(id);
  }
</script>

{#if canSelect}
  <button
    type="button"
    role="checkbox"
    aria-checked={selected}
    aria-label={selected ? t('card.select.deselect') : t('card.select.label')}
    onclick={toggle}
    class="pointer-events-auto absolute left-2 top-2 z-10 inline-flex h-11 w-11 items-center justify-center
           transition-opacity duration-150 focus-visible:outline-none
           {pinned
             ? 'opacity-100'
             : 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100 [@media(hover:none)]:opacity-100'}"
  >
    <span
      class="flex h-6 w-6 items-center justify-center rounded-md border-2 shadow-sm backdrop-blur-sm transition-colors
             {selected
               ? 'border-accent bg-accent text-on-accent'
               : 'border-white/90 bg-black/40 text-transparent'}"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
        <polyline points="20 6 9 17 4 12" />
      </svg>
    </span>
  </button>
{/if}
