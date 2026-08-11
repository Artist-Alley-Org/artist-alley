<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // The generic modal dialog wrapper: overlay, backdrop-click close,
  // Escape close, focus-into-panel on open and focus-restore on close.
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

  function onKeydown(e: KeyboardEvent) {
    if (!open) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      onclose();
    }
  }

  onMount(() => {
    document.addEventListener('keydown', onKeydown);
  });
  onDestroy(() => {
    document.removeEventListener('keydown', onKeydown);
  });

  $effect(() => {
    if (open) {
      previousFocus = document.activeElement as HTMLElement | null;
      queueMicrotask(() => panel?.querySelector<HTMLElement>('input, textarea, button')?.focus());
    } else if (previousFocus) {
      previousFocus.focus();
      previousFocus = null;
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
  <div
    use:modalPortal
    role="dialog"
    aria-modal="true"
    aria-label={title}
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/55 p-4 backdrop-blur-sm"
    onclick={(e) => {
      if (e.target === e.currentTarget) onclose();
    }}
    onkeydown={(e) => { if (e.key === 'Escape') onclose(); }}
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
