<script lang="ts">
  import { theme, type ThemePref } from '$stores/theme.svelte';

  const options: { value: ThemePref; label: string }[] = [
    { value: 'light', label: 'Light' },
    { value: 'system', label: 'System' },
    { value: 'dark', label: 'Dark' },
  ];
</script>

<main class="min-h-screen flex flex-col items-center justify-center p-8">
  <div class="max-w-md w-full text-center space-y-6">
    <h1 class="text-3xl font-semibold tracking-tight">artist-alley</h1>
    <p class="text-fg-muted">
      Frontend scaffold. Phase 1.13.A — SvelteKit + Tailwind + theme. The
      browse page lands in 1.13.D.
    </p>

    <div class="inline-flex rounded-lg border border-border bg-surface-elevated p-1">
      {#each options as opt (opt.value)}
        <button
          class="px-3 py-1.5 text-sm rounded-md transition-colors"
          class:bg-accent={theme.pref === opt.value}
          class:text-accent-fg={theme.pref === opt.value}
          class:text-fg-muted={theme.pref !== opt.value}
          onclick={() => theme.set(opt.value)}
        >
          {opt.label}
        </button>
      {/each}
    </div>

    <p class="text-xs text-fg-muted">
      Resolved theme: <code class="font-mono">{theme.resolved}</code>
    </p>
  </div>
</main>
