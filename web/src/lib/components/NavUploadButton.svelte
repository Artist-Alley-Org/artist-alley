<script lang="ts">
  // Navbar "Upload" button. Icon + label on desktop, icon-only at
  // narrow widths. Opens the upload modal in empty state (the user
  // can then drop / browse files inside it). Drag-anywhere is the
  // faster path; this is the discoverable one.

  import { upload } from '$stores/upload.svelte';
  import { page } from '$app/state';

  function open() {
    // Context prefill: a collection page passes its UUID. The
    // route doesn't exist yet, but the regex anticipates it.
    const m = page.url.pathname.match(/^\/collections\/([0-9a-f-]{36})$/i);
    upload.open_({ collectionId: m ? m[1] : null });
  }
</script>

<button
  type="button"
  onclick={open}
  class="inline-flex items-center gap-1.5 rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white shadow-sm transition-colors hover:bg-accent/90"
  title="Upload (or drop files anywhere)"
>
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
    <polyline points="17 8 12 3 7 8" />
    <line x1="12" y1="3" x2="12" y2="15" />
  </svg>
  <span class="hidden md:inline">Upload</span>
</button>
