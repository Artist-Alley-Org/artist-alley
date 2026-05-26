<script lang="ts">
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { auth } from '$stores/auth.svelte';
  import Button from '$components/Button.svelte';
  import TextField from '$components/TextField.svelte';
  import Alert from '$components/Alert.svelte';

  let username = $state('');
  let password = $state('');
  let submitting = $state(false);
  let error = $state<string | null>(null);

  async function onSubmit(e: SubmitEvent) {
    e.preventDefault();
    if (submitting) return;
    error = null;
    submitting = true;
    try {
      await auth.login(username, password);
      const next = page.url.searchParams.get('next') ?? '/';
      await goto(next);
    } catch (err) {
      error = err instanceof Error ? err.message : 'Sign-in failed';
    } finally {
      submitting = false;
    }
  }
</script>

<svelte:head>
  <title>Sign in — artist-alley</title>
</svelte:head>

<div class="flex-1 flex items-center justify-center px-6 py-12">
  <div class="w-full max-w-sm space-y-8">
    <div class="text-center space-y-2">
      <div class="inline-block h-10 w-10 rounded-lg bg-accent" aria-hidden="true"></div>
      <h1 class="text-2xl font-semibold tracking-tight">Sign in</h1>
      <p class="text-sm text-fg-muted">artist-alley</p>
    </div>

    <form class="space-y-4" onsubmit={onSubmit}>
      {#if error}
        <Alert tone="error">{error}</Alert>
      {/if}

      <TextField
        label="Username or email"
        name="username"
        autocomplete="username"
        required
        autofocus
        bind:value={username}
        disabled={submitting}
      />

      <TextField
        label="Password"
        name="password"
        type="password"
        autocomplete="current-password"
        required
        bind:value={password}
        disabled={submitting}
      />

      <Button type="submit" variant="primary" fullWidth loading={submitting}>
        Sign in
      </Button>
    </form>

    <p class="text-center text-xs text-fg-muted">
      Self-hosted. No account creation here — ask an admin.
    </p>
  </div>
</div>
