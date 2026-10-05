<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The generic modal dialog wrapper: overlay, backdrop-click close,
  // Escape close, focus-into-panel on open, FOCUS CONTAINMENT while
  // open (#1269) and focus-restore on close.
  //
  // Was CollectionModal.svelte, whose header said it stayed collection-
  // scoped "until a second feature surface needs the exact same shape".
  // #881's request-access dialog is that surface, so it was renamed
  // rather than copied — a fourth bespoke `role="dialog"` would be a
  // fourth place for the focus-restore and Escape handling to drift.
  //
  // Consumers: NewCollectionModal / EditCollectionModal /
  // ShareCollectionModal / RequestAccessDialog / ConfirmDeleteDialog.

  import { onDestroy, onMount } from 'svelte';
  import { t } from '$stores/lang.svelte';
  import { portal } from '$lib/portal';
  import { modalStack, pushModal, popModal } from './modalStack';

  interface Props {
    title: string;
    open: boolean;
    onclose: () => void;
    children: import('svelte').Snippet;
    /** Optional footer (action row). */
    footer?: import('svelte').Snippet;
    /** Optional width override (Tailwind class). */
    panelClass?: string;
  }

  let { title, open, onclose, children, footer, panelClass = 'max-w-lg' }: Props = $props();

  let panel: HTMLDivElement | undefined = $state();
  let previousFocus: HTMLElement | null = null;

  // ── Only the TOP modal answers Escape (#1207) ──────────────────────
  //
  // The Escape handler is on the DOCUMENT, so every open instance hears
  // every press. With one modal on screen that was invisible; #1207's
  // cover editor opens from inside the collection edit modal, and two
  // instances both calling `onclose` means one Escape dismisses the
  // editor AND the form behind it — the curator loses unsaved edits to
  // a keystroke that should have stepped back one level.
  //
  // A module-level stack rather than a z-index comparison or a
  // "topmost" prop: stacking order here is open ORDER, the component
  // already knows when it opens and closes, and a prop would put the
  // answer in the hands of every caller that nests anything. `token` is
  // an object identity so two modals can never collide on a key.
  const token = {};
  function isTopModal() {
    return modalStack.length > 0 && modalStack[modalStack.length - 1] === token;
  }

  // ── FOCUS CONTAINMENT (#1269) ──────────────────────────────────────
  //
  // `aria-modal="true"` (below) is a PROMISE to assistive technology
  // that everything outside this dialog is inert. It was not being kept:
  // measured on the collection edit dialog, 45 Tab presses left the
  // panel 38 times and reached the body, Explore, Collections, Review,
  // the nav search box, Advanced search and Upload. A screen reader may
  // stop exposing the rest of the page on the strength of the attribute
  // while a keyboard user tabs straight into it, and the false half is
  // the one that is written down.
  //
  // ⚠️ THERE IS NO NATIVE INERTNESS UNDERNEATH. This is a portalled
  // `<div role="dialog">`, not a `<dialog>` opened with `showModal()`,
  // so the browser contributes nothing here and never did.

  /** Everything inside the panel a Tab can land on, in document order.
   *
   *  Queried at the MOMENT OF THE KEYPRESS rather than cached: the
   *  panel's contents are reactive — a cover picker's grid, a search
   *  result list, a conditional warning — and a cached list would send
   *  focus to a detached node the first time one of them changed.
   *
   *  The visibility test is the rendered-box one (`offsetWidth`,
   *  `offsetHeight`, `getClientRects`) rather than a computed-style
   *  read, because it has to admit `sr-only` controls — the visibility
   *  radios in the collection dialog are exactly that, and they are
   *  genuinely tabbable — while excluding anything inside a
   *  `display: none` branch. */
  const FOCUSABLE = [
    'a[href]',
    'area[href]',
    'input:not([disabled])',
    'select:not([disabled])',
    'textarea:not([disabled])',
    'button:not([disabled])',
    'iframe',
    'object',
    'embed',
    '[tabindex]:not([tabindex="-1"])',
    '[contenteditable="true"]',
  ].join(',');

  function focusables(): HTMLElement[] {
    if (!panel) return [];
    return Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE)).filter(
      (el) =>
        el.tabIndex >= 0 &&
        !el.hasAttribute('inert') &&
        (el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0),
    );
  }

  /** Keep Tab inside the panel.
   *
   *  ⛔ IT GOES THROUGH THE SAME TWO GUARDS AS ESCAPE, and that is not
   *  symmetry for its own sake — the stacking problem is identical.
   *  `isTopModal()` is what makes the containment follow the TOP dialog
   *  rather than whichever instance registered its document listener
   *  first, and `defaultPrevented` is what stops a second instance
   *  acting on an event the top one has already answered. See the
   *  Escape handler for the observed failure both of them close.
   *
   *  Focus landing OUTSIDE any panel is pulled back in rather than
   *  ignored: a click on the backdrop moves focus to the body, and a
   *  Tab from there would otherwise walk into the chrome the attribute
   *  claims is unavailable. */
  function containTab(e: KeyboardEvent) {
    const items = focusables();
    const active = document.activeElement as HTMLElement | null;
    const inside = !!(active && panel?.contains(active));
    if (items.length === 0) {
      // Nothing to land on. The panel itself is not focusable, so the
      // only honest answer is to refuse the move — letting it through
      // would put focus behind a dialog that says it is modal.
      e.preventDefault();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    if (!inside) {
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
      return;
    }
    if (!e.shiftKey && active === last) {
      e.preventDefault();
      first.focus();
    } else if (e.shiftKey && active === first) {
      e.preventDefault();
      last.focus();
    }
    // Anywhere in the middle: the browser's own Tab is correct, and
    // claiming the event would break arrow-key and type-ahead
    // behaviour that composes with it.
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Tab') {
      if (e.defaultPrevented || !open || !isTopModal()) return;
      containTab(e);
      return;
    }
    if (e.key !== 'Escape') return;
    // ⚠️ TWO GUARDS, AND BOTH ARE NECESSARY. Each covers a case the
    // other cannot, and this was arrived at by instrumenting the real
    // page after the stack alone failed.
    //
    // `defaultPrevented` — ONE Escape closes ONE dialog. The stack guard
    // by itself does not achieve that, because Svelte flushes state
    // synchronously at the end of a DOM event handler: the child's
    // handler closes the child, the child's effect pops it, and the
    // parent's document listener — still dispatching the SAME keypress —
    // then finds itself on top and closes too. Observed exactly that:
    // "Cover pictures top=true stack=2" followed by "Edit collection
    // top=true stack=1", one keypress, both gone. Claiming the event is
    // what makes the stack snapshot irrelevant to the rest of the
    // dispatch.
    //
    // The STACK — the right dialog closes. `defaultPrevented` by itself
    // would hand the event to whichever handler runs first, and that is
    // the bubble path when focus is inside a panel (correct: the child)
    // but document-listener registration order when focus is on the body
    // (wrong: the parent, which mounted first).
    if (e.defaultPrevented || !open || !isTopModal()) return;
    e.preventDefault();
    onclose();
  }

  onMount(() => {
    document.addEventListener('keydown', onKeydown);
  });
  onDestroy(() => {
    document.removeEventListener('keydown', onKeydown);
    popModal(token);
  });

  $effect(() => {
    if (open) {
      pushModal(token);
      previousFocus = document.activeElement as HTMLElement | null;
      queueMicrotask(() => panel?.querySelector<HTMLElement>('input, textarea, button')?.focus());
    } else {
      popModal(token);
      if (previousFocus) {
        previousFocus.focus();
        previousFocus = null;
      }
    }
  });

  // The overlay is moved out of its declared box by `$lib/portal` —
  // see that module for why (containing blocks, and the top layer a
  // maximized viewer occupies). `rehome: false`: a modal belongs to the
  // surface that raised it, so when a host dialog closes this should go
  // with it rather than be re-parented to the body and left orphaned.
  const modalPortal = (node: HTMLElement) => portal(node, { rehome: false });
