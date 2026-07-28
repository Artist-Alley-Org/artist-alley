<script lang="ts">
  // Public profile by stable ref (#478, ADR 0070). The identifier path
  // used by notifications and cross-references.
  import { page } from '$app/state';
  import UserProfile from '$components/UserProfile.svelte';
  import { createScrollSnapshot } from '$lib/util/scrollSnapshot';

  const ref = $derived(Number(page.params.ref));

  // See the by-username twin: offset-only restoration (#584).
  export const snapshot = createScrollSnapshot();
</script>

<UserProfile {ref} />
