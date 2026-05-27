<script lang="ts">
  // Post detail modal. Used in two URL modes:
  //
  //   1. Overlay-on-feed: browse renders this when ?post={id} is set
  //      in the URL. Backdrop click / ESC closes the modal back to
  //      the bare feed.
  //   2. Standalone: /posts/[id]/+page.svelte renders this as the
  //      whole page (for shareable / bookmarkable URLs, cmd-click
  //      "open in new tab", direct nav from a search engine).
  //
  // The visual chrome is identical in both modes; only the close
  // behavior differs. Standalone mode goes back to / on close (or
  // history.back() if there's a referrer in our app).
  //
  // F-1 scope: shell only — backdrop, dialog element, ESC handler,
  // scroll-lock on body, focus trap via the native <dialog> element.
  // The actual content (preview / sidebar / member strip) lands in
  // F-2. F-3 wires likes + comments.

  import { onMount, onDestroy } from 'svelte';

  interface Props {
    postId: string;
    /**
     * Closes the modal. Browse-page caller passes a goto('/') stub.
     * Standalone-page caller passes a history-aware fallback.
     */
    onClose: () => void;
    /**
     * Standalone mode shows a tiny "← Back" affordance instead of a
     * close button (the standalone page is the modal, there's nothing
     * to close to). Overlay mode (default) shows the ✕ button.
     */
    standalone?: boolean;
  }

  let { postId, onClose, standalone = false }: Props = $props();

  let dialogEl: HTMLDialogElement | undefined = $state();

  // Open the native <dialog> in modal mode on mount. This gives us:
  //   - Native ESC-to-close (we trap it to call our onClose so we
  //     can update the URL too)
  //   - Built-in focus trap
  //   - Backdrop pseudo-element we can style via ::backdrop
  //   - Inert-ifies the content behind it for screen readers
  onMount(() => {
    dialogEl?.showModal();
    document.body.classList.add('overflow-hidden');
  });

  onDestroy(() => {
    document.body.classList.remove('overflow-hidden');
  });

  function handleDialogClose() {
    // Browser fired the native close (ESC). Let the parent close the
    // URL state too. Guard against the showModal/close race during
    // unmount.
    if (dialogEl?.open === false) {
      onClose();
    }
  }

  function handleBackdropClick(e: MouseEvent) {
    // The native <dialog> backdrop covers the area outside its
    // content box. A click whose target is the dialog element
    // itself (not its children) is a backdrop click.
    if (e.target === dialogEl) {
      onClose();
    }
  }

  function handleClose() {
    dialogEl?.close();
    onClose();
  }
</script>

<svelte:head>
  <!-- Suppress browser autofocus on form inputs when the modal opens. -->
</svelte:head>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dialogEl}
  onclose={handleDialogClose}
  onclick={handleBackdropClick}
  class="post-modal m-0 max-h-none max-w-none w-full h-full bg-transparent p-0 backdrop:bg-black/80 backdrop:backdrop-blur-sm"
  aria-labelledby="post-modal-title"
>
  <div
    class="relative mx-auto flex h-full w-full max-w-screen-2xl flex-col overflow-hidden bg-surface text-fg shadow-2xl sm:my-4 sm:h-[calc(100vh-2rem)] sm:rounded-lg"
    role="presentation"
  >
    <!-- Close / back button. Standalone mode uses ← back; overlay uses ✕. -->
    <button
      type="button"
      onclick={handleClose}
      class="absolute right-4 top-4 z-20 inline-flex h-9 w-9 items-center justify-center rounded-full bg-black/60 text-white backdrop-blur-sm transition-colors hover:bg-black/80"
      aria-label={standalone ? 'Back' : 'Close'}
    >
      {#if standalone}
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="m15 18-6-6 6-6" />
        </svg>
      {:else}
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M18 6 6 18M6 6l12 12" />
        </svg>
      {/if}
    </button>

    <!-- F-1 placeholder. F-2 replaces this with the ArtStation-style
         layout: preview pane left, sidebar right, member strip bottom. -->
    <div class="flex flex-1 items-center justify-center">
      <div class="text-center text-fg-muted">
        <p id="post-modal-title" class="text-lg font-medium text-fg">
          Post detail
        </p>
        <p class="mt-2 text-sm">
          Post ID: <code class="rounded bg-surface-elevated px-1.5 py-0.5 text-xs">{postId}</code>
        </p>
        <p class="mt-1 text-xs">
          F-1 scaffold — preview / sidebar / member strip arrive in F-2.
        </p>
      </div>
    </div>
  </div>
</dialog>

<style>
  /* Override the browser's default dialog reset so our flex layout
     stretches edge-to-edge. The element starts hidden until showModal()
     fires in onMount. */
  dialog.post-modal {
    border: none;
    inset: 0;
  }
  dialog.post-modal:not([open]) {
    display: none;
  }
</style>