</script>

{#if open}
  <!-- `aria-modal="true"` IS NOW TRUE (#1269). It was a claim the code
       did not keep until the Tab handler above; the choice was to make
       it honest or to drop it, and dropping it would have left a dialog
       that is modal in every other respect announcing that it is not. -->
  <!-- ⚠️ The overlay's own `onkeydown` is the SAME guarded handler as the
       document listener, not a second copy of the rule.

       It exists at all because the overlay carries a click-to-dismiss
       and a11y wants a keyboard equivalent beside it. But it sits on the
       BUBBLE PATH, and that is what made the first version of the stack
       guard useless: with a nested dialog open and focus still in the
       HOST panel (which happens whenever the child has not taken focus
       yet), Escape bubbled through the host's overlay, the unguarded
       copy here closed the host, and the guarded document listener
       correctly closed only the child — so one press dismissed both,
       which is precisely the failure the stack was added to prevent,
       arriving through the one handler the guard had not reached. -->
  <div
    use:modalPortal
    role="dialog"
    aria-modal="true"
    aria-label={title}
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-4 backdrop-blur-sm"
    onclick={(e) => {
      if (e.target === e.currentTarget) onclose();
    }}
    onkeydown={onKeydown}
    tabindex="-1"
  >
    <div
      bind:this={panel}
      class="w-full {panelClass} rounded-xl border border-border bg-surface-elevated shadow-xl"
    >
      <header class="flex items-center justify-between border-b border-border px-5 py-3">
        <h2 class="text-base font-semibold">{title}</h2>
        <button
          type="button"
          onclick={onclose}
          class="rounded p-1 text-fg-muted hover:bg-surface hover:text-fg"
          aria-label={t('common.close')}
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <line x1="18" y1="6" x2="6" y2="18" />
            <line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
      </header>
      <div class="px-5 py-4">
        {@render children()}
      </div>
      {#if footer}
        <footer class="flex items-center justify-end gap-2 border-t border-border px-5 py-3">
          {@render footer()}
        </footer>
      {/if}
    </div>
  </div>
{/if}
