<script lang="ts">
  // Public profile by stable ref (#478, ADR 0070). The identifier path
  // used by notifications and cross-references.
  import { page } from '$app/state';
  import UserProfile from '$components/UserProfile.svelte';

  const ref = $derived(Number(page.params.ref));
</script>

<UserProfile {ref} />
