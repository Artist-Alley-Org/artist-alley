<script lang="ts">
  // Full-page drop-zone overlay. Visible only while the user is
  // actively dragging files over the window. Mounted once in
  // +layout.svelte; the upload store's installGlobalDragListeners
  // drives dragDepth.

  import { upload } from '$stores/upload.svelte';

  const visible = $derived(upload.dragDepth > 0);
</script>

{#if visible}
  <!-- pointer-events-none so the dragenter/leave events from the
       window-level listener can still fire on whatever's underneath;
       the actual drop is captured by the window listener too. -->
  <div
    class="pointer-events-none fixed inset-0 z-[100] flex items-center justify-center bg-black/70 backdrop-blur-sm"
    aria-hidden="true"
  >
    <div class="rounded-2xl border-2 border-dashed border-white/60 bg-white/5 px-12 py-10 text-center text-white shadow-2xl">
      <div class="mx-auto mb-3 flex h-14 w-14 items-center justify-center rounded-full bg-white/10">
        <svg xmlns="http://www.w3.org/2000/svg" width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
          <polyline points="17 8 12 3 7 8" />
          <line x1="12" y1="3" x2="12" y2="15" />
        </svg>
      </div>
      <p class="text-lg font-medium">Drop to upload</p>
      <p class="mt-1 text-sm text-white/70">Anywhere on the page works.</p>
    </div>
  </div>
{/if}
