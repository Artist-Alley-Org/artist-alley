<script lang="ts">
  // Navbar "Upload" button. Icon + label on desktop, icon-only at
  // narrow widths. Opens the upload modal in empty state (the user
  // can then drop / browse files inside it). Drag-anywhere is the
  // faster path; this is the discoverable one.

  import { upload } from '$stores/upload.svelte';
  import { page } from '$app/state';
  import { t } from '$stores/lang.svelte';

  function open() {
    // Context prefill: a collection page passes its UUID. The
    // route doesn't exist yet, but the regex anticipates it.
    const m = page.url.pathname.match(/^\/collections\/([0-9a-f-]{36})$/i);
    upload.open_({ collectionId: m ? m[1] : null });
  }
</script>

<!-- Icon-only round button — matches the messages / notifications
     visual rhythm in the right nav cluster. The accent fill keeps it
     as the primary call-to-action; companion icons stay neutral. -->
<button
  type="button"
  onclick={open}
  data-testid="nav-upload-button"
  class="inline-flex h-9 w-9 items-center justify-center rounded-full bg-accent text-on-accent shadow-sm transition-colors hover:bg-accent/90"
  title={t('nav.upload_button_title')}
  aria-label={t('nav.upload')}
>
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
    <polyline points="17 8 12 3 7 8" />
    <line x1="12" y1="3" x2="12" y2="15" />
  </svg>
</button>
