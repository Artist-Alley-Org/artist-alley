<script lang="ts">
  import { auth } from '$stores/auth.svelte';

  const greeting = $derived(() => {
    const name = auth.user?.fullname || auth.user?.username || 'there';
    const hour = new Date().getHours();
    const period = hour < 12 ? 'morning' : hour < 18 ? 'afternoon' : 'evening';
    return `Good ${period}, ${name}`;
  });
</script>

<svelte:head>
  <title>artist-alley</title>
</svelte:head>

<div class="flex-1 flex flex-col items-center justify-center px-6 py-16 text-center">
  <div class="max-w-2xl space-y-6">
    <h1 class="text-3xl font-semibold tracking-tight">{greeting()}</h1>
    <p class="text-fg-muted">
      The browse page lands in Phase 1.13.D — once it does, this is where the recent-art grid renders.
    </p>
    <div class="text-xs text-fg-muted pt-4">
      Signed in as <code class="font-mono">{auth.user?.username}</code>
      (ref {auth.user?.ref}, usergroup {auth.user?.usergroup ?? '—'})
    </div>
  </div>
</div>
