<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 Kenneth Blossom -->
<script lang="ts">
  // Renders the toast queue (#981). One instance, mounted by the root
  // layout beside CardTooltip — the other app-level singleton.
  //
  // Each toast is portalled INDIVIDUALLY rather than the stack being
  // portalled once. The host it belongs in is decided at the moment it
  // is raised, and that is the only moment we can decide it: a toast
  // pushed from the maximized asset viewer belongs inside that dialog
  // (top layer — a body-level node renders under it and cannot be
  // clicked), while the same toast pushed from a route belongs on the
  // body. A single container mounted at layout time would have to pick
  // one answer for the whole session and would be wrong half the time.
  //
  // `scope: 'document'` is load-bearing and was learned the hard way.
  // The default resolves the host from the node's ANCESTORS, and this
  // component is declared in the root layout, which has no dialog above
  // it — so the first cut always landed on the body and the delete
  // toast was invisible under the maximized viewer, correct in the DOM
  // and useless on screen. The host is not an ancestor; it is whatever
  // modal is open when the toast is raised, which only the document can
  // answer.
  //
  // Stacking is by index rather than by flex flow for the same reason:
  // the nodes may not share a parent.

  import { toasts } from '$stores/toasts.svelte';
  import { portal } from '$lib/portal';
  import { t } from '$stores/lang.svelte';

  const toastPortal = (node: HTMLElement) => portal(node, { scope: 'document' });

  // Per-toast expiry timers, keyed by id. Hovering or focusing a toast
  // clears its timer; leaving restarts it. An Undo you cannot reach
  // because the toast expired while you were reading it is the whole
  // failure mode this guards against.
  const timers = new Map<number, ReturnType<typeof setTimeout>>();

  function arm(id: number, ttlMs: number) {
    disarm(id);
    timers.set(
      id,
      setTimeout(() => {
        timers.delete(id);
        toasts.dismiss(id);
      }, ttlMs),
    );
  }

  function disarm(id: number) {
    const h = timers.get(id);
    if (h !== undefined) {
      clearTimeout(h);
      timers.delete(id);
    }
  }

  // A toast element arms its own clock on mount and clears it on
  // destroy. Tying the timer to the DOM node rather than to the push
  // means a toast dismissed by hand never leaves a timer behind to fire
  // against a recycled id.
  function lifecycle(node: HTMLElement, args: { id: number; ttlMs: number }) {
    void node;
    arm(args.id, args.ttlMs);
    return {
      destroy() {
        disarm(args.id);
      },
    };
  }

  async function runAction(id: number, run: () => void | Promise<void>) {
    // Dismiss first: the action may be a round-trip, and a button that
    // stays clickable through it invites a double-undo.
    toasts.dismiss(id);
    await run();
  }
</script>

{#each toasts.items as toast, i (toast.id)}
  <div
    use:toastPortal
    use:lifecycle={{ id: toast.id, ttlMs: toast.ttlMs }}
    role="status"
    aria-live="polite"
    data-testid="toast"
    data-tone={toast.tone}
    class="pointer-events-auto fixed left-1/2 z-[60] w-[min(28rem,calc(100vw-1.5rem))]
           -translate-x-1/2 rounded-lg border px-4 py-3 text-sm shadow-lg
           sm:left-auto sm:right-4 sm:translate-x-0
           {toast.tone === 'error'
      ? 'border-danger/40 bg-danger/10 text-danger'
      : 'border-border bg-surface-elevated text-fg'}"
    style="bottom: {1 + i * 4.5}rem"
    onmouseenter={() => disarm(toast.id)}
    onmouseleave={() => arm(toast.id, toast.ttlMs)}
    onfocusin={() => disarm(toast.id)}
    onfocusout={() => arm(toast.id, toast.ttlMs)}
  >
    <div class="flex items-start gap-3">
      <p class="min-w-0 flex-1">{toast.message}</p>
      <button
        type="button"
        class="-mr-1 -mt-0.5 shrink-0 rounded p-1 text-fg-muted hover:bg-surface hover:text-fg"
        aria-label={t('common.close')}
        data-testid="toast-dismiss"
        onclick={() => toasts.dismiss(toast.id)}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <line x1="18" y1="6" x2="6" y2="18" />
          <line x1="6" y1="6" x2="18" y2="18" />
        </svg>
      </button>
    </div>
    {#if toast.action || toast.href}
      <div class="mt-2 flex items-center gap-3">
        {#if toast.action}
          <button
            type="button"
            class="rounded-md border border-border bg-surface px-2.5 py-1 text-xs font-medium hover:bg-state-hover"
            data-testid="toast-action"
            onclick={() => void runAction(toast.id, toast.action!.run)}
          >
            {toast.action.label}
          </button>
        {/if}
        {#if toast.href}
          <a
            href={toast.href}
            class="text-xs underline underline-offset-2 hover:no-underline"
            data-testid="toast-link"
            onclick={() => toasts.dismiss(toast.id)}
          >
            {toast.linkLabel ?? toast.href}
          </a>
        {/if}
      </div>
    {/if}
  </div>
{/each}
