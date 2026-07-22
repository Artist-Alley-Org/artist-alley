<script lang="ts">
  // Public profile by username (#478, ADR 0070). The human-facing
  // permalink linked from author names, the user menu and the mobile nav.
  import { page } from '$app/state';
  import UserProfile from '$components/UserProfile.svelte';
</script>

<UserProfile username={page.params.username} />
