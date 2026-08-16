<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The floating footer's tab set — the HOST-SUPPLIED middle of
  // ViewControls (#1106).
  //
  // # What was already there, and what was not
  //
  // ViewControls has taken an optional `middle` snippet since #511, so
  // the SEAM for host-supplied centre chrome exists and nothing here
  // adds one. What did not exist was a tab CONTROL to put in it:
  // BrowseFooter hand-rolled its Latest/Following segmented control plus
  // a separate below-`sm` menu pill, sixty lines of markup carrying two
  // presentations, an ARIA tablist, the 390px width decision and the
  // #590-amendment-3 hover rule. A profile hosting Portfolio / About /
  // Likes would have had to reproduce all of it, and the two would have
  // diverged on the first refinement to either.
  //
  // So the CONTROL is shared and the TAB SET is the host's — the same
  // division AssetPlaylist makes between its generic shell and a
  // per-source host, applied to the footer. Browse hosts the feed
  // filter; a profile hosts its sections; the next surface hosts
  // whatever it has.
  //
  // # Why two presentations, swapped structurally
  //
  // Inherited from BrowseFooter verbatim, because the reason is still
  // true: the segmented control does not fit beside the view switcher
  // and the sort toggle at 390px, so below `sm` it becomes a single pill
  // that opens a menu upward. Structural (`sm:hidden` / `hidden
  // sm:inline-flex`), not a reflow, because the two are different
  // controls with different affordances — see the mobile-is-a-reduced-app
  // note in the epic.
  //
  // A tab set with more members makes that fit tighter, not looser: the
  // profile's three labels are wider than Latest/Following, which is
  // exactly why the pill branch had to come along rather than being
  // left behind in BrowseFooter.
  //
  // # State lives with the HOST
  //
  // `active` in, `onSelect` out. This component owns no selection: the
  // browse filter belongs in the browseView store (it survives
  // navigation and drives a query param), while a profile's tab is
  // page-local. Owning it here would have forced one of those two to be
  // wrong.

  import { t } from '$stores/lang.svelte';

  export interface FooterTab {
    id: string;
    /** Already-translated label. The host owns its own copy — these are
     *  its sections, and it is the only thing that knows their keys. */
    label: string;
  }

  interface Props {
    tabs: FooterTab[];
    active: string;
    onSelect: (id: string) => void;
    /** Accessible name for the tablist (and the mobile menu). */
    label: string;
  }

  let { tabs, active, onSelect, label }: Props = $props();

  let open = $state(false);

  /** The pill's label below `sm`. Looked up BY ID with a fall back to
   *  the FIRST tab rather than by position, so a host that passes an
   *  `active` no longer in `tabs` shows a real label instead of
   *  rendering blank — the same defensive lookup BrowseFooter's
   *  DEFAULT_FILTER did, generalised. */
  const activeTab = $derived(tabs.find((x) => x.id === active) ?? tabs[0]);

  function onWindowKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && open) open = false;
  }
  $effect(() => {
    if (!open) return;
    window.addEventListener('keydown', onWindowKey);
    return () => window.removeEventListener('keydown', onWindowKey);
  });

  function pick(id: string) {
    onSelect(id);
    open = false;
  }
</script>

<!-- Below sm — one pill opening a menu upward. -->
<div class="pointer-events-auto relative sm:hidden">
  <button
    type="button"
    onclick={() => (open = !open)}
    aria-haspopup="menu"
    aria-expanded={open}
    aria-label={label}
    class="inline-flex h-11 items-center gap-1.5 rounded-full border border-border bg-surface-elevated px-4 text-sm font-medium text-fg shadow-lg transition-colors hover:bg-surface-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
  >
    {activeTab?.label ?? t('common.loading')}
    <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" class="transition-transform" class:rotate-180={open}>
      <polyline points="18 15 12 9 6 15" />
    </svg>
  </button>
  {#if open}
    <div
      role="menu"
      aria-label={label}
      class="absolute bottom-full left-1/2 mb-2 min-w-[9rem] -translate-x-1/2 rounded-xl border border-border bg-surface-elevated p-1 shadow-lg"
    >
      {#each tabs as tab (tab.id)}
        <button
          type="button"
          role="menuitem"
          onclick={() => pick(tab.id)}
          class={`block w-full rounded-lg px-3 py-2 text-left text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${tab.id === active ? 'bg-accent text-on-accent' : 'text-fg hover:bg-state-hover'}`}
        >
          {tab.label}
        </button>
      {/each}
    </div>
  {/if}
</div>

<!-- sm and up — the full segmented control. -->
<div
  class="pointer-events-auto hidden items-center rounded-full border border-border bg-surface-elevated p-1 shadow-lg sm:inline-flex"
  role="tablist"
  aria-label={label}
>
  <!-- Inactive segments get a real BACKGROUND on hover, not just a
       text-colour change (#590 amendment 3): `hover:text-fg` alone moved
       fg-muted -> fg with no fill behind it, so the inactive segment
       felt dead to the pointer. Active keeps the solid accent, so
       selected stays clearly distinct from hovered. -->
  {#each tabs as tab (tab.id)}
    <button
      type="button"
      role="tab"
      aria-selected={tab.id === active}
      onclick={() => onSelect(tab.id)}
      class={`rounded-full px-4 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${tab.id === active ? 'bg-accent text-on-accent' : 'text-fg-muted hover:bg-surface-hover hover:text-fg'}`}
    >
      {tab.label}
    </button>
  {/each}
</div>
