<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Accessible dropdown menu primitive.
  //
  // Usage:
  //   <Menu>
  //     <svelte:fragment slot="trigger" let:open>
  //       <button ...>{open ? '▾' : '▸'}</button>
  //     </svelte:fragment>
  //     <a href="..." role="menuitem">Item</a>
  //   </Menu>
  //
  // Behaviour:
  //   * Trigger child renders inline; on click it opens/closes.
  //   * Default-slot content renders inside the popup div with
  //     role="menu". Caller is responsible for marking each item
  //     with role="menuitem".
  //   * Closes on Escape, on outside click, and on activating any
  //     <a> / <button> inside (via the click bubbling up).
  //   * Focus returns to the trigger on close.
  //   * Arrow-key navigation moves focus between items.

  import { onDestroy } from 'svelte';

  interface Props {
    /** Trigger content. Receives `open` so the trigger can render
        a state-aware icon. */
    trigger: import('svelte').Snippet<[{ open: boolean }]>;
    /** Menu content. */
    children: import('svelte').Snippet;
    /** Where to anchor the popup, relative to the trigger. */
    align?: 'left' | 'right';
    /** Extra class to apply to the popup container. */
    panelClass?: string;
    /** `data-testid` for the trigger button — pin one when the
        dropdown is meant to be a stable target for Playwright /
        UI dogfood. Optional; menus without it still work, they're
        just not reachable by testid. See helpers/testids.ts. */
    triggerTestId?: string;
    /** `data-testid` for the dropdown panel (rendered when open). */
    panelTestId?: string;
    /** Class for the wrapping trigger BUTTON.
     *
     *  ⚠️ The default `contents` makes this menu unreachable by
     *  keyboard, and that is a real defect, not a style choice.
     *  Measured in Chromium on the browse page (#1097): every
     *  `button[aria-haspopup="menu"]` carrying `class="contents"` —
     *  six of them on `/` alone — reports `getClientRects().length ===
     *  0`, refuses `.focus()`, and is skipped by Tab. An element with
     *  `display: contents` generates no box, and browsers stopped
     *  making an exception for form controls, so the button is styling
     *  scaffolding that no longer exists at layout time. The visible
     *  chip inside it is a `<span>`, which is not focusable either, so
     *  there is nothing left to land on.
     *
     *  Pass a value that GENERATES A BOX — `inline-flex` is the usual
     *  one — and the trigger becomes focusable and tabbable again with
     *  no other change. The default is left alone here deliberately:
     *  flipping it would re-layout every existing menu in the app in
     *  one unrelated commit. Repairing the rest of them is its own
     *  change, and this prop is what it will use.
     *
     *  New callers should pass one. */
    triggerClass?: string;
  }

  let {
    trigger,
    children,
    align = 'right',
    panelClass = '',
    triggerTestId,
    panelTestId,
    triggerClass = 'contents',
  }: Props = $props();

  let open = $state(false);
  let triggerEl: HTMLElement | undefined = $state();
  let panelEl: HTMLDivElement | undefined = $state();

  function toggle(e: MouseEvent) {
    if (e.currentTarget instanceof HTMLElement) {
      triggerEl = e.currentTarget;
    }
    open = !open;
  }

  function close() {
    if (!open) return;
    open = false;
    triggerEl?.focus();
  }

  function handleKeydown(e: KeyboardEvent) {
    if (!open) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (!panelEl) return;
    const items = Array.from(
      panelEl.querySelectorAll<HTMLElement>('[role="menuitem"], a, button'),
    ).filter((el) => !el.hasAttribute('disabled'));
    if (items.length === 0) return;
    const idx = items.indexOf(document.activeElement as HTMLElement);
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      items[(idx + 1 + items.length) % items.length]?.focus();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      items[(idx - 1 + items.length) % items.length]?.focus();
    } else if (e.key === 'Home') {
      e.preventDefault();
      items[0]?.focus();
    } else if (e.key === 'End') {
      e.preventDefault();
      items[items.length - 1]?.focus();
    }
  }

  function handleDocClick(e: MouseEvent) {
    if (!open) return;
    const t = e.target as Node;
    if (panelEl?.contains(t) || triggerEl?.contains(t)) return;
    close();
  }

  function handlePanelClick(e: MouseEvent) {
    // Auto-close when the user picks an item.
    const t = e.target as HTMLElement;
    if (t.closest('a, button[role="menuitem"], [role="menuitem"]')) {
      // Allow the click to navigate first, then close on the next tick.
      queueMicrotask(close);
    }
  }

  $effect(() => {
    if (open) {
      document.addEventListener('click', handleDocClick, true);
      window.addEventListener('keydown', handleKeydown);
      // Move focus into the first menu item once the panel renders.
      queueMicrotask(() => {
        const first = panelEl?.querySelector<HTMLElement>('a, button, [role="menuitem"]');
        first?.focus();
      });
      return () => {
        document.removeEventListener('click', handleDocClick, true);
        window.removeEventListener('keydown', handleKeydown);
      };
    }
  });

  onDestroy(() => {
    document.removeEventListener('click', handleDocClick, true);
    window.removeEventListener('keydown', handleKeydown);
  });
</script>

<div class="relative inline-block">
  <button
    type="button"
    onclick={toggle}
    aria-haspopup="menu"
    aria-expanded={open}
    data-testid={triggerTestId}
    class={triggerClass}
  >
    {@render trigger({ open })}
  </button>

  {#if open}
    <div
      bind:this={panelEl}
      role="menu"
      onclick={handlePanelClick}
      data-testid={panelTestId}
      class={`absolute z-40 mt-1 min-w-[12rem] rounded-md border border-border bg-surface py-1 shadow-lg focus:outline-none ${align === 'right' ? 'right-0' : 'left-0'} ${panelClass}`}
    >
      {@render children()}
    </div>
  {/if}
</div>
