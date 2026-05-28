<script lang="ts">
  // Navbar messages icon. Pill count shows unread; clicking will
  // (eventually) open a right-side slide-in inbox.
  //
  // STUB for Phase 1.16: backend has no messages surface yet, so
  // the count is hard-zero and the button is disabled with a
  // "Coming soon" title. The icon + pill primitive are ready for
  // the real wiring as soon as the messages backend lands.

  import Pill from '$components/Pill.svelte';

  // const unreadCount = 0; // future: from a store
</script>

<button
  type="button"
  disabled
  title="Messages — coming soon"
  aria-label="Messages"
  class="relative inline-flex h-9 w-9 cursor-not-allowed items-center justify-center rounded-full text-fg-muted/60"
>
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <path d="M4 4h16c1.1 0 2 .9 2 2v12c0 1.1-.9 2-2 2H4c-1.1 0-2-.9-2-2V6c0-1.1.9-2 2-2z" />
    <polyline points="22,6 12,13 2,6" />
  </svg>
  <Pill count={0} />
</button>
