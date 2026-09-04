<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // An inline tab strip for a METADATA FORM (#1173, #1119, ADR 0099 §9).
  //
  // # Why this is not FooterTabs, which was looked at first
  //
  // FooterTabs.svelte is a real shared tab control and the right instinct
  // is to reuse it. It does not fit here, for three reasons, and the
  // third one is disqualifying rather than merely awkward:
  //
  //  1. It is a FLOATING FOOTER control. `pointer-events-auto`,
  //     `shadow-lg` and fully rounded pills are the language of something
  //     hovering over the page, not of a fieldset heading inside a form.
  //  2. Its menu branch opens UPWARD (`bottom-full`, `absolute`). In a
  //     footer that is the only direction available; inside a scrolling
  //     form it opens over the content the person was just reading.
  //  3. ⛔ BELOW `sm` IT REPLACES THE TABLIST ENTIRELY with a single pill
  //     that opens a `role="menu"`. That is correct for a footer, where
  //     the strip competes with a view switcher and a sort toggle for
  //     390px. It is wrong for a form, where the tabs are the form's
  //     structure: collapsing them into a menu hides how many sections
  //     the record even has, and it makes the whole tab set two taps deep
  //     on precisely the device where scrolling past fields is worst.
  //
  // So this is a SECOND SHARED PRIMITIVE rather than a copy: it is
  // consumed by FieldValuesSection (asset edit and collection edit) and
  // by /create, which is three surfaces on one implementation. The rule
  // it exists to serve is that four routes already hand-roll
  // `role="tablist"` markup, and a fifth and sixth would have been worse
  // than a second component.
  //
  // # What it adds over hand-rolled markup
  //
  //  * ROVING TABINDEX with arrow, Home and End keys, which is what the
  //    tablist pattern actually requires and which none of the
  //    hand-rolled copies implement.
  //  * `aria-controls` pointing at the host's panel, and matching
  //    `aria-labelledby` on the host side, so a screen reader can say
  //    which panel a tab governs.
  //  * `focus-visible` rather than `focus`, so a MOUSE click does not
  //    leave a ring pinned on the tab. That is #1020's rule and it is
  //    easy to get wrong the other way round.
  //  * Horizontal SCROLL rather than wrap or collapse at narrow widths,
  //    so the strip stays a strip and the page body never scrolls
  //    sideways.

  interface Tab {
    id: string;
    /** Already-translated. The host owns its own labels. */
    label: string;
    /**
     * Rendered beside the label, for a count. Optional and purely
     * decorative: it is not announced separately, because "Print 3" reads
     * fine and "Print, 3" does not.
     */
    badge?: string;
  }

  interface Props {
    tabs: Tab[];
    active: string;
    onSelect: (id: string) => void;
    /** Accessible name for the tablist. */
    label: string;
    /** `id` of the panel these tabs govern, for `aria-controls`. */
    panelId: string;
    /** Prefix for each tab's own `id`, so the panel can point back. */
    idPrefix: string;
    testid?: string;
  }

  let { tabs, active, onSelect, label, panelId, idPrefix, testid = 'form-tabs' }: Props = $props();

  let buttons = $state<Record<string, HTMLButtonElement | null>>({});

  function move(delta: number) {
    const i = tabs.findIndex((x) => x.id === active);
    if (i < 0) return;
    // Wraps, which the tablist pattern expects: Right from the last tab
    // lands on the first rather than doing nothing.
    const next = tabs[(i + delta + tabs.length) % tabs.length];
    onSelect(next.id);
    buttons[next.id]?.focus();
  }

  function pick(id: string) {
    onSelect(id);
    buttons[id]?.focus();
  }

  // The handler sits on each TAB rather than on the tablist. Under a
  // roving tabindex the focused element is always a tab, so the strip
  // itself never receives the key event; putting it on the container
  // would also require the container to be focusable, which is precisely
  // what the roving pattern exists to avoid.
  function onKey(e: KeyboardEvent) {
    switch (e.key) {
      case 'ArrowRight':
      case 'ArrowDown':
        e.preventDefault();
        move(1);
        break;
      case 'ArrowLeft':
      case 'ArrowUp':
        e.preventDefault();
        move(-1);
        break;
      case 'Home':
        e.preventDefault();
        if (tabs.length) pick(tabs[0].id);
        break;
      case 'End':
        e.preventDefault();
        if (tabs.length) pick(tabs[tabs.length - 1].id);
        break;
    }
  }
</script>

<!--
  `overflow-x-auto` on the strip and nothing on the page. A form with
  seven tabs at 390px scrolls its own strip; the body must never scroll
  sideways because of one control.
-->
<div
  role="tablist"
  aria-label={label}
  data-testid={testid}
  class="-mx-1 flex gap-1 overflow-x-auto px-1 pb-1"
>
  {#each tabs as tab (tab.id)}
    <button
      bind:this={buttons[tab.id]}
      type="button"
      role="tab"
      id="{idPrefix}-{tab.id}"
      aria-selected={tab.id === active}
      aria-controls={panelId}
      tabindex={tab.id === active ? 0 : -1}
      data-testid="{testid}-tab-{tab.id || 'default'}"
      onclick={() => onSelect(tab.id)}
      onkeydown={onKey}
      class={`shrink-0 whitespace-nowrap rounded-t border-b-2 px-3 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring ${
        tab.id === active
          ? 'border-accent text-fg'
          : 'border-transparent text-fg-muted hover:bg-surface-hover hover:text-fg'
      }`}
    >
      {tab.label}{#if tab.badge}<span class="ml-1.5 text-xs text-fg-muted">{tab.badge}</span>{/if}
    </button>
  {/each}
</div>
