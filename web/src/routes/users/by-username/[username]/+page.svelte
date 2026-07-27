<script lang="ts">
  // Public profile by username (#478, ADR 0070). The human-facing
  // permalink linked from author names, the user menu and the mobile nav.
  import { page } from '$app/state';
  import UserProfile from '$components/UserProfile.svelte';
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';

  // Come back from a post or asset where you left off (#584). Offset
  // only: the profile loads a fixed first slice of posts / assets /
  // collections with no "load more", so the page is the same height
  // once its fetch resolves.
  export const snapshot = createScrollSnapshot();
</script>

<UserProfile username={page.params.username} />
