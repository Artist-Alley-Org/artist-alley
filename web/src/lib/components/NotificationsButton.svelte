<script lang="ts">
  // Navbar notifications bell. Mirror of MessagesButton — stub until
  // the notifications backend lands. Pill count is hard-zero today;
  // wire it to a notifications store when the feature ships.

  import Pill from '$components/Pill.svelte';
  import { t } from '$stores/lang.svelte';
</script>

<button
  type="button"
  disabled
  title={t('nav.notifications_coming_soon')}
  aria-label={t('nav.notifications')}
  class="relative inline-flex h-9 w-9 cursor-not-allowed items-center justify-center rounded-full text-fg-muted/60"
>
  <svg xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
    <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
    <path d="M13.73 21a2 2 0 0 1-3.46 0" />
  </svg>
  <Pill count={0} />
</button>
