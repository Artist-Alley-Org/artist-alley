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
    /** Which top corner to sit in (#1111). Left by default, which is
     *  every card this component has ever rendered on.
     *
     *  Grid post cards pass `right`, and that is a CONSEQUENCE of the
     *  owner's overlay layout rather than a preference: #1111 puts the
     *  asset-kind icons at top-left and moves the ⋯ menu down to the
     *  identity block, so top-left is now taken and top-right is now
     *  free. Both controls are 44px and both are hover-revealed, so
     *  leaving them stacked would have put a checkbox under an icon on
     *  every hover — the same corner collision #578 resolved for the
     *  multi-asset badge by moving it, not by shrinking it. */
    corner?: 'left' | 'right';
    /** The feed-order id list this card sits in, for Shift+click range
     *  selection (#1127). A THUNK rather than an array so the card does
     *  not re-render every time the feed appends a page — it is only
     *  read at the moment of a shift-click.
     *
     *  Absent on surfaces that have not adopted range selection; the
     *  checkbox then falls back to a plain toggle, which is what it has
     *  always done. */
    orderedIds?: () => string[];
    /** Where this checkbox sits (#1136).
     *
     *  `overlay` — the historical placement: absolutely positioned in a
     *  top corner OF THE ARTWORK, hidden at rest, on a dark translucent
     *  chip so it reads against any picture. Right for a discovery wall,
     *  where chrome is an interruption of the art.
     *
     *  `inline` — an ordinary flow element in a chrome band OUTSIDE the
     *  preview, for thumbnail's frame layout. Two consequences follow
     *  from being off the picture and both are the point: it is ALWAYS
     *  VISIBLE (there is no artwork for it to interrupt, and a working
     *  surface should not hide its select affordance), and it drops the
     *  white-on-black chip for the theme's own border colours, because
     *  a chip designed to survive an unknown photograph looks like a
     *  sticker on a solid panel.
     *
     *  `corner` is ignored under `inline`; the band decides the order. */
    placement?: 'overlay' | 'inline';
  }

  let { id, corner = 'left', orderedIds, placement = 'overlay' }: Props = $props();

  const canSelect = $derived(!!auth.user && !site.demoMode);
  const selected = $derived(selection.has(id));
  // Pinned visible while selecting; otherwise hover/focus/touch-revealed.
  const pinned = $derived(selected || selection.active);

  function toggle(e: MouseEvent) {
    e.preventDefault();
    e.stopPropagation();
    // Shift on the CHECKBOX extends too, not just Shift on the card
    // (#1127). A reader who has started selecting is aiming at
    // checkboxes; making the range gesture work only on the artwork
    // would mean the two targets 6px apart do different things.
    if (e.shiftKey && orderedIds) {
      selection.extendTo(id, orderedIds());
      return;
    }
    selection.toggle(id);
    // A plain toggle re-pivots, so the next Shift+click ranges from
    // here. Dropping the anchor when UNchecking would strand the next
    // range on a stale pivot several screens away.
    selection.setAnchor(id);
  }
</script>

{#if canSelect}
  <button
    type="button"
    role="checkbox"
    aria-checked={selected}
    aria-label={selected ? t('card.select.deselect') : t('card.select.label')}
    onclick={toggle}
    class="pointer-events-auto inline-flex h-11 w-11 items-center justify-center
           transition-opacity duration-150 focus-visible:outline-none
           {placement === 'inline'
      ? 'opacity-100'
      : `absolute ${corner === 'right' ? 'right-2' : 'left-2'} top-2 z-10 ` +
        (pinned
          ? 'opacity-100'
          : 'opacity-0 group-hover:opacity-100 focus-visible:opacity-100 [@media(hover:none)]:opacity-100')}"
  >
    <span
      class="flex h-6 w-6 items-center justify-center rounded-md border-2 shadow-sm transition-colors
             {selected
               ? 'border-accent bg-accent text-on-accent'
               : placement === 'inline'
                 ? 'border-border-strong bg-surface text-transparent'
                 : 'border-white/90 bg-black/40 text-transparent backdrop-blur-sm'}"
    >
      <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round">
        <polyline points="20 6 9 17 4 12" />
      </svg>
    </span>
  </button>
{/if}
