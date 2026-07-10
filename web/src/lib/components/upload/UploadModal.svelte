<script lang="ts">
  // Upload modal shell. Mounted once in +layout.svelte; opens when
  // upload.open flips true (via the navbar button OR a global drag-
  // drop). Composes the file row list, the post compose form, the
  // thumbnail picker, and the bottom action bar.

  import { upload } from '$stores/upload.svelte';
  import { t } from '$stores/lang.svelte';
  import UploadFileRow from './UploadFileRow.svelte';
  import PostComposeForm from './PostComposeForm.svelte';
  import ThumbnailPicker from './ThumbnailPicker.svelte';

  let dialogEl: HTMLDialogElement | undefined = $state();

  // Hidden file input behind the "Add files" button.
  let pickerEl: HTMLInputElement | undefined = $state();

  // Watch upload.open and drive the <dialog> showModal/close. Using
  // a $effect rather than the open prop on dialog so we get
  // ESC-closes-dialog semantics for free.
  $effect(() => {
    const el = dialogEl;
    if (!el) return;
    if (upload.open && !el.open) {
      el.showModal();
      document.body.classList.add('overflow-hidden');
    } else if (!upload.open && el.open) {
      el.close();
      document.body.classList.remove('overflow-hidden');
    }
  });

  function handleDialogClose() {
    // <dialog>'s native ESC-handler fires close. Mirror that into
    // the store so reopening doesn't fight a stale flag.
    if (!dialogEl?.open && upload.open) {
      upload.close();
    }
  }

  function handlePicked(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    if (input.files && input.files.length > 0) {
      upload.addFiles(input.files);
    }
    input.value = '';
  }

  async function handleSubmit() {
    await upload.submit();
  }

  // The submit button is the focal point. Wording adapts to whether
  // a post is being created and how many files are involved.
  const submitLabel = $derived(
    !upload.compose.enabled
      ? upload.readyRows.length === 1
        ? t('upload.modal.submit_save_one')
        : t('upload.modal.submit_save_many', { n: upload.readyRows.length })
      : upload.compose.mode === 'one-per-file'
        ? upload.readyRows.length === 1
          ? t('upload.modal.submit_create_posts_one')
          : t('upload.modal.submit_create_posts_many', { n: upload.readyRows.length })
        : t('upload.modal.submit_create_post'),
  );

  const submitDisabled = $derived(
    upload.composeBusy ||
      upload.anyInFlight ||
      upload.readyRows.length === 0,
  );
</script>

<!-- svelte-ignore a11y_click_events_have_key_events -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<dialog
  bind:this={dialogEl}
  onclose={handleDialogClose}
  class="upload-modal m-0 max-h-none max-w-none w-full h-full bg-transparent p-0 backdrop:bg-black/80 backdrop:backdrop-blur-sm"
  aria-labelledby="upload-modal-title"
>
  <div
    class="relative flex h-full w-full flex-col overflow-hidden bg-surface text-fg shadow-2xl sm:my-2 sm:h-[calc(100vh-1rem)] sm:rounded-lg"
  >
    <!-- Header -->
    <header class="flex items-center justify-between border-b border-border px-5 py-3">
      <h1 id="upload-modal-title" class="text-lg font-semibold">{t('nav.upload')}</h1>
      <button
        type="button"
        onclick={() => upload.close()}
        class="inline-flex h-9 w-9 items-center justify-center rounded-full bg-surface-elevated text-fg-muted hover:text-fg"
        aria-label={t('common.close')}
      >
        <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M18 6 6 18M6 6l12 12" />
        </svg>
      </button>
    </header>

    <!-- Body — single column on mobile, two-column on desktop. The
         file list (left) and the compose form (right) scroll
         independently so a long queue doesn't push the compose form
         off-screen. -->
    <div class="flex flex-1 flex-col overflow-hidden lg:flex-row">
      <!-- Left column: drop zone + file rows. Takes 60% of the width
           on desktop, full width on smaller viewports. -->
      <div class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto border-b border-border p-5 lg:basis-3/5 lg:border-b-0 lg:border-r">
        <button
          type="button"
          onclick={() => pickerEl?.click()}
          class="flex w-full flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed border-border bg-surface-elevated px-6 py-6 text-fg-muted transition-colors hover:border-accent hover:text-fg"
        >
          <svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
            <polyline points="17 8 12 3 7 8" />
            <line x1="12" y1="3" x2="12" y2="15" />
          </svg>
          <span class="text-sm font-medium">{t('upload.modal.dropzone_prompt')}</span>
        </button>
        <input
          bind:this={pickerEl}
          type="file"
          multiple
          class="hidden"
          onchange={handlePicked}
        />

        {#if upload.rows.length > 0}
          <div class="space-y-2">
            {#each upload.rows as row (row.id)}
              <UploadFileRow {row} />
            {/each}
          </div>
        {:else}
          <p class="rounded-md bg-surface-elevated/50 px-3 py-2 text-center text-xs text-fg-muted">
            {t('upload.modal.empty_state')}
          </p>
        {/if}
      </div>

      <!-- Right column: post compose + thumbnail picker. Independent
           scroll so the file list and compose form never fight. -->
      <div class="flex min-h-0 flex-col gap-4 overflow-y-auto p-5 lg:basis-2/5 lg:min-w-[28rem]">
        <PostComposeForm />

        {#if upload.compose.enabled}
          <ThumbnailPicker />
        {/if}

        {#if upload.composeError}
          <p role="alert" class="rounded border border-danger/40 bg-danger-container px-3 py-2 text-sm text-danger">
            {upload.composeError}
          </p>
        {/if}
      </div>
    </div>

    <!-- Footer -->
    <footer class="flex items-center justify-between gap-3 border-t border-border bg-surface-elevated px-5 py-3">
      <p class="text-xs text-fg-muted">
        {#if upload.anyInFlight}
          {t('upload.modal.status_uploading', { ready: upload.readyRows.length, total: upload.rows.length })}
        {:else}
          {t('upload.modal.status_ready', { ready: upload.readyRows.length, total: upload.rows.length })}
        {/if}
      </p>
      <div class="flex items-center gap-2">
        <button
          type="button"
          onclick={() => upload.reset()}
          class="rounded-md border border-border px-3 py-1.5 text-sm text-fg-muted hover:bg-surface hover:text-fg"
        >
          {t('common.cancel')}
        </button>
        <button
          type="button"
          onclick={handleSubmit}
          disabled={submitDisabled}
          class="rounded-md bg-accent px-4 py-1.5 text-sm font-medium text-white shadow transition-colors hover:bg-accent/90 disabled:cursor-not-allowed disabled:bg-accent/40"
        >
          {upload.composeBusy ? t('common.saving') : submitLabel}
        </button>
      </div>
    </footer>
  </div>
</dialog>

<style>
  dialog.upload-modal {
    border: none;
    inset: 0;
  }
  dialog.upload-modal:not([open]) {
    display: none;
  }
</style>
